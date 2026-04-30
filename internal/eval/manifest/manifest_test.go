package manifest_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cidan/memmy/internal/eval/manifest"
)

func TestDatasetManifest_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	in := manifest.DatasetManifest{
		Name:               "alpha",
		SessionsSourcePath: "/tmp/sessions",
		EmbedderModel:      "fake-32",
		EmbedderDim:        32,
		EmbedderKind:       "fake",
		ChunkCount:         42,
		QueryCount:         7,
		CorpusSnapshotHash: "abc123",
		CreatedAt:          time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC),
		UpdatedAt:          time.Date(2026, 4, 27, 13, 0, 0, 0, time.UTC),
		Extra:              map[string]string{"note": "hello"},
	}
	if err := manifest.WriteDataset(path, in); err != nil {
		t.Fatalf("WriteDataset: %v", err)
	}
	out, err := manifest.ReadDataset(path)
	if err != nil {
		t.Fatalf("ReadDataset: %v", err)
	}
	if out.SchemaVersion != manifest.SchemaVersion {
		t.Errorf("SchemaVersion=%d, want %d", out.SchemaVersion, manifest.SchemaVersion)
	}
	if out.Name != in.Name || out.ChunkCount != in.ChunkCount || out.EmbedderDim != in.EmbedderDim {
		t.Errorf("round-trip drift: in=%+v out=%+v", in, out)
	}
	if !out.CreatedAt.Equal(in.CreatedAt) {
		t.Errorf("CreatedAt drift: %v vs %v", in.CreatedAt, out.CreatedAt)
	}
	if out.Extra["note"] != "hello" {
		t.Errorf("Extra dropped: %v", out.Extra)
	}
}

func TestRunManifest_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.json")
	cfgJSON := json.RawMessage(`{"NodeDelta": 1.0}`)
	in := manifest.RunManifest{
		RunID:              "run-001",
		DatasetName:        "alpha",
		StartedAt:          time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC),
		FinishedAt:         time.Date(2026, 4, 27, 12, 0, 5, 0, time.UTC),
		MemmyGitSHA:        "deadbeef",
		ConfigPath:         "configs/baseline.yaml",
		ServiceConfigJSON:  cfgJSON,
		FlatScanThreshold:  100000,
		QueriesExecuted:    25,
		CorpusSnapshotHash: "abc123",
	}
	if err := manifest.WriteRun(path, in); err != nil {
		t.Fatalf("WriteRun: %v", err)
	}
	out, err := manifest.ReadRun(path)
	if err != nil {
		t.Fatalf("ReadRun: %v", err)
	}
	if out.RunID != in.RunID || out.QueriesExecuted != in.QueriesExecuted {
		t.Errorf("round-trip drift: %+v", out)
	}
	var got, want map[string]any
	if err := json.Unmarshal(out.ServiceConfigJSON, &got); err != nil {
		t.Fatalf("decode out ServiceConfigJSON: %v", err)
	}
	if err := json.Unmarshal(cfgJSON, &want); err != nil {
		t.Fatalf("decode want ServiceConfigJSON: %v", err)
	}
	if got["NodeDelta"] != want["NodeDelta"] {
		t.Errorf("ServiceConfigJSON drift: got=%v want=%v", got, want)
	}
}

func TestMemmyGitSHA_NotEmpty(t *testing.T) {
	got := manifest.MemmyGitSHA()
	if got == "" {
		t.Error("MemmyGitSHA returned empty string")
	}
}
