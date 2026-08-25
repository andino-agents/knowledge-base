package app

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/andino-agents/knowledge-base/internal/config"
	"github.com/andino-agents/knowledge-base/internal/store"
	_ "github.com/andino-agents/knowledge-base/internal/store/sqlite"
)

const rerankTestDim = 16

// fakeEmbeddings is a deterministic hash-based embeddings endpoint: the same
// text always maps to the same vector, so a query equal to a document's text
// ranks that document first by vector similarity.
func fakeEmbeddings(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i, text := range req.Input {
			h := fnv.New64a()
			h.Write([]byte(text))
			seed := h.Sum64()
			vec := make([]float32, rerankTestDim)
			for j := range vec {
				seed = seed*6364136223846793005 + 1442695040888963407
				vec[j] = float32(int64(seed>>33))/float32(1<<30) + 0.001
			}
			data = append(data, item{Index: i, Embedding: vec})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeReranker is an OpenAI-compatible /v1/rerank endpoint. It scores each
// document by content (gamma > beta > alpha) and returns the scores with the
// correct input index, exactly like a real cross-encoder: the index is the
// document's position in the request, the score is by relevance. This makes
// the test robust to whatever order fusion produced for the candidate pool.
func fakeReranker(t *testing.T, calls *int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(calls, 1)
		var req struct {
			Documents []string `json:"documents"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		score := func(text string) float64 {
			switch {
			case strings.Contains(text, "gamma"):
				return 0.99 // best
			case strings.Contains(text, "beta"):
				return 0.50
			default:
				return 0.10 // alpha worst
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
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newRerankApp(t *testing.T, embSrv, rerankSrv *httptest.Server) *App {
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
				{Name: "fake-embed", Backend: "fake", Model: "fake", Dimensions: rerankTestDim},
			},
			RerankModels: []config.RerankModel{
				{Name: "fake-rerank", Backend: "fake-rerank", Model: "fake-rerank"},
			},
		},
		Defaults: config.Defaults{EmbeddingModel: "fake-embed"},
		KnowledgeBases: []config.KnowledgeBase{
			// rerank_default "off": reranking only when a request asks for it.
			{Name: "kb", Writable: true, EmbeddingModel: "fake-embed",
				RerankModel: "fake-rerank", RerankDefault: "off"},
		},
	}
	for i := range cfg.KnowledgeBases {
		kb := &cfg.KnowledgeBases[i]
		ch := config.Chunking{Strategy: "flat", MaxTokens: 128, OverlapTokens: 16}
		kb.Chunking = &ch
	}
	a, err := New(context.Background(), cfg, slog.Default())
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

func indexDoc(t *testing.T, a *App, kbName, rel, title, text string) {
	t.Helper()
	kb, err := a.KB(kbName)
	if err != nil {
		t.Fatalf("KB: %v", err)
	}
	vecs, err := kb.Embedder.Embed(context.Background(), []string{text})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if err := kb.Store.UpsertDocument(context.Background(), store.Document{
		SourceName: "test", RelPath: rel, URI: "file:///" + rel, Title: title,
		SHA256: text, SizeBytes: int64(len(text)),
	}, []store.Chunk{{Seq: 0, StartLine: 1, EndLine: 1, Text: text, TokenEst: 3}}, [][]float32{vecs[0]}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

func boolPtr(b bool) *bool { return &b }

// TestRerankFlagGatesAndReorders is a faithful reproduction of #14: with
// rerank_default "off", rerank=false must skip the reranker entirely and
// rerank=true must invoke it and let its scores order the results.
func TestRerankFlagGatesAndReorders(t *testing.T) {
	ctx := context.Background()
	var calls int64
	embSrv := fakeEmbeddings(t)
	rerankSrv := fakeReranker(t, &calls)
	a := newRerankApp(t, embSrv, rerankSrv)

	indexDoc(t, a, "kb", "alpha.md", "Alpha", "quantum alpha terraform apply")
	indexDoc(t, a, "kb", "beta.md", "Beta", "quantum beta kubernetes pods")
	indexDoc(t, a, "kb", "gamma.md", "Gamma", "quantum gamma s3 bucket")

	// rerank=false: the reranker must not be called at all.
	falseRes, err := a.Search(ctx, "kb", "quantum terraform", SearchOpts{Limit: 5, Rerank: boolPtr(false)})
	if err != nil {
		t.Fatalf("search rerank=false: %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("rerank=false called the reranker %d times, want 0 (flag not gating)", got)
	}
	if len(falseRes) == 0 {
		t.Fatal("no results for rerank=false")
	}

	// rerank=true: the reranker must be called once and its #1 (gamma) must
	// win, with the reranker's score carried into relevance.
	trueRes, err := a.Search(ctx, "kb", "quantum terraform", SearchOpts{Limit: 5, Rerank: boolPtr(true)})
	if err != nil {
		t.Fatalf("search rerank=true: %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("rerank=true called the reranker %d times, want 1", got)
	}
	if len(trueRes) == 0 {
		t.Fatal("no results for rerank=true")
	}
	if trueRes[0].RelPath != "gamma.md" {
		t.Errorf("rerank=true top hit = %q, want gamma.md (the reranker's #1)", trueRes[0].RelPath)
	}
	if trueRes[0].Relevance < 0.9 {
		t.Errorf("rerank=true top relevance = %v, want ~0.99 (the reranker score), reranker output not merged", trueRes[0].Relevance)
	}
	// The two paths must disagree on the winner, or reranking changed nothing.
	if falseRes[0].RelPath == trueRes[0].RelPath && falseRes[0].Relevance == trueRes[0].Relevance {
		t.Errorf("rerank=false and rerank=true returned identical top hit %+v: reranking is inert", trueRes[0])
	}
	// rerank_score must be present and equal to the reranker score when
	// reranking ordered the results...
	if trueRes[0].RerankScore == nil || *trueRes[0].RerankScore < 0.9 {
		t.Errorf("rerank=true rerank_score = %v, want ~0.99 present", trueRes[0].RerankScore)
	}
	// ...and omitted when fusion decided the order.
	if falseRes[0].RerankScore != nil {
		t.Errorf("rerank=false rerank_score = %v, want nil (omitted)", falseRes[0].RerankScore)
	}
}
