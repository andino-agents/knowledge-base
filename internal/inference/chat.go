package inference

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Chat is a minimal OpenAI-compatible /v1/chat/completions client for
// index-time work (contextual retrieval). Same failure contract as the
// embedder: transient errors retry with backoff, everything else propagates.
type Chat struct {
	BaseURL    string
	APIKey     string
	Model      string
	MaxTokens  int
	MaxRetries int
	// ExtraBody is merged into every request body (provider-specific knobs
	// like chat_template_kwargs).
	ExtraBody map[string]any
	Client    *http.Client
	Logger    *slog.Logger
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (c *Chat) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	// Index-time generation over long documents can be slow; be generous.
	return &http.Client{Timeout: 5 * time.Minute}
}

func (c *Chat) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// Complete sends one system+user exchange and returns the trimmed response.
func (c *Chat) Complete(ctx context.Context, system, user string) (string, error) {
	return c.complete(ctx, system, any(user))
}

// CompleteWithImage sends a user turn carrying an image (OpenAI content-array
// format with a base64 data URI). The chat model must be vision-capable.
func (c *Chat) CompleteWithImage(ctx context.Context, system, user string, image []byte, mime string) (string, error) {
	dataURI := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)
	content := []map[string]any{
		{"type": "text", "text": user},
		{"type": "image_url", "image_url": map[string]any{"url": dataURI}},
	}
	return c.complete(ctx, system, any(content))
}

func (c *Chat) complete(ctx context.Context, system string, userContent any) (string, error) {
	payload := map[string]any{
		"model": c.Model,
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": userContent},
		},
		"temperature": 0,
	}
	if c.MaxTokens > 0 {
		payload["max_tokens"] = c.MaxTokens
	}
	for k, v := range c.ExtraBody {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	maxRetries := c.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 4
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		text, retryable, err := c.doRequest(ctx, body)
		if err == nil {
			return text, nil
		}
		lastErr = err
		if !retryable || attempt >= maxRetries {
			return "", fmt.Errorf("chat: %w (after %d attempt(s))", lastErr, attempt+1)
		}
		delay := backoff(attempt)
		c.logger().Warn("chat request failed, retrying", "attempt", attempt+1, "delay", delay, "error", err)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delay):
		}
	}
}

// WaitReady polls the endpoint with a real minimal completion until the
// backend answers for this model, or the timeout expires. Router-style servers
// load models lazily and answer 503 "Loading model" for minutes after a
// restart; index-time contextualisation must not start before the chat model is
// actually live. Mirrors Embedder.WaitReady.
func (c *Chat) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := c.probe(probeCtx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("chat endpoint %s not ready after %s: %w", c.BaseURL, timeout, err)
		}
		c.logger().Info("waiting for chat endpoint", "base_url", c.BaseURL, "model", c.Model, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// probe asks the backend for a one-token completion and only checks that the
// request was accepted. It deliberately ignores the response body: a
// thinking-first model can spend a one-token budget entirely on reasoning and
// return empty content, which is a real error for indexing but says nothing
// about whether the model is loaded.
func (c *Chat) probe(ctx context.Context) error {
	payload := map[string]any{
		"model":       c.Model,
		"messages":    []map[string]any{{"role": "user", "content": "ping"}},
		"max_tokens":  1,
		"temperature": 0,
	}
	for k, v := range c.ExtraBody {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, _, err = c.doRequestOpts(ctx, body, true)
	return err
}

func (c *Chat) doRequest(ctx context.Context, body []byte) (text string, retryable bool, err error) {
	return c.doRequestOpts(ctx, body, false)
}

func (c *Chat) doRequestOpts(ctx context.Context, body []byte, probeOnly bool) (text string, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return "", true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		retryable := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return "", retryable, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, c.BaseURL, snippet)
	}
	if probeOnly {
		return "", false, nil
	}
	var parsed chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", true, fmt.Errorf("decoding response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", false, fmt.Errorf("empty choices in response")
	}
	text = strings.TrimSpace(parsed.Choices[0].Message.Content)
	if text == "" {
		// Thinking-first models can burn the whole token budget on
		// reasoning and return empty content. That must surface, not get
		// silently stored: see ChatModel.ExtraBody to disable thinking.
		return "", false, fmt.Errorf("empty content in response (thinking-first model? set extra_body chat_template_kwargs.enable_thinking=false)")
	}
	return text, false, nil
}
