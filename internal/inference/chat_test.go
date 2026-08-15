package inference

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// A router answers 503 "Loading model" for minutes after a restart. WaitReady
// must ride that out instead of letting indexing start against a dead model.
func TestChatWaitReadyRidesOutLoadingModel(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"message":"Loading model","code":503}}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
		})
	}))
	defer srv.Close()

	c := &Chat{BaseURL: srv.URL + "/v1", Model: "m"}
	if err := c.WaitReady(context.Background(), 60*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("probes = %d, want 3", got)
	}
}

// The probe asks for one token. A thinking-first model spends that token on
// reasoning and returns empty content, which Complete rightly treats as a hard
// error. Readiness must NOT inherit that: an empty answer still proves the
// model is loaded. Without this, WaitReady would spin until timeout on exactly
// the models this project runs.
func TestChatWaitReadyAcceptsEmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": ""}}},
		})
	}))
	defer srv.Close()

	c := &Chat{BaseURL: srv.URL + "/v1", Model: "m"}
	if err := c.WaitReady(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("WaitReady must not care about empty content: %v", err)
	}
}

// A 4xx is not a loading model; it is a misconfiguration (wrong model name,
// bad key) and no amount of waiting fixes it. It must surface as a timeout
// error carrying the cause, not hang forever.
func TestChatWaitReadyTimesOutOnPermanentError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"unknown model"}`)
	}))
	defer srv.Close()

	c := &Chat{BaseURL: srv.URL + "/v1", Model: "nope"}
	err := c.WaitReady(context.Background(), 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("want not-ready error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown model") {
		t.Errorf("error should carry the cause, got: %v", err)
	}
}
