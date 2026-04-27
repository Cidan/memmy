// Package vectorindex defines the VectorIndex port-out interface used by
// the Memory Service to store vectors and run top-N similarity search.
//
// Implementations live in internal/storage/<backend>/. They own the
// vectors and hnsw_* collections; they MUST NOT load the entire index
// into memory. Per-query memory must be bounded by query parameters.
package vectorindex

import (
	"context"
	"errors"
)

// ErrNotFound is returned when a requested vector does not exist.
var ErrNotFound = errors.New("vectorindex: not found")

// Hit is one result from Search.
type Hit struct {
	NodeID string
	// Sim is cosine similarity in [-1, 1]. With L2-normalized vectors
	// (the contract) it is essentially the dot product.
	Sim float64
}

// VectorIndex stores vectors and provides top-N similarity search.
//
// Implementations MUST L2-normalize vectors at write time, store them as
// raw little-endian float32 bytes, and never load the full corpus into
// memory. See DESIGN.md §4.8 / §6.
type VectorIndex interface {
	// Insert adds (or replaces) the vector for nodeID. The vector must
	// match the configured dimensionality. Insertion includes adding the
	// node to the HNSW navigation graph (if HNSW is the active mode).
	Insert(ctx context.Context, tenant, nodeID string, vec []float32) error
	// Delete tombstones the node — search results will skip it. Hard-
	// delete cleanup is performed at the storage layer's discretion.
	Delete(ctx context.Context, tenant, nodeID string) error
	// Search returns the top-n hits by similarity for tenant.
	Search(ctx context.Context, tenant string, qVec []float32, n int) ([]Hit, error)
	// Size returns the number of non-tombstoned vectors in the tenant.
	Size(ctx context.Context, tenant string) (int, error)
	// Dim returns the configured vector dimensionality.
	Dim() int
	Close() error
}
