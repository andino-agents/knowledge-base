package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"

	"github.com/andino-agents/knowledge-base/internal/store"
	"github.com/andino-agents/knowledge-base/internal/store/storetest"
)

// TestMigrateV1WithData builds a database with the ORIGINAL v1 schema and
// real rows, then opens it through the provider (which applies migration
// 0002) and verifies the data survived and search still works.
func TestMigrateV1WithData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "kb.db")

	// v1 schema, as shipped in v0.1.0 (context column did not exist).
	v1 := `
CREATE TABLE documents (
    id INTEGER PRIMARY KEY, source_name TEXT NOT NULL, rel_path TEXT NOT NULL,
    uri TEXT NOT NULL, title TEXT NOT NULL DEFAULT '', content_sha256 TEXT NOT NULL,
    size_bytes INTEGER NOT NULL, mtime_unix INTEGER NOT NULL, indexed_at INTEGER NOT NULL,
    UNIQUE (source_name, rel_path));
CREATE TABLE chunks (
    id INTEGER PRIMARY KEY, document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL, heading_path TEXT NOT NULL DEFAULT '', start_line INTEGER NOT NULL,
    end_line INTEGER NOT NULL, text TEXT NOT NULL, token_est INTEGER NOT NULL,
    UNIQUE (document_id, seq));
CREATE INDEX idx_chunks_document ON chunks(document_id);
CREATE VIRTUAL TABLE chunks_fts USING fts5(
    text, heading_path, content='chunks', content_rowid='id',
    tokenize='unicode61 remove_diacritics 2');
CREATE TABLE chunk_embeddings (
    chunk_id INTEGER PRIMARY KEY REFERENCES chunks(id) ON DELETE CASCADE,
    embedding BLOB NOT NULL);
CREATE TABLE source_files (
    source_name TEXT NOT NULL, rel_path TEXT NOT NULL, content_sha256 TEXT NOT NULL,
    size_bytes INTEGER NOT NULL, mtime_unix INTEGER NOT NULL,
    document_id INTEGER REFERENCES documents(id) ON DELETE SET NULL,
    PRIMARY KEY (source_name, rel_path));
CREATE TABLE kb_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO kb_meta VALUES ('embedding_model', 'test-model'), ('embedding_dimensions', '8');
INSERT INTO documents VALUES (1, 'src', 'a.md', 'file:///a.md', 'A', 'sha', 10, 1, 1);
INSERT INTO chunks VALUES (1, 1, 0, 'A', 1, 3, 'terraform state locking with dynamodb', 6);
INSERT INTO chunks_fts(rowid, text, heading_path) VALUES (1, 'terraform state locking with dynamodb', 'A');
INSERT INTO source_files VALUES ('src', 'a.md', 'sha', 10, 1, 1);
PRAGMA user_version = 1;
`
	db, err := driver.Open("file:"+path, func(c *sqlite3.Conn) error { return fts5.Register(c) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(v1); err != nil {
		t.Fatalf("building v1 db: %v", err)
	}
	vec := make([]float32, storetest.Dimensions)
	vec[0] = 1
	if _, err := db.Exec("INSERT INTO chunk_embeddings VALUES (1, ?)", encodeVec(vec)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Open through the provider: migration 0002 must run.
	s, err := store.Open(ctx, "sqlite", store.Options{
		KBName: "kb", ModelName: "test-model", Dimensions: storetest.Dimensions,
		ProviderConfig: map[string]any{"data_dir": dir},
	})
	if err != nil {
		t.Fatalf("open after migration: %v", err)
	}
	defer s.Close()

	// Keyword search over migrated FTS (rebuilt with the context column).
	hits, err := s.HybridSearch(ctx, "terraform dynamodb", vec, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].RelPath != "a.md" || hits[0].FTSRank == 0 {
		t.Fatalf("migrated FTS search failed: %+v", hits)
	}

	// New writes with context work against the migrated schema.
	err = s.UpsertDocument(ctx, store.Document{
		SourceName: "src", RelPath: "b.md", URI: "file:///b.md", SHA256: "x", SizeBytes: 1, MtimeUnix: 1,
	}, []store.Chunk{{Seq: 0, Text: "kafka rebalance troubleshooting", Context: "Runbook for consumer group issues.", StartLine: 1, EndLine: 2, TokenEst: 4}},
		[][]float32{vec})
	if err != nil {
		t.Fatal(err)
	}
	// The context is BM25-searchable even though those words are not in the text.
	hits, err = s.HybridSearch(ctx, "runbook consumer group", vec, 3)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.RelPath == "b.md" && h.FTSRank > 0 {
			found = true
			if h.Context == "" {
				t.Error("hit missing context field")
			}
		}
	}
	if !found {
		t.Fatalf("context words not BM25-searchable: %+v", hits)
	}
}
