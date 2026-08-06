// Package testutil holds cross-package test fixtures.
package testutil

import (
	"bytes"
	"fmt"
)

// BuildFixturePDF builds a minimal but valid PDF with one text page per entry,
// computing the xref offsets so parsers accept it. Empty entries produce
// pages without a text layer (scan stand-ins).
func BuildFixturePDF(pages []string) []byte {
	var objects []string
	kids := ""
	n := len(pages)
	for i := range pages {
		if kids != "" {
			kids += " "
		}
		kids += fmt.Sprintf("%d 0 R", 3+i*2)
	}
	objects = append(objects,
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, n),
	)
	fontRef := 3 + n*2
	for i, text := range pages {
		content := ""
		if text != "" {
			content = fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
		}
		objects = append(objects,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>",
				4+i*2, fontRef),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		)
	}
	objects = append(objects, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for i, obj := range objects {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return buf.Bytes()
}
