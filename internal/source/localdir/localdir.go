// Package localdir is the filesystem source: a directory walked (and later
// watched) for indexable files.
package localdir

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/andino-agents/knowledge-base/internal/source"
)

type Dir struct {
	name    string
	root    string
	include []string
	exclude []string
	exts    map[string]bool // lowercase extension allowlist, with dot
}

// New builds a localdir source. exts is the extractor registry's allowlist;
// include defaults to everything.
func New(name, root string, include, exclude, exts []string) (*Dir, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("localdir %s: %w", name, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("localdir %s: %s is not a directory", name, abs)
	}
	extSet := make(map[string]bool, len(exts))
	for _, e := range exts {
		extSet[strings.ToLower(e)] = true
	}
	return &Dir{name: name, root: abs, include: include, exclude: exclude, exts: extSet}, nil
}

func (d *Dir) Name() string                   { return d.name }
func (d *Dir) Sync(ctx context.Context) error { return nil }
func (d *Dir) URI(relPath string) string {
	return "file://" + filepath.Join(d.root, filepath.FromSlash(relPath))
}
func (d *Dir) Root() string { return d.root }

// Indexable reports whether a rel path (slash-separated) passes the
// extension allowlist and include/exclude globs. The watcher reuses this so
// filtering happens before debouncing.
func (d *Dir) Indexable(relPath string) bool {
	if !d.exts[strings.ToLower(filepath.Ext(relPath))] {
		return false
	}
	for _, g := range d.exclude {
		if ok, _ := doublestar.Match(g, relPath); ok {
			return false
		}
	}
	if len(d.include) == 0 {
		return true
	}
	for _, g := range d.include {
		if ok, _ := doublestar.Match(g, relPath); ok {
			return true
		}
	}
	return false
}

func (d *Dir) List(ctx context.Context) ([]source.FileMeta, error) {
	var metas []source.FileMeta
	err := filepath.WalkDir(d.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(d.root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			// Skip whole excluded trees (dotdirs like .obsidian, .git).
			if rel != "." && (strings.HasPrefix(entry.Name(), ".") || d.excludedDir(rel)) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Indexable(rel) {
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

func (d *Dir) excludedDir(rel string) bool {
	for _, g := range d.exclude {
		// A glob like ".obsidian/**" excludes the dir itself too.
		if ok, _ := doublestar.Match(g, rel); ok {
			return true
		}
		if ok, _ := doublestar.Match(g, rel+"/"); ok {
			return true
		}
		if ok, _ := doublestar.Match(strings.TrimSuffix(g, "/**"), rel); ok {
			return true
		}
	}
	return false
}

func (d *Dir) Read(ctx context.Context, relPath string) (io.ReadCloser, error) {
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return nil, fmt.Errorf("localdir %s: path %q escapes the source root", d.name, relPath)
	}
	return os.Open(filepath.Join(d.root, clean))
}
