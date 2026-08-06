package chunk

import (
	"fmt"
	"strings"
	"testing"

	"github.com/andino-agents/knowledge-base/internal/pipeline/extract"
)

func TestSmallBlocksStayWhole(t *testing.T) {
	doc := extract.Doc{Blocks: []extract.Block{
		{Text: "short section", HeadingPath: "A", StartLine: 1, EndLine: 2},
		{Text: "another one", HeadingPath: "A > B", StartLine: 3, EndLine: 4},
	}}
	chunks := Split(doc, 512, 64, nil)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if chunks[0].Seq != 0 || chunks[1].Seq != 1 {
		t.Errorf("seqs = %d,%d", chunks[0].Seq, chunks[1].Seq)
	}
	if chunks[1].HeadingPath != "A > B" {
		t.Errorf("heading path = %q", chunks[1].HeadingPath)
	}
}

func TestOversizedBlockSplitsWithOverlap(t *testing.T) {
	var lines []string
	for i := 1; i <= 40; i++ {
		lines = append(lines, fmt.Sprintf("line %02d with some padding text here", i))
	}
	doc := extract.Doc{Blocks: []extract.Block{
		{Text: strings.Join(lines, "\n"), HeadingPath: "Big", StartLine: 10, EndLine: 49},
	}}
	chunks := Split(doc, 64, 16, nil)
	if len(chunks) < 3 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.TokenEst > 64+16 { // overlap may push slightly over budget
			t.Errorf("chunk %d estimate %d exceeds budget", i, c.TokenEst)
		}
		if c.HeadingPath != "Big" {
			t.Errorf("chunk %d lost heading path", i)
		}
		if c.StartLine < 10 || c.EndLine > 49 || c.StartLine > c.EndLine {
			t.Errorf("chunk %d bad span %d-%d", i, c.StartLine, c.EndLine)
		}
	}
	// Consecutive chunks overlap: next start <= previous end.
	for i := 1; i < len(chunks); i++ {
		if chunks[i].StartLine > chunks[i-1].EndLine+1 {
			t.Errorf("gap between chunk %d (ends %d) and %d (starts %d)",
				i-1, chunks[i-1].EndLine, i, chunks[i].StartLine)
		}
	}
	// Every original line survives somewhere.
	joined := ""
	for _, c := range chunks {
		joined += c.Text + "\n"
	}
	for _, l := range lines {
		if !strings.Contains(joined, l) {
			t.Errorf("line lost during split: %q", l)
		}
	}
}

func TestNoTrailingOverlapOnlyChunk(t *testing.T) {
	// A split must never emit a chunk that repeats a previous chunk's content
	// (an overlap-only tail). Unique lines make duplicates detectable.
	var lines []string
	for i := 0; i < 8; i++ {
		lines = append(lines, fmt.Sprintf("unique-%d-%s", i, strings.Repeat("x", 56))) // ~16 tokens each
	}
	doc := extract.Doc{Blocks: []extract.Block{
		{Text: strings.Join(lines, "\n"), StartLine: 1, EndLine: 8},
	}}
	chunks := Split(doc, 32, 8, nil) // ~2 lines per chunk
	seen := map[string]bool{}
	for _, c := range chunks {
		if seen[c.Text] {
			t.Fatalf("duplicate chunk emitted: %q", c.Text[:32])
		}
		seen[c.Text] = true
	}
	// And the final chunk must end on the block's real last line.
	if last := chunks[len(chunks)-1]; last.EndLine != 8 {
		t.Errorf("last chunk ends at %d, want 8", last.EndLine)
	}
}

func TestEmptyDocYieldsNoChunks(t *testing.T) {
	if got := Split(extract.Doc{}, 512, 64, nil); len(got) != 0 {
		t.Fatalf("chunks from empty doc: %d", len(got))
	}
	doc := extract.Doc{Blocks: []extract.Block{{Text: "   \n  ", StartLine: 1, EndLine: 2}}}
	if got := Split(doc, 512, 64, nil); len(got) != 0 {
		t.Fatalf("chunks from whitespace doc: %d", len(got))
	}
}
