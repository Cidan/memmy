// Package memmy is the embeddable library facade for the memmy LLM
// memory system. It is the only stable public surface; everything below
// it lives under internal/ and may change without notice.
//
// Typical use:
//
//	emb := memmy.NewFakeEmbedder(64) // or NewGeminiEmbedder(ctx, opts)
//	svc, closer, err := memmy.Open(memmy.Options{
//	    DBPath:   "/var/lib/myapp/memmy.db",
//	    Embedder: emb,
//	})
//	if err != nil { ... }
//	defer closer.Close()
//
//	res, err := svc.Write(ctx, memmy.WriteRequest{
//	    Tenant:  map[string]string{"user": "alice"},
//	    Message: "the quick brown fox",
//	})
//
// The facade mirrors cmd/memmy/main.go's wiring (storage + embedder +
// service) without any transport layer — importers call MemoryService
// methods directly. See DESIGN.md §0 ("Transport adapters wrap a single
// MemoryService") for the architectural rationale.
package memmy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Cidan/memmy/internal/clock"
	"github.com/Cidan/memmy/internal/config"
	"github.com/Cidan/memmy/internal/embed"
	"github.com/Cidan/memmy/internal/embed/fake"
	"github.com/Cidan/memmy/internal/embed/gemini"
	"github.com/Cidan/memmy/internal/service"
	bboltstore "github.com/Cidan/memmy/internal/storage/bbolt"
	"github.com/Cidan/memmy/internal/types"
)

// Service is the transport-neutral memory API (DESIGN.md §9.1).
// All operations take a tenant tuple; the underlying TenantID is derived
// deterministically from the (validated) tuple.
type Service = service.MemoryService

// Request and result value types — direct re-exports of the wire-neutral
// shapes defined in internal/types.
type (
	WriteRequest     = types.WriteRequest
	WriteResult      = types.WriteResult
	RecallRequest    = types.RecallRequest
	RecallResult     = types.RecallResult
	RecallHit        = types.RecallHit
	ScoreBreakdown   = types.ScoreBreakdown
	ForgetRequest    = types.ForgetRequest
	ForgetResult     = types.ForgetResult
	StatsRequest     = types.StatsRequest
	StatsResult      = types.StatsResult
	ReinforceRequest = types.ReinforceRequest
	ReinforceResult  = types.ReinforceResult
	DemoteRequest    = types.DemoteRequest
	DemoteResult     = types.DemoteResult
	MarkRequest      = types.MarkRequest
	MarkResult       = types.MarkResult
	EdgeKind         = types.EdgeKind
)

// EdgeKind values. See DESIGN.md §4.3.
const (
	EdgeStructural  = types.EdgeStructural
	EdgeCoRetrieval = types.EdgeCoRetrieval
	EdgeCoTraversal = types.EdgeCoTraversal
)

// Embedder is the pluggable embedding-provider interface.
// Construct one with NewFakeEmbedder / NewGeminiEmbedder, or supply a
// custom implementation that conforms to the Embed/Dim contract.
type Embedder = embed.Embedder

// EmbedTask classifies how the embedded text will be used. memmy itself
// only emits RetrievalDocument (Write) and RetrievalQuery (Recall);
// the other values are reserved for callers passing their own embedder.
type EmbedTask = embed.EmbedTask

// EmbedTask constants.
const (
	EmbedTaskUnspecified        = embed.EmbedTaskUnspecified
	EmbedTaskRetrievalDocument  = embed.EmbedTaskRetrievalDocument
	EmbedTaskRetrievalQuery     = embed.EmbedTaskRetrievalQuery
	EmbedTaskSemanticSimilarity = embed.EmbedTaskSemanticSimilarity
	EmbedTaskClassification     = embed.EmbedTaskClassification
	EmbedTaskClustering         = embed.EmbedTaskClustering
	EmbedTaskCodeRetrievalQuery = embed.EmbedTaskCodeRetrievalQuery
	EmbedTaskQuestionAnswering  = embed.EmbedTaskQuestionAnswering
	EmbedTaskFactVerification   = embed.EmbedTaskFactVerification
)

// NewFakeEmbedder returns a deterministic test embedder of the given
// dimensionality. Equal inputs always produce equal vectors. Use in
// tests; do not use in production.
func NewFakeEmbedder(dim int) Embedder { return fake.New(dim) }

// GeminiEmbedderOptions configures NewGeminiEmbedder.
type GeminiEmbedderOptions = gemini.Options

// NewGeminiEmbedder constructs a Gemini-backed Embedder. It dials the
// Gemini API immediately to validate credentials.
func NewGeminiEmbedder(ctx context.Context, opts GeminiEmbedderOptions) (Embedder, error) {
	return gemini.New(ctx, opts)
}

// Clock abstracts time so tests can advance it deterministically.
// Real{} is the production wall-clock implementation; tests should pass
// a *FakeClock from NewFakeClock.
type Clock = clock.Clock

// RealClock is the production wall-clock implementation of Clock.
type RealClock = clock.Real

// FakeClock is a controllable Clock for tests.
type FakeClock = clock.Fake

// NewFakeClock returns a *FakeClock initialized to t.
func NewFakeClock(t time.Time) *FakeClock { return clock.NewFake(t) }

// ServiceConfig bundles the runtime tunables for chunking, retrieval,
// decay, reinforcement, and pruning. See DESIGN.md §12.
//
// Options.ServiceConfig is a *ServiceConfig: nil means "use
// DefaultServiceConfig()", non-nil means "use exactly this struct."
// To override a subset of fields, start from DefaultServiceConfig()
// and mutate the returned struct before taking its address. The facade
// deliberately does NOT do field-by-field merging because some fields
// (RefractoryPeriod, LogDampening) accept zero as an intentional
// "disable" signal that a merge-on-zero rule would silently override.
type ServiceConfig = service.Config

// DefaultServiceConfig returns the documented service-tunable defaults.
func DefaultServiceConfig() ServiceConfig { return service.DefaultConfig() }

// TenantSchema validates incoming tenant tuples. Construct one via
// NewTenantSchema, or pass nil to Options.TenantSchema to accept any
// tuple shape.
type TenantSchema = service.TenantSchema

// TenantSchemaConfig describes the shape of a valid tenant tuple in the
// same shape the daemon accepts via YAML. Library callers usually
// construct it programmatically:
//
//	cfg := memmy.TenantSchemaConfig{
//	    Description: "single-user agent",
//	    Keys: map[string]memmy.TenantKeyConfig{
//	        "user":  {Required: true, Pattern: `^[a-zA-Z0-9_.-]+$`},
//	        "scope": {Enum: []string{"chat", "code"}},
//	    },
//	}
type TenantSchemaConfig = config.TenantSchemaConfig

// TenantKeyConfig is one declared key in a TenantSchemaConfig.
type TenantKeyConfig = config.TenantKeyConfig

// ErrTenantInvalid is the typed error returned when a tuple is rejected
// by the configured TenantSchema. Callers can errors.As() it to surface
// a corrective payload back to the originating client.
type ErrTenantInvalid = service.ErrTenantInvalid

// NewTenantSchema compiles a TenantSchema from a config-shaped value.
// Returns (nil, nil) when cfg has no rules — the caller can pass that
// nil straight into Options.TenantSchema to mean "accept any tuple."
func NewTenantSchema(cfg TenantSchemaConfig) (*TenantSchema, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return service.NewTenantSchemaFromConfig(cfg)
}

// HNSWConfig holds the index hyperparameters for the bbolt VectorIndex
// backend. See DESIGN.md §12.
//
// NOTE: this type is currently re-exported directly from
// internal/storage/bbolt and is therefore bbolt-specific. When a second
// storage backend ships, this re-export will need to be abstracted (or
// the field on Options renamed to make the coupling explicit).
type HNSWConfig = bboltstore.HNSWConfig

// DefaultHNSWConfig returns the documented HNSW defaults.
func DefaultHNSWConfig() HNSWConfig { return bboltstore.DefaultHNSWConfig() }

// Options configures Open. DBPath and Embedder are required; everything
// else has a sensible zero-value default.
type Options struct {
	// DBPath is the bbolt database file path. The directory is created
	// if absent. A leading "~/" is expanded to the current user's
	// home directory. Required.
	DBPath string

	// Embedder produces vectors for Write inputs and Recall queries.
	// Construct via NewFakeEmbedder or NewGeminiEmbedder, or supply
	// your own implementation. Required.
	Embedder Embedder

	// Clock is the time source for decay and reinforcement math.
	// Optional; nil means RealClock{}.
	Clock Clock

	// ServiceConfig overrides chunking / retrieval / decay / reinforcement
	// tunables. nil → use DefaultServiceConfig(). Non-nil is treated as
	// the complete config — no field-by-field merging is performed (see
	// the ServiceConfig type doc for why). To override a subset:
	//
	//	cfg := memmy.DefaultServiceConfig()
	//	cfg.NodeDelta = 2.0
	//	opts.ServiceConfig = &cfg
	ServiceConfig *ServiceConfig

	// TenantSchema validates tenant tuples on every operation. Optional;
	// nil accepts any tuple shape (today's daemon default).
	TenantSchema *TenantSchema

	// HNSW configures the per-tenant HNSW index hyperparameters. nil →
	// use DefaultHNSWConfig(). Non-nil is treated as the complete config
	// — partial overrides must be built from DefaultHNSWConfig() the
	// same way ServiceConfig does it.
	HNSW *HNSWConfig

	// FlatScanThreshold is the per-tenant size below which Recall uses a
	// linear scan instead of HNSW. Optional; 0 → 5000 (DESIGN.md §6.1).
	FlatScanThreshold int

	// OpenTimeout caps how long bbolt waits for the file lock during
	// Open. 0 blocks indefinitely.
	OpenTimeout time.Duration

	// HNSWRandSeed seeds the HNSW layer-assignment RNG. 0 → time-derived
	// (production). Tests should pass a fixed seed for determinism.
	HNSWRandSeed uint64
}

// Open constructs a Service backed by bbolt at opts.DBPath. The returned
// io.Closer must be invoked at shutdown to release the database file
// lock. The Embedder's lifecycle is the caller's responsibility — Open
// does NOT close it.
//
// Open does not start any transport (MCP / gRPC / HTTP); callers drive
// the returned Service directly. To run a transport, use the cmd/memmy
// binary with a YAML config instead.
func Open(opts Options) (Service, io.Closer, error) {
	if opts.DBPath == "" {
		return nil, nil, errors.New("memmy: Options.DBPath is required")
	}
	if opts.Embedder == nil {
		return nil, nil, errors.New("memmy: Options.Embedder is required")
	}
	dim := opts.Embedder.Dim()
	if dim < 1 {
		return nil, nil, fmt.Errorf("memmy: embedder reported invalid dim %d", dim)
	}

	hnsw := DefaultHNSWConfig()
	if opts.HNSW != nil {
		hnsw = *opts.HNSW
	}
	svcCfg := DefaultServiceConfig()
	if opts.ServiceConfig != nil {
		svcCfg = *opts.ServiceConfig
	}

	storage, err := bboltstore.Open(bboltstore.Options{
		Path:              opts.DBPath,
		Dim:               dim,
		HNSW:              hnsw,
		FlatScanThreshold: opts.FlatScanThreshold,
		Clock:             opts.Clock,
		RandSeed:          opts.HNSWRandSeed,
		Timeout:           opts.OpenTimeout,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("memmy: open storage: %w", err)
	}

	svc, err := service.New(
		storage.Graph(),
		storage.VectorIndex(),
		opts.Embedder,
		opts.Clock,
		svcCfg,
		opts.TenantSchema,
	)
	if err != nil {
		_ = storage.Close()
		return nil, nil, fmt.Errorf("memmy: build service: %w", err)
	}
	return svc, storage, nil
}
