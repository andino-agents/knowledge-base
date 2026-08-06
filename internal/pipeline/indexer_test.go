package pipeline

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andino-agents/knowledge-base/internal/config"
	"github.com/andino-agents/knowledge-base/internal/inference"
	"github.com/andino-agents/knowledge-base/internal/pipeline/extract"
	"github.com/andino-agents/knowledge-base/internal/source/localdir"
	"github.com/andino-agents/knowledge-base/internal/store"
	_ "github.com/andino-agents/knowledge-base/internal/store/sqlite"
)

const testDim = 16

// fakeEmbeddings is a deterministic OpenAI-compatible /v1/embeddings server:
// each text hashes to a stable vector, so tests never need a real model.
func fakeEmbeddings(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		resp := struct {
			Data []item `json:"data"`
		}{}
		for i, text := range req.Input {
			h := fnv.New64a()
			h.Write([]byte(text))
			seed := h.Sum64()
			vec := make([]float32, testDim)
			for j := range vec {
				seed = seed*6364136223846793005 + 1442695040888963407
				vec[j] = float32(int64(seed>>33))/float32(1<<30) + 0.001
			}
			resp.Data = append(resp.Data, item{Index: i, Embedding: vec})
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func copyFixtureVault(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join("..", "..", "testdata", "vault")
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func newTestIndexer(t *testing.T, vaultDir string) (*Indexer, *localdir.Dir) {
	t.Helper()
	srv := fakeEmbeddings(t)
	reg := extract.Default()
	st, err := store.Open(context.Background(), "sqlite", store.Options{
		KBName: "test", ModelName: "fake", Dimensions: testDim,
		ProviderConfig: map[string]any{"data_dir": t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	src, err := localdir.New("vault", vaultDir,
		[]string{"**/*"}, []string{".obsidian/**"}, reg.Extensions())
	if err != nil {
		t.Fatal(err)
	}
	ix := &Indexer{
		Store:    st,
		Embedder: &inference.Embedder{BaseURL: srv.URL + "/v1", Model: "fake", Dimensions: testDim},
		Registry: reg,
		Chunking: config.Chunking{MaxTokens: 64, OverlapTokens: 8},
	}
	return ix, src
}

func TestFullAndIncrementalSync(t *testing.T) {
	ctx := context.Background()
	vault := copyFixtureVault(t)
	ix, src := newTestIndexer(t, vault)

	// Full sync: 4 indexable files with content (3 md + 1 go), empty.md
	// tracked but not indexed, png/drawio/.obsidian excluded.
	stats, err := ix.SyncSource(ctx, src)
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	if stats.Indexed != 4 || stats.Failed != 0 || stats.Deleted != 0 {
		t.Fatalf("full sync stats = %+v, want 4 indexed", stats)
	}
	st, _ := ix.Store.Stats(ctx)
	if st.Documents != 4 {
		t.Fatalf("documents = %d, want 4", st.Documents)
	}

	// Second sync: everything skipped.
	stats, err = ix.SyncSource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Indexed != 0 || stats.Deleted != 0 || stats.Failed != 0 {
		t.Fatalf("no-op sync stats = %+v", stats)
	}

	// Touch one file with new content: exactly one reindex.
	target := filepath.Join(vault, "notes", "terraform.md")
	if err := os.WriteFile(target, []byte("# Terraform state\n\nMoved to GCS backend.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Ensure mtime moves even on coarse filesystems.
	past := time.Now().Add(2 * time.Second)
	os.Chtimes(target, past, past)
	stats, err = ix.SyncSource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Indexed != 1 {
		t.Fatalf("after touch stats = %+v, want exactly 1 indexed", stats)
	}

	// Delete a file: removed from the index.
	if err := os.Remove(filepath.Join(vault, "notes", "kubernetes.md")); err != nil {
		t.Fatal(err)
	}
	stats, err = ix.SyncSource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 1 {
		t.Fatalf("after delete stats = %+v, want 1 deleted", stats)
	}

	// Search reaches the new content end-to-end.
	vecs, err := ix.Embedder.Embed(ctx, []string{"Terraform state\n\n# Terraform state\n\nMoved to GCS backend."})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := ix.Store.HybridSearch(ctx, "GCS backend", vecs[0], 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].RelPath != "notes/terraform.md" {
		t.Fatalf("hybrid search after resync: %+v", hits)
	}
}

func TestSyncPathReindexesAndDeletes(t *testing.T) {
	ctx := context.Background()
	vault := copyFixtureVault(t)
	ix, src := newTestIndexer(t, vault)
	if _, err := ix.SyncSource(ctx, src); err != nil {
		t.Fatal(err)
	}

	// Atomic-write style replacement: temp file + rename over the original.
	target := filepath.Join(vault, "notes", "terraform.md")
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, []byte("# Terraform state\n\nAtomic rename content.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, target); err != nil {
		t.Fatal(err)
	}
	if err := ix.SyncPath(ctx, src, "notes/terraform.md"); err != nil {
		t.Fatal(err)
	}
	doc, err := ix.Store.GetDocument(ctx, "vault", "notes/terraform.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Atomic rename content."; !strings.Contains(doc.Text, want) {
		t.Fatalf("document text %q does not contain %q", doc.Text, want)
	}

	// Path gone from disk: SyncPath deletes it from the index.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := ix.SyncPath(ctx, src, "notes/terraform.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Store.GetDocument(ctx, "vault", "notes/terraform.md"); err == nil {
		t.Fatal("document survived SyncPath after deletion")
	}
}

func TestEmbeddingFailureAbortsWithoutPartialState(t *testing.T) {
	ctx := context.Background()
	vault := copyFixtureVault(t)
	ix, src := newTestIndexer(t, vault)

	// Point the embedder at a dead endpoint: the sync must abort (fatal), not
	// grind through every file, and must leave nothing indexed.
	ix.Embedder = &inference.Embedder{
		BaseURL: "http://127.0.0.1:1", Model: "fake", Dimensions: testDim,
		MaxRetries: 1, Client: &http.Client{Timeout: 200 * time.Millisecond},
	}
	_, err := ix.SyncSource(ctx, src)
	if err == nil {
		t.Fatal("sync with dead embedding endpoint must fail")
	}
	st, _ := ix.Store.Stats(ctx)
	if st.Documents != 0 {
		t.Fatalf("partial state after aborted sync: %+v", st)
	}
}
