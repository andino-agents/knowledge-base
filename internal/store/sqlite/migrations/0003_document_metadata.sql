-- Per-document metadata: a flat string map (JSON) set by agent writes and
-- future sources, filterable at search time.
ALTER TABLE documents ADD COLUMN metadata TEXT NOT NULL DEFAULT '{}';
