package sqlite

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/andino-agents/knowledge-base/internal/store"
)

// candidatesPerLeg is how many results each retrieval leg contributes to
// fusion. RRF over 50+50 candidates is the standard production setup.
const candidatesPerLeg = 50

// rrfK is the standard Reciprocal Rank Fusion constant.
const rrfK = 60.0

func (s *sqliteStore) HybridSearch(ctx context.Context, query string, queryVec []float32, k int) ([]store.Hit, error) {
	if len(queryVec) != s.dim {
		return nil, fmt.Errorf("sqlite: query vector has dimension %d, store expects %d", len(queryVec), s.dim)
	}
	if k <= 0 {
		k = 8
	}

	ftsRanks, err := s.ftsSearch(ctx, query, candidatesPerLeg)
	if err != nil {
		return nil, err
	}
	vecRanks := s.vidx.search(queryVec, candidatesPerLeg)

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

func (s *sqliteStore) ftsSearch(ctx context.Context, query string, limit int) ([]int64, error) {
	match := sanitizeFTSQuery(query)
	if match == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT rowid FROM chunks_fts WHERE chunks_fts MATCH ? ORDER BY bm25(chunks_fts) LIMIT ?",
		match, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: FTS query: %w", err)
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

func (s *sqliteStore) hydrate(ctx context.Context, chunkIDs []int64) ([]hydratedHit, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunkIDs)), ",")
	args := make([]any, len(chunkIDs))
	for i, id := range chunkIDs {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT c.id, c.heading_path, c.start_line, c.end_line, c.text, c.context,
		       d.id, d.source_name, d.rel_path, d.uri, d.title, d.metadata
		FROM chunks c JOIN documents d ON d.id = c.document_id
		WHERE c.id IN (%s)`, placeholders), args...)
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

// sanitizeFTSQuery turns arbitrary agent-written text into a safe FTS5 MATCH
// expression. Raw queries like `what's "cache-reuse"?` are FTS5 syntax errors
// (silent zero results at best); quoting each token as a phrase and OR-ing
// them is robust and lets bm25 do the ranking.
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
