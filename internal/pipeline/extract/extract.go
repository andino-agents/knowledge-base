// Package extract turns raw files into structured documents ready for
// chunking. Extractors are registered per file extension; adding pdf/docx
// support later means adding an Extractor, nothing else changes.
package extract

import (
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// Block is a structural unit of a document (a markdown section, a whole
// plaintext file). Line numbers are 1-based and inclusive.
type Block struct {
	Text        string
	HeadingPath string // "H1 > H2 > H3", empty when the format has no headings
	StartLine   int
	EndLine     int
}

// Doc is an extracted document.
type Doc struct {
	Title  string // best-effort; empty means "use the filename"
	Blocks []Block
}

// Extractor parses one family of file formats.
type Extractor interface {
	// Extensions lists the lowercase file extensions (with dot) this
	// extractor handles.
	Extensions() []string
	Extract(relPath string, r io.Reader) (Doc, error)
}

// ScanAware is implemented by extractors that can report pages without a
// usable text layer (scan candidates for OCR).
type ScanAware interface {
	ExtractWithScans(relPath string, r io.Reader) (Doc, []int, error)
}

// Registry maps extensions to extractors and doubles as the indexable-files
// allowlist: anything without a registered extension never enters a manifest.
type Registry struct {
	byExt map[string]Extractor
}

func NewRegistry(extractors ...Extractor) (*Registry, error) {
	r := &Registry{byExt: map[string]Extractor{}}
	for _, e := range extractors {
		for _, ext := range e.Extensions() {
			ext = strings.ToLower(ext)
			if prev, dup := r.byExt[ext]; dup {
				return nil, fmt.Errorf("extract: extension %q claimed by both %T and %T", ext, prev, e)
			}
			r.byExt[ext] = e
		}
	}
	return r, nil
}

// Default returns the standard registry: markdown, plaintext, code, PDF
// and DOCX.
func Default() *Registry {
	r, err := NewRegistry(Markdown{}, Plaintext{}, Code{}, PDF{}, Docx{})
	if err != nil {
		panic(err) // static extractor set; a clash is a programming error
	}
	return r
}

// For returns the extractor for a path, or nil if the file is not indexable.
func (r *Registry) For(relPath string) Extractor {
	return r.byExt[strings.ToLower(path.Ext(relPath))]
}

// Extensions returns the sorted allowlist of indexable extensions.
func (r *Registry) Extensions() []string {
	exts := make([]string, 0, len(r.byExt))
	for e := range r.byExt {
		exts = append(exts, e)
	}
	sort.Strings(exts)
	return exts
}
