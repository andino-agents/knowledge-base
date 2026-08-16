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
	cmd.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for the embedding and chat endpoints at startup")
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
			// Contextual retrieval calls the CHAT model, which on a router
			// loads separately from (and much slower than) the embedding
			// model. Waiting only for embeddings let the initial sync start
			// against a chat endpoint still answering 503 "Loading model"
			// (or 400 "model is not loaded", if it has not been asked for).
			if chat := kb.Indexer.Contextual; chat != nil {
				a.SetReady(name, fmt.Errorf("waiting for chat backend"))
				if err := chat.WaitReady(ctx, wait); err != nil {
					a.SetReady(name, err)
					logger.Error("chat backend never became ready", "kb", name, "error", err)
					return
				}
			}
			a.SetReady(name, fmt.Errorf("initial sync running"))
			if err := initialSync(ctx, name, kb, metrics, logger); err != nil {
				a.SetReady(name, fmt.Errorf("initial sync failed: %w", err))
				return
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
		func(r *http.Request) *mcp.Server {
			return mcpserver.New(a, version, writesAllowed(cfg, r))
		},
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

// initialSyncAttempts bounds how many times the whole initial sync is retried
// before the KB is declared unready. The sync used to be one-shot: a single
// transient backend failure (a model still loading, or evicted by a router
// sitting at its model cap) aborted it, and the KB then stayed unready and
// silent until someone restarted the service by hand.
const initialSyncAttempts = 5

// initialSyncBackoff is the pause before retry N. Deliberately long: what this
// waits out is a model load, which takes minutes, not milliseconds.
func initialSyncBackoff(attempt int) time.Duration {
	d := 15 * time.Second << attempt
	if d > 2*time.Minute {
		d = 2 * time.Minute
	}
	return d
}

// initialSync indexes every source of a KB, retrying the whole pass on error.
// Sources are re-synced from the top on retry; that is cheap because syncing is
// incremental and already-indexed documents are skipped by manifest.
func initialSync(ctx context.Context, name string, kb *app.KB, metrics *ops.Metrics, logger *slog.Logger) error {
	var lastErr error
	for attempt := 0; attempt < initialSyncAttempts; attempt++ {
		lastErr = nil
		for _, src := range kb.Sources {
			stats, err := kb.Indexer.SyncSource(ctx, src)
			if err != nil {
				lastErr = fmt.Errorf("source %s: %w", src.Name(), err)
				logger.Error("initial sync failed", "kb", name, "source", src.Name(),
					"attempt", attempt+1, "of", initialSyncAttempts, "error", err)
				break
			}
			metrics.IndexOps.WithLabelValues(name, "indexed").Add(float64(stats.Indexed))
			metrics.IndexOps.WithLabelValues(name, "deleted").Add(float64(stats.Deleted))
			metrics.IndexOps.WithLabelValues(name, "failed").Add(float64(stats.Failed))
		}
		if lastErr == nil {
			return nil
		}
		if attempt == initialSyncAttempts-1 {
			break
		}
		delay := initialSyncBackoff(attempt)
		logger.Warn("retrying initial sync", "kb", name, "delay", delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	logger.Error("initial sync gave up", "kb", name, "attempts", initialSyncAttempts, "error", lastErr)
	return lastErr
}

// authMCP guards the MCP endpoint with the same bearer keys as the REST API.
// It only decides whether the request gets in at all; which tools it may use
// is decided per request by writesAllowed. Without configured keys the
// endpoint is open (localhost use).
func authMCP(cfg *config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(cfg.Server.APIKeys) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := config.BearerToken(r.Header.Get("Authorization"))
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if _, ok := cfg.Server.LookupKey(token); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writesAllowed reports whether the request's key may use the MCP write
// tools. It re-reads the header instead of taking a scope threaded through
// the request context from authMCP: the extra lookup is free next to a search,
// and it keeps the guarantee local rather than resting on the SDK handing the
// server factory the very request the middleware saw. Without configured keys
// the server is open, matching authMCP.
func writesAllowed(cfg *config.Config, r *http.Request) bool {
	if len(cfg.Server.APIKeys) == 0 {
		return true
	}
	token, ok := config.BearerToken(r.Header.Get("Authorization"))
	if !ok {
		return false
	}
	key, ok := cfg.Server.LookupKey(token)
	return ok && key.Scope == "readwrite"
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
