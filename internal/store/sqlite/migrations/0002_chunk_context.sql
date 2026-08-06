-- Contextual retrieval: a short LLM-generated context per chunk, embedded
-- and BM25-indexed alongside the text (Anthropic's contextual retrieval).
ALTER TABLE chunks ADD COLUMN context TEXT NOT NULL DEFAULT '';

-- Recreate the external-content FTS index with the context column and
-- repopulate it from the chunks table.
DROP TABLE chunks_fts;
CREATE VIRTUAL TABLE chunks_fts USING fts5(
    text, heading_path, context,
    content='chunks', content_rowid='id',
    tokenize='unicode61 remove_diacritics 2'
);
INSERT INTO chunks_fts(chunks_fts) VALUES('rebuild');
