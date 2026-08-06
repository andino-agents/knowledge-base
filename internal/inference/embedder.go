// Package inference holds the OpenAI-compatible clients: embeddings now,
// reranking in a later phase.
package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"
)

// Embedder calls an OpenAI-compatible /v1/embeddings endpoint.
//
// Contract, learned the hard way: there is NO fallback vector code path.
// Every error propagates; callers abort the affected document and the
// previous index state survives. A dummy-vector fallback once tricked a
// dimension check into wiping an entire collection.
type Embedder struct {
	BaseURL    string // e.g. http://127.0.0.1:8080/v1
	APIKey     string
	Model      string
	Dimensions int
	BatchSize  int
	MaxRetries int
	Client     *http.Client
	Logger     *slog.Logger
}

type embeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (e *Embedder) client() *http.Client {
	if e.Client != nil {
		return e.Client
	}
	return http.DefaultClient
}

func (e *Embedder) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

// Embed returns one vector per input text, batching requests internally.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	batchSize := e.BatchSize
	if batchSize <= 0 {
		batchSize = 32
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := min(start+batchSize, len(texts))
		vecs, err := e.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (e *Embedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embeddingRequest{Model: e.Model, Input: texts})
	if err != nil {
		return nil, err
	}

	maxRetries := e.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 4
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		vecs, retryable, err := e.doRequest(ctx, body, len(texts))
		if err == nil {
			return vecs, nil
		}
		lastErr = err
		if !retryable || attempt >= maxRetries {
			return nil, fmt.Errorf("embeddings: %w (after %d attempt(s))", lastErr, attempt+1)
		}
		delay := backoff(attempt)
		e.logger().Warn("embedding request failed, retrying",
			"attempt", attempt+1, "delay", delay, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// doRequest performs one HTTP call. retryable reports whether the failure is
// transient (5xx, 429, network) as opposed to a caller bug (4xx) or a
// response that violates the contract (wrong count/dimension) — those fail
// immediately.
func (e *Embedder) doRequest(ctx context.Context, body []byte, wantCount int) (vecs [][]float32, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := e.client().Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		retryable := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return nil, retryable, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, e.BaseURL, snippet)
	}

	var parsed embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, true, fmt.Errorf("decoding response: %w", err)
	}
	if len(parsed.Data) != wantCount {
		return nil, false, fmt.Errorf("endpoint returned %d embeddings for %d inputs", len(parsed.Data), wantCount)
	}
	vecs = make([][]float32, wantCount)
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= wantCount {
			return nil, false, fmt.Errorf("endpoint returned out-of-range index %d", d.Index)
		}
		if len(d.Embedding) != e.Dimensions {
			return nil, false, fmt.Errorf("endpoint returned dimension %d, config says %d — wrong model behind %q?",
				len(d.Embedding), e.Dimensions, e.Model)
		}
		vecs[d.Index] = d.Embedding
	}
	for i, v := range vecs {
		if v == nil {
			return nil, false, fmt.Errorf("endpoint response missing embedding for input %d", i)
		}
	}
	return vecs, false, nil
}

// WaitReady polls the endpoint with a real 1-text embedding request until it
// answers correctly or the timeout expires. Inference servers can take
// minutes to load models after a reboot; indexing must not start before the
// endpoint is truly ready.
func (e *Embedder) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := e.embedProbe(probeCtx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("embeddings endpoint %s not ready after %s: %w", e.BaseURL, timeout, err)
		}
		e.logger().Info("waiting for embeddings endpoint", "base_url", e.BaseURL, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (e *Embedder) embedProbe(ctx context.Context) ([][]float32, error) {
	body, _ := json.Marshal(embeddingRequest{Model: e.Model, Input: []string{"ping"}})
	vecs, _, err := e.doRequest(ctx, body, 1)
	return vecs, err
}

func backoff(attempt int) time.Duration {
	base := 250 * time.Millisecond << attempt
	if base > 30*time.Second {
		base = 30 * time.Second
	}
	jitter := time.Duration(rand.Int64N(int64(base / 4)))
	return base + jitter
}
