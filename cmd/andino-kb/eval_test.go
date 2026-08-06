package main

import "testing"

type rp = struct {
	RelPath string `json:"rel_path"`
}

func TestDocRank(t *testing.T) {
	results := []rp{
		{"a.md"}, {"a.md"}, {"b.md"}, {"c.md"}, {"a.md"}, {"d.md"},
	}
	// Chunk list dedupes to docs [a b c d]; b is the first expected -> rank 2.
	if got := docRank(results, []string{"b.md"}, 5); got != 2 {
		t.Errorf("rank = %d, want 2", got)
	}
	if got := docRank(results, []string{"zz"}, 5); got != 0 {
		t.Errorf("miss rank = %d, want 0", got)
	}
	// topN cuts the doc list before matching.
	if got := docRank(results, []string{"d.md"}, 3); got != 0 {
		t.Errorf("beyond-topN rank = %d, want 0", got)
	}
}

func TestPercentile(t *testing.T) {
	vals := []int64{5, 1, 9, 3, 7}
	if p := percentile(vals, 50); p != 5 {
		t.Errorf("p50 = %d", p)
	}
	if p := percentile(vals, 95); p != 9 {
		t.Errorf("p95 = %d", p)
	}
	if p := percentile(nil, 50); p != 0 {
		t.Errorf("empty = %d", p)
	}
}
