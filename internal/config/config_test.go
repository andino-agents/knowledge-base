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
  - name: bucket-docs
    sources:
      - name: policies
        type: s3
        bucket: corp-documents
        prefix: policies/
        paths: ["**/*.pdf"]
        poll_interval: 10m
        endpoint: "http://minio.internal:9000"
        path_style: true
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
	s3 := cfg.KnowledgeBases[3].Sources[0]
	if s3.Bucket != "corp-documents" || s3.Prefix != "policies/" || !s3.PathStyle {
		t.Errorf("s3 fields not loaded: %+v", s3)
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
		"s3_needs_bucket":     {find: "        bucket: corp-documents\n", replace: "        bucket: \"\"\n", wantErr: "bucket is required"},
		"localdir_on_s3":      {find: "        path_style: true", replace: "        path_style: true\n        path: /tmp/nope", wantErr: "localdir fields"},
		"openai_needs_url":    {find: `      base_url: "http://127.0.0.1:8080/v1"` + "\n", replace: "", wantErr: "base_url is required"},
		"unknown_backend":     {find: "    - name: local-llama\n", replace: "    - name: local-llama\n      type: vertex\n", wantErr: "must be openai or bedrock"},
		"bedrock_needs_region": {find: `      base_url: "http://127.0.0.1:8080/v1"` + "\n      api_key: \"k\"\n",
			replace: "      type: bedrock\n", wantErr: "needs a region"},
		// A key that would be ignored makes a config look authenticated when it is not.
		"bedrock_refuses_key": {find: `      base_url: "http://127.0.0.1:8080/v1"` + "\n",
			replace: "      type: bedrock\n      region: us-east-1\n", wantErr: "takes no base_url or api_key"},
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

func TestBearerToken(t *testing.T) {
	cases := map[string]struct {
		header    string
		wantToken string
		wantOK    bool
	}{
		"valid":            {"Bearer abc", "abc", true},
		"empty header":     {"", "", false},
		"no prefix":        {"abc", "", false},
		"wrong case":       {"bearer abc", "", false},
		"prefix only":      {"Bearer ", "", false},
		"basic auth":       {"Basic abc", "", false},
		"token with space": {"Bearer a b", "a b", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			token, ok := BearerToken(tc.header)
			if token != tc.wantToken || ok != tc.wantOK {
				t.Fatalf("BearerToken(%q) = (%q, %v), want (%q, %v)", tc.header, token, ok, tc.wantToken, tc.wantOK)
			}
		})
	}
}

func TestLookupKey(t *testing.T) {
	srv := Server{APIKeys: []APIKey{
		{Key: "r", Scope: "read"},
		{Key: "rw", Scope: "readwrite"},
	}}

	for _, tc := range []struct{ token, wantScope string }{{"r", "read"}, {"rw", "readwrite"}} {
		key, ok := srv.LookupKey(tc.token)
		if !ok || key.Scope != tc.wantScope {
			t.Errorf("LookupKey(%q) = (%+v, %v), want scope %q", tc.token, key, ok, tc.wantScope)
		}
	}
	for _, token := range []string{"", "nope", "r ", "rw-longer"} {
		if _, ok := srv.LookupKey(token); ok {
			t.Errorf("LookupKey(%q) matched, want miss", token)
		}
	}
	// An empty key list matches nothing; callers decide what that means.
	if _, ok := (&Server{}).LookupKey("r"); ok {
		t.Error("LookupKey on an empty key list matched")
	}
}

func TestAuthorizeOps(t *testing.T) {
	on, off := true, false
	keys := []APIKey{{Key: "r", Scope: "read"}}

	cases := map[string]struct {
		srv    Server
		header string
		want   bool
	}{
		"no keys, ops open by default":     {Server{}, "", true},
		"keys configured, ops closed":      {Server{APIKeys: keys}, "", false},
		"keys configured, valid key opens": {Server{APIKeys: keys}, "Bearer r", true},
		"keys configured, bad key closed":  {Server{APIKeys: keys}, "Bearer nope", false},
		"explicit off with keys":           {Server{APIKeys: keys, OpsRequireAuth: &off}, "", true},
		"explicit on without keys":         {Server{OpsRequireAuth: &on}, "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.srv.AuthorizeOps(tc.header); got != tc.want {
				t.Fatalf("AuthorizeOps(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestBedrockBackendLoads(t *testing.T) {
	t.Setenv("TEST_ANDINO_KEY", "sekrit")
	mutated := strings.Replace(validYAML, `      base_url: "http://127.0.0.1:8080/v1"`+"\n      api_key: \"k\"\n",
		"      type: bedrock\n      region: us-east-1\n", 1)
	cfg, err := Load(write(t, mutated))
	if err != nil {
		t.Fatal(err)
	}
	if b := cfg.Inference.Backends[0]; !b.IsBedrock() || b.Region != "us-east-1" {
		t.Fatalf("backend not read as bedrock: %+v", b)
	}
}

func TestRerankOnBedrockIsRefusedAtLoad(t *testing.T) {
	t.Setenv("TEST_ANDINO_KEY", "sekrit")
	mutated := strings.Replace(validYAML, `      base_url: "http://127.0.0.1:8080/v1"`+"\n      api_key: \"k\"\n",
		"      type: bedrock\n      region: us-east-1\n", 1)
	mutated = strings.Replace(mutated, "  embedding_models:", "  rerank_models:\n    - name: rr\n      backend: local-llama\n      model: x\n  embedding_models:", 1)
	_, err := Load(write(t, mutated))
	if err == nil || !strings.Contains(err.Error(), "not supported on a bedrock backend") {
		t.Fatalf("want the rerank refusal at load, not at the first search: %v", err)
	}
}
