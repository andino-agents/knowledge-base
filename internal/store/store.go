// Package store defines the storage provider interface for knowledge bases.
//
// The boundary is semantic ("index this document", "run this hybrid query"),
// not SQL: each provider resolves hybrid search with whatever mechanism its
// backend offers. The v0.1 provider is SQLite (FTS5 + sqlite-vec + RRF in Go);
// pgvector, OpenSearch and S3 Vectors can plug in later through Register
// without touching the engine.
package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Document is the indexed unit: one file, one managed memory, one page.
type Document struct {
	ID         int64
	SourceName string
	RelPath    string // path within the source, or generated id for managed docs
	URI        string // file://..., git URL#branch:path, managed://<kb>/<id>
	Title      string
	SHA256     string
	SizeBytes  int64
	MtimeUnix  int64
}

// Chunk is a retrievable slice of a document.
type Chunk struct {
	Seq         int
	HeadingPath string // "H1 > H2 > H3", empty for flat documents
	StartLine   int
	EndLine     int
	Text        string
	TokenEst    int
}

// FileState is what incremental sync compares against a source listing.
type FileState struct {
	SHA256    string
	SizeBytes int64
	MtimeUnix int64
}

// Hit is one hybrid search result.
type Hit struct {
	DocumentID  int64
	SourceName  string
	RelPath     string
	URI         string
	Title       string
	HeadingPath string
	StartLine   int
	EndLine     int
	Text        string
	Score       float64 // fused (RRF) or reranked score, higher is better
	FTSRank     int     // 1-based rank in the keyword result list, 0 if absent
	VecRank     int     // 1-based rank in the vector result list, 0 if absent
}

// Stats summarizes a knowledge base.
type Stats struct {
	Documents     int64
	Chunks        int64
	LastIndexedAt int64
}

// DocumentContent is a full document as stored, for get_document.
type DocumentContent struct {
	Document Document
	Text     string // reassembled from chunks in seq order
}

// Store is one knowledge base's storage. Implementations must make
// UpsertDocument and DeleteDocument atomic: a failure leaves the previous
// version of the document fully intact.
type Store interface {
	// UpsertDocument replaces the document at (doc.SourceName, doc.RelPath)
	// with the given chunks and their embeddings, atomically.
	// len(chunks) must equal len(embeddings).
	UpsertDocument(ctx context.Context, doc Document, chunks []Chunk, embeddings [][]float32) error

	// DeleteDocument removes a document and its chunks. Deleting a document
	// that does not exist is not an error.
	DeleteDocument(ctx context.Context, sourceName, relPath string) error

	// Manifest returns the sync state of every tracked file for a source.
	Manifest(ctx context.Context, sourceName string) (map[string]FileState, error)

	// TouchManifest updates a file's manifest entry without reindexing, for
	// files whose size/mtime changed but whose content hash did not.
	TouchManifest(ctx context.Context, sourceName, relPath string, fs FileState) error

	// HybridSearch runs keyword and vector retrieval and fuses the results.
	// queryVec dimension must match the store's configured dimension.
	HybridSearch(ctx context.Context, query string, queryVec []float32, k int) ([]Hit, error)

	// GetDocument fetches one document by (sourceName, relPath). Returns
	// ErrNotFound if it does not exist. sourceName may be empty to search
	// across all sources of the KB (first match wins; rel paths are unique
	// per source).
	GetDocument(ctx context.Context, sourceName, relPath string) (*DocumentContent, error)

	// ListDocuments pages through documents ordered by rel_path. A prefix
	// filters by rel_path prefix; cursor is the last rel_path of the
	// previous page ("" for the first page).
	ListDocuments(ctx context.Context, prefix, cursor string, limit int) ([]Document, error)

	Stats(ctx context.Context) (Stats, error)

	Close() error
}

// ErrNotFound is returned when a requested document does not exist.
var ErrNotFound = fmt.Errorf("document not found")

// Options carries provider-independent identity for a knowledge base store.
// Providers must record ModelName and Dimensions at creation time and refuse
// to open (hard error, never wipe) when an existing store disagrees.
type Options struct {
	KBName     string
	ModelName  string
	Dimensions int
	// ProviderConfig is the raw per-provider config block from the YAML.
	ProviderConfig map[string]any
}

// Factory opens (creating if needed) the store for one knowledge base.
type Factory func(ctx context.Context, opts Options) (Store, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register makes a provider available under a name. It panics on duplicate
// registration, mirroring database/sql.
func Register(name string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("store: Register called twice for provider %q", name))
	}
	registry[name] = f
}

// Open opens a knowledge base store with the named provider.
func Open(ctx context.Context, provider string, opts Options) (Store, error) {
	registryMu.RLock()
	f, ok := registry[provider]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("store: unknown provider %q (registered: %v)", provider, Providers())
	}
	return f(ctx, opts)
}

// Providers lists registered provider names, sorted.
func Providers() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
