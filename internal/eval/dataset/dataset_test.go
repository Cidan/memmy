package dataset_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cidan/memmy/internal/eval/dataset"
)

func TestOpen_CreatesLayout(t *testing.T) {
	root := t.TempDir()
	d, err := dataset.Open(root, "alpha")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if d.Name != "alpha" {
		t.Errorf("Name=%q, want alpha", d.Name)
	}
	if d.Root != filepath.Join(root, "alpha") {
		t.Errorf("Root=%q, want %q", d.Root, filepath.Join(root, "alpha"))
	}
	if _, err := os.Stat(filepath.Join(root, "alpha", "runs")); err != nil {
		t.Errorf("runs dir not created: %v", err)
	}
}

func TestOpen_IsIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := dataset.Open(root, "alpha"); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	manifest := filepath.Join(root, "alpha", "manifest.json")
	if err := os.WriteFile(manifest, []byte(`{"chunk_count":42}`), 0o600); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	if _, err := dataset.Open(root, "alpha"); err != nil {
		t.Fatalf("second Open: %v", err)
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var got struct{ ChunkCount int `json:"chunk_count"` }
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ChunkCount != 42 {
		t.Errorf("manifest clobbered: got chunk_count=%d, want 42", got.ChunkCount)
	}
}

func TestOpen_RejectsBadName(t *testing.T) {
	root := t.TempDir()
	bad := []string{"", "..", ".", "../escape", "with/slash", ".hidden"}
	for _, name := range bad {
		if _, err := dataset.Open(root, name); err == nil {
			t.Errorf("expected error for name %q", name)
		}
	}
}

func TestRunDir_CreatesAndReturns(t *testing.T) {
	root := t.TempDir()
	d, err := dataset.Open(root, "alpha")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := d.RunDir("run-001")
	if err != nil {
		t.Fatalf("RunDir: %v", err)
	}
	if got != filepath.Join(root, "alpha", "runs", "run-001") {
		t.Errorf("RunDir path mismatch: %q", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("run dir not created: %v", err)
	}
}

func TestListDatasets_NonexistentRoot(t *testing.T) {
	got, err := dataset.ListDatasets(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for missing root", got)
	}
}

func TestListDatasets_WalksAndFillsStats(t *testing.T) {
	root := t.TempDir()
	d, err := dataset.Open(root, "alpha")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	manifest := []byte(`{"chunk_count":7,"query_count":3,"updated_at":"2026-01-02T03:04:05Z"}`)
	if err := os.WriteFile(d.ManifestPath(), manifest, 0o600); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	if err := os.WriteFile(d.CorpusDBPath(), []byte("not really a sqlite file"), 0o600); err != nil {
		t.Fatalf("seed corpus: %v", err)
	}
	if _, err := d.RunDir("run-A"); err != nil {
		t.Fatalf("RunDir: %v", err)
	}
	if _, err := dataset.Open(root, "beta"); err != nil {
		t.Fatalf("Open beta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "beta", "manifest.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("seed beta manifest: %v", err)
	}

	stats, err := dataset.ListDatasets(root)
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("got %d datasets, want 2", len(stats))
	}
	a := stats[0]
	if a.Name != "alpha" {
		t.Fatalf("first dataset Name=%q, want alpha", a.Name)
	}
	if a.ChunkCount != 7 || a.QueryCount != 3 {
		t.Errorf("counts: got chunks=%d queries=%d", a.ChunkCount, a.QueryCount)
	}
	if !a.HasCorpus {
		t.Errorf("HasCorpus=false, want true")
	}
	if a.RunCount != 1 {
		t.Errorf("RunCount=%d, want 1", a.RunCount)
	}
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if !a.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt=%s, want %s", a.UpdatedAt, want)
	}
}

func TestDefaultRoot_RespectsEnvOverride(t *testing.T) {
	t.Setenv(dataset.EnvHome, "/tmp/memmy-eval-test-override")
	got, err := dataset.DefaultRoot()
	if err != nil {
		t.Fatalf("DefaultRoot: %v", err)
	}
	if got != "/tmp/memmy-eval-test-override" {
		t.Errorf("DefaultRoot=%q, want override", got)
	}
}
