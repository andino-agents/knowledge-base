package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/andino-agents/knowledge-base/internal/store"
	"github.com/andino-agents/knowledge-base/internal/store/storetest"
)

func openTestStore(t *testing.T, kb string) store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), "sqlite", store.Options{
		KBName:         kb,
		ModelName:      "test-model",
		Dimensions:     storetest.Dimensions,
		ProviderConfig: map[string]any{"data_dir": t.TempDir()},
	})
	if err != nil {
		t.Fatalf("opening sqlite store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestConformance runs the provider-independent suite. This is also the
// phase-1 de-risking spike: it proves FTS5 and vec0 both work in the
// ncruces + sqlite-vec WASM build with CGO_ENABLED=0.
func TestConformance(t *testing.T) {
	storetest.TestStore(t, openTestStore(t, "conformance"))
}

func TestIdentityMismatchIsHardError(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	open := func(model string, dim int) (store.Store, error) {
		return store.Open(ctx, "sqlite", store.Options{
			KBName: "kb", ModelName: model, Dimensions: dim,
			ProviderConfig: map[string]any{"data_dir": dir},
		})
	}
	s, err := open("model-a", storetest.Dimensions)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := open("model-b", storetest.Dimensions); err == nil ||
		!strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("model change must be a hard error, got: %v", err)
	}
	if _, err := open("model-a", storetest.Dimensions+8); err == nil ||
		!strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("dimension change must be a hard error, got: %v", err)
	}
	// Same identity reopens fine.
	s, err = open("model-a", storetest.Dimensions)
	if err != nil {
		t.Fatalf("reopen with same identity: %v", err)
	}
	s.Close()
}

func TestSanitizeFTSQuery(t *testing.T) {
	cases := map[string]string{
		`what's "cache-reuse"?`:     `"what" OR "s" OR "cache-reuse"`,
		`ttm.pages_limit=25165824`:  `"ttm.pages_limit" OR "25165824"`,
		`(a AND b) NOT c*`:          `"a" OR "AND" OR "b" OR "NOT" OR "c"`,
		`   `:                       ``,
		`configuración de búsqueda`: `"configuración" OR "de" OR "búsqueda"`,
	}
	for in, want := range cases {
		if got := sanitizeFTSQuery(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEmptyIndexGuard(t *testing.T) {
	// Manifest must report empty when chunks are gone but state survived,
	// forcing a full resync instead of a silent "no changes".
	ctx := context.Background()
	s := openTestStore(t, "guard").(*sqliteStore)

	d := store.Document{SourceName: "src", RelPath: "a.md", URI: "file:///a.md", SHA256: "x", SizeBytes: 1, MtimeUnix: 1}
	err := s.UpsertDocument(ctx, d,
		[]store.Chunk{{Seq: 0, Text: "hello", StartLine: 1, EndLine: 1, TokenEst: 1}},
		[][]float32{make([]float32, storetest.Dimensions)})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate index loss with surviving manifest.
	if _, err := s.db.Exec("DELETE FROM chunks"); err != nil {
		t.Fatal(err)
	}
	m, err := s.Manifest(ctx, "src")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Fatalf("guard failed: manifest reported %d entries over an empty index", len(m))
	}
}
