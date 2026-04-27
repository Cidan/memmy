// Package service implements the MemoryService — the transport-neutral
// input port (DESIGN.md §9.1). It composes the Embedder, VectorIndex,
// and Graph ports to deliver Write / Recall / Forget / Stats operations
// with lazy decay, Hebbian reinforcement, and weight-aware retrieval.
//
// The service is stateless across calls (DESIGN.md §0 #3): no caches,
// no in-memory accumulators. All state lives in the database.
package service

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/Cidan/memmy/internal/clock"
	"github.com/Cidan/memmy/internal/embed"
	"github.com/Cidan/memmy/internal/graph"
	"github.com/Cidan/memmy/internal/types"
	"github.com/Cidan/memmy/internal/vectorindex"
)

// MemoryService is the transport-neutral application port. All transport
// adapters (MCP, gRPC, HTTP) call into this interface.
type MemoryService interface {
	Write(ctx context.Context, req types.WriteRequest) (types.WriteResult, error)
	Recall(ctx context.Context, req types.RecallRequest) (types.RecallResult, error)
	Forget(ctx context.Context, req types.ForgetRequest) (types.ForgetResult, error)
	Stats(ctx context.Context, req types.StatsRequest) (types.StatsResult, error)
}

// Service is the concrete MemoryService implementation.
type Service struct {
	graph    graph.Graph
	vidx     vectorindex.VectorIndex
	embedder embed.Embedder
	clock    clock.Clock
	cfg      Config
	entropy  *ulid.MonotonicEntropy
}

// Config bundles the tunables for chunking, retrieval, decay, and
// reinforcement. See DESIGN.md §12.
type Config struct {
	// Chunking
	ChunkWindowSize int
	ChunkStride     int

	// Retrieval defaults
	DefaultK           int
	DefaultHops        int
	DefaultOversample  int
	SimAlpha           float64
	WeightBeta         float64
	DepthPenaltyFactor float64 // multiplier per hop, default 2.0 → /2 per depth

	// Decay (per-second)
	NodeLambda             float64
	EdgeStructuralLambda   float64
	EdgeCoRetrievalLambda  float64
	EdgeCoTraversalLambda  float64

	// Reinforcement
	NodeDelta                float64
	EdgeCoRetrievalBase      float64
	EdgeCoTraversalMultiplier float64
	EdgeStructuralWeight     float64
	EdgeStructuralTemporalWeight float64

	// Pruning
	EdgeFloor float64
	NodeFloor float64
	WeightCap float64

	// Structural temporal recency
	StructuralRecentN     int
	StructuralRecentDelta time.Duration
}

// DefaultConfig returns the documented defaults from DESIGN.md §12.
func DefaultConfig() Config {
	return Config{
		ChunkWindowSize:    3,
		ChunkStride:        2,
		DefaultK:           8,
		DefaultHops:        2,
		DefaultOversample:  300,
		SimAlpha:           1.0,
		WeightBeta:         0.5,
		DepthPenaltyFactor: 2.0,

		NodeLambda:            8.0e-8,
		EdgeStructuralLambda:  4.0e-8,
		EdgeCoRetrievalLambda: 2.7e-7,
		EdgeCoTraversalLambda: 1.3e-7,

		NodeDelta:                    1.0,
		EdgeCoRetrievalBase:          0.5,
		EdgeCoTraversalMultiplier:    1.5,
		EdgeStructuralWeight:         1.0,
		EdgeStructuralTemporalWeight: 0.3,

		EdgeFloor: 0.05,
		NodeFloor: 0.01,
		WeightCap: 100.0,

		StructuralRecentN:     16,
		StructuralRecentDelta: 5 * time.Minute,
	}
}

// New constructs a Service. All four ports are required.
func New(g graph.Graph, v vectorindex.VectorIndex, e embed.Embedder, c clock.Clock, cfg Config) (*Service, error) {
	if g == nil || v == nil || e == nil {
		return nil, errors.New("service: graph, vectorindex, and embedder are required")
	}
	if c == nil {
		c = clock.Real{}
	}
	if cfg == (Config{}) {
		cfg = DefaultConfig()
	}
	return &Service{
		graph:    g,
		vidx:     v,
		embedder: e,
		clock:    c,
		cfg:      cfg,
		entropy:  ulid.Monotonic(rand.Reader, 0),
	}, nil
}

// newID generates a ULID using the service's clock for the time bits and
// a process-monotonic entropy source for the random bits. ULIDs are
// lex-sortable AND chronologically sortable, which lets us scan recent
// nodes by time via a simple cursor.
func (s *Service) newID() string {
	t := s.clock.Now()
	id, err := ulid.New(uint64(t.UnixMilli()), s.entropy)
	if err != nil {
		// MonotonicEntropy can fail if the clock goes backwards; in that
		// case we fall back to crypto/rand and the original time.
		id = ulid.MustNew(uint64(t.UnixMilli()), rand.Reader)
	}
	return id.String()
}

// resolveTenant normalizes a tuple to a TenantID and ensures the tenant
// is registered. Returns the canonical TenantID.
func (s *Service) resolveTenant(ctx context.Context, tuple map[string]string) (string, error) {
	if len(tuple) == 0 {
		return "", errors.New("service: tenant tuple is empty")
	}
	id := types.TenantID(tuple)
	// Idempotent upsert; cheap.
	err := s.graph.UpsertTenant(ctx, types.TenantInfo{
		ID:        id,
		Tuple:     tuple,
		CreatedAt: s.clock.Now(),
	})
	return id, err
}

// Compile-time interface conformance check.
var _ MemoryService = (*Service)(nil)
