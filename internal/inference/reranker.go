package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
)

// Reranker calls an OpenAI-compatible /v1/rerank endpoint (llama-server,
// Jina, Cohere-compatible shapes). Reranking is an enhancement: callers must
// degrade gracefully to fusion order when it fails.
type Reranker struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
	Logger  *slog.Logger
}

// Ranked is one rerank result. Score is normalized to (0,1) via sigmoid so
// raw cross-encoder logits stay comparable with min_score filters.
type Ranked struct {
	Index int
	Score float64
}

type rerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

type rerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

func (r *Reranker) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return http.DefaultClient
}

// Rerank scores documents against the query and returns them best-first.
func (r *Reranker) Rerank(ctx context.Context, query string, documents []string, topN int) ([]Ranked, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(rerankRequest{Model: r.Model, Query: query, Documents: documents, TopN: topN})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.BaseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.APIKey)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("rerank: HTTP %d: %s", resp.StatusCode, snippet)
	}
	var parsed rerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("rerank: decoding response: %w", err)
	}
	out := make([]Ranked, 0, len(parsed.Results))
	for _, res := range parsed.Results {
		if res.Index < 0 || res.Index >= len(documents) {
			return nil, fmt.Errorf("rerank: out-of-range index %d", res.Index)
		}
		out = append(out, Ranked{Index: res.Index, Score: sigmoid(res.RelevanceScore)})
	}
	// Endpoints are supposed to return best-first, but do not rely on it.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Score > out[j-1].Score; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	return out, nil
}

func sigmoid(x float64) float64 { return 1.0 / (1.0 + math.Exp(-x)) }
