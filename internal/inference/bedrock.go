package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"
)

// bedrockAPI is the slice of the Bedrock runtime client this package uses, so
// tests can stand in for AWS.
type bedrockAPI interface {
	InvokeModel(ctx context.Context, in *bedrockruntime.InvokeModelInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error)
	Converse(ctx context.Context, in *bedrockruntime.ConverseInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

// Bedrock is the transport for an inference backend of type bedrock. It has no
// key of its own: credentials come from the AWS default chain, which on a VM is
// the instance role, so a config file never holds one.
type Bedrock struct {
	Region string
	api    bedrockAPI
}

// NewBedrock builds a client for region with the SDK's own retries turned off.
// Embedder and Chat already retry transient failures with backoff; leaving the
// SDK's on as well would multiply every attempt by three.
func NewBedrock(ctx context.Context, region string) (*Bedrock, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithRetryMaxAttempts(1),
	)
	if err != nil {
		return nil, fmt.Errorf("bedrock: loading AWS config: %w", err)
	}
	return &Bedrock{Region: region, api: bedrockruntime.NewFromConfig(cfg)}, nil
}

func (b *Bedrock) endpoint() string { return "bedrock:" + b.Region }

// titanEmbedModel is the only embedding family accepted. Titan v2 takes the
// output size as a request field, so the configured dimensions are honoured
// instead of assumed; v1 and the Cohere models have fixed sizes or need a
// query/document input type this interface does not carry.
const titanEmbedModel = "amazon.titan-embed-text-v2"

type titanRequest struct {
	InputText  string `json:"inputText"`
	Dimensions int    `json:"dimensions"`
	Normalize  bool   `json:"normalize"`
}

type titanResponse struct {
	Embedding []float32 `json:"embedding"`
}

// embed returns one vector per text. Titan takes one text per call, so a
// batch is a loop; the first failure aborts the batch like any other backend.
func (b *Bedrock) embed(ctx context.Context, model string, dims int, texts []string) ([][]float32, bool, error) {
	if !strings.HasPrefix(model, titanEmbedModel) {
		return nil, false, fmt.Errorf("bedrock embeddings support %s only, got %q", titanEmbedModel, model)
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		body, err := json.Marshal(titanRequest{InputText: text, Dimensions: dims, Normalize: true})
		if err != nil {
			return nil, false, err
		}
		resp, err := b.api.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
			ModelId:     aws.String(model),
			Body:        body,
			ContentType: aws.String("application/json"),
			Accept:      aws.String("application/json"),
		})
		if err != nil {
			return nil, bedrockRetryable(err), fmt.Errorf("%s: %w", b.endpoint(), err)
		}
		var parsed titanResponse
		if err := json.Unmarshal(resp.Body, &parsed); err != nil {
			return nil, true, fmt.Errorf("decoding response: %w", err)
		}
		if len(parsed.Embedding) != dims {
			return nil, false, fmt.Errorf("endpoint returned dimension %d, config says %d: wrong model behind %q?",
				len(parsed.Embedding), dims, model)
		}
		out[i] = parsed.Embedding
	}
	return out, false, nil
}

// converse sends one system+user exchange. temperature is not sent: Bedrock
// rejects it with a ValidationException on recent Anthropic models (seen with
// Opus 5 on 2026-09-22), and index-time summaries do not need it pinned.
func (b *Bedrock) converse(ctx context.Context, c *Chat, system, user string, image []byte, mime string, maxTokens int) (string, bool, error) {
	content := []types.ContentBlock{&types.ContentBlockMemberText{Value: user}}
	if image != nil {
		format, err := imageFormat(mime)
		if err != nil {
			return "", false, err
		}
		content = append(content, &types.ContentBlockMemberImage{Value: types.ImageBlock{
			Format: format,
			Source: &types.ImageSourceMemberBytes{Value: image},
		}})
	}
	in := &bedrockruntime.ConverseInput{
		ModelId:  aws.String(c.Model),
		Messages: []types.Message{{Role: types.ConversationRoleUser, Content: content}},
	}
	if system != "" {
		in.System = []types.SystemContentBlock{&types.SystemContentBlockMemberText{Value: system}}
	}
	if maxTokens > 0 {
		in.InferenceConfig = &types.InferenceConfiguration{MaxTokens: aws.Int32(int32(maxTokens))}
	}
	if len(c.ExtraBody) > 0 {
		in.AdditionalModelRequestFields = document.NewLazyDocument(c.ExtraBody)
	}
	resp, err := b.api.Converse(ctx, in)
	if err != nil {
		return "", bedrockRetryable(err), fmt.Errorf("%s: %w", b.endpoint(), err)
	}
	msg, ok := resp.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return "", false, fmt.Errorf("%s: response carries no message", b.endpoint())
	}
	var text strings.Builder
	for _, block := range msg.Value.Content {
		if t, ok := block.(*types.ContentBlockMemberText); ok {
			text.WriteString(t.Value)
		}
	}
	return strings.TrimSpace(text.String()), false, nil
}

func imageFormat(mime string) (types.ImageFormat, error) {
	switch mime {
	case "image/png":
		return types.ImageFormatPng, nil
	case "image/jpeg", "image/jpg":
		return types.ImageFormatJpeg, nil
	case "image/gif":
		return types.ImageFormatGif, nil
	case "image/webp":
		return types.ImageFormatWebp, nil
	}
	return "", fmt.Errorf("bedrock: image type %q is not one of png, jpeg, gif or webp", mime)
}

// bedrockRetryable mirrors the HTTP rule the OpenAI path uses (5xx and 429
// retry, other 4xx do not) in the error codes Bedrock returns instead of
// statuses. A failure with no API code never reached the service, like a
// network error, and retries.
func bedrockRetryable(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return true
	}
	switch apiErr.ErrorCode() {
	case "ThrottlingException", "ServiceUnavailableException", "InternalServerException",
		"ModelNotReadyException", "ModelTimeoutException":
		return true
	}
	return false
}
