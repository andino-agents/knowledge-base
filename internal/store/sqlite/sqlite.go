// Package sqlite implements the store.Store interface on a single SQLite
// file per knowledge base: FTS5 for keyword retrieval, and embeddings
// persisted as BLOBs served by an in-memory cosine KNN index (see vec.go).
// It is the only storage provider in v0.1.
//
// The build is pure Go (ncruces/go-sqlite3, wasm2go): CGO_ENABLED=0 holds
// and the binary stays static.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"

	"github.com/andino-agents/knowledge-base/internal/store"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func init() {
	store.Register("sqlite", open)
}

// Provider config keys (from the YAML storage block).
const (
	cfgDataDir = "data_dir"
)

type sqliteStore struct {
	db   *sql.DB
	dim  int
	path string
	vidx *vecIndex
}

func open(ctx context.Context, opts store.Options) (store.Store, error) {
	if opts.KBName == "" || opts.ModelName == "" || opts.Dimensions <= 0 {
		return nil, fmt.Errorf("sqlite: KBName, ModelName and Dimensions are required")
	}
	dataDir, _ := opts.ProviderConfig[cfgDataDir].(string)
	if dataDir == "" {
		return nil, fmt.Errorf("sqlite: provider config %q is required", cfgDataDir)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("sqlite: creating data dir: %w", err)
	}
	path := filepath.Join(dataDir, opts.KBName+".db")

	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(normal)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=busy_timeout(5000)"
	db, err := driver.Open(dsn, func(c *sqlite3.Conn) error {
		return fts5.Register(c)
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: opening %s: %w", path, err)
	}
	// SQLite has one writer; a single connection sidesteps SQLITE_BUSY between
	// our own writes while WAL still allows external readers.
	db.SetMaxOpenConns(1)

	s := &sqliteStore{db: db, dim: opts.Dimensions, path: path, vidx: newVecIndex(opts.Dimensions)}
	for _, step := range []func(context.Context) error{
		s.migrate,
		func(ctx context.Context) error { return s.checkIdentity(ctx, opts) },
		s.loadVecIndex,
	} {
		if err := step(ctx); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *sqliteStore) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("sqlite: reading user_version: %w", err)
	}
	names, err := migrationNames()
	if err != nil {
		return err
	}
	if version > len(names) {
		return fmt.Errorf("sqlite: database %s is at schema version %d, newer than this binary supports (%d); upgrade andino-kb",
			s.path, version, len(names))
	}
	for i := version; i < len(names); i++ {
		ddl, err := migrationsFS.ReadFile("migrations/" + names[i])
		if err != nil {
			return fmt.Errorf("sqlite: reading migration %s: %w", names[i], err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(ddl)); err != nil {
			tx.Rollback()
			return fmt.Errorf("sqlite: applying migration %s: %w", names[i], err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("sqlite: bumping user_version after %s: %w", names[i], err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func migrationNames() ([]string, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("sqlite: listing migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// checkIdentity records the embedding model and dimensions on first open and
// hard-fails on any later mismatch. Re-embedding requires an explicit
// `andino-kb index --rebuild`, which recreates the DB file — this code never
// wipes data on its own.
func (s *sqliteStore) checkIdentity(ctx context.Context, opts store.Options) error {
	want := map[string]string{
		"embedding_model":      opts.ModelName,
		"embedding_dimensions": strconv.Itoa(opts.Dimensions),
	}
	for key, wantVal := range want {
		var got string
		err := s.db.QueryRowContext(ctx, "SELECT value FROM kb_meta WHERE key = ?", key).Scan(&got)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := s.db.ExecContext(ctx,
				"INSERT INTO kb_meta(key, value) VALUES (?, ?)", key, wantVal); err != nil {
				return fmt.Errorf("sqlite: recording %s: %w", key, err)
			}
		case err != nil:
			return fmt.Errorf("sqlite: reading %s: %w", key, err)
		case got != wantVal:
			return fmt.Errorf("sqlite: %s mismatch for %s: store has %q, config says %q; "+
				"refusing to touch the index — run `andino-kb index --rebuild` to re-embed from scratch",
				key, s.path, got, wantVal)
		}
	}
	return nil
}

func (s *sqliteStore) loadVecIndex(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT chunk_id, embedding FROM chunk_embeddings")
	if err != nil {
		return fmt.Errorf("sqlite: loading embeddings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return err
		}
		vec, err := decodeVec(blob, s.dim)
		if err != nil {
			return fmt.Errorf("sqlite: chunk %d: %w (index built with another model? run `andino-kb index --rebuild`)", id, err)
		}
		s.vidx.add(id, vec)
	}
	return rows.Err()
}

func (s *sqliteStore) UpsertDocument(ctx context.Context, doc store.Document, chunks []store.Chunk, embeddings [][]float32) error {
	if len(chunks) != len(embeddings) {
		return fmt.Errorf("sqlite: %d chunks but %d embeddings", len(chunks), len(embeddings))
	}
	for i, e := range embeddings {
		if len(e) != s.dim {
			return fmt.Errorf("sqlite: embedding %d has dimension %d, store expects %d", i, len(e), s.dim)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	removedIDs, err := deleteDocumentTx(ctx, tx, doc.SourceName, doc.RelPath)
	if err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO documents(source_name, rel_path, uri, title, content_sha256, size_bytes, mtime_unix, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		doc.SourceName, doc.RelPath, doc.URI, doc.Title, doc.SHA256, doc.SizeBytes, doc.MtimeUnix, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("sqlite: inserting document %s: %w", doc.RelPath, err)
	}
	docID, err := res.LastInsertId()
	if err != nil {
		return err
	}

	insChunk, err := tx.PrepareContext(ctx, `
		INSERT INTO chunks(document_id, seq, heading_path, start_line, end_line, text, context, token_est)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insChunk.Close()
	insFTS, err := tx.PrepareContext(ctx,
		"INSERT INTO chunks_fts(rowid, text, heading_path, context) VALUES (?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer insFTS.Close()
	insVec, err := tx.PrepareContext(ctx,
		"INSERT INTO chunk_embeddings(chunk_id, embedding) VALUES (?, ?)")
	if err != nil {
		return err
	}
	defer insVec.Close()

	addedIDs := make([]int64, len(chunks))
	for i, c := range chunks {
		res, err := insChunk.ExecContext(ctx, docID, c.Seq, c.HeadingPath, c.StartLine, c.EndLine, c.Text, c.Context, c.TokenEst)
		if err != nil {
			return fmt.Errorf("sqlite: inserting chunk %d of %s: %w", c.Seq, doc.RelPath, err)
		}
		chunkID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := insFTS.ExecContext(ctx, chunkID, c.Text, c.HeadingPath, c.Context); err != nil {
			return fmt.Errorf("sqlite: indexing chunk %d of %s in FTS: %w", c.Seq, doc.RelPath, err)
		}
		if _, err := insVec.ExecContext(ctx, chunkID, encodeVec(embeddings[i])); err != nil {
			return fmt.Errorf("sqlite: storing embedding for chunk %d of %s: %w", c.Seq, doc.RelPath, err)
		}
		addedIDs[i] = chunkID
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO source_files(source_name, rel_path, content_sha256, size_bytes, mtime_unix, document_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_name, rel_path) DO UPDATE SET
			content_sha256 = excluded.content_sha256,
			size_bytes = excluded.size_bytes,
			mtime_unix = excluded.mtime_unix,
			document_id = excluded.document_id`,
		doc.SourceName, doc.RelPath, doc.SHA256, doc.SizeBytes, doc.MtimeUnix, docID); err != nil {
		return fmt.Errorf("sqlite: updating manifest for %s: %w", doc.RelPath, err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	// The in-memory index only changes after a successful commit, so a failed
	// transaction can never leave phantom vectors behind.
	for _, id := range removedIDs {
		s.vidx.remove(id)
	}
	for i, id := range addedIDs {
		s.vidx.add(id, embeddings[i])
	}
	return nil
}

func (s *sqliteStore) DeleteDocument(ctx context.Context, sourceName, relPath string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	removedIDs, err := deleteDocumentTx(ctx, tx, sourceName, relPath)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM source_files WHERE source_name = ? AND rel_path = ?", sourceName, relPath); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, id := range removedIDs {
		s.vidx.remove(id)
	}
	return nil
}

// deleteDocumentTx removes a document and its index entries, returning the
// ids of the removed chunks so the caller can update the in-memory vector
// index after commit. FTS5 external-content tables need explicit per-row
// deletes; chunk_embeddings rows go with the chunks via cascade.
func deleteDocumentTx(ctx context.Context, tx *sql.Tx, sourceName, relPath string) ([]int64, error) {
	var docID int64
	err := tx.QueryRowContext(ctx,
		"SELECT id FROM documents WHERE source_name = ? AND rel_path = ?", sourceName, relPath).Scan(&docID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx,
		"SELECT id, text, heading_path, context FROM chunks WHERE document_id = ?", docID)
	if err != nil {
		return nil, err
	}
	type ftsRow struct {
		id                    int64
		text, heading, extCtx string
	}
	var old []ftsRow
	for rows.Next() {
		var r ftsRow
		if err := rows.Scan(&r.id, &r.text, &r.heading, &r.extCtx); err != nil {
			rows.Close()
			return nil, err
		}
		old = append(old, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ids := make([]int64, 0, len(old))
	for _, r := range old {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO chunks_fts(chunks_fts, rowid, text, heading_path, context) VALUES ('delete', ?, ?, ?, ?)",
			r.id, r.text, r.heading, r.extCtx); err != nil {
			return nil, fmt.Errorf("sqlite: removing chunk %d from FTS: %w", r.id, err)
		}
		ids = append(ids, r.id)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM documents WHERE id = ?", docID); err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *sqliteStore) Manifest(ctx context.Context, sourceName string) (map[string]store.FileState, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT rel_path, content_sha256, size_bytes, mtime_unix FROM source_files WHERE source_name = ?", sourceName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]store.FileState{}
	for rows.Next() {
		var relPath string
		var fs store.FileState
		if err := rows.Scan(&relPath, &fs.SHA256, &fs.SizeBytes, &fs.MtimeUnix); err != nil {
			return nil, err
		}
		m[relPath] = fs
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Empty-index guard: a non-empty manifest with zero chunks means the
	// index was lost while the state survived. Report an empty manifest so
	// the indexer performs a full resync instead of trusting "no changes".
	if len(m) > 0 {
		var chunkCount int64
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM chunks").Scan(&chunkCount); err != nil {
			return nil, err
		}
		if chunkCount == 0 {
			return map[string]store.FileState{}, nil
		}
	}
	return m, nil
}

func (s *sqliteStore) TouchManifest(ctx context.Context, sourceName, relPath string, fs store.FileState) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO source_files(source_name, rel_path, content_sha256, size_bytes, mtime_unix, document_id)
		VALUES (?, ?, ?, ?, ?, (SELECT id FROM documents WHERE source_name = ? AND rel_path = ?))
		ON CONFLICT(source_name, rel_path) DO UPDATE SET
			content_sha256 = excluded.content_sha256,
			size_bytes = excluded.size_bytes,
			mtime_unix = excluded.mtime_unix`,
		sourceName, relPath, fs.SHA256, fs.SizeBytes, fs.MtimeUnix, sourceName, relPath)
	return err
}

func (s *sqliteStore) GetDocument(ctx context.Context, sourceName, relPath string) (*store.DocumentContent, error) {
	q := "SELECT id, source_name, rel_path, uri, title, content_sha256, size_bytes, mtime_unix FROM documents WHERE rel_path = ?"
	args := []any{relPath}
	if sourceName != "" {
		q += " AND source_name = ?"
		args = append(args, sourceName)
	}
	var d store.Document
	err := s.db.QueryRowContext(ctx, q, args...).Scan(
		&d.ID, &d.SourceName, &d.RelPath, &d.URI, &d.Title, &d.SHA256, &d.SizeBytes, &d.MtimeUnix)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT text FROM chunks WHERE document_id = ? ORDER BY seq", d.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		parts = append(parts, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &store.DocumentContent{Document: d, Text: strings.Join(parts, "\n\n")}, nil
}

func (s *sqliteStore) ListDocuments(ctx context.Context, prefix, cursor string, limit int) ([]store.Document, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source_name, rel_path, uri, title, content_sha256, size_bytes, mtime_unix
		FROM documents
		WHERE rel_path LIKE ? ESCAPE '\' AND rel_path > ?
		ORDER BY rel_path LIMIT ?`,
		likePrefix(prefix), cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []store.Document
	for rows.Next() {
		var d store.Document
		if err := rows.Scan(&d.ID, &d.SourceName, &d.RelPath, &d.URI, &d.Title, &d.SHA256, &d.SizeBytes, &d.MtimeUnix); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

func likePrefix(prefix string) string {
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	return esc + "%"
}

func (s *sqliteStore) Stats(ctx context.Context) (store.Stats, error) {
	var st store.Stats
	if err := s.db.QueryRowContext(ctx,
		"SELECT count(*), coalesce(max(indexed_at), 0) FROM documents").Scan(&st.Documents, &st.LastIndexedAt); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM chunks").Scan(&st.Chunks); err != nil {
		return st, err
	}
	return st, nil
}

func (s *sqliteStore) Close() error {
	// TRUNCATE the WAL so the .db file is self-contained after shutdown.
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return s.db.Close()
}
