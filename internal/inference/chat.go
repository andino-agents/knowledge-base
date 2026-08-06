package inference

import (
	"bytes"
	"context"
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
	Client     *http.Client
	Logger     *slog.Logger
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
	body, err := json.Marshal(chatRequest{
		Model: c.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0,
		MaxTokens:   c.MaxTokens,
	})
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

func (c *Chat) doRequest(ctx context.Context, body []byte) (text string, retryable bool, err error) {
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
	var parsed chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", true, fmt.Errorf("decoding response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", false, fmt.Errorf("empty choices in response")
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), false, nil
}
