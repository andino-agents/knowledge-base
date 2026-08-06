package inference

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatExtraBodyAndContent(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": " situated context "}}},
		})
	}))
	defer srv.Close()

	c := &Chat{
		BaseURL: srv.URL + "/v1", Model: "m", MaxTokens: 100,
		ExtraBody: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}},
	}
	text, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if text != "situated context" {
		t.Errorf("text = %q", text)
	}
	kw, _ := gotBody["chat_template_kwargs"].(map[string]any)
	if kw == nil || kw["enable_thinking"] != false {
		t.Errorf("extra_body not merged into request: %v", gotBody)
	}
}

// A thinking-first model that burns its budget returns empty content; that
// must be an error, never a silently stored empty context.
func TestChatEmptyContentIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "", "reasoning_content": "hmm..."}}},
		})
	}))
	defer srv.Close()
	c := &Chat{BaseURL: srv.URL + "/v1", Model: "m", MaxRetries: 1}
	_, err := c.Complete(context.Background(), "sys", "user")
	if err == nil || !strings.Contains(err.Error(), "empty content") {
		t.Fatalf("want empty-content error, got: %v", err)
	}
}
