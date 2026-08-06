package extract

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// Docx extracts text from Office Open XML word documents. A .docx is a zip
// with the document body in word/document.xml; paragraphs styled Heading1..6
// drive the heading path exactly like markdown headings do. Pure Go.
type Docx struct{}

func (Docx) Extensions() []string { return []string{".docx"} }

// docx XML shapes (only what we consume).
type docxDocument struct {
	Body struct {
		Paragraphs []docxParagraph `xml:"p"`
	} `xml:"body"`
}

type docxParagraph struct {
	Props struct {
		Style struct {
			Val string `xml:"val,attr"`
		} `xml:"pStyle"`
	} `xml:"pPr"`
	Runs []struct {
		Texts []string `xml:"t"`
	} `xml:"r"`
}

func (Docx) Extract(relPath string, r io.Reader) (Doc, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return Doc{}, err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return Doc{}, fmt.Errorf("docx %s: not a zip archive: %w", relPath, err)
	}
	var docXML []byte
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				return Doc{}, err
			}
			docXML, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return Doc{}, err
			}
			break
		}
	}
	if docXML == nil {
		return Doc{}, fmt.Errorf("docx %s: missing word/document.xml", relPath)
	}

	var parsed docxDocument
	if err := xml.Unmarshal(docXML, &parsed); err != nil {
		return Doc{}, fmt.Errorf("docx %s: parsing document.xml: %w", relPath, err)
	}

	var (
		doc     Doc
		stack   []string
		current []string
		start   = 1
		lineNo  = 0
	)
	flush := func(end int) {
		text := strings.TrimSpace(strings.Join(current, "\n"))
		if text != "" {
			doc.Blocks = append(doc.Blocks, Block{
				Text:        text,
				HeadingPath: joinHeadings(stack),
				StartLine:   start,
				EndLine:     end,
			})
		}
		current = nil
	}

	for _, p := range parsed.Body.Paragraphs {
		lineNo++
		var sb strings.Builder
		for _, run := range p.Runs {
			for _, t := range run.Texts {
				sb.WriteString(t)
			}
		}
		text := sb.String()

		if level := headingLevel(p.Props.Style.Val); level > 0 && strings.TrimSpace(text) != "" {
			flush(lineNo - 1)
			if level <= len(stack) {
				stack = stack[:level-1]
			}
			for len(stack) < level-1 {
				stack = append(stack, "")
			}
			stack = append(stack, strings.TrimSpace(text))
			if doc.Title == "" && level == 1 {
				doc.Title = strings.TrimSpace(text)
			}
			start = lineNo
			current = append(current, text)
			continue
		}
		current = append(current, text)
	}
	flush(lineNo)
	return doc, nil
}

// headingLevel maps Word paragraph styles to heading levels. Both the
// English defaults (Heading1) and the internal style ids used by most
// generators (heading 1, Ttulo1 variants come through the same ids).
func headingLevel(style string) int {
	s := strings.ToLower(strings.ReplaceAll(style, " ", ""))
	if !strings.HasPrefix(s, "heading") {
		return 0
	}
	switch strings.TrimPrefix(s, "heading") {
	case "1":
		return 1
	case "2":
		return 2
	case "3":
		return 3
	case "4":
		return 4
	case "5":
		return 5
	case "6":
		return 6
	}
	return 0
}
