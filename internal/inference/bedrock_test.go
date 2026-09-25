package inference

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"
)

type fakeBedrock struct {
	invokes   []titanRequest
	converses []*bedrockruntime.ConverseInput
	// failures are returned, in order, before any call succeeds.
	failures []error
	dims     int
	reply    string
}

func (f *fakeBedrock) fail() error {
	if len(f.failures) == 0 {
		return nil
	}
	err := f.failures[0]
	f.failures = f.failures[1:]
	return err
}

func (f *fakeBedrock) InvokeModel(_ context.Context, in *bedrockruntime.InvokeModelInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	var req titanRequest
	if err := json.Unmarshal(in.Body, &req); err != nil {
		return nil, err
	}
	f.invokes = append(f.invokes, req)
	vec := make([]float32, f.dims)
	// The first component tags the vector with its input, so order is checkable.
	vec[0] = float32(len(req.InputText))
	body, _ := json.Marshal(titanResponse{Embedding: vec})
	return &bedrockruntime.InvokeModelOutput{Body: body}, nil
}

func (f *fakeBedrock) Converse(_ context.Context, in *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	f.converses = append(f.converses, in)
	return &bedrockruntime.ConverseOutput{Output: &types.ConverseOutputMemberMessage{Value: types.Message{
		Role:    types.ConversationRoleAssistant,
		Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: f.reply}},
	}}}, nil
}

func apiError(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code}
}

func titanEmbedder(f *fakeBedrock) *Embedder {
	return &Embedder{
		Model: "amazon.titan-embed-text-v2:0", Dimensions: f.dims, MaxRetries: 3,
		Bedrock: &Bedrock{Region: "us-east-1", api: f},
	}
}

func TestBedrockEmbedsOneTextPerCallAtTheConfiguredSize(t *testing.T) {
	f := &fakeBedrock{dims: 1024}
	vecs, err := titanEmbedder(f).Embed(context.Background(), []string{"a", "bbb", "cc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.invokes) != 3 {
		t.Fatalf("want one call per text, got %d", len(f.invokes))
	}
	for i, want := range []float32{1, 3, 2} {
		if vecs[i][0] != want {
			t.Fatalf("vector %d belongs to another input: %v", i, vecs[i][0])
		}
	}
	// Titan v2 defaults to 1024 but also serves 256 and 512: the size has to be
	// asked for, or an index built at 512 would silently receive 1024.
	if f.invokes[0].Dimensions != 1024 || !f.invokes[0].Normalize {
		t.Fatalf("request did not carry the configured size: %+v", f.invokes[0])
	}
}

func TestBedrockEmbeddingOfTheWrongSizeFailsWithoutRetrying(t *testing.T) {
	f := &fakeBedrock{dims: 512}
	e := titanEmbedder(f)
	e.Dimensions = 1024
	_, err := e.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "dimension 512") {
		t.Fatalf("want a dimension error, got %v", err)
	}
	if len(f.invokes) != 1 {
		t.Fatalf("a contract violation must not retry, got %d calls", len(f.invokes))
	}
}

func TestBedrockThrottlingRetriesAndValidationDoesNot(t *testing.T) {
	f := &fakeBedrock{dims: 8, failures: []error{apiError("ThrottlingException")}}
	if _, err := titanEmbedder(f).Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("throttling should have been retried: %v", err)
	}

	f = &fakeBedrock{dims: 8, failures: []error{apiError("ValidationException"), apiError("ValidationException")}}
	_, err := titanEmbedder(f).Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "after 1 attempt") {
		t.Fatalf("a validation error must fail on the first attempt, got %v", err)
	}
}

func TestBedrockRefusesEmbeddingModelsItCannotSize(t *testing.T) {
	f := &fakeBedrock{dims: 1024}
	e := titanEmbedder(f)
	e.Model = "cohere.embed-multilingual-v3"
	_, err := e.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "titan-embed-text-v2") {
		t.Fatalf("want a clear refusal, got %v", err)
	}
	if len(f.invokes) != 0 {
		t.Fatal("the refusal must come before any call")
	}
}

func TestBedrockChatSendsSystemTokensAndExtraFieldsButNoTemperature(t *testing.T) {
	f := &fakeBedrock{reply: "  a summary  "}
	c := &Chat{
		Model: "us.anthropic.claude-haiku-4-5", MaxTokens: 200,
		ExtraBody: map[string]any{"top_k": 5},
		Bedrock:   &Bedrock{Region: "us-east-1", api: f},
	}
	got, err := c.Complete(context.Background(), "be brief", "the chunk")
	if err != nil {
		t.Fatal(err)
	}
	if got != "a summary" {
		t.Fatalf("reply not trimmed: %q", got)
	}
	in := f.converses[0]
	if sys := in.System[0].(*types.SystemContentBlockMemberText).Value; sys != "be brief" {
		t.Fatalf("system prompt lost: %q", sys)
	}
	if *in.InferenceConfig.MaxTokens != 200 {
		t.Fatalf("max tokens lost: %d", *in.InferenceConfig.MaxTokens)
	}
	if in.InferenceConfig.Temperature != nil {
		t.Fatal("temperature must not be sent: recent Anthropic models on Bedrock reject it")
	}
	if in.AdditionalModelRequestFields == nil {
		t.Fatal("extra_body did not travel as additional model fields")
	}
}

func TestBedrockChatCarriesImagesAndRejectsUnknownTypes(t *testing.T) {
	f := &fakeBedrock{reply: "red"}
	c := &Chat{Model: "m", Bedrock: &Bedrock{Region: "us-east-1", api: f}}
	if _, err := c.CompleteWithImage(context.Background(), "", "what color", []byte{1, 2}, "image/png"); err != nil {
		t.Fatal(err)
	}
	img := f.converses[0].Messages[0].Content[1].(*types.ContentBlockMemberImage).Value
	if img.Format != types.ImageFormatPng {
		t.Fatalf("wrong format: %s", img.Format)
	}
	if _, err := c.CompleteWithImage(context.Background(), "", "x", []byte{1}, "image/tiff"); err == nil {
		t.Fatal("tiff is not a Bedrock image type and must be refused")
	}
}

func TestBedrockEmptyReplyIsAnErrorButTheProbeAcceptsIt(t *testing.T) {
	f := &fakeBedrock{reply: ""}
	c := &Chat{Model: "m", MaxRetries: 1, Bedrock: &Bedrock{Region: "us-east-1", api: f}}
	if _, err := c.Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("an empty summary must surface, not be stored")
	}
	// The probe only proves the model answers, like the HTTP one.
	if err := c.probe(context.Background()); err != nil {
		t.Fatalf("probe should accept an empty reply: %v", err)
	}
}

func TestBedrockErrorsWithoutAnAPICodeRetry(t *testing.T) {
	if !bedrockRetryable(errors.New("dial tcp: connection refused")) {
		t.Fatal("a failure that never reached the service should retry")
	}
	if bedrockRetryable(apiError("AccessDeniedException")) {
		t.Fatal("a missing permission will not fix itself on retry")
	}
}
