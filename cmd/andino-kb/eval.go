package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"
)

// evalCmd measures retrieval quality over a golden query set with
// document-level metrics: recall@1, recall@N, MRR@N and latency percentiles.
// With --a/--b it runs the same set under two search configurations and
// reports per-query deltas, which is how retrieval changes get gated.
func evalCmd(configPath *string) *cobra.Command {
	var (
		queriesPath string
		baseURL     string
		kbName      string
		topN        int
		jsonOut     string
		paramsA     string
		paramsB     string
	)
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Measure retrieval quality (recall@k, MRR, latency) over a golden query set",
		RunE: func(cmd *cobra.Command, args []string) error {
			queries, err := loadEvalQueries(queriesPath)
			if err != nil {
				return err
			}
			client := &http.Client{Timeout: 60 * time.Second}

			overridesA, err := parseParams(paramsA)
			if err != nil {
				return fmt.Errorf("--a: %w", err)
			}
			runA, err := runEval(client, baseURL, kbName, queries, topN, overridesA)
			if err != nil {
				return err
			}

			if paramsB == "" {
				printRun("run", runA)
				return writeRunsJSON(jsonOut, map[string]*evalRun{"run": runA})
			}

			overridesB, err := parseParams(paramsB)
			if err != nil {
				return fmt.Errorf("--b: %w", err)
			}
			runB, err := runEval(client, baseURL, kbName, queries, topN, overridesB)
			if err != nil {
				return err
			}
			printRun("A "+paramsA, runA)
			printRun("B "+paramsB, runB)
			printDeltas(runA, runB)
			return writeRunsJSON(jsonOut, map[string]*evalRun{"a": runA, "b": runB})
		},
	}
	cmd.Flags().StringVar(&queriesPath, "queries", "parity/queries.yaml", "golden queries YAML (query + expect)")
	cmd.Flags().StringVar(&baseURL, "url", "http://127.0.0.1:8180", "andino-kb base URL")
	cmd.Flags().StringVar(&kbName, "kb", "", "knowledge base to query (empty = all)")
	cmd.Flags().IntVar(&topN, "top", 5, "documents considered per query")
	cmd.Flags().StringVar(&jsonOut, "json", "", "write full results to this JSON file")
	cmd.Flags().StringVar(&paramsA, "a", "", `search param overrides for run A, e.g. '{"rerank":false}'`)
	cmd.Flags().StringVar(&paramsB, "b", "", "search param overrides for run B (enables compare mode)")
	return cmd
}

type evalQuery struct {
	Query  string   `yaml:"query" json:"query"`
	Expect []string `yaml:"expect" json:"-"`
}

type evalResult struct {
	Query     string `json:"query"`
	Rank      int    `json:"rank"` // 1-based rank of the first expected doc; 0 = miss
	LatencyMS int64  `json:"latency_ms"`
}

type evalRun struct {
	Params     map[string]any `json:"params"`
	TopN       int            `json:"top_n"`
	Results    []evalResult   `json:"results"`
	Recall1    float64        `json:"recall_at_1"`
	RecallN    float64        `json:"recall_at_n"`
	MRR        float64        `json:"mrr"`
	LatencyP50 int64          `json:"latency_p50_ms"`
	LatencyP95 int64          `json:"latency_p95_ms"`
}

func loadEvalQueries(path string) ([]evalQuery, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var spec struct {
		Queries []evalQuery `yaml:"queries"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return nil, err
	}
	if len(spec.Queries) == 0 {
		return nil, fmt.Errorf("no queries in %s", path)
	}
	return spec.Queries, nil
}

func parseParams(s string) (map[string]any, error) {
	if s == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}

func runEval(client *http.Client, baseURL, kb string, queries []evalQuery, topN int, overrides map[string]any) (*evalRun, error) {
	run := &evalRun{Params: overrides, TopN: topN}
	var latencies []int64
	for _, q := range queries {
		body := map[string]any{"query": q.Query, "limit": topN * 4}
		for k, v := range overrides {
			body[k] = v
		}
		payload, _ := json.Marshal(body)
		url := baseURL + "/v1/search"
		if kb != "" {
			url = baseURL + "/v1/kb/" + kb + "/search"
		}
		start := time.Now()
		resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("query %q: %w", q.Query, err)
		}
		latency := time.Since(start).Milliseconds()
		var parsed struct {
			Results []struct {
				RelPath string `json:"rel_path"`
			} `json:"results"`
			Error string `json:"error"`
		}
		err = json.NewDecoder(resp.Body).Decode(&parsed)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("query %q: %w", q.Query, err)
		}
		if parsed.Error != "" {
			return nil, fmt.Errorf("query %q: %s", q.Query, parsed.Error)
		}

		rank := docRank(parsed.Results, q.Expect, topN)
		run.Results = append(run.Results, evalResult{Query: q.Query, Rank: rank, LatencyMS: latency})
		latencies = append(latencies, latency)
		if rank == 1 {
			run.Recall1++
		}
		if rank >= 1 {
			run.RecallN++
			run.MRR += 1.0 / float64(rank)
		}
	}
	n := float64(len(queries))
	run.Recall1 /= n
	run.RecallN /= n
	run.MRR /= n
	run.LatencyP50 = percentile(latencies, 50)
	run.LatencyP95 = percentile(latencies, 95)
	return run, nil
}

// docRank walks chunk results, deduplicating into a document ranking, and
// returns the 1-based rank of the first document matching any expectation.
func docRank(results []struct {
	RelPath string `json:"rel_path"`
}, expects []string, topN int) int {
	var docs []string
	for _, r := range results {
		if len(docs) >= topN {
			break
		}
		dup := false
		for _, d := range docs {
			if d == r.RelPath {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		docs = append(docs, r.RelPath)
	}
	for i, d := range docs {
		for _, want := range expects {
			if contains(d, want) {
				return i + 1
			}
		}
	}
	return 0
}

func contains(s, sub string) bool {
	return len(sub) > 0 && bytes.Contains([]byte(s), []byte(sub))
}

func percentile(values []int64, p int) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted) - 1) * p / 100
	return sorted[idx]
}

func printRun(label string, run *evalRun) {
	fmt.Printf("\n=== %s (n=%d, top %d)\n", label, len(run.Results), run.TopN)
	fmt.Printf("recall@1 %.0f%%   recall@%d %.0f%%   MRR %.3f   latency p50 %dms p95 %dms\n",
		run.Recall1*100, run.TopN, run.RecallN*100, run.MRR, run.LatencyP50, run.LatencyP95)
	for _, r := range run.Results {
		if r.Rank == 0 {
			fmt.Printf("  MISS  %s\n", truncate(r.Query, 70))
		}
	}
}

func printDeltas(a, b *evalRun) {
	fmt.Printf("\n=== deltas (B - A)\n")
	fmt.Printf("recall@1 %+.0f pt   recall@N %+.0f pt   MRR %+.3f   p50 %+dms\n",
		(b.Recall1-a.Recall1)*100, (b.RecallN-a.RecallN)*100, b.MRR-a.MRR, b.LatencyP50-a.LatencyP50)
	for i := range a.Results {
		ra, rb := a.Results[i].Rank, b.Results[i].Rank
		if ra != rb {
			fmt.Printf("  rank %2d -> %2d  %s\n", ra, rb, truncate(a.Results[i].Query, 60))
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func writeRunsJSON(path string, runs map[string]*evalRun) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(runs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
