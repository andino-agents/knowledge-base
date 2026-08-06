// Package config loads and validates the andino-kb YAML configuration.
//
// Decoding is strict: unknown fields are errors, so a typo in a pipeline
// definition fails at startup instead of silently indexing nothing.
// ${VAR} references are expanded from the environment before parsing;
// unset variables expand to the empty string.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/goccy/go-yaml"
)

type Config struct {
	Server         Server          `yaml:"server"`
	Storage        Storage         `yaml:"storage"`
	Inference      Inference       `yaml:"inference"`
	Defaults       Defaults        `yaml:"defaults"`
	KnowledgeBases []KnowledgeBase `yaml:"knowledge_bases"`
}

type Server struct {
	Bind      string   `yaml:"bind"`
	DataDir   string   `yaml:"data_dir"`
	APIKeys   []APIKey `yaml:"api_keys"`
	LogLevel  string   `yaml:"log_level"`
	LogFormat string   `yaml:"log_format"`
}

// APIKey grants access to the REST and MCP APIs. Scope "read" allows search
// and reads; "readwrite" additionally allows store/delete on writable KBs.
type APIKey struct {
	Key   string `yaml:"key"`
	Scope string `yaml:"scope"`
}

type Storage struct {
	Provider string         `yaml:"provider"`
	Options  map[string]any `yaml:"options"`
}

type Inference struct {
	Backends        []Backend        `yaml:"backends"`
	EmbeddingModels []EmbeddingModel `yaml:"embedding_models"`
	RerankModels    []RerankModel    `yaml:"rerank_models"`
	ChatModels      []ChatModel      `yaml:"chat_models"`
}

type Backend struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

type EmbeddingModel struct {
	Name       string `yaml:"name"`
	Backend    string `yaml:"backend"`
	Model      string `yaml:"model"`
	Dimensions int    `yaml:"dimensions"`
	BatchSize  int    `yaml:"batch_size"`
	MaxRetries int    `yaml:"max_retries"`
}

type RerankModel struct {
	Name    string `yaml:"name"`
	Backend string `yaml:"backend"`
	Model   string `yaml:"model"`
}

// ChatModel is a chat-completions model used for index-time work such as
// contextual retrieval.
type ChatModel struct {
	Name      string `yaml:"name"`
	Backend   string `yaml:"backend"`
	Model     string `yaml:"model"`
	MaxTokens int    `yaml:"max_tokens"`
	// ExtraBody is merged into the /v1/chat/completions request body.
	// Needed e.g. to disable reasoning on thinking-first models
	// (llama.cpp/vLLM: chat_template_kwargs: {enable_thinking: false}),
	// whose reasoning otherwise consumes max_tokens and returns empty
	// content.
	ExtraBody map[string]any `yaml:"extra_body"`
}

// Contextual enables contextual retrieval for a knowledge base: an LLM
// generates a short situating context per chunk at index time, which is
// embedded and BM25-indexed alongside the text.
type Contextual struct {
	Enabled bool   `yaml:"enabled"`
	Model   string `yaml:"model"` // ref into inference.chat_models
}

type Chunking struct {
	Strategy      string `yaml:"strategy"`
	MaxTokens     int    `yaml:"max_tokens"`
	OverlapTokens int    `yaml:"overlap_tokens"`
}

type Defaults struct {
	Chunking       Chunking `yaml:"chunking"`
	EmbeddingModel string   `yaml:"embedding_model"`
	RerankModel    string   `yaml:"rerank_model"`
}

type KnowledgeBase struct {
	Name           string      `yaml:"name"`
	Description    string      `yaml:"description"`
	Writable       bool        `yaml:"writable"`
	Sources        []Source    `yaml:"sources"`
	Chunking       *Chunking   `yaml:"chunking"`
	EmbeddingModel string      `yaml:"embedding_model"`
	RerankModel    string      `yaml:"rerank_model"`
	Contextual     *Contextual `yaml:"contextual"`
}

// Source is a single ingestion pipeline. Type-specific fields are flat; the
// validator enforces which apply to which type.
type Source struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"` // localdir | git

	// localdir
	Path       string   `yaml:"path"`
	Include    []string `yaml:"include"`
	Exclude    []string `yaml:"exclude"`
	Watch      bool     `yaml:"watch"`
	DebounceMS int      `yaml:"debounce_ms"`

	// git
	URL          string        `yaml:"url"`
	Branch       string        `yaml:"branch"`
	Paths        []string      `yaml:"paths"`
	PollInterval time.Duration `yaml:"poll_interval"`
	TokenEnv     string        `yaml:"token_env"`
}

// ManagedSourceName is the implicit source name for agent-written documents
// in writable knowledge bases.
const ManagedSourceName = "managed"

// Load reads, expands, parses and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	expanded := os.Expand(string(raw), func(name string) string { return os.Getenv(name) })

	var cfg Config
	if err := yaml.UnmarshalWithOptions([]byte(expanded), &cfg, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Bind == "" {
		c.Server.Bind = "127.0.0.1:8180"
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = "info"
	}
	if c.Server.LogFormat == "" {
		c.Server.LogFormat = "text"
	}
	if c.Storage.Provider == "" {
		c.Storage.Provider = "sqlite"
	}
	if c.Defaults.Chunking.Strategy == "" {
		c.Defaults.Chunking.Strategy = "markdown"
	}
	if c.Defaults.Chunking.MaxTokens == 0 {
		c.Defaults.Chunking.MaxTokens = 512
	}
	if c.Defaults.Chunking.OverlapTokens == 0 {
		c.Defaults.Chunking.OverlapTokens = 64
	}
	for i := range c.Inference.EmbeddingModels {
		m := &c.Inference.EmbeddingModels[i]
		if m.BatchSize == 0 {
			m.BatchSize = 32
		}
		if m.MaxRetries == 0 {
			m.MaxRetries = 4
		}
	}
	for i := range c.Inference.ChatModels {
		if c.Inference.ChatModels[i].MaxTokens == 0 {
			c.Inference.ChatModels[i].MaxTokens = 200
		}
	}
	for i := range c.Server.APIKeys {
		if c.Server.APIKeys[i].Scope == "" {
			c.Server.APIKeys[i].Scope = "readwrite"
		}
	}
	for i := range c.KnowledgeBases {
		kb := &c.KnowledgeBases[i]
		if kb.EmbeddingModel == "" {
			kb.EmbeddingModel = c.Defaults.EmbeddingModel
		}
		if kb.RerankModel == "" {
			kb.RerankModel = c.Defaults.RerankModel
		}
		if kb.Chunking == nil {
			ch := c.Defaults.Chunking
			kb.Chunking = &ch
		}
		for j := range kb.Sources {
			s := &kb.Sources[j]
			if s.Type == "localdir" && s.DebounceMS == 0 {
				s.DebounceMS = 2000
			}
			if s.Type == "git" {
				if s.Branch == "" {
					s.Branch = "main"
				}
				if s.PollInterval == 0 {
					s.PollInterval = 5 * time.Minute
				}
			}
		}
	}
}

func (c *Config) Validate() error {
	if c.Server.DataDir == "" {
		return fmt.Errorf("server.data_dir is required")
	}
	for _, k := range c.Server.APIKeys {
		if k.Key == "" {
			return fmt.Errorf("server.api_keys: empty key (unset environment variable?)")
		}
		if k.Scope != "read" && k.Scope != "readwrite" {
			return fmt.Errorf("server.api_keys: scope %q must be read or readwrite", k.Scope)
		}
	}

	backends := map[string]bool{}
	for _, b := range c.Inference.Backends {
		if b.Name == "" || b.BaseURL == "" {
			return fmt.Errorf("inference.backends: name and base_url are required")
		}
		if backends[b.Name] {
			return fmt.Errorf("inference.backends: duplicate name %q", b.Name)
		}
		backends[b.Name] = true
	}
	embModels := map[string]bool{}
	for _, m := range c.Inference.EmbeddingModels {
		if m.Name == "" || m.Model == "" {
			return fmt.Errorf("inference.embedding_models: name and model are required")
		}
		if !backends[m.Backend] {
			return fmt.Errorf("inference.embedding_models[%s]: unknown backend %q", m.Name, m.Backend)
		}
		if m.Dimensions <= 0 {
			return fmt.Errorf("inference.embedding_models[%s]: dimensions is required", m.Name)
		}
		if embModels[m.Name] {
			return fmt.Errorf("inference.embedding_models: duplicate name %q", m.Name)
		}
		embModels[m.Name] = true
	}
	rerankModels := map[string]bool{}
	for _, m := range c.Inference.RerankModels {
		if m.Name == "" || m.Model == "" {
			return fmt.Errorf("inference.rerank_models: name and model are required")
		}
		if !backends[m.Backend] {
			return fmt.Errorf("inference.rerank_models[%s]: unknown backend %q", m.Name, m.Backend)
		}
		rerankModels[m.Name] = true
	}
	chatModels := map[string]bool{}
	for _, m := range c.Inference.ChatModels {
		if m.Name == "" || m.Model == "" {
			return fmt.Errorf("inference.chat_models: name and model are required")
		}
		if !backends[m.Backend] {
			return fmt.Errorf("inference.chat_models[%s]: unknown backend %q", m.Name, m.Backend)
		}
		chatModels[m.Name] = true
	}

	if len(c.KnowledgeBases) == 0 {
		return fmt.Errorf("at least one knowledge base is required")
	}
	kbNames := map[string]bool{}
	for _, kb := range c.KnowledgeBases {
		if kb.Name == "" {
			return fmt.Errorf("knowledge_bases: name is required")
		}
		if kbNames[kb.Name] {
			return fmt.Errorf("knowledge_bases: duplicate name %q", kb.Name)
		}
		kbNames[kb.Name] = true
		if !embModels[kb.EmbeddingModel] {
			return fmt.Errorf("knowledge_bases[%s]: unknown embedding_model %q (set it or defaults.embedding_model)", kb.Name, kb.EmbeddingModel)
		}
		if kb.RerankModel != "" && !rerankModels[kb.RerankModel] {
			return fmt.Errorf("knowledge_bases[%s]: unknown rerank_model %q", kb.Name, kb.RerankModel)
		}
		if kb.Contextual != nil && kb.Contextual.Enabled && !chatModels[kb.Contextual.Model] {
			return fmt.Errorf("knowledge_bases[%s]: contextual.model %q not found in inference.chat_models", kb.Name, kb.Contextual.Model)
		}
		if len(kb.Sources) == 0 && !kb.Writable {
			return fmt.Errorf("knowledge_bases[%s]: needs at least one source or writable: true", kb.Name)
		}
		srcNames := map[string]bool{}
		for _, s := range kb.Sources {
			if s.Name == "" {
				return fmt.Errorf("knowledge_bases[%s]: source name is required", kb.Name)
			}
			if s.Name == ManagedSourceName {
				return fmt.Errorf("knowledge_bases[%s]: source name %q is reserved", kb.Name, ManagedSourceName)
			}
			if srcNames[s.Name] {
				return fmt.Errorf("knowledge_bases[%s]: duplicate source name %q", kb.Name, s.Name)
			}
			srcNames[s.Name] = true
			if err := validateSource(kb.Name, s); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSource(kb string, s Source) error {
	prefix := fmt.Sprintf("knowledge_bases[%s].sources[%s]", kb, s.Name)
	switch s.Type {
	case "localdir":
		if s.Path == "" {
			return fmt.Errorf("%s: path is required for localdir", prefix)
		}
		for _, g := range append(append([]string{}, s.Include...), s.Exclude...) {
			if !doublestar.ValidatePattern(g) {
				return fmt.Errorf("%s: invalid glob %q", prefix, g)
			}
		}
		if s.URL != "" || s.Branch != "" || len(s.Paths) > 0 || s.PollInterval != 0 || s.TokenEnv != "" {
			return fmt.Errorf("%s: git fields set on a localdir source", prefix)
		}
	case "git":
		if s.URL == "" {
			return fmt.Errorf("%s: url is required for git", prefix)
		}
		for _, g := range s.Paths {
			if !doublestar.ValidatePattern(g) {
				return fmt.Errorf("%s: invalid glob %q", prefix, g)
			}
		}
		if s.Path != "" || len(s.Include) > 0 || len(s.Exclude) > 0 || s.Watch || s.DebounceMS != 2000 && s.DebounceMS != 0 {
			return fmt.Errorf("%s: localdir fields set on a git source", prefix)
		}
	default:
		return fmt.Errorf("%s: unknown type %q (localdir | git)", prefix, s.Type)
	}
	return nil
}

// ChatModelFor resolves a KB's contextual chat model definition.
func (c *Config) ChatModelFor(kb *KnowledgeBase) (ChatModel, Backend, error) {
	if kb.Contextual == nil {
		return ChatModel{}, Backend{}, fmt.Errorf("config: kb %q has no contextual section", kb.Name)
	}
	for _, m := range c.Inference.ChatModels {
		if m.Name == kb.Contextual.Model {
			for _, b := range c.Inference.Backends {
				if b.Name == m.Backend {
					return m, b, nil
				}
			}
		}
	}
	return ChatModel{}, Backend{}, fmt.Errorf("config: chat model %q not found for kb %q", kb.Contextual.Model, kb.Name)
}

// EmbeddingModelFor resolves a KB's embedding model definition.
func (c *Config) EmbeddingModelFor(kb *KnowledgeBase) (EmbeddingModel, Backend, error) {
	for _, m := range c.Inference.EmbeddingModels {
		if m.Name == kb.EmbeddingModel {
			for _, b := range c.Inference.Backends {
				if b.Name == m.Backend {
					return m, b, nil
				}
			}
		}
	}
	return EmbeddingModel{}, Backend{}, fmt.Errorf("config: embedding model %q not found for kb %q", kb.EmbeddingModel, kb.Name)
}
