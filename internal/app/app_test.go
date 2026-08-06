package app

import (
	"testing"

	"github.com/andino-agents/knowledge-base/internal/store"
)

func TestCapPerDocument(t *testing.T) {
	hits := []store.Hit{
		{DocumentID: 1, StartLine: 10}, {DocumentID: 1, StartLine: 20}, {DocumentID: 1, StartLine: 30},
		{DocumentID: 2, StartLine: 1}, {DocumentID: 1, StartLine: 40}, {DocumentID: 3, StartLine: 1},
	}
	capped := capPerDocument(hits, 2)
	var got []int64
	for _, h := range capped {
		got = append(got, h.DocumentID)
	}
	want := []int64{1, 1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("capped = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("capped = %v, want %v", got, want)
		}
	}
	// Order within the survivors is preserved.
	if capped[0].StartLine != 10 || capped[1].StartLine != 20 {
		t.Errorf("order not preserved: %+v", capped)
	}
}
