// Package storetest is a reusable conformance harness for store.Store
// implementations. Any future provider (pgvector, OpenSearch, ...) must pass
// TestStore to be considered conformant.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/andino-agents/knowledge-base/internal/store"
)

// Dimensions is the vector size the harness uses. Providers under test must
// be opened with this dimension.
const Dimensions = 8

// vec returns a deterministic unit-ish vector whose direction encodes seed.
func vec(seed int) []float32 {
	v := make([]float32, Dimensions)
	for i := range v {
		v[i] = float32((seed*31+i*7)%13) / 13.0
	}
	v[seed%Dimensions] = 1.0
	return v
}

func doc(source, relPath, title string) store.Document {
	return store.Document{
		SourceName: source,
		RelPath:    relPath,
		URI:        "file:///" + relPath,
		Title:      title,
		SHA256:     "sha-" + relPath,
		SizeBytes:  100,
		MtimeUnix:  1700000000,
		Metadata:   map[string]string{"origin": "harness", "path": relPath},
	}
}

// TestStore runs the conformance suite against a freshly-opened, empty store.
func TestStore(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("upsert_and_get", func(t *testing.T) {
		d := doc("src", "notes/alpha.md", "Alpha")
		chunks := []store.Chunk{
			{Seq: 0, HeadingPath: "Alpha", StartLine: 1, EndLine: 5, Text: "the quick brown fox jumps over kubernetes", TokenEst: 8},
			{Seq: 1, HeadingPath: "Alpha > Details", StartLine: 6, EndLine: 10, Text: "terraform state locking with dynamodb tables", TokenEst: 7},
		}
		if err := s.UpsertDocument(ctx, d, chunks, [][]float32{vec(1), vec(2)}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		got, err := s.GetDocument(ctx, "src", "notes/alpha.md")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Document.Title != "Alpha" {
			t.Errorf("title = %q, want Alpha", got.Document.Title)
		}
		if got.Document.Metadata["origin"] != "harness" {
			t.Errorf("metadata lost on round-trip: %v", got.Document.Metadata)
		}
		if got.Text == "" {
			t.Error("empty reassembled text")
		}
	})

	t.Run("keyword_hit", func(t *testing.T) {
		hits, err := s.HybridSearch(ctx, `terraform "state locking?"`, vec(99), 5)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("no hits for keyword query")
		}
		found := false
		for _, h := range hits {
			if h.RelPath == "notes/alpha.md" && h.FTSRank > 0 {
				found = true
				if h.Metadata["origin"] != "harness" {
					t.Errorf("hit missing metadata: %v", h.Metadata)
				}
			}
		}
		if !found {
			t.Errorf("expected notes/alpha.md with FTS rank, got %+v", hits)
		}
	})

	t.Run("vector_hit", func(t *testing.T) {
		// Query with exactly chunk 0's vector: it must come back first.
		hits, err := s.HybridSearch(ctx, "zzz-no-keyword-match-zzz", vec(1), 3)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(hits) == 0 || hits[0].VecRank != 1 {
			t.Fatalf("expected top hit by vector rank 1, got %+v", hits)
		}
	})

	t.Run("upsert_replaces", func(t *testing.T) {
		d := doc("src", "notes/alpha.md", "Alpha v2")
		chunks := []store.Chunk{
			{Seq: 0, StartLine: 1, EndLine: 3, Text: "completely rewritten content about opensearch", TokenEst: 6},
		}
		if err := s.UpsertDocument(ctx, d, chunks, [][]float32{vec(3)}); err != nil {
			t.Fatalf("upsert v2: %v", err)
		}
		hits, err := s.HybridSearch(ctx, "terraform dynamodb", vec(99), 5)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			if h.RelPath == "notes/alpha.md" && h.FTSRank > 0 {
				t.Errorf("stale FTS content survived replacement: %+v", h)
			}
		}
		st, err := s.Stats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.Documents != 1 || st.Chunks != 1 {
			t.Errorf("stats = %+v, want 1 doc / 1 chunk", st)
		}
	})

	t.Run("mismatched_embeddings_rejected", func(t *testing.T) {
		d := doc("src", "notes/bad.md", "Bad")
		err := s.UpsertDocument(ctx, d,
			[]store.Chunk{{Seq: 0, Text: "x", StartLine: 1, EndLine: 1, TokenEst: 1}},
			[][]float32{})
		if err == nil {
			t.Fatal("chunks/embeddings length mismatch was accepted")
		}
		wrong := make([]float32, Dimensions+1)
		err = s.UpsertDocument(ctx, d,
			[]store.Chunk{{Seq: 0, Text: "x", StartLine: 1, EndLine: 1, TokenEst: 1}},
			[][]float32{wrong})
		if err == nil {
			t.Fatal("wrong-dimension embedding was accepted")
		}
		if _, err := s.GetDocument(ctx, "src", "notes/bad.md"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("failed upsert left partial state: err=%v", err)
		}
	})

	t.Run("manifest_and_delete", func(t *testing.T) {
		m, err := s.Manifest(ctx, "src")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := m["notes/alpha.md"]; !ok {
			t.Errorf("manifest missing notes/alpha.md: %v", m)
		}
		if err := s.DeleteDocument(ctx, "src", "notes/alpha.md"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if err := s.DeleteDocument(ctx, "src", "notes/alpha.md"); err != nil {
			t.Fatalf("double delete should be a no-op, got: %v", err)
		}
		m, err = s.Manifest(ctx, "src")
		if err != nil {
			t.Fatal(err)
		}
		if len(m) != 0 {
			t.Errorf("manifest not empty after delete: %v", m)
		}
		if _, err := s.GetDocument(ctx, "src", "notes/alpha.md"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("want ErrNotFound after delete, got %v", err)
		}
	})

	t.Run("list_documents_paging", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			rel := fmt.Sprintf("pages/p%02d.md", i)
			d := doc("src", rel, rel)
			err := s.UpsertDocument(ctx, d,
				[]store.Chunk{{Seq: 0, Text: "page " + rel, StartLine: 1, EndLine: 1, TokenEst: 2}},
				[][]float32{vec(10 + i)})
			if err != nil {
				t.Fatal(err)
			}
		}
		page1, err := s.ListDocuments(ctx, "pages/", "", 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(page1) != 3 {
			t.Fatalf("page1 len = %d, want 3", len(page1))
		}
		page2, err := s.ListDocuments(ctx, "pages/", page1[len(page1)-1].RelPath, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(page2) != 2 {
			t.Fatalf("page2 len = %d, want 2", len(page2))
		}
	})
}
