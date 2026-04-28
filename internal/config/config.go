// Package config loads memmy's YAML configuration. Schema mirrors
// DESIGN.md §12.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration loaded at startup.
type Config struct {
	Server      ServerConfig       `yaml:"server"`
	Storage     StorageConfig      `yaml:"storage"`
	Embedder    EmbedderConfig     `yaml:"embedder"`
	VectorIndex VectorIndexConfig  `yaml:"vector_index"`
	Memory      MemoryConfig       `yaml:"memory"`
	Tenant      TenantSchemaConfig `yaml:"tenant"`
}

// TenantSchemaConfig is the optional tenant-tuple schema. When empty,
// memmy accepts any string-keyed tuple; when set, every Service
// operation rejects tuples that don't match.
//
// Stored memories are NOT migrated when the schema changes —
// TenantID is derived from the (validated) tuple as today, so data
// written under one schema remains addressable if the schema is
// changed back to one that accepts the original tuple shape.
type TenantSchemaConfig struct {
	Description string                       `yaml:"description"`
	Keys        map[string]TenantKeyConfig   `yaml:"keys"`
	OneOf       [][]string                   `yaml:"one_of"`
}

// TenantKeyConfig describes one key in the tenant tuple. Tenant values
// are always strings (Go: map[string]string), so the type is implicit.
type TenantKeyConfig struct {
	Description string   `yaml:"description"`
	Pattern     string   `yaml:"pattern"`
	Enum        []string `yaml:"enum"`
	Required    bool     `yaml:"required"`
}

// IsConfigured reports whether the tenant schema has any rules. An
// unconfigured schema means "accept any tuple" — today's behavior.
func (t TenantSchemaConfig) IsConfigured() bool {
	return len(t.Keys) > 0 || len(t.OneOf) > 0
}

type ServerConfig struct {
	Transports map[string]TransportConfig `yaml:"transports"`
}

type TransportConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"`
}

type StorageConfig struct {
	Backend string             `yaml:"backend"`
	BBolt   BBoltStorageConfig `yaml:"bbolt"`
}

type BBoltStorageConfig struct {
	Path string `yaml:"path"`
}

type EmbedderConfig struct {
	Backend string         `yaml:"backend"`
	Gemini  GeminiConfig   `yaml:"gemini"`
	Fake    FakeEmbedderConfig `yaml:"fake"`
}

type GeminiConfig struct {
	Model       string `yaml:"model"`
	APIKey      string `yaml:"api_key"`
	Dim         int    `yaml:"dim"`
	Concurrency int    `yaml:"concurrency"`
}

type FakeEmbedderConfig struct {
	Dim int `yaml:"dim"`
}

type VectorIndexConfig struct {
	FlatScanThreshold int        `yaml:"flat_scan_threshold"`
	HNSW              HNSWParams `yaml:"hnsw"`
}

type HNSWParams struct {
	M              int     `yaml:"m"`
	M0             int     `yaml:"m0"`
	EfConstruction int     `yaml:"ef_construction"`
	EfSearch       int     `yaml:"ef_search"`
	ML             float64 `yaml:"ml"`
}

type MemoryConfig struct {
	ChunkWindowSize      int           `yaml:"chunk_window_size"`
	ChunkStride          int           `yaml:"chunk_stride"`
	RetrievalK           int           `yaml:"retrieval_k"`
	RetrievalHops        int           `yaml:"retrieval_hops"`
	RetrievalOversample  int           `yaml:"retrieval_oversample"`
	StructuralRecentN    int           `yaml:"structural_recent_n"`
	StructuralRecentDelta time.Duration `yaml:"structural_recent_delta"`

	// RefractoryPeriod blocks repeated explicit Reinforce/Demote/Mark
	// bumps on the same node within the window. Implicit Recall
	// co-retrieval bumps are NOT throttled. Set to 0 to disable.
	RefractoryPeriod time.Duration `yaml:"refractory_period"`

	// LogDampening scales positive Reinforce/Mark deltas by
	// (1 - weight/WeightCap), so saturation is asymptotic instead of a
	// hard wall. Demote is unaffected.
	LogDampening bool `yaml:"log_dampening"`

	// MarkMaxNodes caps the number of recent nodes a single Mark call
	// will walk.
	MarkMaxNodes int `yaml:"mark_max_nodes"`

	Scoring   ScoringConfig   `yaml:"scoring"`
	Decay     DecayConfig     `yaml:"decay"`
	Reinforce ReinforceConfig `yaml:"reinforce"`
	Prune     PruneConfig     `yaml:"prune"`
	WeightCap float64         `yaml:"weight_cap"`
}

type ScoringConfig struct {
	SimAlpha   float64 `yaml:"sim_alpha"`
	WeightBeta float64 `yaml:"weight_beta"`
}

type DecayConfig struct {
	NodeLambda            float64 `yaml:"node_lambda"`
	EdgeStructuralLambda  float64 `yaml:"edge_structural_lambda"`
	EdgeCoRetrievalLambda float64 `yaml:"edge_coretrieval_lambda"`
	EdgeCoTraversalLambda float64 `yaml:"edge_cotraversal_lambda"`
}

type ReinforceConfig struct {
	NodeDelta                  float64 `yaml:"node_delta"`
	EdgeCoRetrievalBase        float64 `yaml:"edge_coretrieval_base"`
	EdgeCoTraversalMultiplier  float64 `yaml:"edge_cotraversal_multiplier"`
	EdgeStructuralWeight       float64 `yaml:"edge_structural_weight"`
	EdgeStructuralTemporalWeight float64 `yaml:"edge_structural_temporal_weight"`
}

type PruneConfig struct {
	EdgeFloor float64 `yaml:"edge_floor"`
	NodeFloor float64 `yaml:"node_floor"`
}

// Default returns a Config populated with the documented defaults.
//
// No transport is enabled by default — the operator must explicitly
// declare which one(s) to run via server.transports. An empty
// transports map fails Validate() so config typos can't accidentally
// bring up an unwanted listener (or, in the case of stdio, take over
// the parent's stdin/stdout silently).
func Default() Config {
	return Config{
		Server: ServerConfig{
			Transports: map[string]TransportConfig{},
		},
		Storage: StorageConfig{
			Backend: "bbolt",
			BBolt:   BBoltStorageConfig{Path: "./data/memmy.db"},
		},
		Embedder: EmbedderConfig{
			Backend: "fake",
			Fake:    FakeEmbedderConfig{Dim: 64},
			Gemini: GeminiConfig{
				// gemini-embedding-2 is the recommended model. It uses
				// in-band prompt prefixes for task hints (RetrievalDoc /
				// RetrievalQuery / etc.) — memmy's gemini embedder
				// applies them automatically based on call intent.
				Model: "gemini-embedding-2",
				// 3072 is gemini-embedding-2's native max dimension
				// (Matryoshka — operators can drop to 1536/768 if
				// they want smaller storage at minor quality cost).
				Dim:         3072,
				Concurrency: 8,
				// APIKey intentionally left empty — operators set it
				// in their own config; memmy does not assume an env
				// var name.
			},
		},
		VectorIndex: VectorIndexConfig{
			FlatScanThreshold: 5000,
			HNSW: HNSWParams{
				M:              16,
				M0:             32,
				EfConstruction: 200,
				EfSearch:       100,
				ML:             0.36,
			},
		},
		Memory: MemoryConfig{
			ChunkWindowSize:       3,
			ChunkStride:           2,
			RetrievalK:            8,
			RetrievalHops:         2,
			RetrievalOversample:   300,
			StructuralRecentN:     16,
			StructuralRecentDelta: 5 * time.Minute,
			RefractoryPeriod:      60 * time.Second,
			LogDampening:          true,
			MarkMaxNodes:          256,
			Scoring:               ScoringConfig{SimAlpha: 1.0, WeightBeta: 0.5},
			Decay: DecayConfig{
				NodeLambda:            8.0e-8,
				EdgeStructuralLambda:  4.0e-8,
				EdgeCoRetrievalLambda: 2.7e-7,
				EdgeCoTraversalLambda: 1.3e-7,
			},
			Reinforce: ReinforceConfig{
				NodeDelta:                    1.0,
				EdgeCoRetrievalBase:          0.5,
				EdgeCoTraversalMultiplier:    1.5,
				EdgeStructuralWeight:         1.0,
				EdgeStructuralTemporalWeight: 0.3,
			},
			Prune:     PruneConfig{EdgeFloor: 0.05, NodeFloor: 0.01},
			WeightCap: 100.0,
		},
	}
}

// Load reads and parses a YAML file at path, applying any unspecified
// fields from Default(). Returns a validated Config.
func Load(path string) (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: read %q: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: unmarshal %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports an error if any required field is missing or any
// numeric tunable is outside a sane range.
func (c Config) Validate() error {
	if c.Storage.Backend != "bbolt" {
		return fmt.Errorf("config: unsupported storage backend %q (only bbolt for v1)", c.Storage.Backend)
	}
	if c.Storage.BBolt.Path == "" {
		return errors.New("config: storage.bbolt.path required")
	}
	switch c.Embedder.Backend {
	case "gemini":
		if c.Embedder.Gemini.APIKey == "" {
			return errors.New("config: embedder.gemini.api_key required")
		}
		if c.Embedder.Gemini.Dim < 1 {
			return errors.New("config: embedder.gemini.dim must be >= 1")
		}
	case "fake":
		if c.Embedder.Fake.Dim < 1 {
			return errors.New("config: embedder.fake.dim must be >= 1")
		}
	default:
		return fmt.Errorf("config: unsupported embedder backend %q", c.Embedder.Backend)
	}
	if c.VectorIndex.FlatScanThreshold < 0 {
		return errors.New("config: vector_index.flat_scan_threshold must be >= 0")
	}
	if c.VectorIndex.HNSW.M <= 0 || c.VectorIndex.HNSW.M0 <= 0 || c.VectorIndex.HNSW.EfConstruction <= 0 {
		return errors.New("config: vector_index.hnsw parameters must be > 0")
	}
	if c.Memory.ChunkWindowSize < 1 || c.Memory.ChunkStride < 1 {
		return errors.New("config: chunk window/stride must be >= 1")
	}
	if c.Memory.WeightCap <= 0 {
		return errors.New("config: weight_cap must be > 0")
	}
	if len(c.Server.Transports) == 0 {
		return errors.New("config: server.transports must enable at least one transport")
	}
	var enabledNames []string
	for name, t := range c.Server.Transports {
		if !t.Enabled {
			continue
		}
		enabledNames = append(enabledNames, name)
	}
	if len(enabledNames) == 0 {
		return errors.New("config: at least one server.transports entry must be enabled")
	}
	if err := c.Tenant.Validate(); err != nil {
		return err
	}
	// stdio is mutually exclusive with every other transport. The MCP
	// stdio transport owns the process's stdin/stdout exclusively, so
	// running an HTTP listener alongside makes no sense (and would put
	// log lines on the same stream the JSON-RPC frames travel over).
	if slices.Contains(enabledNames, TransportStdio) && len(enabledNames) > 1 {
		other := make([]string, 0, len(enabledNames)-1)
		for _, n := range enabledNames {
			if n != TransportStdio {
				other = append(other, n)
			}
		}
		// Sort so the diagnostic is stable across runs (Go map
		// iteration order is unspecified).
		sort.Strings(other)
		return fmt.Errorf("config: transport %q is mutually exclusive with all other transports; also enabled: %v", TransportStdio, other)
	}
	for _, name := range enabledNames {
		t := c.Server.Transports[name]
		if name == TransportStdio {
			// stdio has no listen address.
			continue
		}
		if t.Addr == "" {
			return fmt.Errorf("config: enabled transport %q missing addr", name)
		}
	}
	return nil
}

// Transport name constants. The set of known names is closed so the
// validator can reason about mutual exclusivity (stdio).
const (
	TransportMCP   = "mcp"   // streamable HTTP MCP transport
	TransportStdio = "stdio" // MCP over stdin/stdout
	TransportGRPC  = "grpc"  // reserved
	TransportHTTP  = "http"  // reserved
)

// Validate parses regex patterns and cross-checks the tenant schema
// for internal consistency. Returns nil on success; the schema may
// then be handed to service.NewTenantSchema.
func (t TenantSchemaConfig) Validate() error {
	if !t.IsConfigured() {
		return nil
	}
	for name, k := range t.Keys {
		if k.Pattern != "" {
			if _, err := regexp.Compile(k.Pattern); err != nil {
				return fmt.Errorf("config: tenant.keys.%s.pattern: %w", name, err)
			}
		}
	}
	for i, set := range t.OneOf {
		if len(set) == 0 {
			return fmt.Errorf("config: tenant.one_of[%d] is empty", i)
		}
		for _, k := range set {
			if _, ok := t.Keys[k]; !ok {
				return fmt.Errorf("config: tenant.one_of[%d] references undeclared key %q", i, k)
			}
		}
	}
	return nil
}

// EmbedderDim returns the dimensionality of the configured embedder.
func (c Config) EmbedderDim() int {
	switch c.Embedder.Backend {
	case "gemini":
		return c.Embedder.Gemini.Dim
	case "fake":
		return c.Embedder.Fake.Dim
	}
	return 0
}
