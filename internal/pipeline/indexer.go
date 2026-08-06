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
	"time"

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
	// Contextual, when set, enables contextual retrieval: an LLM writes a
	// short situating context per chunk at index time.
	Contextual *inference.Chat
	Logger     *slog.Logger
}

// contextualDocBudget bounds how much of the source document is shown to the
// context model, in estimated tokens. Documents longer than this are
// truncated for the prompt (the chunks themselves are never truncated).
const contextualDocBudget = 8000

const contextualSystemPrompt = "You situate document chunks for search retrieval. " +
	"Answer only with the requested context, nothing else."

// contextualUserPrompt builds the per-chunk prompt. The document comes FIRST
// and identically for every chunk of the same document, so an inference
// server with prefix caching pays the document's prompt cost once.
func contextualUserPrompt(docText, chunkText string) string {
	return "<document>\n" + docText + "\n</document>\n\n" +
		"Here is the chunk we want to situate within the whole document:\n" +
		"<chunk>\n" + chunkText + "\n</chunk>\n\n" +
		"Give a short succinct context (1-2 sentences) situating this chunk within the overall document " +
		"to improve search retrieval of the chunk. Write in the same language as the document. " +
		"Answer only with the succinct context."
}

// contextualize fills chunk.Context for every chunk. Single-chunk documents
// are skipped: the chunk IS the document, a generated context adds noise.
func (ix *Indexer) contextualize(ctx context.Context, title string, content []byte, chunks []store.Chunk) error {
	if ix.Contextual == nil || len(chunks) < 2 {
		return nil
	}
	docText := title + "\n\n" + string(content)
	if est := chunk.Estimate(docText); est > contextualDocBudget {
		docText = docText[:contextualDocBudget*chunk.CharsPerToken]
	}
	for i := range chunks {
		text, err := ix.Contextual.Complete(ctx, contextualSystemPrompt,
			contextualUserPrompt(docText, chunks[i].Text))
		if err != nil {
			return err
		}
		chunks[i].Context = text
	}
	return nil
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

	title := doc.Title
	if title == "" {
		title = strings.TrimSuffix(path.Base(relPath), path.Ext(relPath))
	}

	if err := ix.contextualize(ctx, title, content, chunks); err != nil {
		return false, fatalError{fmt.Errorf("contextualizing %s: %w", relPath, err)}
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = embeddingText(c)
	}
	embeddings, err := ix.Embedder.Embed(ctx, texts)
	if err != nil {
		return false, fatalError{fmt.Errorf("embedding %s: %w", relPath, err)}
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

// IndexManaged indexes agent-provided content into the reserved "managed"
// source of a writable knowledge base. Content is treated as markdown.
func (ix *Indexer) IndexManaged(ctx context.Context, kbName, id, title, content string) error {
	doc, err := extract.Markdown{}.Extract(id+".md", strings.NewReader(content))
	if err != nil {
		return err
	}
	chunks := chunk.Split(doc, ix.Chunking.MaxTokens, ix.Chunking.OverlapTokens, nil)
	if len(chunks) == 0 {
		return fmt.Errorf("content produced no indexable chunks")
	}
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = embeddingText(c)
	}
	embeddings, err := ix.Embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("embedding managed document: %w", err)
	}
	if title == "" {
		if doc.Title != "" {
			title = doc.Title
		} else {
			title = id
		}
	}
	sum := sha256.Sum256([]byte(content))
	now := time.Now().Unix()
	return ix.Store.UpsertDocument(ctx, store.Document{
		SourceName: config.ManagedSourceName,
		RelPath:    id,
		URI:        "managed://" + kbName + "/" + id,
		Title:      title,
		SHA256:     hex.EncodeToString(sum[:]),
		SizeBytes:  int64(len(content)),
		MtimeUnix:  now,
	}, chunks, embeddings)
}

// embeddingText is what actually gets embedded for a chunk: the generated
// context and heading path prefixed to the text, so document semantics
// survive chunking (contextual embeddings).
func embeddingText(c store.Chunk) string {
	var b strings.Builder
	if c.Context != "" {
		b.WriteString(c.Context)
		b.WriteString("\n\n")
	}
	if c.HeadingPath != "" {
		b.WriteString(c.HeadingPath)
		b.WriteString("\n\n")
	}
	b.WriteString(c.Text)
	return b.String()
}

func readFile(ctx context.Context, src source.Source, relPath string) ([]byte, error) {
	rc, err := src.Read(ctx, relPath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
