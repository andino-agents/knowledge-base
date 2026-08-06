package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/andino-agents/knowledge-base/internal/app"
	"github.com/andino-agents/knowledge-base/internal/config"
	"github.com/andino-agents/knowledge-base/internal/ops"
	"github.com/andino-agents/knowledge-base/internal/restapi"
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
		}(name, kb)
	}

	mux := http.NewServeMux()
	rest := restapi.New(a, logger)
	mux.Handle("/v1/", rest.Handler())
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
