package gitrepo

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// upstream builds a real repository on disk to clone from.
func upstream(t *testing.T) (dir string, commit func(files map[string]string)) {
	t.Helper()
	dir = t.TempDir()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{Bare: false})
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	commit = func(files map[string]string) {
		for rel, content := range files {
			p := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := wt.Add(rel); err != nil {
				t.Fatal(err)
			}
		}
		_, err := wt.Commit("update", &git.CommitOptions{
			Author: &object.Signature{Name: "test", Email: "t@example.com", When: time.Now()},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return dir, commit
}

func TestCloneListReadAndResync(t *testing.T) {
	ctx := context.Background()
	up, commit := upstream(t)
	commit(map[string]string{
		"docs/guide.md":  "# Guide\n\nHello.",
		"docs/skip.png":  "PNG",
		"README.md":      "# Readme",
		"internal/x.txt": "not matched by paths",
	})

	branch := currentBranch(t, up)
	r := New("docs", up, branch, []string{"docs/**", "README.md"},
		filepath.Join(t.TempDir(), "cache"), "", []string{".md"})

	if err := r.Sync(ctx); err != nil {
		t.Fatalf("initial sync (clone): %v", err)
	}
	metas, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range metas {
		got[m.RelPath] = true
	}
	if !got["docs/guide.md"] || !got["README.md"] || len(got) != 2 {
		t.Fatalf("listing = %v, want docs/guide.md and README.md only", got)
	}

	rc, err := r.Read(ctx, "docs/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(rc)
	rc.Close()
	if string(content) != "# Guide\n\nHello." {
		t.Fatalf("content = %q", content)
	}

	// Upstream changes; a re-sync picks them up.
	commit(map[string]string{"docs/guide.md": "# Guide\n\nUpdated content."})
	if err := r.Sync(ctx); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	rc, err = r.Read(ctx, "docs/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	content, _ = io.ReadAll(rc)
	rc.Close()
	if string(content) != "# Guide\n\nUpdated content." {
		t.Fatalf("after re-sync content = %q", content)
	}

	if r.URI("docs/guide.md") != up+"#"+branch+":docs/guide.md" {
		t.Errorf("URI = %q", r.URI("docs/guide.md"))
	}
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return ref.Name().Short()
}
