// Package pipeline orchestrates indexing: source listing, incremental
// manifest diff, extraction, chunking, embedding and transactional writes.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"

	"github.com/andino-agents/knowledge-base/internal/config"
	"github.com/andino-agents/knowledge-base/internal/inference"
	"github.com/andino-agents/knowledge-base/internal/pipeline/chunk"
	"github.com/andino-agents/knowledge-base/internal/pipeline/extract"
	"github.com/andino-agents/knowledge-base/internal/source"
	"github.com/andino-agents/knowledge-base/internal/store"
)

// Indexer syncs sources into one knowledge base's store.
type Indexer struct {
	Store    store.Store
	Embedder *inference.Embedder
	Registry *extract.Registry
	Chunking config.Chunking
	Logger   *slog.Logger
}

type SyncStats struct {
	Indexed int
	Deleted int
	Skipped int
	Failed  int
}

// fatalError marks failures that make continuing the sync pointless
// (embedding backend down); per-file problems are logged and counted instead.
type fatalError struct{ error }

func (ix *Indexer) logger() *slog.Logger {
	if ix.Logger != nil {
		return ix.Logger
	}
	return slog.Default()
}

// SyncSource performs one incremental sync of a source. It aborts on
// infrastructure failures and carries on over per-file ones.
func (ix *Indexer) SyncSource(ctx context.Context, src source.Source) (SyncStats, error) {
	var stats SyncStats
	if err := src.Sync(ctx); err != nil {
		return stats, fmt.Errorf("syncing source %s: %w", src.Name(), err)
	}
	manifest, err := ix.Store.Manifest(ctx, src.Name())
	if err != nil {
		return stats, err
	}
	listing, err := src.List(ctx)
	if err != nil {
		return stats, fmt.Errorf("listing source %s: %w", src.Name(), err)
	}

	seen := make(map[string]bool, len(listing))
	for _, fm := range listing {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		seen[fm.RelPath] = true
		prev, known := manifest[fm.RelPath]
		if known && prev.SizeBytes == fm.SizeBytes && prev.MtimeUnix == fm.MtimeUnix {
			stats.Skipped++
			continue
		}

		content, err := readFile(ctx, src, fm.RelPath)
		if err != nil {
			ix.logger().Error("reading file failed", "source", src.Name(), "path", fm.RelPath, "error", err)
			stats.Failed++
			continue
		}
		sum := sha256.Sum256(content)
		sha := hex.EncodeToString(sum[:])
		state := store.FileState{SHA256: sha, SizeBytes: fm.SizeBytes, MtimeUnix: fm.MtimeUnix}

		if known && prev.SHA256 == sha {
			// Touched but unchanged: refresh the manifest, skip re-embedding.
			if err := ix.Store.TouchManifest(ctx, src.Name(), fm.RelPath, state); err != nil {
				return stats, err
			}
			stats.Skipped++
			continue
		}

		indexed, err := ix.indexFile(ctx, src, fm.RelPath, state, content)
		if err != nil {
			var fatal fatalError
			if errors.As(err, &fatal) {
				return stats, fmt.Errorf("aborting sync of %s: %w", src.Name(), err)
			}
			ix.logger().Error("indexing file failed", "source", src.Name(), "path", fm.RelPath, "error", err)
			stats.Failed++
			continue
		}
		if !indexed {
			stats.Skipped++ // tracked, but no indexable content
			continue
		}
		ix.logger().Info("indexed", "source", src.Name(), "path", fm.RelPath)
		stats.Indexed++
	}

	for relPath := range manifest {
		if seen[relPath] {
			continue
		}
		if err := ix.Store.DeleteDocument(ctx, src.Name(), relPath); err != nil {
			return stats, err
		}
		ix.logger().Info("deleted from index", "source", src.Name(), "path", relPath)
		stats.Deleted++
	}
	return stats, nil
}

// SyncPath re-syncs a single rel path after a watcher event: reindex if the
// file is listed, delete if it is gone. Reality (a fresh stat) decides, not
// the event type.
func (ix *Indexer) SyncPath(ctx context.Context, src source.Source, relPath string) error {
	listing, err := src.List(ctx)
	if err != nil {
		return err
	}
	for _, fm := range listing {
		if fm.RelPath != relPath {
			continue
		}
		content, err := readFile(ctx, src, relPath)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		state := store.FileState{SHA256: hex.EncodeToString(sum[:]), SizeBytes: fm.SizeBytes, MtimeUnix: fm.MtimeUnix}
		manifest, err := ix.Store.Manifest(ctx, src.Name())
		if err != nil {
			return err
		}
		if prev, known := manifest[relPath]; known && prev.SHA256 == state.SHA256 {
			return ix.Store.TouchManifest(ctx, src.Name(), relPath, state)
		}
		_, err = ix.indexFile(ctx, src, relPath, state, content)
		return err
	}
	return ix.Store.DeleteDocument(ctx, src.Name(), relPath)
}

// indexFile extracts, chunks, embeds and stores one file. indexed=false with
// a nil error means the file has no indexable content and was only tracked.
func (ix *Indexer) indexFile(ctx context.Context, src source.Source, relPath string, state store.FileState, content []byte) (indexed bool, err error) {
	extractor := ix.Registry.For(relPath)
	if extractor == nil {
		return false, fmt.Errorf("no extractor for %s (source listed a non-indexable file?)", relPath)
	}
	doc, err := extractor.Extract(relPath, strings.NewReader(string(content)))
	if err != nil {
		return false, fmt.Errorf("extracting %s: %w", relPath, err)
	}
	chunks := chunk.Split(doc, ix.Chunking.MaxTokens, ix.Chunking.OverlapTokens, nil)
	if len(chunks) == 0 {
		// Empty documents are tracked (manifest) but not indexed.
		return false, ix.Store.TouchManifest(ctx, src.Name(), relPath, state)
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = embeddingText(c)
	}
	embeddings, err := ix.Embedder.Embed(ctx, texts)
	if err != nil {
		return false, fatalError{fmt.Errorf("embedding %s: %w", relPath, err)}
	}

	title := doc.Title
	if title == "" {
		title = strings.TrimSuffix(path.Base(relPath), path.Ext(relPath))
	}
	return true, ix.Store.UpsertDocument(ctx, store.Document{
		SourceName: src.Name(),
		RelPath:    relPath,
		URI:        src.URI(relPath),
		Title:      title,
		SHA256:     state.SHA256,
		SizeBytes:  state.SizeBytes,
		MtimeUnix:  state.MtimeUnix,
	}, chunks, embeddings)
}

// embeddingText is what actually gets embedded for a chunk: the heading
// context prefixed to the text, so section semantics survive chunking.
func embeddingText(c store.Chunk) string {
	if c.HeadingPath == "" {
		return c.Text
	}
	return c.HeadingPath + "\n\n" + c.Text
}

func readFile(ctx context.Context, src source.Source, relPath string) ([]byte, error) {
	rc, err := src.Read(ctx, relPath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
