// Package ops provides health, readiness and Prometheus metrics endpoints.
package ops

import (
	"encoding/json"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/andino-agents/knowledge-base/internal/app"
)

// Metrics is the service's Prometheus instrumentation.
type Metrics struct {
	Registry       *prometheus.Registry
	SearchDuration *prometheus.HistogramVec
	IndexOps       *prometheus.CounterVec
	WatcherEvents  *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	factory := promauto.With(reg)
	return &Metrics{
		Registry: reg,
		SearchDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "andino_search_duration_seconds",
			Help:    "Hybrid search latency per knowledge base.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"kb"}),
		IndexOps: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "andino_index_operations_total",
			Help: "Indexing operations by knowledge base and outcome.",
		}, []string{"kb", "op"}),
		WatcherEvents: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "andino_watcher_events_total",
			Help: "Filesystem watcher events accepted after filtering.",
		}, []string{"kb", "source"}),
	}
}

// Register adds /healthz, /readyz and /metrics to a mux.
func Register(mux *http.ServeMux, a *app.App, m *Metrics) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		readiness := a.Readiness()
		detail := map[string]string{}
		allReady := true
		for kb, err := range readiness {
			if err != nil {
				allReady = false
				detail[kb] = err.Error()
			} else {
				detail[kb] = "ready"
			}
		}
		status := http.StatusOK
		if !allReady {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"ready": allReady, "knowledge_bases": detail})
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
}
