package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `
server:
  bind: "127.0.0.1:8180"
  data_dir: /tmp/andino-kb
  api_keys:
    - key: "${TEST_ANDINO_KEY}"
      scope: read
inference:
  backends:
    - name: local-llama
      base_url: "http://127.0.0.1:8080/v1"
      api_key: "k"
  embedding_models:
    - name: qwen3-embed
      backend: local-llama
      model: qwen3-embedding-0.6b
      dimensions: 1024
defaults:
  embedding_model: qwen3-embed
knowledge_bases:
  - name: vault
    sources:
      - name: notes
        type: localdir
        path: /tmp/notes
        include: ["**/*.md"]
        exclude: [".obsidian/**"]
        watch: true
  - name: agent-memory
    writable: true
  - name: team-docs
    sources:
      - name: docs
        type: git
        url: "https://example.com/docs.git"
        paths: ["docs/**/*.md"]
`

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	t.Setenv("TEST_ANDINO_KEY", "sekrit")
	cfg, err := Load(write(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.APIKeys[0].Key != "sekrit" {
		t.Errorf("env expansion failed: %q", cfg.Server.APIKeys[0].Key)
	}
	if cfg.Server.APIKeys[0].Scope != "read" {
		t.Errorf("scope = %q", cfg.Server.APIKeys[0].Scope)
	}
	kb := cfg.KnowledgeBases[0]
	if kb.EmbeddingModel != "qwen3-embed" {
		t.Errorf("default embedding model not applied: %q", kb.EmbeddingModel)
	}
	if kb.Chunking == nil || kb.Chunking.MaxTokens != 512 {
		t.Errorf("default chunking not applied: %+v", kb.Chunking)
	}
	if kb.Sources[0].DebounceMS != 2000 {
		t.Errorf("localdir debounce default = %d", kb.Sources[0].DebounceMS)
	}
	git := cfg.KnowledgeBases[2].Sources[0]
	if git.Branch != "main" || git.PollInterval.Minutes() != 5 {
		t.Errorf("git defaults not applied: %+v", git)
	}
	if _, _, err := cfg.EmbeddingModelFor(&kb); err != nil {
		t.Errorf("EmbeddingModelFor: %v", err)
	}
	if cfg.Storage.Provider != "sqlite" {
		t.Errorf("storage provider default = %q", cfg.Storage.Provider)
	}
}

func TestLoadErrors(t *testing.T) {
	t.Setenv("TEST_ANDINO_KEY", "sekrit")
	cases := map[string]struct{ find, replace, wantErr string }{
		"unknown_field":       {find: "server:", replace: "server:\n  nope: 1", wantErr: "nope"},
		"missing_dimensions":  {find: "      dimensions: 1024", replace: "      dimensions: 0", wantErr: "dimensions"},
		"unknown_model_ref":   {find: "  embedding_model: qwen3-embed", replace: "  embedding_model: no-such-model", wantErr: "embedding_model"},
		"bad_glob":            {find: `include: ["**/*.md"]`, replace: `include: ["[/*.md"]`, wantErr: "invalid glob"},
		"git_fields_on_local": {find: "        watch: true", replace: "        watch: true\n        url: \"https://x\"", wantErr: "git fields"},
		"unknown_source_type": {find: "type: git", replace: "type: svn", wantErr: "unknown type"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			mutated := strings.Replace(validYAML, tc.find, tc.replace, 1)
			if mutated == validYAML {
				t.Fatalf("mutation %q did not apply", tc.find)
			}
			_, err := Load(write(t, mutated))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestEmptyAPIKeyFromUnsetEnv(t *testing.T) {
	os.Unsetenv("TEST_ANDINO_KEY")
	_, err := Load(write(t, validYAML))
	if err == nil || !strings.Contains(err.Error(), "empty key") {
		t.Fatalf("unset env var must fail loudly, got: %v", err)
	}
}
