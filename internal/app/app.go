// Package app wires config, stores, embedders and indexers into the running
// service shared by the REST API, the MCP server and the CLI.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andino-agents/knowledge-base/internal/config"
	"github.com/andino-agents/knowledge-base/internal/inference"
	"github.com/andino-agents/knowledge-base/internal/pipeline"
	"github.com/andino-agents/knowledge-base/internal/pipeline/extract"
	"github.com/andino-agents/knowledge-base/internal/source"
	"github.com/andino-agents/knowledge-base/internal/source/gitrepo"
	"github.com/andino-agents/knowledge-base/internal/source/localdir"
	s3source "github.com/andino-agents/knowledge-base/internal/source/s3"
	"github.com/andino-agents/knowledge-base/internal/store"
)

// maxRRFScore normalizes RRF scores into [0,1]: a chunk ranked #1 by both
// retrieval legs scores 2/(rrfK+1).
const maxRRFScore = 2.0 / 61.0

// KB is one knowledge base's runtime.
type KB struct {
	Config   *config.KnowledgeBase
	Store    store.Store
	Embedder *inference.Embedder
	Reranker *inference.Reranker // nil when reranking is disabled
	Indexer  *pipeline.Indexer
	Sources  []source.Source
}

// Hit is a search result with a normalized relevance score in [0,1].
type Hit struct {
	store.Hit
	KnowledgeBase string  `json:"knowledge_base"`
	Relevance     float64 `json:"relevance"`
}

type App struct {
	Config *config.Config
	Logger *slog.Logger

	mu    sync.RWMutex
	kbs   map[string]*KB
	ready map[string]error // per-KB readiness; nil value = ready
}

// New builds the runtime: opens every KB's store and constructs sources.
// It does NOT wait for embedding backends; call WaitReady from serve/index.
func New(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*App, error) {
	a := &App{
		Config: cfg,
		Logger: logger,
		kbs:    map[string]*KB{},
		ready:  map[string]error{},
	}
	registry := extract.Default()
	for i := range cfg.KnowledgeBases {
		kbCfg := &cfg.KnowledgeBases[i]
		model, backend, err := cfg.EmbeddingModelFor(kbCfg)
		if err != nil {
			a.Close()
			return nil, err
		}
		embedder := &inference.Embedder{
			BaseURL:    backend.BaseURL,
			APIKey:     backend.APIKey,
			Model:      model.Model,
			Dimensions: model.Dimensions,
			BatchSize:  model.BatchSize,
			MaxRetries: model.MaxRetries,
			Logger:     logger,
		}
		providerCfg := map[string]any{"data_dir": cfg.Server.DataDir}
		for k, v := range cfg.Storage.Options {
			providerCfg[k] = v
		}
		st, err := store.Open(ctx, cfg.Storage.Provider, store.Options{
			KBName:         kbCfg.Name,
			ModelName:      model.Model,
			Dimensions:     model.Dimensions,
			ProviderConfig: providerCfg,
		})
		if err != nil {
			a.Close()
			return nil, fmt.Errorf("kb %s: %w", kbCfg.Name, err)
		}
		var sources []source.Source
		for _, sc := range kbCfg.Sources {
			switch sc.Type {
			case "localdir":
				src, err := localdir.New(sc.Name, sc.Path, sc.Include, sc.Exclude, registry.Extensions())
				if err != nil {
					a.Close()
					return nil, fmt.Errorf("kb %s: %w", kbCfg.Name, err)
				}
				sources = append(sources, src)
			case "git":
				token := ""
				if sc.TokenEnv != "" {
					token = os.Getenv(sc.TokenEnv)
				}
				cacheDir := filepath.Join(cfg.Server.DataDir, "git", kbCfg.Name+"-"+sc.Name)
				sources = append(sources, gitrepo.New(sc.Name, sc.URL, sc.Branch, sc.Paths, cacheDir, token, registry.Extensions()))
			case "s3":
				src, err := s3source.New(ctx, s3source.Options{
					Name: sc.Name, Bucket: sc.Bucket, Prefix: sc.Prefix,
					Region: sc.Region, Endpoint: sc.Endpoint, PathStyle: sc.PathStyle,
					Paths: sc.Paths, Exts: registry.Extensions(),
				})
				if err != nil {
					a.Close()
					return nil, fmt.Errorf("kb %s: %w", kbCfg.Name, err)
				}
				sources = append(sources, src)
			}
		}
		var reranker *inference.Reranker
		if kbCfg.RerankModel != "" {
			for _, rm := range cfg.Inference.RerankModels {
				if rm.Name != kbCfg.RerankModel {
					continue
				}
				for _, b := range cfg.Inference.Backends {
					if b.Name == rm.Backend {
						reranker = &inference.Reranker{
							BaseURL: b.BaseURL, APIKey: b.APIKey, Model: rm.Model, Logger: logger,
						}
					}
				}
			}
		}
		newChat := func(ref string) (*inference.Chat, error) {
			chatModel, chatBackend, err := cfg.ChatModelByName(ref)
			if err != nil {
				return nil, err
			}
			return &inference.Chat{
				BaseURL:   chatBackend.BaseURL,
				APIKey:    chatBackend.APIKey,
				Model:     chatModel.Model,
				MaxTokens: chatModel.MaxTokens,
				ExtraBody: chatModel.ExtraBody,
				Logger:    logger,
			}, nil
		}
		var contextual, ocrChat *inference.Chat
		if kbCfg.Contextual != nil && kbCfg.Contextual.Enabled {
			if contextual, err = newChat(kbCfg.Contextual.Model); err != nil {
				a.Close()
				return nil, err
			}
		}
		if kbCfg.OCR != nil && kbCfg.OCR.Enabled {
			if ocrChat, err = newChat(kbCfg.OCR.Model); err != nil {
				a.Close()
				return nil, err
			}
		}
		a.kbs[kbCfg.Name] = &KB{
			Config:   kbCfg,
			Store:    st,
			Embedder: embedder,
			Reranker: reranker,
			Indexer: &pipeline.Indexer{
				Store:      st,
				Embedder:   embedder,
				Registry:   registry,
				Chunking:   *kbCfg.Chunking,
				Contextual: contextual,
				OCR:        ocrChat,
				Logger:     logger,
			},
			Sources: sources,
		}
		a.ready[kbCfg.Name] = fmt.Errorf("initial sync pending")
	}
	return a, nil
}

func (a *App) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for name, kb := range a.kbs {
		if err := kb.Store.Close(); err != nil {
			a.Logger.Error("closing store", "kb", name, "error", err)
		}
	}
	a.kbs = map[string]*KB{}
}

// KBNames lists knowledge bases, sorted.
func (a *App) KBNames() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	names := make([]string, 0, len(a.kbs))
	for n := range a.kbs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// KB fetches one knowledge base's runtime.
func (a *App) KB(name string) (*KB, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	kb, ok := a.kbs[name]
	if !ok {
		return nil, fmt.Errorf("unknown knowledge base %q (have: %s)", name, strings.Join(a.kbNamesLocked(), ", "))
	}
	return kb, nil
}

func (a *App) kbNamesLocked() []string {
	names := make([]string, 0, len(a.kbs))
	for n := range a.kbs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SetReady records a KB's readiness state (nil = ready).
func (a *App) SetReady(kbName string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ready[kbName] = err
}

// Readiness returns a copy of the per-KB readiness map.
func (a *App) Readiness() map[string]error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]error, len(a.ready))
	for k, v := range a.ready {
		out[k] = v
	}
	return out
}

// SearchOpts tunes one search request. Zero values mean "use defaults".
type SearchOpts struct {
	Limit    int
	MinScore float64
	// Rerank overrides the KB default: nil = rerank when the KB has a
	// reranker configured; &false disables it for this request; &true is a
	// no-op when no reranker exists.
	Rerank *bool
	// MaxPerDoc caps how many chunks of the same document appear in the
	// results. 0 = the default cap (2); negative = no cap.
	MaxPerDoc int
	// Filter keeps only documents whose metadata contains every given
	// key/value pair (AND semantics).
	Filter map[string]string
}

// defaultMaxPerDoc keeps result lists diverse by default: without a cap one
// strong document can fill an agent's whole retrieval budget.
const defaultMaxPerDoc = 2

// rerankPool bounds how many fused candidates the cross-encoder scores per
// query. Reranking cost is linear in pool size (~35ms/doc on a local 0.6B);
// beyond ~24 candidates the extra recall is not worth the latency.
const rerankPool = 24

// Search runs a hybrid query against one KB. MinScore filters on the
// normalized relevance (0..1).
func (a *App) Search(ctx context.Context, kbName, query string, opts SearchOpts) ([]Hit, error) {
	kb, err := a.KB(kbName)
	if err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 8
	}
	minScore := opts.MinScore
	rerankByDefault := kb.Config.RerankDefault != "off"
	useRerank := kb.Reranker != nil &&
		((opts.Rerank == nil && rerankByDefault) || (opts.Rerank != nil && *opts.Rerank))
	maxPerDoc := opts.MaxPerDoc
	if maxPerDoc == 0 {
		maxPerDoc = defaultMaxPerDoc
	}

	vecs, err := kb.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}

	// With reranking or a per-doc cap active, fetch a wider candidate pool;
	// otherwise the fused top-limit is the answer.
	fetchK := limit
	if useRerank || maxPerDoc > 0 {
		fetchK = max(limit*4, 24)
	}
	raw, err := kb.Store.HybridSearch(ctx, query, vecs[0], fetchK)
	if err != nil {
		return nil, err
	}
	if len(opts.Filter) > 0 {
		raw = filterByMetadata(raw, opts.Filter)
	}

	if useRerank && len(raw) > 0 {
		// Bound the cross-encoder's work: score the best fused candidates.
		// The per-doc cap is applied AFTER reranking, never before: fusion
		// order and cross-encoder preference disagree about which chunk of a
		// document is best, and capping early throws away the winner.
		pool := raw
		if len(pool) > rerankPool {
			pool = pool[:rerankPool]
		}
		docs := make([]string, len(pool))
		for i, h := range pool {
			docs[i] = h.Text
		}
		ranked, err := kb.Reranker.Rerank(ctx, query, docs, 0 /* rank the whole pool */)
		if err != nil {
			// Reranking is an enhancement, unlike embeddings: degrade to
			// fusion order with a warning instead of failing the query.
			a.Logger.Warn("rerank failed, falling back to fusion order", "kb", kbName, "error", err)
		} else {
			ordered := make([]store.Hit, 0, len(ranked))
			scores := make(map[int64]map[int]float64) // docID -> startLine -> score
			for _, rk := range ranked {
				h := pool[rk.Index]
				ordered = append(ordered, h)
				if scores[h.DocumentID] == nil {
					scores[h.DocumentID] = map[int]float64{}
				}
				scores[h.DocumentID][h.StartLine] = rk.Score
			}
			if maxPerDoc > 0 {
				ordered = capPerDocument(ordered, maxPerDoc)
			}
			if len(ordered) > limit {
				ordered = ordered[:limit]
			}
			hits := make([]Hit, 0, len(ordered))
			for _, h := range ordered {
				score := scores[h.DocumentID][h.StartLine]
				if score < minScore {
					continue
				}
				hits = append(hits, Hit{Hit: h, KnowledgeBase: kbName, Relevance: score})
			}
			return hits, nil
		}
	}

	if maxPerDoc > 0 {
		raw = capPerDocument(raw, maxPerDoc)
	}
	if len(raw) > limit {
		raw = raw[:limit]
	}
	hits := make([]Hit, 0, len(raw))
	for _, h := range raw {
		rel := h.Score / maxRRFScore
		if rel > 1 {
			rel = 1
		}
		if rel < minScore {
			continue
		}
		hits = append(hits, Hit{Hit: h, KnowledgeBase: kbName, Relevance: rel})
	}
	return hits, nil
}

// SearchAll queries every KB and merges results by relevance.
func (a *App) SearchAll(ctx context.Context, query string, opts SearchOpts) ([]Hit, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 8
	}
	var all []Hit
	for _, name := range a.KBNames() {
		hits, err := a.Search(ctx, name, query, opts)
		if err != nil {
			return nil, fmt.Errorf("kb %s: %w", name, err)
		}
		all = append(all, hits...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Relevance > all[j].Relevance })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// filterByMetadata keeps hits whose document metadata matches every pair.
func filterByMetadata(hits []store.Hit, filter map[string]string) []store.Hit {
	out := hits[:0]
	for _, h := range hits {
		ok := true
		for k, v := range filter {
			if h.Metadata[k] != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, h)
		}
	}
	return out
}

// capPerDocument keeps at most n chunks per document, preserving order.
// Retrieval budgets are small; without a cap one strong document can crowd
// every other source out of the results an agent sees.
func capPerDocument(hits []store.Hit, n int) []store.Hit {
	seen := map[int64]int{}
	out := hits[:0]
	for _, h := range hits {
		if seen[h.DocumentID] >= n {
			continue
		}
		seen[h.DocumentID]++
		out = append(out, h)
	}
	return out
}

// StoreDocument writes agent-provided content into a writable KB and indexes
// it synchronously. The returned id follows the strands memory convention.
func (a *App) StoreDocument(ctx context.Context, kbName, title, content string, metadata map[string]string) (string, error) {
	kb, err := a.KB(kbName)
	if err != nil {
		return "", err
	}
	if !kb.Config.Writable {
		return "", fmt.Errorf("knowledge base %q is not writable", kbName)
	}
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("content is empty")
	}
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	id := fmt.Sprintf("memory_%s_%s", time.Now().UTC().Format("20060102_150405"), hex.EncodeToString(rnd[:]))
	if err := kb.Indexer.IndexManaged(ctx, kbName, id, title, content, metadata); err != nil {
		return "", err
	}
	return id, nil
}

// DeleteDocument removes a managed document from a writable KB.
func (a *App) DeleteDocument(ctx context.Context, kbName, id string) error {
	kb, err := a.KB(kbName)
	if err != nil {
		return err
	}
	if !kb.Config.Writable {
		return fmt.Errorf("knowledge base %q is not writable", kbName)
	}
	if _, err := kb.Store.GetDocument(ctx, config.ManagedSourceName, id); err != nil {
		return err // store.ErrNotFound propagates for a proper 404
	}
	return kb.Store.DeleteDocument(ctx, config.ManagedSourceName, id)
}
