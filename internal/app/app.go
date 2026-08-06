// Package app wires config, stores, embedders and indexers into the running
// service shared by the REST API, the MCP server and the CLI.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andino-agents/knowledge-base/internal/config"
	"github.com/andino-agents/knowledge-base/internal/inference"
	"github.com/andino-agents/knowledge-base/internal/pipeline"
	"github.com/andino-agents/knowledge-base/internal/pipeline/extract"
	"github.com/andino-agents/knowledge-base/internal/source"
	"github.com/andino-agents/knowledge-base/internal/source/localdir"
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
				logger.Warn("git sources not implemented yet, skipping", "kb", kbCfg.Name, "source", sc.Name)
			}
		}
		a.kbs[kbCfg.Name] = &KB{
			Config:   kbCfg,
			Store:    st,
			Embedder: embedder,
			Indexer: &pipeline.Indexer{
				Store:    st,
				Embedder: embedder,
				Registry: registry,
				Chunking: *kbCfg.Chunking,
				Logger:   logger,
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

// Search runs a hybrid query against one KB. minScore filters on the
// normalized relevance (0..1).
func (a *App) Search(ctx context.Context, kbName, query string, limit int, minScore float64) ([]Hit, error) {
	kb, err := a.KB(kbName)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 8
	}
	vecs, err := kb.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}
	raw, err := kb.Store.HybridSearch(ctx, query, vecs[0], limit)
	if err != nil {
		return nil, err
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
func (a *App) SearchAll(ctx context.Context, query string, limit int, minScore float64) ([]Hit, error) {
	if limit <= 0 {
		limit = 8
	}
	var all []Hit
	for _, name := range a.KBNames() {
		hits, err := a.Search(ctx, name, query, limit, minScore)
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

// StoreDocument writes agent-provided content into a writable KB and indexes
// it synchronously. The returned id follows the strands memory convention.
func (a *App) StoreDocument(ctx context.Context, kbName, title, content string) (string, error) {
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
	if err := kb.Indexer.IndexManaged(ctx, kbName, id, title, content); err != nil {
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
