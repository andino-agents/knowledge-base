package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"
)

// parityCmd compares retrieval quality against a legacy RAG endpoint using a
// golden query set: recall@N over unique documents. The gate for replacing
// the legacy system is andino recall >= legacy recall.
func parityCmd(configPath *string) *cobra.Command {
	var (
		queriesPath string
		andinoURL   string
		legacyURL   string
		kbName      string
		topN        int
	)
	cmd := &cobra.Command{
		Use:   "parity",
		Short: "Compare retrieval recall against a legacy RAG endpoint over a golden query set",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(queriesPath)
			if err != nil {
				return err
			}
			var spec struct {
				Queries []struct {
					Query  string   `yaml:"query"`
					Expect []string `yaml:"expect"`
				} `yaml:"queries"`
			}
			if err := yaml.Unmarshal(raw, &spec); err != nil {
				return err
			}
			if len(spec.Queries) == 0 {
				return fmt.Errorf("no queries in %s", queriesPath)
			}

			client := &http.Client{Timeout: 30 * time.Second}
			var andinoHits, legacyHits int
			fmt.Printf("%-52s %-8s %-8s\n", "query", "andino", "legacy")
			for _, q := range spec.Queries {
				aDocs, err := andinoTopDocs(client, andinoURL, kbName, q.Query, topN)
				if err != nil {
					return fmt.Errorf("andino query %q: %w", q.Query, err)
				}
				lDocs, lErr := legacyTopDocs(client, legacyURL, q.Query, topN)

				aHit := matchesAny(aDocs, q.Expect)
				lHit := lErr == nil && matchesAny(lDocs, q.Expect)
				if aHit {
					andinoHits++
				}
				if lHit {
					legacyHits++
				}
				label := q.Query
				if len(label) > 50 {
					label = label[:50] + "…"
				}
				fmt.Printf("%-52s %-8s %-8s\n", label, mark(aHit), markErr(lHit, lErr))
			}

			n := len(spec.Queries)
			fmt.Printf("\nrecall@%d: andino %d/%d (%.0f%%) vs legacy %d/%d (%.0f%%)\n",
				topN, andinoHits, n, 100*float64(andinoHits)/float64(n),
				legacyHits, n, 100*float64(legacyHits)/float64(n))
			if andinoHits < legacyHits {
				return fmt.Errorf("parity gate FAILED: andino recall below legacy")
			}
			fmt.Println("parity gate PASSED")
			return nil
		},
	}
	cmd.Flags().StringVar(&queriesPath, "queries", "parity/queries.yaml", "golden queries YAML")
	cmd.Flags().StringVar(&andinoURL, "andino-url", "http://127.0.0.1:8180", "andino-kb base URL")
	cmd.Flags().StringVar(&legacyURL, "legacy-url", "http://127.0.0.1:8000", "legacy RAG base URL (empty to skip)")
	cmd.Flags().StringVar(&kbName, "kb", "", "knowledge base to query on the andino side")
	cmd.Flags().IntVar(&topN, "top", 5, "documents considered per query")
	return cmd
}

func andinoTopDocs(client *http.Client, base, kb, query string, topN int) ([]string, error) {
	body, _ := json.Marshal(map[string]any{"query": query, "limit": topN * 4})
	url := base + "/v1/search"
	if kb != "" {
		url = base + "/v1/kb/" + kb + "/search"
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var parsed struct {
		Results []struct {
			RelPath string `json:"rel_path"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	var docs []string
	for _, r := range parsed.Results {
		docs = appendUnique(docs, r.RelPath, topN)
	}
	return docs, nil
}

// legacyTopDocs speaks the vault-mcp REST shape (POST /query, results or
// sources carrying file_path).
func legacyTopDocs(client *http.Client, base, query string, topN int) ([]string, error) {
	if base == "" {
		return nil, fmt.Errorf("legacy disabled")
	}
	body, _ := json.Marshal(map[string]any{"query": query})
	resp, err := client.Post(base+"/query", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	items, _ := parsed["results"].([]any)
	if items == nil {
		items, _ = parsed["sources"].([]any)
	}
	var docs []string
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		fp, _ := m["file_path"].(string)
		if fp == "" {
			if meta, _ := m["metadata"].(map[string]any); meta != nil {
				fp, _ = meta["file_path"].(string)
			}
		}
		if fp != "" {
			docs = appendUnique(docs, fp, topN)
		}
	}
	return docs, nil
}

func appendUnique(docs []string, doc string, limit int) []string {
	if len(docs) >= limit {
		return docs
	}
	for _, d := range docs {
		if d == doc {
			return docs
		}
	}
	return append(docs, doc)
}

func matchesAny(docs, expects []string) bool {
	for _, doc := range docs {
		for _, want := range expects {
			if strings.Contains(doc, want) {
				return true
			}
		}
	}
	return false
}

func mark(hit bool) string {
	if hit {
		return "HIT"
	}
	return "miss"
}

func markErr(hit bool, err error) string {
	if err != nil {
		return "err"
	}
	return mark(hit)
}
