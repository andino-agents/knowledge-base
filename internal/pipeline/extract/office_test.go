package extract

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/andino-agents/knowledge-base/internal/testutil"
)

func TestPDFExtractsTextPerPage(t *testing.T) {
	raw := testutil.BuildFixturePDF([]string{
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
	raw := testutil.BuildFixturePDF([]string{
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
