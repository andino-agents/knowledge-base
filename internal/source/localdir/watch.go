package localdir

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// Watch streams the rel paths of indexable files that change under the
// source root until ctx is done.
//
// Design rule, learned the hard way: never branch on the event type. Editors
// and agents write via temp file + rename, which arrives as Create/Rename
// rather than Write. Every event just marks its path dirty; the consumer
// re-stats reality (SyncPath) and decides between reindex and delete.
// Debouncing also lives in the consumer, so this stays a thin event pump.
func (d *Dir) Watch(ctx context.Context, dirty chan<- string) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	addRecursive := func(root string) error {
		return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				// A directory can vanish mid-walk (temp dirs); skip it.
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if !entry.IsDir() {
				return nil
			}
			rel, rerr := filepath.Rel(d.root, path)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			if rel != "." && (strings.HasPrefix(entry.Name(), ".") || d.excludedDir(rel)) {
				return fs.SkipDir
			}
			return w.Add(path)
		})
	}
	if err := addRecursive(d.root); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			return err
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			rel, err := filepath.Rel(d.root, ev.Name)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)

			// New directories need their own watches (fsnotify is not
			// recursive); files inside them arrive via the fresh watch.
			if ev.Op.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					if base := filepath.Base(ev.Name); !strings.HasPrefix(base, ".") && !d.excludedDir(rel) {
						_ = addRecursive(ev.Name)
					}
					continue
				}
			}

			if !d.Indexable(rel) {
				continue
			}
			select {
			case dirty <- rel:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}
