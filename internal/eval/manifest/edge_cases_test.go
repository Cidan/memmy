package manifest_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Cidan/memmy/internal/eval/manifest"
)

func TestWriteDataset_RejectsEmptyPath(t *testing.T) {
	if err := manifest.WriteDataset("", manifest.DatasetManifest{Name: "x"}); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestReadDataset_MissingFileWrapsPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such.json")
	_, err := manifest.ReadDataset(missing)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected wrapped os.ErrNotExist, got %v", err)
	}
}

// Two writers racing to update the same manifest. The atomic
// tmp+rename used inside WriteDataset means the file should always be
// readable as a complete manifest after both finish — no partial JSON,
// no leftover .tmp under normal completion.
func TestWriteDataset_ConcurrentRacersLeaveValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			if err := manifest.WriteDataset(path, manifest.DatasetManifest{
				Name:        "alpha",
				ChunkCount:  i + 1,
				EmbedderDim: 32,
				UpdatedAt:   time.Unix(int64(1700000000+i), 0).UTC(),
			}); err != nil {
				t.Errorf("WriteDataset %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	got, err := manifest.ReadDataset(path)
	if err != nil {
		t.Fatalf("ReadDataset: %v", err)
	}
	if got.Name != "alpha" || got.SchemaVersion != manifest.SchemaVersion {
		t.Errorf("post-race manifest=%+v", got)
	}

	// No .tmp leftovers after successful writes.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Errorf(".tmp file leaked after successful writes: %s", e.Name())
		}
	}
}

func TestReadDataset_TolersUnknownFutureSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	// Pre-seed a manifest with a future schema_version + an unknown
	// field. The Go decoder ignores unknown fields by default; what we
	// want to confirm is that ReadDataset doesn't reject the file
	// outright on the bumped version.
	body := `{
	  "schema_version": 99,
	  "name": "alpha",
	  "chunk_count": 7,
	  "future_only_field": "ignored",
	  "updated_at": "2026-04-27T12:00:00Z"
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := manifest.ReadDataset(path)
	if err != nil {
		t.Fatalf("ReadDataset: %v", err)
	}
	if got.SchemaVersion != 99 {
		t.Errorf("SchemaVersion=%d, want 99 (preserved)", got.SchemaVersion)
	}
	if got.Name != "alpha" || got.ChunkCount != 7 {
		t.Errorf("decode dropped known fields: %+v", got)
	}
}

func TestWriteRun_RejectsEmptyPath(t *testing.T) {
	if err := manifest.WriteRun("", manifest.RunManifest{RunID: "r"}); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestReadRun_MissingFile(t *testing.T) {
	_, err := manifest.ReadRun(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected wrapped os.ErrNotExist, got %v", err)
	}
}
