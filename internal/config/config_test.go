package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Cidan/memmy/internal/config"
)

func TestConfig_LoadDefaultsAndOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memmy.yaml")
	contents := `
server:
  transports:
    mcp:
      enabled: true
      addr: "127.0.0.1:9999"
storage:
  backend: bbolt
  bbolt:
    path: "/tmp/memmy.db"
embedder:
  backend: fake
  fake:
    dim: 32
memory:
  chunk_window_size: 3
  chunk_stride: 2
  weight_cap: 100.0
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Transports["mcp"].Addr != "127.0.0.1:9999" {
		t.Fatalf("addr=%q", cfg.Server.Transports["mcp"].Addr)
	}
	if cfg.Storage.BBolt.Path != "/tmp/memmy.db" {
		t.Fatalf("path=%q", cfg.Storage.BBolt.Path)
	}
	if cfg.Embedder.Backend != "fake" || cfg.EmbedderDim() != 32 {
		t.Fatalf("embedder %+v", cfg.Embedder)
	}
	// Defaults preserved where not overridden.
	if cfg.VectorIndex.HNSW.M != 16 {
		t.Fatalf("HNSW.M=%d, want 16 (default)", cfg.VectorIndex.HNSW.M)
	}
}

func TestConfig_RejectsUnknownStorage(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Backend = "mongodb"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unknown storage backend")
	}
}

func TestConfig_RejectsNoTransport(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Transports = map[string]config.TransportConfig{
		"mcp": {Enabled: false, Addr: ""},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when no transport enabled")
	}
}

func TestConfig_RejectsZeroDim(t *testing.T) {
	cfg := config.Default()
	cfg.Embedder.Fake.Dim = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for fake.dim == 0")
	}
}
