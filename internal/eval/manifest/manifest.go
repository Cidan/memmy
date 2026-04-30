// Package manifest defines the on-disk metadata records that document
// what produced an eval dataset and what an eval run actually exercised.
//
// Two kinds:
//
//	DatasetManifest  — written by ingest into <dataset>/manifest.json
//	RunManifest      — written by run/sweep into runs/<id>/manifest.json
//
// Both are JSON-encoded value types with a SchemaVersion field so future
// readers can detect format changes.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
)

// SchemaVersion bumps when on-disk shape changes incompatibly.
const SchemaVersion = 1

// DatasetManifest documents the corpus an `ingest` run produced.
type DatasetManifest struct {
	SchemaVersion      int               `json:"schema_version"`
	Name               string            `json:"name"`
	SessionsSourcePath string            `json:"sessions_source_path"`
	EmbedderModel      string            `json:"embedder_model"`
	EmbedderDim        int               `json:"embedder_dim"`
	EmbedderKind       string            `json:"embedder_kind"` // fake | gemini
	ChunkCount         int               `json:"chunk_count"`
	QueryCount         int               `json:"query_count"`
	CorpusSnapshotHash string            `json:"corpus_snapshot_hash"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	Extra              map[string]string `json:"extra,omitempty"`
}

// RunManifest documents one `run` invocation's parameters and provenance.
type RunManifest struct {
	SchemaVersion      int               `json:"schema_version"`
	RunID              string            `json:"run_id"`
	DatasetName        string            `json:"dataset_name"`
	StartedAt          time.Time         `json:"started_at"`
	FinishedAt         time.Time         `json:"finished_at"`
	MemmyGitSHA        string            `json:"memmy_git_sha"`
	ConfigPath         string            `json:"config_path,omitempty"`
	ServiceConfigJSON  json.RawMessage   `json:"service_config_json,omitempty"`
	FlatScanThreshold  int               `json:"flat_scan_threshold,omitempty"`
	QueriesExecuted    int               `json:"queries_executed"`
	CorpusSnapshotHash string            `json:"corpus_snapshot_hash,omitempty"`
	Extra              map[string]string `json:"extra,omitempty"`
}

// WriteDataset writes a DatasetManifest to path atomically.
func WriteDataset(path string, m DatasetManifest) error {
	if m.SchemaVersion == 0 {
		m.SchemaVersion = SchemaVersion
	}
	return writeJSON(path, m)
}

// ReadDataset reads a DatasetManifest from path.
func ReadDataset(path string) (DatasetManifest, error) {
	var m DatasetManifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("manifest: read %q: %w", path, err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("manifest: decode %q: %w", path, err)
	}
	return m, nil
}

// WriteRun writes a RunManifest to path atomically.
func WriteRun(path string, m RunManifest) error {
	if m.SchemaVersion == 0 {
		m.SchemaVersion = SchemaVersion
	}
	return writeJSON(path, m)
}

// ReadRun reads a RunManifest from path.
func ReadRun(path string) (RunManifest, error) {
	var m RunManifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("manifest: read %q: %w", path, err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("manifest: decode %q: %w", path, err)
	}
	return m, nil
}

// MemmyGitSHA returns the vcs.revision recorded by `go build` if
// available, or "unknown" when running under `go test` / `go run` where
// the VCS stamp is intentionally suppressed.
func MemmyGitSHA() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return "unknown"
}

func writeJSON(path string, v any) error {
	if path == "" {
		return errors.New("manifest: path is required")
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("manifest: marshal: %w", err)
	}
	// Per-call unique tmp filename so concurrent writers in the same
	// directory don't trample each other's rename.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("manifest: create tmp: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("manifest: write tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("manifest: close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("manifest: rename: %w", err)
	}
	return nil
}
