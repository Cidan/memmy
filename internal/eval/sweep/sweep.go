// Package sweep runs an eval harness over a matrix of memmy configs
// to find a parameter setting that scores best on a labeled query set.
//
// A sweep YAML file looks like:
//
//	base: configs/baseline.yaml      # optional path
//	matrix:
//	  - name: low-decay
//	    overrides:
//	      NodeLambda: 4.0e-8
//	  - name: high-reinforce
//	    overrides:
//	      NodeDelta: 2.5
//
// Each entry produces a fresh memmy db, runs the same query battery,
// and writes per-config metrics under runs/<sweep_id>/<config_name>/.
package sweep

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Matrix is the loaded sweep YAML.
type Matrix struct {
	Base    string  `yaml:"base"`
	Entries []Entry `yaml:"matrix"`
}

// Entry is one configuration to sweep over. Overrides are applied
// over the loaded ServiceConfig via sweep.ApplyServiceOverrides.
type Entry struct {
	Name      string         `yaml:"name"`
	Overrides map[string]any `yaml:"overrides"`
}

// Load reads a sweep YAML.
func Load(path string) (Matrix, error) {
	if path == "" {
		return Matrix{}, errors.New("sweep: path required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Matrix{}, fmt.Errorf("sweep: read %q: %w", path, err)
	}
	var m Matrix
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return Matrix{}, fmt.Errorf("sweep: parse %q: %w", path, err)
	}
	if len(m.Entries) == 0 {
		return Matrix{}, fmt.Errorf("sweep: %q has no matrix entries", path)
	}
	for i, e := range m.Entries {
		if e.Name == "" {
			return Matrix{}, fmt.Errorf("sweep: entry %d missing name", i)
		}
	}
	return m, nil
}
