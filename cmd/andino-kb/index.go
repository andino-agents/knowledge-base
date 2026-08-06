package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/andino-agents/knowledge-base/internal/config"
	"github.com/andino-agents/knowledge-base/internal/inference"
	"github.com/andino-agents/knowledge-base/internal/pipeline"
	"github.com/andino-agents/knowledge-base/internal/pipeline/extract"
	"github.com/andino-agents/knowledge-base/internal/source"
	"github.com/andino-agents/knowledge-base/internal/source/localdir"
	"github.com/andino-agents/knowledge-base/internal/store"
	_ "github.com/andino-agents/knowledge-base/internal/store/sqlite"
)

func indexCmd(configPath *string) *cobra.Command {
	var (
		kbFilter string
		rebuild  bool
		wait     time.Duration
	)
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Run one full/incremental sync of every source into its knowledge base",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			logger := newLogger(cfg)
			ctx := cmd.Context()

			for i := range cfg.KnowledgeBases {
				kb := &cfg.KnowledgeBases[i]
				if kbFilter != "" && kb.Name != kbFilter {
					continue
				}
				if len(kb.Sources) == 0 {
					logger.Info("knowledge base has no sources, skipping", "kb", kb.Name)
					continue
				}
				if err := indexKB(ctx, cfg, kb, rebuild, wait, logger); err != nil {
					return fmt.Errorf("kb %s: %w", kb.Name, err)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kbFilter, "kb", "", "only sync this knowledge base")
	cmd.Flags().BoolVar(&rebuild, "rebuild", false, "drop and re-embed the knowledge base from scratch")
	cmd.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for the embeddings endpoint")
	return cmd
}

func indexKB(ctx context.Context, cfg *config.Config, kb *config.KnowledgeBase, rebuild bool, wait time.Duration, logger *slog.Logger) error {
	embedder, err := newEmbedder(cfg, kb, logger)
	if err != nil {
		return err
	}
	logger.Info("waiting for embeddings endpoint", "kb", kb.Name, "model", embedder.Model)
	if err := embedder.WaitReady(ctx, wait); err != nil {
		return err
	}

	if rebuild {
		base := filepath.Join(cfg.Server.DataDir, kb.Name+".db")
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(base + suffix); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("rebuild: %w", err)
			}
		}
		logger.Warn("rebuilt: dropped existing index", "kb", kb.Name, "path", base)
	}

	st, err := openStore(ctx, cfg, kb)
	if err != nil {
		return err
	}
	defer st.Close()

	registry := extract.Default()
	ix := &pipeline.Indexer{
		Store:    st,
		Embedder: embedder,
		Registry: registry,
		Chunking: *kb.Chunking,
		Logger:   logger,
	}

	for _, sc := range kb.Sources {
		src, err := buildSource(sc, registry)
		if err != nil {
			return err
		}
		if src == nil {
			logger.Warn("source type not implemented yet, skipping", "kb", kb.Name, "source", sc.Name, "type", sc.Type)
			continue
		}
		start := time.Now()
		stats, err := ix.SyncSource(ctx, src)
		if err != nil {
			return err
		}
		logger.Info("sync finished", "kb", kb.Name, "source", sc.Name,
			"indexed", stats.Indexed, "deleted", stats.Deleted,
			"skipped", stats.Skipped, "failed", stats.Failed,
			"duration", time.Since(start).Round(time.Millisecond))
	}

	st2, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %d documents, %d chunks\n", kb.Name, st2.Documents, st2.Chunks)
	return nil
}

func newEmbedder(cfg *config.Config, kb *config.KnowledgeBase, logger *slog.Logger) (*inference.Embedder, error) {
	model, backend, err := cfg.EmbeddingModelFor(kb)
	if err != nil {
		return nil, err
	}
	return &inference.Embedder{
		BaseURL:    backend.BaseURL,
		APIKey:     backend.APIKey,
		Model:      model.Model,
		Dimensions: model.Dimensions,
		BatchSize:  model.BatchSize,
		MaxRetries: model.MaxRetries,
		Logger:     logger,
	}, nil
}

func openStore(ctx context.Context, cfg *config.Config, kb *config.KnowledgeBase) (store.Store, error) {
	model, _, err := cfg.EmbeddingModelFor(kb)
	if err != nil {
		return nil, err
	}
	providerCfg := map[string]any{"data_dir": cfg.Server.DataDir}
	for k, v := range cfg.Storage.Options {
		providerCfg[k] = v
	}
	return store.Open(ctx, cfg.Storage.Provider, store.Options{
		KBName:         kb.Name,
		ModelName:      model.Model,
		Dimensions:     model.Dimensions,
		ProviderConfig: providerCfg,
	})
}

// buildSource returns nil (no error) for source types that land in a later
// phase, so configs can be forward-compatible.
func buildSource(sc config.Source, registry *extract.Registry) (source.Source, error) {
	switch sc.Type {
	case "localdir":
		return localdir.New(sc.Name, sc.Path, sc.Include, sc.Exclude, registry.Extensions())
	case "git":
		return nil, nil // phase 5
	default:
		return nil, fmt.Errorf("unknown source type %q", sc.Type)
	}
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Server.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.Server.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}
