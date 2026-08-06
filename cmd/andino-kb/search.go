package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/andino-agents/knowledge-base/internal/app"
	"github.com/andino-agents/knowledge-base/internal/config"
)

func searchCmd(configPath *string) *cobra.Command {
	var (
		kbName   string
		limit    int
		minScore float64
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Debug: run a hybrid search from the command line",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			logger := newLogger(cfg)
			a, err := app.New(cmd.Context(), cfg, logger)
			if err != nil {
				return err
			}
			defer a.Close()

			query := strings.Join(args, " ")
			start := time.Now()
			opts := app.SearchOpts{Limit: limit, MinScore: minScore}
			var hits []app.Hit
			if kbName != "" {
				hits, err = a.Search(cmd.Context(), kbName, query, opts)
			} else {
				hits, err = a.SearchAll(cmd.Context(), query, opts)
			}
			if err != nil {
				return err
			}
			elapsed := time.Since(start)
			for i, h := range hits {
				fmt.Printf("%2d. [%.2f] %s (%s:%d-%d)\n", i+1, h.Relevance, h.RelPath, h.KnowledgeBase, h.StartLine, h.EndLine)
				if h.HeadingPath != "" {
					fmt.Printf("    § %s\n", h.HeadingPath)
				}
				text := h.Text
				if len(text) > 200 {
					text = text[:200] + "…"
				}
				fmt.Printf("    %s\n", strings.ReplaceAll(text, "\n", " "))
			}
			fmt.Printf("\n%d hit(s) in %s\n", len(hits), elapsed.Round(time.Millisecond))
			return nil
		},
	}
	cmd.Flags().StringVar(&kbName, "kb", "", "restrict to one knowledge base")
	cmd.Flags().IntVar(&limit, "limit", 8, "maximum results")
	cmd.Flags().Float64Var(&minScore, "min-score", 0, "minimum normalized relevance (0..1)")
	return cmd
}
