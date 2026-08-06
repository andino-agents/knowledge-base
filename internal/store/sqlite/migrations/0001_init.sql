CREATE TABLE documents (
    id             INTEGER PRIMARY KEY,
    source_name    TEXT NOT NULL,
    rel_path       TEXT NOT NULL,
    uri            TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    content_sha256 TEXT NOT NULL,
    size_bytes     INTEGER NOT NULL,
    mtime_unix     INTEGER NOT NULL,
    indexed_at     INTEGER NOT NULL,
    UNIQUE (source_name, rel_path)
);

CREATE TABLE chunks (
    id           INTEGER PRIMARY KEY,
    document_id  INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    heading_path TEXT NOT NULL DEFAULT '',
    start_line   INTEGER NOT NULL,
    end_line     INTEGER NOT NULL,
    text         TEXT NOT NULL,
    token_est    INTEGER NOT NULL,
    UNIQUE (document_id, seq)
);
CREATE INDEX idx_chunks_document ON chunks(document_id);

-- External-content FTS5 over chunks: no duplicated text storage. Kept in
-- sync explicitly by the store in the same transaction as chunk writes.
CREATE VIRTUAL TABLE chunks_fts USING fts5(
    text, heading_path,
    content='chunks', content_rowid='id',
    tokenize='unicode61 remove_diacritics 2'
);

-- Embeddings, little-endian float32 BLOBs (sqlite-vec layout). Loaded into an
-- in-memory KNN index on open; the cascade keeps them consistent with chunks.
CREATE TABLE chunk_embeddings (
    chunk_id  INTEGER PRIMARY KEY REFERENCES chunks(id) ON DELETE CASCADE,
    embedding BLOB NOT NULL
);

-- Sync state manifest: only indexable files ever appear here.
CREATE TABLE source_files (
    source_name    TEXT NOT NULL,
    rel_path       TEXT NOT NULL,
    content_sha256 TEXT NOT NULL,
    size_bytes     INTEGER NOT NULL,
    mtime_unix     INTEGER NOT NULL,
    document_id    INTEGER REFERENCES documents(id) ON DELETE SET NULL,
    PRIMARY KEY (source_name, rel_path)
);

-- KB identity (embedding model, dimensions, schema bookkeeping). A mismatch
-- on open is a hard error, never an auto-wipe.
CREATE TABLE kb_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
