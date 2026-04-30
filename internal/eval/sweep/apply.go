package sweep

import (
	"encoding/json"
	"fmt"
	"maps"

	"github.com/Cidan/memmy"
)

// ApplyServiceOverrides starts from a base ServiceConfig and applies
// the overrides map by JSON-marshalling the base, merging the
// overrides into the resulting object, then unmarshalling back. This
// keeps the sweep YAML free of typed knowledge of every field while
// still type-checking via the ServiceConfig schema on the way back.
func ApplyServiceOverrides(base memmy.ServiceConfig, overrides map[string]any) (memmy.ServiceConfig, error) {
	if len(overrides) == 0 {
		return base, nil
	}
	raw, err := json.Marshal(base)
	if err != nil {
		return memmy.ServiceConfig{}, fmt.Errorf("sweep: marshal base: %w", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		return memmy.ServiceConfig{}, fmt.Errorf("sweep: unmarshal base: %w", err)
	}
	maps.Copy(asMap, overrides)
	merged, err := json.Marshal(asMap)
	if err != nil {
		return memmy.ServiceConfig{}, fmt.Errorf("sweep: marshal merged: %w", err)
	}
	out := base
	if err := json.Unmarshal(merged, &out); err != nil {
		return memmy.ServiceConfig{}, fmt.Errorf("sweep: unmarshal merged: %w", err)
	}
	return out, nil
}

// ApplyHNSWOverrides is the HNSW-config equivalent of ApplyServiceOverrides.
func ApplyHNSWOverrides(base memmy.HNSWConfig, overrides map[string]any) (memmy.HNSWConfig, error) {
	if len(overrides) == 0 {
		return base, nil
	}
	raw, err := json.Marshal(base)
	if err != nil {
		return memmy.HNSWConfig{}, fmt.Errorf("sweep: marshal hnsw: %w", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		return memmy.HNSWConfig{}, fmt.Errorf("sweep: unmarshal hnsw: %w", err)
	}
	maps.Copy(asMap, overrides)
	merged, err := json.Marshal(asMap)
	if err != nil {
		return memmy.HNSWConfig{}, fmt.Errorf("sweep: marshal merged hnsw: %w", err)
	}
	out := base
	if err := json.Unmarshal(merged, &out); err != nil {
		return memmy.HNSWConfig{}, fmt.Errorf("sweep: unmarshal merged hnsw: %w", err)
	}
	return out, nil
}
