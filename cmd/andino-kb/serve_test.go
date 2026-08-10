package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/andino-agents/knowledge-base/internal/config"
	"github.com/andino-agents/knowledge-base/internal/mcpserver"
)

// The scope of a key must decide what it can do over MCP, exactly as it does
// over REST. Before the fix authMCP accepted any valid key and the MCP server
// handed it the write tools, so a read-scoped key could store and delete.
func TestWritesAllowed(t *testing.T) {
	keys := []config.APIKey{
		{Key: "read-key", Scope: "read"},
		{Key: "write-key", Scope: "readwrite"},
	}

	tests := []struct {
		name   string
		keys   []config.APIKey
		header string
		want   bool
	}{
		{"no keys configured, server is open", nil, "", true},
		{"read-scoped key cannot write", keys, "Bearer read-key", false},
		{"readwrite-scoped key can write", keys, "Bearer write-key", true},
		{"unknown token cannot write", keys, "Bearer nope", false},
		{"missing header cannot write", keys, "", false},
		{"malformed header cannot write", keys, "write-key", false},
		{"prefix is case-sensitive", keys, "bearer write-key", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{Server: config.Server{APIKeys: tt.keys}}
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			if got := writesAllowed(cfg, r); got != tt.want {
				t.Errorf("writesAllowed = %v, want %v", got, tt.want)
			}
		})
	}
}

// authMCP is the door: a valid key of any scope gets in, everything else does
// not. What it may do once inside is writesAllowed's job.
func TestAuthMCP(t *testing.T) {
	keys := []config.APIKey{{Key: "read-key", Scope: "read"}}

	tests := []struct {
		name   string
		keys   []config.APIKey
		header string
		want   int
	}{
		{"no keys configured, open", nil, "", http.StatusOK},
		{"valid read key gets in", keys, "Bearer read-key", http.StatusOK},
		{"unknown token rejected", keys, "Bearer nope", http.StatusUnauthorized},
		{"missing header rejected", keys, "", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{Server: config.Server{APIKeys: tt.keys}}
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			w := httptest.NewRecorder()
			authMCP(cfg, next).ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

// bearerRT stamps an Authorization header on every request.
type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// End-to-end over real HTTP, because the whole fix rests on one assumption:
// that the SDK calls the server factory with the same request the middleware
// saw, so the factory can read the key off it. A unit test of writesAllowed
// cannot prove that; this can.
func TestMCPToolsOverHTTP(t *testing.T) {
	cfg := &config.Config{Server: config.Server{APIKeys: []config.APIKey{
		{Key: "read-key", Scope: "read"},
		{Key: "write-key", Scope: "readwrite"},
	}}}

	// A nil *app.App is enough: this asserts on tools/list, which registers
	// handlers without ever invoking them.
	srv := httptest.NewServer(authMCP(cfg, mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			return mcpserver.New(nil, "test", writesAllowed(cfg, r))
		},
		&mcp.StreamableHTTPOptions{Stateless: true},
	)))
	defer srv.Close()

	listTools := func(t *testing.T, token string) []string {
		t.Helper()
		ctx := context.Background()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint:             srv.URL,
				HTTPClient:           &http.Client{Transport: bearerRT{token}},
				DisableStandaloneSSE: true,
			}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		res, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		slices.Sort(names)
		return names
	}

	readOnly := []string{"get_document", "list_documents", "list_knowledge_bases", "search"}
	if got := listTools(t, "read-key"); !slices.Equal(got, readOnly) {
		t.Errorf("read key sees %v, want %v", got, readOnly)
	}
	if got := listTools(t, "write-key"); !slices.Contains(got, "store") {
		t.Errorf("readwrite key sees %v, want it to include store", got)
	}
}
