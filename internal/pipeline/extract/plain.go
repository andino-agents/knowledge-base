package extract

import (
	"bufio"
	"io"
	"strings"
)

// Plaintext handles unstructured text formats as a single block.
type Plaintext struct{}

func (Plaintext) Extensions() []string { return []string{".txt", ".rst", ".adoc"} }

func (Plaintext) Extract(relPath string, r io.Reader) (Doc, error) {
	return wholeFile(r)
}

// Code handles source files as a single block; the chunker splits them by
// lines. A syntax-aware extractor can replace this later.
type Code struct{}

func (Code) Extensions() []string {
	return []string{
		".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".java", ".rb", ".rs",
		".c", ".h", ".cpp", ".hpp", ".cs", ".sh", ".bash", ".sql", ".tf",
		".yaml", ".yml", ".toml", ".json", ".proto",
	}
}

func (Code) Extract(relPath string, r io.Reader) (Doc, error) {
	return wholeFile(r)
}

func wholeFile(r io.Reader) (Doc, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return Doc{}, err
	}
	text := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if strings.TrimSpace(text) == "" {
		return Doc{}, nil
	}
	return Doc{Blocks: []Block{{Text: text, StartLine: 1, EndLine: len(lines)}}}, nil
}
