package inference

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRerankAgainstMockedLlamaServer exercises the llama-server /v1/rerank
// response shape until the real reranker GGUF is deployed.
func TestRerankAgainstMockedLlamaServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			http.NotFound(w, r)
			return
		}
		var req rerankRequest
		json.NewDecoder(r.Body).Decode(&req)
		// Score documents by naive containment of the query word, as raw
		// logits (llama-server returns unbounded scores).
		type res struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		}
		var results []res
		for i, d := range req.Documents {
			score := -4.0
			if len(d) > 0 && d[0] == 'R' { // "Relevant..." docs win
				score = 6.0
			}
			results = append(results, res{Index: i, RelevanceScore: score})
		}
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer srv.Close()

	r := &Reranker{BaseURL: srv.URL + "/v1", Model: "mock-rerank"}
	ranked, err := r.Rerank(context.Background(), "query", []string{
		"irrelevant one", "Relevant document", "another miss",
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranked) != 2 {
		t.Fatalf("got %d results, want 2", len(ranked))
	}
	if ranked[0].Index != 1 {
		t.Errorf("top index = %d, want 1", ranked[0].Index)
	}
	if ranked[0].Score <= 0.9 || ranked[1].Score >= 0.1 {
		t.Errorf("sigmoid normalization off: %+v", ranked)
	}
}

func TestRerankErrorsPropagate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	r := &Reranker{BaseURL: srv.URL + "/v1", Model: "mock"}
	if _, err := r.Rerank(context.Background(), "q", []string{"d"}, 1); err == nil {
		t.Fatal("expected error from 503")
	}
}
