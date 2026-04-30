package sweep_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Cidan/memmy"
	"github.com/Cidan/memmy/internal/eval/sweep"
)

const sampleYAML = `
matrix:
  - name: low-decay
    overrides:
      NodeLambda: 4.0e-8
      NodeDelta: 1.5
  - name: high-reinforce
    overrides:
      NodeDelta: 2.5
    hnsw:
      EfSearch: 64
`

func TestLoad_AndOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sweep.yaml")
	if err := os.WriteFile(path, []byte(sampleYAML), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := sweep.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("Entries=%d, want 2", len(m.Entries))
	}
	if m.Entries[0].Name != "low-decay" {
		t.Errorf("Entry[0].Name=%q", m.Entries[0].Name)
	}

	base := memmy.DefaultServiceConfig()
	got, err := sweep.ApplyServiceOverrides(base, m.Entries[0].Overrides)
	if err != nil {
		t.Fatalf("ApplyServiceOverrides: %v", err)
	}
	if got.NodeDelta != 1.5 {
		t.Errorf("NodeDelta=%v, want 1.5", got.NodeDelta)
	}
	if got.NodeLambda != 4.0e-8 {
		t.Errorf("NodeLambda=%v, want 4e-8", got.NodeLambda)
	}
	// Untouched field carried over from base.
	if got.WeightCap != base.WeightCap {
		t.Errorf("WeightCap=%v, want untouched %v", got.WeightCap, base.WeightCap)
	}

	hnswBase := memmy.DefaultHNSWConfig()
	hnsw, err := sweep.ApplyHNSWOverrides(hnswBase, m.Entries[1].HNSW)
	if err != nil {
		t.Fatalf("ApplyHNSWOverrides: %v", err)
	}
	if hnsw.EfSearch != 64 {
		t.Errorf("EfSearch=%v, want 64", hnsw.EfSearch)
	}
	if hnsw.M != hnswBase.M {
		t.Errorf("M=%v, want untouched %v", hnsw.M, hnswBase.M)
	}
}

func TestLoad_RejectsMissingMatrix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(path, []byte("matrix: []"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := sweep.Load(path); err == nil {
		t.Error("expected error for empty matrix")
	}
}
