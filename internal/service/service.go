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
	Reinforce(ctx context.Context, req types.ReinforceRequest) (types.ReinforceResult, error)
	Demote(ctx context.Context, req types.DemoteRequest) (types.DemoteResult, error)
	Mark(ctx context.Context, req types.MarkRequest) (types.MarkResult, error)
}

// Service is the concrete MemoryService implementation.
type Service struct {
	graph    graph.Graph
	vidx     vectorindex.VectorIndex
	embedder embed.Embedder
	clock    clock.Clock
	cfg      Config
	entropy  *ulid.MonotonicEntropy
	schema   *TenantSchema // nil → accept any tuple
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

	// Explicit-bump throttling (Reinforce/Demote/Mark only).
	// RefractoryPeriod blocks repeated explicit bumps on the same node
	// within the window — the call still updates LastTouched and
	// AccessCount but applies no delta. Implicit Recall co-retrieval
	// bumps are NOT refractory-gated. Set to 0 to disable.
	RefractoryPeriod time.Duration

	// LogDampening makes positive bumps approach WeightCap asymptotically
	// instead of hitting a hard wall: effective_delta = delta * (1 - w/cap).
	// Demote (negative delta) is unaffected. Implicit Recall co-retrieval
	// bumps are NOT log-dampened.
	LogDampening bool

	// MarkMaxNodes caps the number of recent nodes Mark walks per call.
	MarkMaxNodes int
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

		RefractoryPeriod: 60 * time.Second,
		LogDampening:     true,
		MarkMaxNodes:     256,
	}
}

// New constructs a Service. All four ports are required. The schema
// is optional — pass nil to accept any tenant tuple (today's behavior).
func New(g graph.Graph, v vectorindex.VectorIndex, e embed.Embedder, c clock.Clock, cfg Config, schema *TenantSchema) (*Service, error) {
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
		schema:   schema,
	}, nil
}

// Schema returns the active TenantSchema, or nil if none is configured.
// Used by transport adapters to render the schema into wire-level
// input schemas and to surface corrective errors back to clients.
func (s *Service) Schema() *TenantSchema { return s.schema }

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

// resolveTenant validates the tuple against the configured schema (if
// any), normalizes it to a TenantID, and ensures the tenant is
// registered. Returns the canonical TenantID.
func (s *Service) resolveTenant(ctx context.Context, tuple map[string]string) (string, error) {
	if len(tuple) == 0 {
		return "", errors.New("service: tenant tuple is empty")
	}
	if err := s.schema.Validate(tuple); err != nil {
		return "", err
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

// requireValidTenant validates the tuple for read-only paths (Recall,
// Forget, Stats) that don't auto-register tenants. Returns the
// TenantID on success.
func (s *Service) requireValidTenant(tuple map[string]string) (string, error) {
	if len(tuple) == 0 {
		return "", errors.New("service: tenant tuple is empty")
	}
	if err := s.schema.Validate(tuple); err != nil {
		return "", err
	}
	return types.TenantID(tuple), nil
}

// Compile-time interface conformance check.
var _ MemoryService = (*Service)(nil)
