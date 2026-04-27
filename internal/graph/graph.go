// Package graph defines the Graph port-out interface used by the Memory
// Service to persist nodes, messages, and Hebbian memory edges.
//
// Implementations live in internal/storage/<backend>/. They own the
// nodes, messages, memory_edges_out, and memory_edges_in collections; they
// MUST NOT touch the vectors or hnsw_* collections — those belong to the
// VectorIndex interface (see DESIGN.md §0 #6 / §4.6).
package graph

import (
	"context"
	"errors"

	"github.com/Cidan/memmy/internal/types"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("graph: not found")

// Graph is the persistence port for nodes, messages, and memory edges.
//
// All Update* methods accept a closure: the implementation reads the
// current value, calls the closure with a pointer to it, and writes the
// (possibly mutated) value back, all inside a single backend transaction.
// This is essential for atomic decay+reinforce semantics. If the entity
// does not exist, the closure is not called and ErrNotFound is returned.
type Graph interface {
	PutNode(ctx context.Context, n types.Node) error
	GetNode(ctx context.Context, tenant, id string) (types.Node, error)
	UpdateNode(ctx context.Context, tenant, id string, fn func(*types.Node) error) error
	DeleteNode(ctx context.Context, tenant, id string) error

	PutMessage(ctx context.Context, m types.Message) error
	GetMessage(ctx context.Context, tenant, id string) (types.Message, error)
	DeleteMessage(ctx context.Context, tenant, id string) error

	// PutEdge upserts an edge in both physical directions atomically.
	PutEdge(ctx context.Context, e types.MemoryEdge) error
	// GetEdge returns (edge, true, nil) when present, (zero, false, nil) when absent.
	GetEdge(ctx context.Context, tenant, from, to string) (types.MemoryEdge, bool, error)
	// UpdateEdge applies fn to the existing edge inside one tx; both
	// directions are written back atomically. If the edge does not exist
	// returns ErrNotFound.
	UpdateEdge(ctx context.Context, tenant, from, to string, fn func(*types.MemoryEdge) error) error
	// DeleteEdge removes both directions atomically.
	DeleteEdge(ctx context.Context, tenant, from, to string) error

	// Neighbors returns outbound edges from id (uses memory_edges_out).
	Neighbors(ctx context.Context, tenant, id string) ([]types.MemoryEdge, error)
	// InboundNeighbors returns inbound edges to id (uses memory_edges_in).
	InboundNeighbors(ctx context.Context, tenant, id string) ([]types.MemoryEdge, error)

	// Tenant management
	UpsertTenant(ctx context.Context, info types.TenantInfo) error
	GetTenant(ctx context.Context, id string) (types.TenantInfo, error)
	ListTenants(ctx context.Context) ([]types.TenantInfo, error)

	Close() error
}
