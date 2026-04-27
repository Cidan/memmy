// Package config loads memmy's YAML configuration. Schema mirrors
// DESIGN.md §12.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration loaded at startup.
type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Storage     StorageConfig     `yaml:"storage"`
	Embedder    EmbedderConfig    `yaml:"embedder"`
	VectorIndex VectorIndexConfig `yaml:"vector_index"`
	Memory      MemoryConfig      `yaml:"memory"`
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
	APIKeyEnv   string `yaml:"api_key_env"`
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
func Default() Config {
	return Config{
		Server: ServerConfig{
			Transports: map[string]TransportConfig{
				"mcp": {Enabled: true, Addr: "0.0.0.0:8765"},
			},
		},
		Storage: StorageConfig{
			Backend: "bbolt",
			BBolt:   BBoltStorageConfig{Path: "./data/memmy.db"},
		},
		Embedder: EmbedderConfig{
			Backend: "fake",
			Fake:    FakeEmbedderConfig{Dim: 64},
			Gemini: GeminiConfig{
				Model:       "text-embedding-004",
				APIKeyEnv:   "GEMINI_API_KEY",
				Dim:         768,
				Concurrency: 8,
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
		if c.Embedder.Gemini.APIKeyEnv == "" {
			return errors.New("config: embedder.gemini.api_key_env required")
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
	enabled := false
	for _, t := range c.Server.Transports {
		if t.Enabled {
			enabled = true
			if t.Addr == "" {
				return errors.New("config: enabled transport missing addr")
			}
		}
	}
	if !enabled {
		return errors.New("config: at least one server.transports entry must be enabled")
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
