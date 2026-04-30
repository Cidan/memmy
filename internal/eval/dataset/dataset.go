// Package dataset owns the on-disk layout for memmy-eval datasets.
//
// One dataset = one directory under MEMMY_EVAL_HOME (default
// ~/.local/share/memmy-eval/<name>/) containing:
//
//	manifest.json    — DatasetManifest written by ingest
//	corpus.sqlite    — chunks + embeddings (embedcache schema)
//	queries.sqlite   — labeled queries (queries package schema)
//	runs/<run_id>/   — per-run config + metrics + result rows
package dataset

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// EnvHome overrides the dataset root. Empty -> ~/.local/share/memmy-eval.
const EnvHome = "MEMMY_EVAL_HOME"

// DefaultRoot returns the directory where datasets live by default.
// Honors EnvHome before falling back to the XDG-conventional location.
func DefaultRoot() (string, error) {
	if v := strings.TrimSpace(os.Getenv(EnvHome)); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("dataset: resolve home: %w", err)
	}
	return filepath.Join(home, ".local", "share", "memmy-eval"), nil
}

// Dataset is a handle to one dataset directory. It is a value type — no
// resources are held open, so there is no Close.
type Dataset struct {
	Name string
	Root string // the dataset directory (root/<name>)
}

// Open creates or returns a handle to the named dataset under root.
// If root is empty, DefaultRoot() is used. The directory tree is
// created idempotently; an existing manifest is preserved.
func Open(root, name string) (*Dataset, error) {
	if name == "" {
		return nil, errors.New("dataset: name is required")
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	if root == "" {
		r, err := DefaultRoot()
		if err != nil {
			return nil, err
		}
		root = r
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o700); err != nil {
		return nil, fmt.Errorf("dataset: mkdir %q: %w", dir, err)
	}
	return &Dataset{Name: name, Root: dir}, nil
}

// CorpusDBPath returns the absolute path to corpus.sqlite.
func (d *Dataset) CorpusDBPath() string { return filepath.Join(d.Root, "corpus.sqlite") }

// QueriesDBPath returns the absolute path to queries.sqlite.
func (d *Dataset) QueriesDBPath() string { return filepath.Join(d.Root, "queries.sqlite") }

// ManifestPath returns the absolute path to manifest.json.
func (d *Dataset) ManifestPath() string { return filepath.Join(d.Root, "manifest.json") }

// RunsDir returns the absolute path to the per-run output directory.
func (d *Dataset) RunsDir() string { return filepath.Join(d.Root, "runs") }

// RunDir returns runs/<runID>, creating it if missing.
func (d *Dataset) RunDir(runID string) (string, error) {
	if runID == "" {
		return "", errors.New("dataset: runID required")
	}
	p := filepath.Join(d.Root, "runs", runID)
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", fmt.Errorf("dataset: mkdir run %q: %w", p, err)
	}
	return p, nil
}

// Stats summarizes a dataset for the `ls` subcommand. Counts come from
// the on-disk JSON manifest plus best-effort directory walks.
type Stats struct {
	Name        string
	Root        string
	HasCorpus   bool
	HasQueries  bool
	ChunkCount  int
	QueryCount  int
	RunCount    int
	LastRunTime time.Time // zero when no runs
	UpdatedAt   time.Time // from manifest
}

// ListDatasets walks root and returns per-dataset stats.
// A dataset is "any direct child directory containing manifest.json."
// Counts that need a SQLite query are read from the manifest, not the
// db file, so this stays cheap on cold startup.
func ListDatasets(root string) ([]Stats, error) {
	if root == "" {
		r, err := DefaultRoot()
		if err != nil {
			return nil, err
		}
		root = r
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("dataset: list %q: %w", root, err)
	}
	var out []Stats
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		manifest := filepath.Join(dir, "manifest.json")
		if _, err := os.Stat(manifest); err != nil {
			continue
		}
		s := Stats{Name: e.Name(), Root: dir}
		if mf, err := readDatasetManifest(manifest); err == nil {
			s.ChunkCount = mf.ChunkCount
			s.QueryCount = mf.QueryCount
			s.UpdatedAt = mf.UpdatedAt
		}
		if st, err := os.Stat(filepath.Join(dir, "corpus.sqlite")); err == nil && !st.IsDir() {
			s.HasCorpus = true
		}
		if st, err := os.Stat(filepath.Join(dir, "queries.sqlite")); err == nil && !st.IsDir() {
			s.HasQueries = true
		}
		runs, err := os.ReadDir(filepath.Join(dir, "runs"))
		if err == nil {
			for _, r := range runs {
				if !r.IsDir() {
					continue
				}
				s.RunCount++
				info, err := r.Info()
				if err == nil && info.ModTime().After(s.LastRunTime) {
					s.LastRunTime = info.ModTime()
				}
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// validateName rejects names that would escape the dataset root or
// collide with hidden filesystem entries.
func validateName(name string) error {
	if strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("dataset: name %q contains a path separator", name)
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return fmt.Errorf("dataset: name %q is reserved", name)
	}
	return nil
}

// readDatasetManifest loads the slim subset needed by ListDatasets.
// Returning a typed error here would force every caller to import the
// manifest package, so we use a private mirror struct.
func readDatasetManifest(path string) (datasetManifestSlim, error) {
	var out datasetManifestSlim
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(raw, &out)
}

type datasetManifestSlim struct {
	ChunkCount int       `json:"chunk_count"`
	QueryCount int       `json:"query_count"`
	UpdatedAt  time.Time `json:"updated_at"`
}
