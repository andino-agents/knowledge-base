package extract

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// makePDF builds a minimal but valid PDF with one text page per entry,
// computing the xref offsets so parsers accept it. Empty entries produce
// pages without a text layer (scan stand-ins).
func makePDF(pages []string) []byte {
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

func TestPDFExtractsTextPerPage(t *testing.T) {
	raw := makePDF([]string{
		"Kubernetes eviction thresholds and node pressure handling notes",
		"Terraform remote state locking with DynamoDB for teams",
	})
	res, err := ExtractPDF("doc.pdf", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Doc.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (%+v)", len(res.Doc.Blocks), res)
	}
	if res.Doc.Blocks[0].HeadingPath != "Page 1" || res.Doc.Blocks[1].HeadingPath != "Page 2" {
		t.Errorf("heading paths: %q %q", res.Doc.Blocks[0].HeadingPath, res.Doc.Blocks[1].HeadingPath)
	}
	if !strings.Contains(res.Doc.Blocks[1].Text, "DynamoDB") {
		t.Errorf("page 2 text = %q", res.Doc.Blocks[1].Text)
	}
	if len(res.ScannedPages) != 0 {
		t.Errorf("unexpected scan candidates: %v", res.ScannedPages)
	}
}

func TestPDFDetectsScannedPages(t *testing.T) {
	raw := makePDF([]string{
		"A perfectly extractable page with plenty of native text",
		"", // no text layer: scan stand-in
	})
	res, err := ExtractPDF("scan.pdf", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Doc.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(res.Doc.Blocks))
	}
	if len(res.ScannedPages) != 1 || res.ScannedPages[0] != 2 {
		t.Fatalf("scanned pages = %v, want [2]", res.ScannedPages)
	}
}

// makeDocx builds a minimal .docx: a zip with word/document.xml.
func makeDocx(t *testing.T, paragraphs []struct{ Style, Text string }) []byte {
	t.Helper()
	var body strings.Builder
	for _, p := range paragraphs {
		body.WriteString("<w:p>")
		if p.Style != "" {
			fmt.Fprintf(&body, `<w:pPr><w:pStyle w:val="%s"/></w:pPr>`, p.Style)
		}
		fmt.Fprintf(&body, "<w:r><w:t>%s</w:t></w:r></w:p>", p.Text)
	}
	docXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>` + body.String() + `</w:body></w:document>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml":   docXML,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	zw.Close()
	return buf.Bytes()
}

func TestDocxHeadingsAndText(t *testing.T) {
	raw := makeDocx(t, []struct{ Style, Text string }{
		{"Heading1", "Runbook de despliegue"},
		{"", "Pasos generales del proceso."},
		{"Heading2", "Rollback"},
		{"", "Como revertir un despliegue fallido."},
	})
	doc, err := Docx{}.Extract("runbook.docx", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "Runbook de despliegue" {
		t.Errorf("title = %q", doc.Title)
	}
	if len(doc.Blocks) != 2 {
		t.Fatalf("blocks = %+v, want 2", doc.Blocks)
	}
	if doc.Blocks[1].HeadingPath != "Runbook de despliegue > Rollback" {
		t.Errorf("heading path = %q", doc.Blocks[1].HeadingPath)
	}
	if !strings.Contains(doc.Blocks[1].Text, "revertir") {
		t.Errorf("block text = %q", doc.Blocks[1].Text)
	}
}

func TestDocxRejectsGarbage(t *testing.T) {
	if _, err := (Docx{}).Extract("x.docx", bytes.NewReader([]byte("not a zip"))); err == nil {
		t.Fatal("garbage accepted as docx")
	}
}
