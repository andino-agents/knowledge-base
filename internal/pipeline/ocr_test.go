package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andino-agents/knowledge-base/internal/inference"
	"github.com/andino-agents/knowledge-base/internal/testutil"
)

// fakeVision transcribes any image as a fixed marker sentence and records
// that it actually received an image content part.
func fakeVision(t *testing.T, sawImage *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if strings.Contains(string(m.Content), "image_url") && strings.Contains(string(m.Content), "base64,") {
				*sawImage = true
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{
				"content": "ACTA DE REUNION escaneada: aprobar el presupuesto zorzal.",
			}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOCRTranscribesScannedPages(t *testing.T) {
	ctx := context.Background()
	vault := t.TempDir()
	// A PDF whose page 2 has no text layer (see extract fixtures).
	raw := testutil.BuildFixturePDF([]string{"Native text page with plenty of characters to pass", ""})
	if err := os.WriteFile(filepath.Join(vault, "acta.pdf"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	ix, src := newTestIndexer(t, vault)
	sawImage := false
	visionSrv := fakeVision(t, &sawImage)
	ix.OCR = &inference.Chat{BaseURL: visionSrv.URL + "/v1", Model: "fake-vision"}

	// Inject the page-image extractor: the hand-made fixture has no real
	// image object, and rasterizing is out of scope by design.
	orig := pageImage
	pageImage = func(pdfRaw []byte, page int) ([]byte, string, error) {
		return []byte("fake-png-bytes"), "image/png", nil
	}
	t.Cleanup(func() { pageImage = orig })

	if _, err := ix.SyncSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	if !sawImage {
		t.Fatal("vision endpoint never received an image content part")
	}

	// The OCR text is searchable and the document is marked as OCR'd.
	vecs, _ := ix.Embedder.Embed(ctx, []string{"presupuesto zorzal"})
	hits, err := ix.Store.HybridSearch(ctx, "presupuesto zorzal", vecs[0], 5)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.RelPath == "acta.pdf" && h.FTSRank > 0 {
			found = true
			if h.HeadingPath != "Page 2 (OCR)" {
				t.Errorf("heading = %q, want Page 2 (OCR)", h.HeadingPath)
			}
			if h.Metadata["ocr"] != "vlm" || h.Metadata["ocr_pages"] != "2" {
				t.Errorf("ocr metadata = %v", h.Metadata)
			}
		}
	}
	if !found {
		t.Fatalf("OCR text not searchable: %+v", hits)
	}
}

func TestOCRFailureAbortsDocument(t *testing.T) {
	ctx := context.Background()
	vault := t.TempDir()
	raw := testutil.BuildFixturePDF([]string{""}) // single scanned page... single block, but scanned
	if err := os.WriteFile(filepath.Join(vault, "scan.pdf"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	ix, src := newTestIndexer(t, vault)
	ix.OCR = &inference.Chat{BaseURL: "http://127.0.0.1:1", Model: "dead", MaxRetries: 1,
		Client: &http.Client{Timeout: 200_000_000}}
	orig := pageImage
	pageImage = func(pdfRaw []byte, page int) ([]byte, string, error) {
		return []byte("img"), "image/png", nil
	}
	t.Cleanup(func() { pageImage = orig })

	if _, err := ix.SyncSource(ctx, src); err == nil {
		t.Fatal("sync with dead OCR backend must abort")
	}
}
