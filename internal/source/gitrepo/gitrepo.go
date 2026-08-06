// Package gitrepo is the git source: a repository shallow-cloned into a
// cache directory and kept fresh by polling. Pure Go (go-git), so air-gapped
// hosts need no git binary.
package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/andino-agents/knowledge-base/internal/source"
)

type Repo struct {
	name     string
	url      string
	branch   string
	paths    []string // doublestar globs within the repo; empty = everything
	cacheDir string
	token    string
	exts     map[string]bool
}

// New builds a git source. cacheDir is where the working copy lives
// (typically <data_dir>/git/<kb>-<source>). token may be empty for public
// repositories.
func New(name, url, branch string, paths []string, cacheDir, token string, exts []string) *Repo {
	extSet := make(map[string]bool, len(exts))
	for _, e := range exts {
		extSet[strings.ToLower(e)] = true
	}
	return &Repo{
		name: name, url: url, branch: branch, paths: paths,
		cacheDir: cacheDir, token: token, exts: extSet,
	}
}

func (r *Repo) Name() string { return r.name }

func (r *Repo) URI(relPath string) string {
	return r.url + "#" + r.branch + ":" + relPath
}

func (r *Repo) auth() *http.BasicAuth {
	if r.token == "" {
		return nil
	}
	// GitHub/GitLab accept any username with a token password.
	return &http.BasicAuth{Username: "token", Password: r.token}
}

// Sync clones on first use and fetch+resets afterwards. The worktree is
// disposable cache: any local drift is discarded.
func (r *Repo) Sync(ctx context.Context) error {
	repo, err := git.PlainOpen(r.cacheDir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		_, err := git.PlainCloneContext(ctx, r.cacheDir, false, &git.CloneOptions{
			URL:           r.url,
			ReferenceName: plumbing.NewBranchReferenceName(r.branch),
			SingleBranch:  true,
			Depth:         1,
			Auth:          r.auth(),
		})
		if err != nil {
			return fmt.Errorf("git %s: clone: %w", r.name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("git %s: open cache: %w", r.name, err)
	}

	err = repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		Depth:      1,
		Auth:       r.auth(),
		Force:      true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("git %s: fetch: %w", r.name, err)
	}
	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", r.branch), true)
	if err != nil {
		return fmt.Errorf("git %s: resolving origin/%s: %w", r.name, r.branch, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	if err := wt.Reset(&git.ResetOptions{Commit: remoteRef.Hash(), Mode: git.HardReset}); err != nil {
		return fmt.Errorf("git %s: reset: %w", r.name, err)
	}
	return nil
}

func (r *Repo) indexable(rel string) bool {
	if !r.exts[strings.ToLower(filepath.Ext(rel))] {
		return false
	}
	if len(r.paths) == 0 {
		return true
	}
	for _, g := range r.paths {
		if ok, _ := doublestar.Match(g, rel); ok {
			return true
		}
	}
	return false
}

func (r *Repo) List(ctx context.Context) ([]source.FileMeta, error) {
	var metas []source.FileMeta
	err := filepath.WalkDir(r.cacheDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(r.cacheDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel != "." && strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir // .git and friends
			}
			return nil
		}
		if !r.indexable(rel) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		metas = append(metas, source.FileMeta{
			RelPath:   rel,
			SizeBytes: info.Size(),
			MtimeUnix: info.ModTime().Unix(),
		})
		return nil
	})
	return metas, err
}

func (r *Repo) Read(ctx context.Context, relPath string) (io.ReadCloser, error) {
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return nil, fmt.Errorf("git %s: path %q escapes the repository", r.name, relPath)
	}
	return os.Open(filepath.Join(r.cacheDir, clean))
}
