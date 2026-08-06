package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/andino-agents/knowledge-base/internal/app"
	"github.com/andino-agents/knowledge-base/internal/config"
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

			if rebuild {
				for i := range cfg.KnowledgeBases {
					kb := &cfg.KnowledgeBases[i]
					if kbFilter != "" && kb.Name != kbFilter {
						continue
					}
					base := filepath.Join(cfg.Server.DataDir, kb.Name+".db")
					for _, suffix := range []string{"", "-wal", "-shm"} {
						if err := os.Remove(base + suffix); err != nil && !os.IsNotExist(err) {
							return fmt.Errorf("rebuild: %w", err)
						}
					}
					logger.Warn("rebuild: dropped existing index", "kb", kb.Name, "path", base)
				}
			}

			a, err := app.New(ctx, cfg, logger)
			if err != nil {
				return err
			}
			defer a.Close()

			for _, name := range a.KBNames() {
				if kbFilter != "" && name != kbFilter {
					continue
				}
				kb, _ := a.KB(name)
				if len(kb.Sources) == 0 {
					logger.Info("knowledge base has no sources, skipping", "kb", name)
					continue
				}
				logger.Info("waiting for embeddings endpoint", "kb", name, "model", kb.Embedder.Model)
				if err := kb.Embedder.WaitReady(ctx, wait); err != nil {
					return err
				}
				if err := syncKB(ctx, name, kb, logger); err != nil {
					return fmt.Errorf("kb %s: %w", name, err)
				}
				st, err := kb.Store.Stats(ctx)
				if err != nil {
					return err
				}
				fmt.Printf("%s: %d documents, %d chunks\n", name, st.Documents, st.Chunks)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kbFilter, "kb", "", "only sync this knowledge base")
	cmd.Flags().BoolVar(&rebuild, "rebuild", false, "drop and re-embed the knowledge base from scratch")
	cmd.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for the embeddings endpoint")
	return cmd
}

func syncKB(ctx context.Context, name string, kb *app.KB, logger *slog.Logger) error {
	for _, src := range kb.Sources {
		start := time.Now()
		stats, err := kb.Indexer.SyncSource(ctx, src)
		if err != nil {
			return err
		}
		logger.Info("sync finished", "kb", name, "source", src.Name(),
			"indexed", stats.Indexed, "deleted", stats.Deleted,
			"skipped", stats.Skipped, "failed", stats.Failed,
			"duration", time.Since(start).Round(time.Millisecond))
	}
	return nil
}

// discardLogger silences runtime noise in commands whose output IS the
// report (doctor).
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 4}))
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
