package extract

import (
	"bufio"
	"io"
	"strings"
)

// Markdown extracts structure from CommonMark-ish files: YAML frontmatter is
// skipped, ATX headings (#..######) build the heading path, and each section
// becomes a block. Fenced code blocks are kept verbatim inside their section
// (a # inside a fence is not a heading).
type Markdown struct{}

func (Markdown) Extensions() []string { return []string{".md", ".markdown"} }

func (Markdown) Extract(relPath string, r io.Reader) (Doc, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	type section struct {
		headingPath string
		start       int
		lines       []string
	}
	var (
		doc       Doc
		stack     []string // heading text per level, stack[0] = H1
		current   = &section{headingPath: "", start: 1}
		sections  []*section
		lineNo    int
		inFence   bool
		fenceMark string
		inFront   bool
		frontDone bool
	)

	flush := func() {
		if len(current.lines) > 0 {
			sections = append(sections, current)
		}
	}

	for sc.Scan() {
		lineNo++
		line := sc.Text()
		trimmed := strings.TrimSpace(line)

		// YAML frontmatter: only at the very top of the file.
		if lineNo == 1 && trimmed == "---" {
			inFront = true
			continue
		}
		if inFront {
			if trimmed == "---" || trimmed == "..." {
				inFront = false
				frontDone = true
				current.start = lineNo + 1
			}
			continue
		}

		// Fenced code blocks: headings inside are literal text.
		if !inFence {
			if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
				inFence = true
				fenceMark = trimmed[:3]
			}
		} else if strings.HasPrefix(trimmed, fenceMark) {
			inFence = false
		}

		if !inFence || strings.HasPrefix(trimmed, fenceMark) {
			if level, text, ok := atxHeading(line); ok && !inFence {
				flush()
				// Trim the stack to the parent level and push this heading.
				if level <= len(stack) {
					stack = stack[:level-1]
				}
				for len(stack) < level-1 {
					stack = append(stack, "")
				}
				stack = append(stack, text)
				if doc.Title == "" && level == 1 {
					doc.Title = text
				}
				current = &section{headingPath: joinHeadings(stack), start: lineNo}
				current.lines = append(current.lines, line)
				continue
			}
		}
		current.lines = append(current.lines, line)
	}
	if err := sc.Err(); err != nil {
		return Doc{}, err
	}
	flush()
	_ = frontDone

	for _, s := range sections {
		text := strings.TrimSpace(strings.Join(s.lines, "\n"))
		if text == "" {
			continue
		}
		doc.Blocks = append(doc.Blocks, Block{
			Text:        text,
			HeadingPath: s.headingPath,
			StartLine:   s.start,
			EndLine:     s.start + len(s.lines) - 1,
		})
	}
	return doc, nil
}

func atxHeading(line string) (level int, text string, ok bool) {
	trimmed := strings.TrimSpace(line)
	i := 0
	for i < len(trimmed) && trimmed[i] == '#' {
		i++
	}
	if i == 0 || i > 6 || i == len(trimmed) || trimmed[i] != ' ' {
		return 0, "", false
	}
	return i, strings.TrimSpace(strings.TrimRight(trimmed[i:], "#")), true
}

func joinHeadings(stack []string) string {
	parts := make([]string, 0, len(stack))
	for _, h := range stack {
		if h != "" {
			parts = append(parts, h)
		}
	}
	return strings.Join(parts, " > ")
}
