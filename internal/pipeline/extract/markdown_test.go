package extract

import (
	"strings"
	"testing"
)

const sample = `---
tags: [x]
---

# Title here

Intro paragraph.

## Section A

Content A.

` + "```bash\n# not a heading\necho hi\n```" + `

### Deep

Nested content.

## Section B

Content B.
`

func TestMarkdownStructure(t *testing.T) {
	doc, err := Markdown{}.Extract("a.md", strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "Title here" {
		t.Errorf("title = %q", doc.Title)
	}
	var paths []string
	for _, b := range doc.Blocks {
		paths = append(paths, b.HeadingPath)
	}
	want := []string{
		"Title here",
		"Title here > Section A",
		"Title here > Section A > Deep",
		"Title here > Section B",
	}
	if len(paths) != len(want) {
		t.Fatalf("blocks = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("block %d path = %q, want %q", i, paths[i], want[i])
		}
	}

	// The fenced # line stays inside Section A's text, not as a heading.
	for _, b := range doc.Blocks {
		if b.HeadingPath == "Title here > Section A" && !strings.Contains(b.Text, "# not a heading") {
			t.Errorf("fence content lost from section A: %q", b.Text)
		}
	}
}

func TestMarkdownLineNumbers(t *testing.T) {
	doc, err := Markdown{}.Extract("a.md", strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(sample, "\n")
	for _, b := range doc.Blocks {
		if b.StartLine < 1 || b.EndLine > len(lines) || b.StartLine > b.EndLine {
			t.Errorf("block %q has bad line span %d-%d", b.HeadingPath, b.StartLine, b.EndLine)
		}
		// The first line of the span must be the heading that opens the block.
		first := strings.TrimSpace(lines[b.StartLine-1])
		if !strings.HasPrefix(first, "#") {
			t.Errorf("block %q starts at %d (%q), expected its heading", b.HeadingPath, b.StartLine, first)
		}
	}
}

func TestMarkdownHeadingLevelSkips(t *testing.T) {
	src := "### Deep first\n\nx\n\n# Then top\n\ny\n"
	doc, err := Markdown{}.Extract("a.md", strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Blocks) != 2 {
		t.Fatalf("blocks = %+v", doc.Blocks)
	}
	if doc.Blocks[0].HeadingPath != "Deep first" {
		t.Errorf("skip-level path = %q", doc.Blocks[0].HeadingPath)
	}
	if doc.Blocks[1].HeadingPath != "Then top" {
		t.Errorf("reset path = %q", doc.Blocks[1].HeadingPath)
	}
	if doc.Title != "Then top" {
		t.Errorf("title = %q", doc.Title)
	}
}
