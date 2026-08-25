// Package pgvector implements store.Store on PostgreSQL with the pgvector
// extension: tsvector full-text search for keyword retrieval, a pgvector
// (HNSW) index for dense retrieval, and Reciprocal Rank Fusion in Go. It is
// the second storage provider, alongside sqlite.
//
// The driver is pgx (pure Go): CGO_ENABLED=0 still holds and the binary stays
// static. Embeddings bind and scan through pgvector-go's Vector type, which
// implements driver.Valuer and sql.Scanner.
package pgvector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pgvector/pgvector-go"

	"github.com/andino-agents/knowledge-base/internal/store"
)

// lineComment matches a -- comment to end of line, for splitting the schema.
var lineComment = regexp.MustCompile(`--[^\n]*`)

// pgvector caps embedding dimensions at 16000; larger is a schema error.
const (
	maxDim = 16000
	cfgDSN = "dsn"
)

func init() {
	store.Register("postgres", open)
}

type pgvectorStore struct {
	db     *sql.DB
	dim    int
	dsn    string
	kbName string
}

// schema returns the KB's schema as a quoted identifier, so it is safe to drop
// straight into DDL even when the name carries characters Postgres would read
// as syntax.
func (s *pgvectorStore) schema() string {
	return quoteIdent(s.kbName)
}

// quoteIdent wraps a name in double quotes and escapes any embedded quote,
// following PostgreSQL's identifier rules, so an arbitrary KB name is a safe
// schema name rather than a SQL-injection vector.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// withSearchPath scopes a connection string to one schema. The schema is listed
// before public so the provider's own tables win, while extension types still
// resolve. The value is URL-encoded for the DSN query string.
func withSearchPath(dsn, kbName string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + url.QueryEscape(quoteIdent(kbName)+", public")
}

func open(ctx context.Context, opts store.Options) (store.Store, error) {
	if opts.KBName == "" || opts.ModelName == "" || opts.Dimensions <= 0 {
		return nil, fmt.Errorf("postgres: KBName, ModelName and Dimensions are required")
	}
	if opts.Dimensions > maxDim {
		return nil, fmt.Errorf("postgres: embedding dimensions %d exceed pgvector's %d", opts.Dimensions, maxDim)
	}
	dsn, _ := opts.ProviderConfig[cfgDSN].(string)
	if dsn == "" {
		return nil, fmt.Errorf("postgres: provider config %q is required", cfgDSN)
	}

	// Each knowledge base lives in its own schema, so several KBs can share a
	// single Postgres database without colliding on rows. search_path is set on
	// the connection (via the DSN, ahead of "public" so extension types still
	// resolve); every pooled connection and every unqualified query lands there.
	connDSN := withSearchPath(dsn, opts.KBName)

	db, err := sql.Open("pgx", connDSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: opening %s: %w", cfgDSN, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres: connecting to %s: %w", cfgDSN, err)
	}

	s := &pgvectorStore{db: db, dim: opts.Dimensions, dsn: connDSN, kbName: opts.KBName}
	for _, step := range []func(context.Context) error{
		s.migrate,
		func(ctx context.Context) error { return s.checkIdentity(ctx, opts) },
	} {
		if err := step(ctx); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

// migrate creates the schema idempotently. A fresh provider has no versioned
// migration history, so every object uses IF NOT EXISTS and re-running is safe.
func (s *pgvectorStore) migrate(ctx context.Context) error {
	// The schema is created before the extension so pgvector's objects land in
	// it too; IF NOT EXISTS keeps re-runs idempotent.
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", s.schema())); err != nil {
		return fmt.Errorf("postgres: creating schema %s: %w", s.kbName, err)
	}
	if err := s.createExtension(ctx); err != nil {
		return err
	}
	stmts := splitDDL(schemaFor(s.dim))
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("postgres: applying schema statement: %w (statement: %s)", err, truncate(stmt, 120))
		}
	}
	s.createDenseIndex(ctx)
	return nil
}

// createExtension installs pgvector. The extension ships under different names
// across installs: the postgresql-contrib package registers it as "pgvector",
// while some images (and the pgvector Makefile) name it "vector". Try both;
// the vector type is required for every table below, so a failure here is fatal.
func (s *pgvectorStore) createExtension(ctx context.Context) error {
	// SCHEMA public pins the extension's objects (the vector type, operators)
	// where every KB's search_path can see them; a bare CREATE EXTENSION would
	// land them in the first KB's schema and strand the rest.
	for _, name := range []string{"pgvector", "vector"} {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %s SCHEMA public", name)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("postgres: pgvector extension not available (expected one of %q or %q)", "pgvector", "vector")
}

// createDenseIndex builds the hnsw index for the dense leg. It is best-effort:
// some pgvector builds lack the hnsw access method or a matching operator
// class, and failing there would make the provider unusable. On any error we
// log and keep going — the ORDER BY <=> query still returns correct results
// via a sequential scan, just slower.
func (s *pgvectorStore) createDenseIndex(ctx context.Context) {
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_chunk_embeddings_embedding
		ON chunk_embeddings USING hnsw (embedding cosine_similarity)`); err != nil {
		slog.Warn("pgvector: hnsw index not created; dense search will use a sequential scan", "error", err)
	}
}

// schemaFor renders the DDL with the configured dimension substituted into the
// pgvector type. dim is validated in open, so the fixed placeholder :dim is
// replaced verbatim (no Sprintf: the DDL carries no format directives).
func schemaFor(dim int) string {
	return strings.ReplaceAll(schemaTemplate, ":dim", strconv.Itoa(dim))
}

const schemaTemplate = `
CREATE TABLE IF NOT EXISTS documents (
    id             BIGSERIAL PRIMARY KEY,
    source_name    TEXT NOT NULL,
    rel_path       TEXT NOT NULL,
    uri            TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    content_sha256 TEXT NOT NULL,
    size_bytes     BIGINT NOT NULL,
    mtime_unix     BIGINT NOT NULL,
    indexed_at     BIGINT NOT NULL,
    metadata       TEXT NOT NULL DEFAULT '{}',
    UNIQUE (source_name, rel_path)
);

CREATE TABLE IF NOT EXISTS chunks (
    id           BIGSERIAL PRIMARY KEY,
    document_id  BIGINT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    heading_path TEXT NOT NULL DEFAULT '',
    start_line   INTEGER NOT NULL,
    end_line     INTEGER NOT NULL,
    text         TEXT NOT NULL,
    context      TEXT NOT NULL DEFAULT '',
    token_est    INTEGER NOT NULL,
    UNIQUE (document_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_chunks_document ON chunks(document_id);

-- Full-text search over text + heading + context (contextual retrieval).
-- The expression is indexed so bm25 ranking stays cheap at scale.
CREATE INDEX IF NOT EXISTS idx_chunks_fts ON chunks USING gin (
    to_tsvector('english', text || ' ' || heading_path || ' ' || COALESCE(context, ''))
);

-- Dense retrieval. The hnsw index below is created separately (best-effort);
-- the ORDER BY <=> query is correct without it, so a missing index only
-- affects speed, never results.
CREATE TABLE IF NOT EXISTS chunk_embeddings (
    chunk_id  BIGINT PRIMARY KEY REFERENCES chunks(id) ON DELETE CASCADE,
    embedding vector(:dim) NOT NULL
);

CREATE TABLE IF NOT EXISTS source_files (
    source_name    TEXT NOT NULL,
    rel_path       TEXT NOT NULL,
    content_sha256 TEXT NOT NULL,
    size_bytes     BIGINT NOT NULL,
    mtime_unix     BIGINT NOT NULL,
    document_id    BIGINT REFERENCES documents(id) ON DELETE SET NULL,
    PRIMARY KEY (source_name, rel_path)
);

CREATE TABLE IF NOT EXISTS kb_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// checkIdentity records the embedding model and dimensions on first open and
// hard-fails on any later mismatch. A mismatch means re-embedding from scratch
// (`andino-kb index --rebuild`); this code never drops data on its own.
func (s *pgvectorStore) checkIdentity(ctx context.Context, opts store.Options) error {
	want := map[string]string{
		"embedding_model":      opts.ModelName,
		"embedding_dimensions": fmt.Sprintf("%d", opts.Dimensions),
	}
	for key, wantVal := range want {
		var got string
		err := s.db.QueryRowContext(ctx, "SELECT value FROM kb_meta WHERE key = $1", key).Scan(&got)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := s.db.ExecContext(ctx,
				"INSERT INTO kb_meta(key, value) VALUES ($1, $2)", key, wantVal); err != nil {
				return fmt.Errorf("postgres: recording %s: %w", key, err)
			}
		case err != nil:
			return fmt.Errorf("postgres: reading %s: %w", key, err)
		case got != wantVal:
			return fmt.Errorf("postgres: %s mismatch for %s: store has %q, config says %q; "+
				"refusing to touch the index — run `andino-kb index --rebuild` to re-embed from scratch",
				key, opts.KBName, got, wantVal)
		}
	}
	return nil
}

func (s *pgvectorStore) UpsertDocument(ctx context.Context, doc store.Document, chunks []store.Chunk, embeddings [][]float32) error {
	if len(chunks) != len(embeddings) {
		return fmt.Errorf("postgres: %d chunks but %d embeddings", len(chunks), len(embeddings))
	}
	for i, e := range embeddings {
		if len(e) != s.dim {
			return fmt.Errorf("postgres: embedding %d has dimension %d, store expects %d", i, len(e), s.dim)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := deleteDocumentTx(ctx, tx, doc.SourceName, doc.RelPath); err != nil {
		return err
	}

	metaJSON, err := json.Marshal(orEmpty(doc.Metadata))
	if err != nil {
		return fmt.Errorf("postgres: marshaling metadata for %s: %w", doc.RelPath, err)
	}
	// pgx's stdlib driver does not report LastInsertId, so the ids come back
	// via RETURNING.
	var docID int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO documents(source_name, rel_path, uri, title, content_sha256, size_bytes, mtime_unix, indexed_at, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		doc.SourceName, doc.RelPath, doc.URI, doc.Title, doc.SHA256, doc.SizeBytes, doc.MtimeUnix, time.Now().Unix(), string(metaJSON)).Scan(&docID); err != nil {
		return fmt.Errorf("postgres: inserting document %s: %w", doc.RelPath, err)
	}

	insChunk, err := tx.PrepareContext(ctx, `
		INSERT INTO chunks(document_id, seq, heading_path, start_line, end_line, text, context, token_est)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`)
	if err != nil {
		return err
	}
	defer insChunk.Close()
	insVec, err := tx.PrepareContext(ctx,
		"INSERT INTO chunk_embeddings(chunk_id, embedding) VALUES ($1, $2)")
	if err != nil {
		return err
	}
	defer insVec.Close()

	for i, c := range chunks {
		var chunkID int64
		if err := insChunk.QueryRowContext(ctx, docID, c.Seq, c.HeadingPath, c.StartLine, c.EndLine, c.Text, c.Context, c.TokenEst).
			Scan(&chunkID); err != nil {
			return fmt.Errorf("postgres: inserting chunk %d of %s: %w", c.Seq, doc.RelPath, err)
		}
		if _, err := insVec.ExecContext(ctx, chunkID, pgvector.NewVector(embeddings[i])); err != nil {
			return fmt.Errorf("postgres: storing embedding for chunk %d of %s: %w", c.Seq, doc.RelPath, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO source_files(source_name, rel_path, content_sha256, size_bytes, mtime_unix, document_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT(source_name, rel_path) DO UPDATE SET
			content_sha256 = excluded.content_sha256,
			size_bytes = excluded.size_bytes,
			mtime_unix = excluded.mtime_unix,
			document_id = excluded.document_id`,
		doc.SourceName, doc.RelPath, doc.SHA256, doc.SizeBytes, doc.MtimeUnix, docID); err != nil {
		return fmt.Errorf("postgres: updating manifest for %s: %w", doc.RelPath, err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *pgvectorStore) DeleteDocument(ctx context.Context, sourceName, relPath string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteDocumentTx(ctx, tx, sourceName, relPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM source_files WHERE source_name = $1 AND rel_path = $2", sourceName, relPath); err != nil {
		return fmt.Errorf("postgres: removing manifest entry for %s: %w", relPath, err)
	}
	return tx.Commit()
}

// deleteDocumentTx removes the document; chunks and their embeddings go with
// it via ON DELETE CASCADE. Returns nothing: the vector search is server-side
// (HNSW), so there is no in-memory index to keep consistent.
func deleteDocumentTx(ctx context.Context, tx *sql.Tx, sourceName, relPath string) error {
	_, err := tx.ExecContext(ctx,
		"DELETE FROM documents WHERE source_name = $1 AND rel_path = $2", sourceName, relPath)
	return err
}

func (s *pgvectorStore) Manifest(ctx context.Context, sourceName string) (map[string]store.FileState, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT rel_path, content_sha256, size_bytes, mtime_unix FROM source_files WHERE source_name = $1", sourceName)
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

func (s *pgvectorStore) TouchManifest(ctx context.Context, sourceName, relPath string, fs store.FileState) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO source_files(source_name, rel_path, content_sha256, size_bytes, mtime_unix, document_id)
		VALUES ($1, $2, $3, $4, $5, (SELECT id FROM documents WHERE source_name = $6 AND rel_path = $7))
		ON CONFLICT(source_name, rel_path) DO UPDATE SET
			content_sha256 = excluded.content_sha256,
			size_bytes = excluded.size_bytes,
			mtime_unix = excluded.mtime_unix`,
		sourceName, relPath, fs.SHA256, fs.SizeBytes, fs.MtimeUnix, sourceName, relPath)
	return err
}

func (s *pgvectorStore) GetDocument(ctx context.Context, sourceName, relPath string) (*store.DocumentContent, error) {
	q := "SELECT id, source_name, rel_path, uri, title, content_sha256, size_bytes, mtime_unix, metadata FROM documents WHERE rel_path = $1"
	args := []any{relPath}
	if sourceName != "" {
		q += " AND source_name = $2"
		args = append(args, sourceName)
	}
	var d store.Document
	var metaJSON string
	err := s.db.QueryRowContext(ctx, q, args...).Scan(
		&d.ID, &d.SourceName, &d.RelPath, &d.URI, &d.Title, &d.SHA256, &d.SizeBytes, &d.MtimeUnix, &metaJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if d.Metadata, err = unmarshalMeta(metaJSON); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT text FROM chunks WHERE document_id = $1 ORDER BY seq", d.ID)
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

func (s *pgvectorStore) ListDocuments(ctx context.Context, prefix, cursor string, limit int) ([]store.Document, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source_name, rel_path, uri, title, content_sha256, size_bytes, mtime_unix, metadata
		FROM documents
		WHERE rel_path LIKE $1 ESCAPE '\' AND rel_path > $2
		ORDER BY rel_path LIMIT $3`,
		likePrefix(prefix), cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []store.Document
	for rows.Next() {
		var d store.Document
		var metaJSON string
		if err := rows.Scan(&d.ID, &d.SourceName, &d.RelPath, &d.URI, &d.Title, &d.SHA256, &d.SizeBytes, &d.MtimeUnix, &metaJSON); err != nil {
			return nil, err
		}
		if d.Metadata, err = unmarshalMeta(metaJSON); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

func (s *pgvectorStore) Stats(ctx context.Context) (store.Stats, error) {
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

func (s *pgvectorStore) Close() error {
	return s.db.Close()
}

// splitDDL breaks the schema into individual statements for ExecContext, which
// runs one statement at a time. -- line comments are stripped first so a
// semicolon inside a comment cannot split a statement.
func splitDDL(schema string) []string {
	noComments := lineComment.ReplaceAllString(schema, "")
	var stmts []string
	for _, stmt := range strings.Split(noComments, ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			stmts = append(stmts, s)
		}
	}
	return stmts
}

func likePrefix(prefix string) string {
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	return esc + "%"
}

// orEmpty keeps stored metadata JSON canonical: nil maps serialize as {}.
func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func unmarshalMeta(raw string) (map[string]string, error) {
	if raw == "" || raw == "{}" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("postgres: corrupt metadata %q: %w", raw, err)
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
