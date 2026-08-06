package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"

	"github.com/andino-agents/knowledge-base/internal/pipeline/extract"
)

const ocrSystemPrompt = "You are a faithful OCR engine. Transcribe the text in the image exactly, " +
	"preserving reading order and line breaks. Do not translate, summarize, describe layout, or add anything. " +
	"If the image contains no text, answer with an empty response marker: [no text]."

const ocrUserPrompt = "Transcribe all text in this scanned document page."

// pageImage extracts the raster image of one PDF page. Scanned pages are a
// single image object; there is no pure-Go PDF rasterizer, so pages whose
// content is not an extractable image are reported as such. Overridable for
// tests.
var pageImage = func(pdfRaw []byte, page int) (data []byte, mime string, err error) {
	images, err := api.ExtractImagesRaw(bytes.NewReader(pdfRaw), []string{strconv.Itoa(page)}, nil)
	if err != nil {
		return nil, "", fmt.Errorf("extracting images from page %d: %w", page, err)
	}
	for _, byObj := range images {
		for _, img := range byObj {
			raw, err := io.ReadAll(img)
			if err != nil {
				return nil, "", err
			}
			mime := "image/" + strings.TrimPrefix(img.FileType, ".")
			if img.FileType == "jpg" {
				mime = "image/jpeg"
			}
			return raw, mime, nil
		}
	}
	return nil, "", fmt.Errorf("page %d has no extractable image object", page)
}

// ocrScannedPages transcribes scanned PDF pages through the vision model and
// returns them as blocks. Failures are hard errors: a partially-OCR'd
// document must not exist silently (same contract as embeddings).
func (ix *Indexer) ocrScannedPages(ctx context.Context, relPath string, content []byte, pages []int, nextLine int) ([]extract.Block, error) {
	blocks := make([]extract.Block, 0, len(pages))
	line := nextLine
	for _, page := range pages {
		img, mime, err := pageImage(content, page)
		if err != nil {
			return nil, fmt.Errorf("ocr %s: %w", relPath, err)
		}
		text, err := ix.OCR.CompleteWithImage(ctx, ocrSystemPrompt, ocrUserPrompt, img, mime)
		if err != nil {
			return nil, fmt.Errorf("ocr %s page %d: %w", relPath, page, err)
		}
		text = strings.TrimSpace(text)
		if text == "" || strings.EqualFold(text, "[no text]") {
			ix.logger().Info("ocr found no text on page", "path", relPath, "page", page)
			continue
		}
		lines := strings.Count(text, "\n") + 1
		blocks = append(blocks, extract.Block{
			Text:        text,
			HeadingPath: fmt.Sprintf("Page %d (OCR)", page),
			StartLine:   line,
			EndLine:     line + lines - 1,
		})
		line += lines
	}
	return blocks, nil
}
