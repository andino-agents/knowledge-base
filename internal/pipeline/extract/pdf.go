package extract

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/ledongthuc/pdf"
)

// PDF extracts the native text layer page by page. Each page becomes one
// block with "Page N" as its heading path, so hits point users at the right
// page. Pages whose text layer is (nearly) empty are counted as scan
// candidates: with OCR configured the pipeline hands them to a vision model;
// without it they are reported and skipped.
type PDF struct{}

func (PDF) Extensions() []string { return []string{".pdf"} }

// scannedPageThreshold is the extracted-character count under which a page
// is considered to have no usable text layer.
const scannedPageThreshold = 20

// PDFResult carries per-page details the pipeline needs beyond Doc. Extract
// fills Doc with text pages only; ScannedPages lists 1-based page numbers
// needing OCR.
type PDFResult struct {
	Doc          Doc
	ScannedPages []int
}

func (PDF) Extract(relPath string, r io.Reader) (Doc, error) {
	res, err := ExtractPDF(relPath, r)
	return res.Doc, err
}

// ExtractWithScans implements ScanAware.
func (PDF) ExtractWithScans(relPath string, r io.Reader) (Doc, []int, error) {
	res, err := ExtractPDF(relPath, r)
	return res.Doc, res.ScannedPages, err
}

// ExtractPDF is the detailed variant used by the indexing pipeline.
func ExtractPDF(relPath string, r io.Reader) (PDFResult, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return PDFResult{}, err
	}
	reader, err := pdf.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return PDFResult{}, fmt.Errorf("pdf %s: %w", relPath, err)
	}

	var res PDFResult
	line := 1
	for n := 1; n <= reader.NumPage(); n++ {
		page := reader.Page(n)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			// A single malformed page should not sink the document.
			res.ScannedPages = append(res.ScannedPages, n)
			continue
		}
		text = strings.TrimSpace(text)
		if len(text) < scannedPageThreshold {
			res.ScannedPages = append(res.ScannedPages, n)
			continue
		}
		lines := strings.Count(text, "\n") + 1
		res.Doc.Blocks = append(res.Doc.Blocks, Block{
			Text:        text,
			HeadingPath: fmt.Sprintf("Page %d", n),
			StartLine:   line,
			EndLine:     line + lines - 1,
		})
		line += lines
	}
	return res, nil
}
