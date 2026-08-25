package restapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/andino-agents/knowledge-base/internal/app"
	"github.com/andino-agents/knowledge-base/internal/config"
	_ "github.com/andino-agents/knowledge-base/internal/store/sqlite"
)

const rerankHTTPDim = 16

// fakeReranker scores by content (gamma > beta > alpha) and counts calls, so
// the test can assert the per-query flag gates the call and that reranking
// reorders the HTTP results.
func fakeReranker(t *testing.T, calls *int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(calls, 1)
		var req struct {
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		score := func(text string) float64 {
			switch {
			case containsStr(text, "gamma"):
				return 0.99
			case containsStr(text, "beta"):
				return 0.50
			default:
				return 0.10
			}
		}
		type res struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		}
		results := make([]res, len(req.Documents))
		for i, d := range req.Documents {
			results[i] = res{Index: i, RelevanceScore: score(d)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// newRerankHTTPServer builds a full REST server with a reranker and a KB whose
// rerank_default is "off", mirroring the #14 reproduction.
func newRerankHTTPServer(t *testing.T, embSrv, rerankSrv *httptest.Server) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Server:  config.Server{Bind: "127.0.0.1:0", DataDir: t.TempDir(), LogLevel: "error", LogFormat: "text"},
		Storage: config.Storage{Provider: "sqlite"},
		Inference: config.Inference{
			Backends: []config.Backend{
				{Name: "fake", BaseURL: embSrv.URL + "/v1"},
				{Name: "fake-rerank", BaseURL: rerankSrv.URL + "/v1"},
			},
			EmbeddingModels: []config.EmbeddingModel{
				{Name: "fake-embed", Backend: "fake", Model: "fake", Dimensions: rerankHTTPDim},
			},
			RerankModels: []config.RerankModel{
				{Name: "fake-rerank", Backend: "fake-rerank", Model: "fake-rerank"},
			},
		},
		Defaults: config.Defaults{EmbeddingModel: "fake-embed"},
		KnowledgeBases: []config.KnowledgeBase{
			{Name: "kb", Writable: true, EmbeddingModel: "fake-embed",
				RerankModel: "fake-rerank", RerankDefault: "off"},
		},
	}
	for i := range cfg.KnowledgeBases {
		kb := &cfg.KnowledgeBases[i]
		ch := config.Chunking{Strategy: "flat", MaxTokens: 128, OverlapTokens: 16}
		kb.Chunking = &ch
	}
	a, err := app.New(context.Background(), cfg, slog.Default())
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(a.Close)
	return httptest.NewServer(New(a, slog.Default()).Handler())
}

// TestRerankOverHTTP reproduces #14 exactly as the reporter observed it: two
// HTTP searches differing only in the rerank flag, with rerank_default "off".
func TestRerankOverHTTP(t *testing.T) {
	var calls int64
	embSrv := fakeEmbeddings(t)
	rerankSrv := fakeReranker(t, &calls)
	srv := newRerankHTTPServer(t, embSrv, rerankSrv)

	store := func(rel, title, content string) {
		resp, body := do(t, "POST", srv.URL+"/v1/kb/kb/documents", "",
			`{"title":"`+title+`","content":"`+content+`"}`)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("store %s: %d %v", rel, resp.StatusCode, body)
		}
	}
	store("alpha.md", "Alpha", "quantum alpha terraform apply")
	store("beta.md", "Beta", "quantum beta kubernetes pods")
	store("gamma.md", "Gamma", "quantum gamma s3 bucket")

	search := func(rerank bool) map[string]any {
		resp, body := do(t, "POST", srv.URL+"/v1/kb/kb/search", "",
			`{"query":"quantum terraform","limit":5,"rerank":`+jsonBool(rerank)+`}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("search rerank=%v: %d %v", rerank, resp.StatusCode, body)
		}
		return body
	}

	f := search(false)
	fr := f["results"].([]any)
	if len(fr) == 0 {
		t.Fatal("no results for rerank=false")
	}
	// rerank=false must not touch the reranker.
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("rerank=false called the reranker %d times, want 0", got)
	}

	tf := search(true)
	tr := tf["results"].([]any)
	if len(tr) == 0 {
		t.Fatal("no results for rerank=true")
	}
	// rerank=true must call the reranker exactly once.
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("rerank=true called the reranker %d times, want 1", got)
	}

	firstTrue := tr[0].(map[string]any)
	if text, _ := firstTrue["text"].(string); !containsStr(text, "gamma") {
		t.Errorf("rerank=true top text = %v, want it to contain gamma (the reranker's #1)", text)
	}
	if rel, _ := firstTrue["rel_path"].(string); rel == fr[0].(map[string]any)["rel_path"] {
		t.Errorf("rerank=false and rerank=true returned the same top hit %v: reranking did not reorder", rel)
	}
	// The wire schema must expose rerank_score when reranking won...
	if rs, ok := firstTrue["rerank_score"].(float64); !ok || rs < 0.9 {
		t.Errorf("rerank=true wire rerank_score = %v (%t), want ~0.99 present", firstTrue["rerank_score"], ok)
	}
	// ...and omit it when fusion decided.
	if _, present := fr[0].(map[string]any)["rerank_score"]; present {
		t.Errorf("rerank=false wire rerank_score present = %v, want omitted", fr[0].(map[string]any)["rerank_score"])
	}
}

func jsonBool(b bool) string {
	bb, _ := json.Marshal(b)
	return string(bb)
}
