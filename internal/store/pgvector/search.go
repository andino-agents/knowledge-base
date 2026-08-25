package pgvector

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pgvector/pgvector-go"

	"github.com/andino-agents/knowledge-base/internal/store"
)

// candidatesPerLeg is how many results each retrieval leg contributes to
// fusion. RRF over 50+50 candidates is the standard production setup.
const candidatesPerLeg = 50

// rrfK is the standard Reciprocal Rank Fusion constant.
const rrfK = 60.0

func (s *pgvectorStore) HybridSearch(ctx context.Context, query string, queryVec []float32, k int) ([]store.Hit, error) {
	if len(queryVec) != s.dim {
		return nil, fmt.Errorf("postgres: query vector has dimension %d, store expects %d", len(queryVec), s.dim)
	}
	if k <= 0 {
		k = 8
	}

	ftsRanks, err := s.ftsSearch(ctx, query, candidatesPerLeg)
	if err != nil {
		return nil, err
	}
	vecRanks, err := s.vecSearch(ctx, queryVec, candidatesPerLeg)
	if err != nil {
		return nil, err
	}

	// Reciprocal Rank Fusion: score = sum over legs of 1/(rrfK + rank).
	type fused struct {
		score            float64
		ftsRank, vecRank int
	}
	byChunk := map[int64]*fused{}
	for rank, id := range ftsRanks {
		f := byChunk[id]
		if f == nil {
			f = &fused{}
			byChunk[id] = f
		}
		f.ftsRank = rank + 1
		f.score += 1.0 / (rrfK + float64(rank+1))
	}
	for rank, id := range vecRanks {
		f := byChunk[id]
		if f == nil {
			f = &fused{}
			byChunk[id] = f
		}
		f.vecRank = rank + 1
		f.score += 1.0 / (rrfK + float64(rank+1))
	}

	ids := make([]int64, 0, len(byChunk))
	for id := range byChunk {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		fi, fj := byChunk[ids[i]], byChunk[ids[j]]
		if fi.score != fj.score {
			return fi.score > fj.score
		}
		return ids[i] < ids[j] // deterministic tie-break
	})
	if len(ids) > k {
		ids = ids[:k]
	}
	if len(ids) == 0 {
		return nil, nil
	}

	hits, err := s.hydrate(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range hits {
		f := byChunk[hits[i].chunkID]
		hits[i].Hit.Score = f.score
		hits[i].Hit.FTSRank = f.ftsRank
		hits[i].Hit.VecRank = f.vecRank
	}
	out := make([]store.Hit, len(hits))
	for i, h := range hits {
		out[i] = h.Hit
	}
	return out, nil
}

// ftsSearch runs the keyword leg: a tsvector full-text match ranked with
// bm25. The query is sanitized first so arbitrary agent input cannot become a
// tsquery syntax error (which would fail the whole query, not just return no
// hits — failure mode is the spec here).
func (s *pgvectorStore) ftsSearch(ctx context.Context, query string, limit int) ([]int64, error) {
	match := sanitizeFTSQuery(query)
	if match == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH q(t) AS (SELECT websearch_to_tsquery('english', $1))
		SELECT c.id
		FROM chunks c, q
		WHERE to_tsvector('english', c.text || ' ' || c.heading_path || ' ' || COALESCE(c.context, '')) @@ q.t
		ORDER BY ts_rank(
			to_tsvector('english', c.text || ' ' || c.heading_path || ' ' || COALESCE(c.context, '')),
			q.t
		) DESC
		LIMIT $2`, match, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: FTS query: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// vecSearch runs the dense leg over the pgvector HNSW index. <=> is cosine
// distance; ordering by it ascending is nearest-first.
func (s *pgvectorStore) vecSearch(ctx context.Context, queryVec []float32, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT chunk_id FROM chunk_embeddings ORDER BY embedding <=> $1 LIMIT $2",
		pgvector.NewVector(queryVec), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: vector query: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type hydratedHit struct {
	chunkID int64
	Hit     store.Hit
}

func (s *pgvectorStore) hydrate(ctx context.Context, chunkIDs []int64) ([]hydratedHit, error) {
	// pgx's stdlib driver does not translate "?" placeholders the way the
	// database/sql drivers do, so the raw "?" reaches Postgres as its JSONB
	// operator and fails. Use $n placeholders like the rest of the provider.
	placeholders := make([]string, len(chunkIDs))
	args := make([]any, len(chunkIDs))
	for i, id := range chunkIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT c.id, c.heading_path, c.start_line, c.end_line, c.text, c.context,
		       d.id, d.source_name, d.rel_path, d.uri, d.title, d.metadata
		FROM chunks c JOIN documents d ON d.id = c.document_id
		WHERE c.id IN (%s)`, strings.Join(placeholders, ", ")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byID := map[int64]hydratedHit{}
	for rows.Next() {
		var h hydratedHit
		var metaJSON string
		if err := rows.Scan(&h.chunkID, &h.Hit.HeadingPath, &h.Hit.StartLine, &h.Hit.EndLine, &h.Hit.Text, &h.Hit.Context,
			&h.Hit.DocumentID, &h.Hit.SourceName, &h.Hit.RelPath, &h.Hit.URI, &h.Hit.Title, &metaJSON); err != nil {
			return nil, err
		}
		var err error
		if h.Hit.Metadata, err = unmarshalMeta(metaJSON); err != nil {
			return nil, err
		}
		byID[h.chunkID] = h
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Preserve fusion order.
	out := make([]hydratedHit, 0, len(chunkIDs))
	for _, id := range chunkIDs {
		if h, ok := byID[id]; ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// sanitizeFTSQuery turns arbitrary agent-written text into a safe websearch
// term. Raw queries like `what's "cache-reuse"?` are passed through as quoted
// phrases OR-ed together, which websearch_to_tsquery parses robustly; bm25
// then ranks them. This never yields a tsquery syntax error.
func sanitizeFTSQuery(query string) string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		// Split on anything that is not letter, digit, or intra-word
		// punctuation we want to keep inside phrases.
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		case r == '_', r == '-', r == '.', r >= 0x80: // keep unicode word chars
			return false
		}
		return true
	})
	var terms []string
	for _, f := range fields {
		f = strings.Trim(f, "-._")
		if f == "" {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(f, `"`, ``)+`"`)
	}
	return strings.Join(terms, " OR ")
}
