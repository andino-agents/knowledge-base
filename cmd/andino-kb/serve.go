package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/andino-agents/knowledge-base/internal/app"
	"github.com/andino-agents/knowledge-base/internal/config"
	"github.com/andino-agents/knowledge-base/internal/mcpserver"
	"github.com/andino-agents/knowledge-base/internal/ops"
	"github.com/andino-agents/knowledge-base/internal/restapi"
	"github.com/andino-agents/knowledge-base/internal/source"
)

func serveCmd(configPath *string) *cobra.Command {
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the knowledge bases over REST (and MCP) with live source syncing",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			return runServe(cmd.Context(), cfg, wait)
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for embedding endpoints at startup")
	return cmd
}

func runServe(ctx context.Context, cfg *config.Config, wait time.Duration) error {
	logger := newLogger(cfg)
	a, err := app.New(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer a.Close()

	metrics := ops.NewMetrics()

	// Background startup per KB: wait for the embedding backend, then run the
	// initial sync. The HTTP server comes up immediately; /readyz reports
	// per-KB progress and queries return once their KB is ready.
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	for _, name := range a.KBNames() {
		kb, _ := a.KB(name)
		go func(name string, kb *app.KB) {
			a.SetReady(name, fmt.Errorf("waiting for embeddings backend"))
			if err := kb.Embedder.WaitReady(ctx, wait); err != nil {
				a.SetReady(name, err)
				logger.Error("embedding backend never became ready", "kb", name, "error", err)
				return
			}
			a.SetReady(name, fmt.Errorf("initial sync running"))
			for _, src := range kb.Sources {
				stats, err := kb.Indexer.SyncSource(ctx, src)
				if err != nil {
					a.SetReady(name, fmt.Errorf("initial sync failed: %w", err))
					logger.Error("initial sync failed", "kb", name, "source", src.Name(), "error", err)
					return
				}
				metrics.IndexOps.WithLabelValues(name, "indexed").Add(float64(stats.Indexed))
				metrics.IndexOps.WithLabelValues(name, "deleted").Add(float64(stats.Deleted))
				metrics.IndexOps.WithLabelValues(name, "failed").Add(float64(stats.Failed))
			}
			a.SetReady(name, nil)
			logger.Info("knowledge base ready", "kb", name)
			startWatchers(ctx, name, kb, metrics, logger)
			startPollers(ctx, name, kb, metrics, logger)
		}(name, kb)
	}

	mux := http.NewServeMux()
	rest := restapi.New(a, logger)
	rest.ObserveSearch = func(kb string, seconds float64) {
		metrics.SearchDuration.WithLabelValues(kb).Observe(seconds)
	}
	mux.Handle("/v1/", rest.Handler())
	mux.Handle("/mcp", authMCP(cfg, mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpserver.New(a, version) },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)))
	ops.Register(mux, a, metrics)

	srv := &http.Server{
		Addr:              cfg.Server.Bind,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("serving", "bind", cfg.Server.Bind)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	return srv.Shutdown(shutdownCtx)
}

// authMCP guards the MCP endpoint with the same bearer keys as the REST API.
// Any valid key grants tool access; write tools re-check writability in the
// app layer. Without configured keys the endpoint is open (localhost use).
func authMCP(cfg *config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys := cfg.Server.APIKeys
		if len(keys) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		for _, k := range keys {
			if auth == "Bearer "+k.Key {
				next.ServeHTTP(w, r)
				return
			}
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// startPollers runs a periodic full sync for poll-based sources (git, s3).
func startPollers(ctx context.Context, kbName string, kb *app.KB, metrics *ops.Metrics, logger *slog.Logger) {
	for i, src := range kb.Sources {
		t := kb.Config.Sources[i].Type
		if t != "git" && t != "s3" {
			continue
		}
		interval := kb.Config.Sources[i].PollInterval
		src := src
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					stats, err := kb.Indexer.SyncSource(ctx, src)
					if err != nil {
						if ctx.Err() == nil {
							logger.Error("poll sync failed", "kb", kbName, "source", src.Name(), "error", err)
						}
						continue
					}
					if stats.Indexed+stats.Deleted > 0 {
						logger.Info("poll sync", "kb", kbName, "source", src.Name(),
							"indexed", stats.Indexed, "deleted", stats.Deleted)
					}
					metrics.IndexOps.WithLabelValues(kbName, "indexed").Add(float64(stats.Indexed))
					metrics.IndexOps.WithLabelValues(kbName, "deleted").Add(float64(stats.Deleted))
				}
			}
		}()
		logger.Info("polling source", "kb", kbName, "source", src.Name(), "interval", interval)
	}
}

// startWatchers wires each watchable source into the indexer: watcher events
// mark a path dirty, a per-path debounce timer absorbs editor bursts, and on
// fire SyncPath re-examines reality (reindex or delete).
func startWatchers(ctx context.Context, kbName string, kb *app.KB, metrics *ops.Metrics, logger *slog.Logger) {
	for i, src := range kb.Sources {
		w, ok := src.(source.Watchable)
		if !ok {
			continue
		}
		srcCfg := kb.Config.Sources[i]
		if !srcCfg.Watch {
			continue
		}
		debounce := time.Duration(srcCfg.DebounceMS) * time.Millisecond
		src := src
		go func() {
			dirty := make(chan string, 256)
			go func() {
				if err := w.Watch(ctx, dirty); err != nil && ctx.Err() == nil {
					logger.Error("watcher stopped", "kb", kbName, "source", src.Name(), "error", err)
				}
			}()

			var (
				mu     sync.Mutex
				timers = map[string]*time.Timer{}
			)
			for {
				select {
				case <-ctx.Done():
					return
				case rel := <-dirty:
					metrics.WatcherEvents.WithLabelValues(kbName, src.Name()).Inc()
					mu.Lock()
					if t, ok := timers[rel]; ok {
						t.Reset(debounce)
					} else {
						timers[rel] = time.AfterFunc(debounce, func() {
							mu.Lock()
							delete(timers, rel)
							mu.Unlock()
							if err := kb.Indexer.SyncPath(ctx, src, rel); err != nil && ctx.Err() == nil {
								logger.Error("watch resync failed", "kb", kbName, "source", src.Name(), "path", rel, "error", err)
								metrics.IndexOps.WithLabelValues(kbName, "failed").Inc()
								return
							}
							logger.Info("watch resync", "kb", kbName, "source", src.Name(), "path", rel)
							metrics.IndexOps.WithLabelValues(kbName, "indexed").Inc()
						})
					}
					mu.Unlock()
				}
			}
		}()
		logger.Info("watching source", "kb", kbName, "source", src.Name(), "debounce", debounce)
	}
}
