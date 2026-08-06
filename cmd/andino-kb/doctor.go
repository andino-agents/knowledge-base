package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"time"

	"github.com/spf13/cobra"

	"github.com/andino-agents/knowledge-base/internal/app"
	"github.com/andino-agents/knowledge-base/internal/config"
	"github.com/andino-agents/knowledge-base/internal/inference"
)

// doctorCmd walks the whole dependency chain and reports exactly what works
// and what is broken, with the fix next to the failure. This is the answer
// to "it doesn't work on my setup".
func doctorCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the full chain: config, backends, models, stores and sources",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.Context(), *configPath)
		},
	}
}

type check struct {
	name string
	err  error
	note string
}

func runDoctor(ctx context.Context, configPath string) error {
	var checks []check
	report := func(name string, err error, note string) {
		checks = append(checks, check{name, err, note})
		if err != nil {
			fmt.Printf("  ✗ %-42s %v\n", name, err)
			if note != "" {
				fmt.Printf("      fix: %s\n", note)
			}
		} else {
			ok := "ok"
			if note != "" {
				ok = note
			}
			fmt.Printf("  ✓ %-42s %s\n", name, ok)
		}
	}

	fmt.Println("config")
	cfg, err := config.Load(configPath)
	if err != nil {
		report("load "+configPath, err, "the error above points at the exact line; config is strict on purpose")
		return summarize(checks)
	}
	report("load "+configPath, nil, fmt.Sprintf("%d knowledge base(s)", len(cfg.KnowledgeBases)))

	// Inference checks are per referenced model, deduplicated.
	fmt.Println("inference")
	checkedEmb := map[string]bool{}
	checkedChat := map[string]bool{}
	checkedRerank := map[string]bool{}

	for i := range cfg.KnowledgeBases {
		kb := &cfg.KnowledgeBases[i]

		if !checkedEmb[kb.EmbeddingModel] {
			checkedEmb[kb.EmbeddingModel] = true
			model, backend, err := cfg.EmbeddingModelFor(kb)
			if err != nil {
				report("embedding model "+kb.EmbeddingModel, err, "")
				continue
			}
			emb := &inference.Embedder{
				BaseURL: backend.BaseURL, APIKey: backend.APIKey,
				Model: model.Model, Dimensions: model.Dimensions, MaxRetries: 1,
				Logger: discardLogger(),
			}
			probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err = emb.Embed(probeCtx, []string{"doctor probe"})
			cancel()
			report(fmt.Sprintf("embeddings %s (%dd)", model.Model, model.Dimensions), err,
				pick(err, "is the inference server up? does the model name match? dims mismatch means the wrong model answers", ""))
		}

		if kb.RerankModel != "" && !checkedRerank[kb.RerankModel] {
			checkedRerank[kb.RerankModel] = true
			var rm config.RerankModel
			var bk config.Backend
			for _, m := range cfg.Inference.RerankModels {
				if m.Name == kb.RerankModel {
					rm = m
					for _, b := range cfg.Inference.Backends {
						if b.Name == m.Backend {
							bk = b
						}
					}
				}
			}
			rr := &inference.Reranker{BaseURL: bk.BaseURL, APIKey: bk.APIKey, Model: rm.Model, Logger: discardLogger()}
			probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			ranked, err := rr.Rerank(probeCtx, "which doc mentions kubernetes",
				[]string{"kubernetes eviction notes", "banana bread recipe"}, 2)
			cancel()
			if err == nil && (len(ranked) != 2 || ranked[0].Index != 0) {
				err = fmt.Errorf("degenerate scores: the relevant document did not win (%+v)", ranked)
			}
			report("rerank "+rm.Model, err,
				pick(err, "needs --reranking --pooling rank on the model; community Qwen3-Reranker GGUFs often miss cls.output.weight", ""))
		}

		if kb.Contextual != nil && kb.Contextual.Enabled && !checkedChat["ctx:"+kb.Contextual.Model] {
			checkedChat["ctx:"+kb.Contextual.Model] = true
			chatModel, backend, err := cfg.ChatModelByName(kb.Contextual.Model)
			if err != nil {
				report("contextual model "+kb.Contextual.Model, err, "")
			} else {
				chat := &inference.Chat{BaseURL: backend.BaseURL, APIKey: backend.APIKey,
					Model: chatModel.Model, MaxTokens: chatModel.MaxTokens, ExtraBody: chatModel.ExtraBody, MaxRetries: 1,
					Logger: discardLogger()}
				probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				_, err = chat.Complete(probeCtx, "Answer with one word.", "Say: ready")
				cancel()
				report("contextual chat "+chatModel.Model, err,
					pick(err, "empty content usually means a thinking-first model: set extra_body chat_template_kwargs.enable_thinking=false", ""))
			}
		}

		if kb.OCR != nil && kb.OCR.Enabled && !checkedChat["ocr:"+kb.OCR.Model] {
			checkedChat["ocr:"+kb.OCR.Model] = true
			chatModel, backend, err := cfg.ChatModelByName(kb.OCR.Model)
			if err != nil {
				report("ocr model "+kb.OCR.Model, err, "")
			} else {
				chat := &inference.Chat{BaseURL: backend.BaseURL, APIKey: backend.APIKey,
					Model: chatModel.Model, MaxTokens: chatModel.MaxTokens, ExtraBody: chatModel.ExtraBody, MaxRetries: 1,
					Logger: discardLogger()}
				probeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
				_, err = chat.CompleteWithImage(probeCtx, "Describe the image in one word.", "What color is this image?", doctorPNG(), "image/png")
				cancel()
				report("ocr vision "+chatModel.Model, err,
					pick(err, "the model must be vision-capable (llama.cpp needs an mmproj; or point at a vision VLM endpoint)", ""))
			}
		}
	}

	// Stores and sources through the real runtime constructor.
	fmt.Println("stores and sources")
	a, err := app.New(ctx, cfg, discardLogger())
	if err != nil {
		report("open stores", err, "identity mismatches mean the config changed model/dims: andino-kb index --rebuild re-embeds explicitly")
		return summarize(checks)
	}
	defer a.Close()
	for _, name := range a.KBNames() {
		kb, _ := a.KB(name)
		st, err := kb.Store.Stats(ctx)
		report("store "+name, err, pick(err, "", fmt.Sprintf("%d docs, %d chunks", st.Documents, st.Chunks)))
		for _, src := range kb.Sources {
			listCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			var listErr error
			var n int
			if err := src.Sync(listCtx); err != nil {
				listErr = err
			} else if metas, err := src.List(listCtx); err != nil {
				listErr = err
			} else {
				n = len(metas)
			}
			cancel()
			report(fmt.Sprintf("source %s/%s", name, src.Name()), listErr,
				pick(listErr, "check paths/credentials; S3 uses the standard AWS credential chain", fmt.Sprintf("%d indexable file(s)", n)))
		}
	}
	return summarize(checks)
}

func summarize(checks []check) error {
	failed := 0
	for _, c := range checks {
		if c.err != nil {
			failed++
		}
	}
	fmt.Println()
	if failed == 0 {
		fmt.Printf("all %d checks passed\n", len(checks))
		return nil
	}
	return fmt.Errorf("%d of %d checks failed", failed, len(checks))
}

func pick(err error, onFail, onOK string) string {
	if err != nil {
		return onFail
	}
	return onOK
}

// doctorPNG renders a tiny solid-red PNG for the vision probe.
func doctorPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}
