// Package restapi exposes the knowledge bases over HTTP: search, document
// reads, agent writes, reindex jobs, health and metrics.
package restapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andino-agents/knowledge-base/internal/app"
	"github.com/andino-agents/knowledge-base/internal/store"
)

type Server struct {
	App    *app.App
	Logger *slog.Logger

	jobsMu sync.Mutex
	jobs   map[string]*reindexJob
	jobSeq int
}

type reindexJob struct {
	ID       string    `json:"id"`
	KB       string    `json:"knowledge_base"`
	Status   string    `json:"status"` // running | done | failed
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started_at"`
	Finished time.Time `json:"finished_at,omitzero"`
}

func New(a *app.App, logger *slog.Logger) *Server {
	return &Server{App: a, Logger: logger, jobs: map[string]*reindexJob{}}
}

// Routes registers the API on a mux. Auth wrapping happens in Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/kb", s.withAuth("read", s.listKBs))
	mux.HandleFunc("GET /v1/kb/{kb}", s.withAuth("read", s.getKB))
	mux.HandleFunc("POST /v1/kb/{kb}/search", s.withAuth("read", s.searchKB))
	mux.HandleFunc("POST /v1/search", s.withAuth("read", s.searchAll))
	mux.HandleFunc("GET /v1/kb/{kb}/documents", s.withAuth("read", s.listDocuments))
	mux.HandleFunc("GET /v1/kb/{kb}/document", s.withAuth("read", s.getDocument))
	mux.HandleFunc("POST /v1/kb/{kb}/documents", s.withAuth("readwrite", s.storeDocument))
	mux.HandleFunc("DELETE /v1/kb/{kb}/documents/{id}", s.withAuth("readwrite", s.deleteDocument))
	mux.HandleFunc("POST /v1/kb/{kb}/reindex", s.withAuth("readwrite", s.startReindex))
	mux.HandleFunc("GET /v1/kb/{kb}/reindex/{id}", s.withAuth("read", s.getReindex))
	return mux
}

// withAuth enforces API keys when configured. Scope "readwrite" implies
// "read"; a read-scoped key cannot call write endpoints.
func (s *Server) withAuth(need string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys := s.App.Config.Server.APIKeys
		if len(keys) == 0 {
			next(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if len(auth) <= len(prefix) || auth[:len(prefix)] != prefix {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		token := auth[len(prefix):]
		for _, k := range keys {
			if k.Key != token {
				continue
			}
			if need == "readwrite" && k.Scope != "readwrite" {
				writeErr(w, http.StatusForbidden, "this API key is read-only")
				return
			}
			next(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, "invalid API key")
	}
}

func (s *Server) listKBs(w http.ResponseWriter, r *http.Request) {
	type kbInfo struct {
		Name          string `json:"name"`
		Description   string `json:"description,omitempty"`
		Writable      bool   `json:"writable"`
		Documents     int64  `json:"documents"`
		Chunks        int64  `json:"chunks"`
		LastIndexedAt int64  `json:"last_indexed_at"`
	}
	var out []kbInfo
	for _, name := range s.App.KBNames() {
		kb, err := s.App.KB(name)
		if err != nil {
			continue
		}
		st, err := kb.Store.Stats(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, kbInfo{
			Name: name, Description: kb.Config.Description, Writable: kb.Config.Writable,
			Documents: st.Documents, Chunks: st.Chunks, LastIndexedAt: st.LastIndexedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"knowledge_bases": out})
}

func (s *Server) getKB(w http.ResponseWriter, r *http.Request) {
	kb, err := s.App.KB(r.PathValue("kb"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	st, err := kb.Store.Stats(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": kb.Config.Name, "description": kb.Config.Description,
		"writable": kb.Config.Writable, "documents": st.Documents,
		"chunks": st.Chunks, "last_indexed_at": st.LastIndexedAt,
	})
}

type searchRequest struct {
	Query     string            `json:"query"`
	Limit     int               `json:"limit"`
	MinScore  float64           `json:"min_score"`
	Rerank    *bool             `json:"rerank"`      // nil = KB default
	MaxPerDoc int               `json:"max_per_doc"` // 0 = default (2), negative = no cap
	Filter    map[string]string `json:"filter"`      // metadata equality, AND
}

func (r searchRequest) opts() app.SearchOpts {
	return app.SearchOpts{Limit: r.Limit, MinScore: r.MinScore, Rerank: r.Rerank, MaxPerDoc: r.MaxPerDoc, Filter: r.Filter}
}

func (s *Server) searchKB(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Query == "" {
		writeErr(w, http.StatusBadRequest, "body must be JSON with a non-empty query")
		return
	}
	start := time.Now()
	hits, err := s.App.Search(r.Context(), r.PathValue("kb"), req.Query, req.opts())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results":     hits,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func (s *Server) searchAll(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Query == "" {
		writeErr(w, http.StatusBadRequest, "body must be JSON with a non-empty query")
		return
	}
	start := time.Now()
	hits, err := s.App.SearchAll(r.Context(), req.Query, req.opts())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results":     hits,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func (s *Server) listDocuments(w http.ResponseWriter, r *http.Request) {
	kb, err := s.App.KB(r.PathValue("kb"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	docs, err := kb.Store.ListDocuments(r.Context(), r.URL.Query().Get("prefix"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	next := ""
	if len(docs) > 0 {
		next = docs[len(docs)-1].RelPath
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs, "next_cursor": next})
}

func (s *Server) getDocument(w http.ResponseWriter, r *http.Request) {
	kb, err := s.App.KB(r.PathValue("kb"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	doc, err := kb.Store.GetDocument(r.Context(), r.URL.Query().Get("source"), path)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	text := doc.Text
	if s0 := r.URL.Query().Get("start_line"); s0 != "" || r.URL.Query().Get("end_line") != "" {
		text = sliceLines(text,
			atoiDefault(r.URL.Query().Get("start_line"), 1),
			atoiDefault(r.URL.Query().Get("end_line"), 1<<30))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"document": doc.Document,
		"content":  text,
	})
}

type storeRequest struct {
	Content  string            `json:"content"`
	Title    string            `json:"title"`
	Metadata map[string]string `json:"metadata"`
}

func (s *Server) storeDocument(w http.ResponseWriter, r *http.Request) {
	var req storeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	id, err := s.App.StoreDocument(r.Context(), r.PathValue("kb"), req.Title, req.Content, req.Metadata)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"document_id": id})
}

func (s *Server) deleteDocument(w http.ResponseWriter, r *http.Request) {
	err := s.App.DeleteDocument(r.Context(), r.PathValue("kb"), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

func (s *Server) startReindex(w http.ResponseWriter, r *http.Request) {
	kbName := r.PathValue("kb")
	kb, err := s.App.KB(kbName)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	s.jobsMu.Lock()
	s.jobSeq++
	job := &reindexJob{
		ID: strconv.Itoa(s.jobSeq), KB: kbName,
		Status: "running", Started: time.Now(),
	}
	s.jobs[kbName+"/"+job.ID] = job
	s.jobsMu.Unlock()

	go func() {
		// Detached from the request context on purpose: the job outlives it.
		ctx := context.Background()
		var jobErr error
		for _, src := range kb.Sources {
			if _, err := kb.Indexer.SyncSource(ctx, src); err != nil {
				jobErr = err
				break
			}
		}
		s.jobsMu.Lock()
		defer s.jobsMu.Unlock()
		job.Finished = time.Now()
		if jobErr != nil {
			job.Status = "failed"
			job.Error = jobErr.Error()
		} else {
			job.Status = "done"
		}
	}()
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) getReindex(w http.ResponseWriter, r *http.Request) {
	s.jobsMu.Lock()
	job, ok := s.jobs[r.PathValue("kb")+"/"+r.PathValue("id")]
	s.jobsMu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "no such reindex job")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func sliceLines(text string, start, end int) string {
	lines := strings.Split(text, "\n")
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) || start > end {
		return ""
	}
	return strings.Join(lines[start-1:end], "\n")
}
