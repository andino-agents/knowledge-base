package sqlite

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/andino-agents/knowledge-base/internal/store"
)

// BenchmarkHybridSearch measures the full hybrid query path (FTS5 + in-memory
// KNN + RRF + hydration) over 10k chunks of 1024-dim vectors, the documented
// comfortable scale. Query embedding is excluded (it happens in the caller).
func BenchmarkHybridSearch(b *testing.B) {
	ctx := context.Background()
	const dim = 1024
	s, err := store.Open(ctx, "sqlite", store.Options{
		KBName: "bench", ModelName: "bench", Dimensions: dim,
		ProviderConfig: map[string]any{"data_dir": b.TempDir()},
	})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	rng := rand.New(rand.NewSource(42))
	randomVec := func() []float32 {
		v := make([]float32, dim)
		for i := range v {
			v[i] = rng.Float32()*2 - 1
		}
		return v
	}
	words := []string{"kubernetes", "terraform", "vault", "latency", "postgres",
		"deployment", "incident", "runbook", "network", "storage", "backup", "kafka"}

	const docs, chunksPerDoc = 500, 20 // 10k chunks
	for d := 0; d < docs; d++ {
		doc := store.Document{
			SourceName: "bench", RelPath: fmt.Sprintf("doc%04d.md", d),
			URI: "file:///bench", SHA256: fmt.Sprintf("sha%d", d), SizeBytes: 1, MtimeUnix: 1,
		}
		chunks := make([]store.Chunk, chunksPerDoc)
		vecs := make([][]float32, chunksPerDoc)
		for c := 0; c < chunksPerDoc; c++ {
			text := fmt.Sprintf("section %d %s %s %s content with details", c,
				words[rng.Intn(len(words))], words[rng.Intn(len(words))], words[rng.Intn(len(words))])
			chunks[c] = store.Chunk{Seq: c, StartLine: c*10 + 1, EndLine: c*10 + 9, Text: text, TokenEst: 10}
			vecs[c] = randomVec()
		}
		if err := s.UpsertDocument(ctx, doc, chunks, vecs); err != nil {
			b.Fatal(err)
		}
	}

	query := randomVec()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hits, err := s.HybridSearch(ctx, "terraform incident runbook", query, 8)
		if err != nil {
			b.Fatal(err)
		}
		if len(hits) == 0 {
			b.Fatal("no hits")
		}
	}
}
