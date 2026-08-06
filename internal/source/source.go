// Package source defines where documents come from. A Source lists and reads
// files; the indexer decides what changed. Watch support is a separate
// optional interface so poll-only sources (git) stay simple.
package source

import (
	"context"
	"io"
)

// FileMeta is a cheap listing entry, used for incremental sync before any
// content is read.
type FileMeta struct {
	RelPath   string
	SizeBytes int64
	MtimeUnix int64
}

// Source is one ingestion pipeline's origin. List returns only indexable
// files: sources filter by include/exclude globs and the extractor
// extension allowlist, so nothing unindexable ever reaches a manifest.
type Source interface {
	Name() string
	// Sync brings the local view up to date (git: fetch+reset; localdir: no-op).
	Sync(ctx context.Context) error
	List(ctx context.Context) ([]FileMeta, error)
	Read(ctx context.Context, relPath string) (io.ReadCloser, error)
	// URI renders the canonical identifier for a file (file://..., git remote).
	URI(relPath string) string
}

// Watchable is implemented by sources that can push change notifications.
// The value sent on dirty is the rel path that changed; an empty string
// means "unknown, rescan everything".
type Watchable interface {
	Watch(ctx context.Context, dirty chan<- string) error
}
