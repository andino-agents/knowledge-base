package restapi

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andino-agents/knowledge-base/internal/app"
	"github.com/andino-agents/knowledge-base/internal/config"
	_ "github.com/andino-agents/knowledge-base/internal/store/sqlite"
)

const testDim = 16

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
			vec := make([]float32, testDim)
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

func newTestServer(t *testing.T, keys []config.APIKey) *httptest.Server {
	t.Helper()
	embSrv := fakeEmbeddings(t)
	cfg := &config.Config{
		Server: config.Server{
			Bind: "127.0.0.1:0", DataDir: t.TempDir(), APIKeys: keys,
			LogLevel: "error", LogFormat: "text",
		},
		Storage: config.Storage{Provider: "sqlite"},
		Inference: config.Inference{
			Backends: []config.Backend{{Name: "fake", BaseURL: embSrv.URL + "/v1"}},
			EmbeddingModels: []config.EmbeddingModel{{
				Name: "fake-embed", Backend: "fake", Model: "fake", Dimensions: testDim,
			}},
		},
		Defaults: config.Defaults{EmbeddingModel: "fake-embed"},
		KnowledgeBases: []config.KnowledgeBase{
			{Name: "memory", Writable: true},
		},
	}
	cfg2 := *cfg // applyDefaults is unexported; emulate the needed bits
	for i := range cfg2.KnowledgeBases {
		kb := &cfg2.KnowledgeBases[i]
		kb.EmbeddingModel = "fake-embed"
		ch := config.Chunking{Strategy: "markdown", MaxTokens: 128, OverlapTokens: 16}
		kb.Chunking = &ch
	}
	a, err := app.New(context.Background(), &cfg2, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	srv := httptest.NewServer(New(a, slog.Default()).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url, token, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	json.NewDecoder(resp.Body).Decode(&parsed)
	return resp, parsed
}

func TestWriteFlowEndToEnd(t *testing.T) {
	srv := newTestServer(t, nil)

	// store
	resp, body := do(t, "POST", srv.URL+"/v1/kb/memory/documents", "",
		`{"title":"Deploy preference","content":"User prefers systemd user units with linger."}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("store status = %d (%v)", resp.StatusCode, body)
	}
	id, _ := body["document_id"].(string)
	if !strings.HasPrefix(id, "memory_") {
		t.Fatalf("document_id = %q", id)
	}

	// retrieve via search
	resp, body = do(t, "POST", srv.URL+"/v1/kb/memory/search", "",
		`{"query":"systemd user units linger preference"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d", resp.StatusCode)
	}
	results := body["results"].([]any)
	if len(results) == 0 {
		t.Fatal("no results for stored memory")
	}
	first := results[0].(map[string]any)
	if first["rel_path"] != id {
		t.Errorf("top hit = %v, want %s", first["rel_path"], id)
	}

	// get by id
	resp, body = do(t, "GET", srv.URL+"/v1/kb/memory/document?path="+id, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d (%v)", resp.StatusCode, body)
	}

	// delete, then 404
	resp, _ = do(t, "DELETE", srv.URL+"/v1/kb/memory/documents/"+id, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp, _ = do(t, "DELETE", srv.URL+"/v1/kb/memory/documents/"+id, "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", resp.StatusCode)
	}
}

func TestAuthScopes(t *testing.T) {
	srv := newTestServer(t, []config.APIKey{
		{Key: "reader", Scope: "read"},
		{Key: "writer", Scope: "readwrite"},
	})
	cases := []struct {
		name, method, path, token, body string
		want                            int
	}{
		{"no_token", "GET", "/v1/kb", "", "", http.StatusUnauthorized},
		{"bad_token", "GET", "/v1/kb", "nope", "", http.StatusUnauthorized},
		{"read_ok", "GET", "/v1/kb", "reader", "", http.StatusOK},
		{"read_cannot_write", "POST", "/v1/kb/memory/documents", "reader", `{"content":"x"}`, http.StatusForbidden},
		{"writer_can_write", "POST", "/v1/kb/memory/documents", "writer", `{"content":"note about auth"}`, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := do(t, tc.method, srv.URL+tc.path, tc.token, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (%v)", resp.StatusCode, tc.want, body)
			}
		})
	}
}

func TestNonWritableKBRejectsStore(t *testing.T) {
	srv := newTestServer(t, nil)
	resp, body := do(t, "POST", srv.URL+"/v1/kb/nope/documents", "", `{"content":"x"}`)
	if resp.StatusCode == http.StatusCreated {
		t.Fatalf("store on unknown KB succeeded: %v", body)
	}
}
