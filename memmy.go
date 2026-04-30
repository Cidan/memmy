// Package memmy is the embeddable library facade for the memmy LLM
// memory system. It is the only stable public surface; everything below
// it lives under internal/ and may change without notice.
//
// Typical use:
//
//	emb := memmy.NewFakeEmbedder(64) // or NewGeminiEmbedder(ctx, opts)
//	svc, closer, err := memmy.Open(ctx, memmy.Options{
//	    Neo4j: memmy.Neo4jOptions{
//	        URI:      "bolt://localhost:7687",
//	        User:     "neo4j",
//	        Password: os.Getenv("NEO4J_PASSWORD"),
//	    },
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
// The schema is bundled in the binary via embed.FS but is NOT applied
// automatically — operators must call memmy.Migrate() (or the binary's
// `memmy migrate` subcommand) once before Open. Open refuses to start
// against a database whose schema version doesn't match what this
// library was built for.
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
	neo4jstore "github.com/Cidan/memmy/internal/storage/neo4j"
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

// Neo4jOptions bundles the Neo4j backend credentials. The URI, User,
// and Password are required; everything else has a sensible default.
// The struct is shared between memmy.Open (Options.Neo4j) and
// memmy.Migrate (MigrationOptions.Neo4j) so callers configure the
// backend in one place regardless of which entry point they invoke.
type Neo4jOptions struct {
	// URI is the bolt:// (or neo4j+s://) address of the Neo4j
	// instance. Required.
	URI string

	// User / Password are the credentials for URI. Required.
	User     string
	Password string

	// Database selects the database within the Neo4j instance.
	// Optional; "neo4j" by default.
	Database string

	// ConnectTimeout caps the initial connectivity verification.
	// Optional; 10s by default.
	ConnectTimeout time.Duration
}

// Options configures Open. Neo4j and Embedder are required;
// everything else has a sensible zero-value default.
type Options struct {
	// Neo4j configures the storage backend. URI, User, and Password
	// are required.
	Neo4j Neo4jOptions

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

	// FlatScanThreshold is the per-tenant size below which Recall uses a
	// linear scan instead of the native vector index. Optional; 0 → 5000
	// (DESIGN.md §6.1).
	FlatScanThreshold int

	// SkipMigrationCheck disables the schema-version guard at Open.
	// Tests-only — production callers should never set this. The
	// neo4jtest helper sets it because it manages migrations itself.
	SkipMigrationCheck bool
}

// MigrationOptions configures Migrate. Neo4j configures the backend
// the migrations are applied to; Dim is the dimensionality the native
// vector index is created with on first migration.
type MigrationOptions struct {
	Neo4j Neo4jOptions
	Dim   int
}

// Open constructs a Service backed by Neo4j at opts.Neo4j.URI. The
// returned io.Closer must be invoked at shutdown to release the bolt
// driver connection pool. The Embedder's lifecycle is the caller's
// responsibility — Open does NOT close it.
//
// Open does not start any transport (MCP / gRPC / HTTP); callers drive
// the returned Service directly. To run a transport, use the cmd/memmy
// binary with a YAML config instead.
//
// Open refuses to start against a database whose schema version does
// not match what this build of memmy expects. The fix is to run
// memmy.Migrate (or the binary's `memmy migrate` subcommand) once.
func Open(ctx context.Context, opts Options) (Service, io.Closer, error) {
	if opts.Neo4j.URI == "" {
		return nil, nil, errors.New("memmy: Options.Neo4j.URI is required")
	}
	if opts.Neo4j.User == "" {
		return nil, nil, errors.New("memmy: Options.Neo4j.User is required")
	}
	if opts.Neo4j.Password == "" {
		return nil, nil, errors.New("memmy: Options.Neo4j.Password is required")
	}
	if opts.Embedder == nil {
		return nil, nil, errors.New("memmy: Options.Embedder is required")
	}
	dim := opts.Embedder.Dim()
	if dim < 1 {
		return nil, nil, fmt.Errorf("memmy: embedder reported invalid dim %d", dim)
	}

	svcCfg := DefaultServiceConfig()
	if opts.ServiceConfig != nil {
		svcCfg = *opts.ServiceConfig
	}

	storage, err := neo4jstore.Open(ctx, neo4jstore.Options{
		URI:               opts.Neo4j.URI,
		Username:          opts.Neo4j.User,
		Password:          opts.Neo4j.Password,
		Database:          opts.Neo4j.Database,
		Dim:               dim,
		FlatScanThreshold: opts.FlatScanThreshold,
		Clock:             opts.Clock,
		ConnectTimeout:    opts.Neo4j.ConnectTimeout,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("memmy: open storage: %w", err)
	}

	if !opts.SkipMigrationCheck {
		want, err := neo4jstore.RequiredSchemaVersion()
		if err != nil {
			_ = storage.Close()
			return nil, nil, fmt.Errorf("memmy: read required schema version: %w", err)
		}
		got, err := storage.CurrentSchemaVersion(ctx)
		if err != nil {
			_ = storage.Close()
			return nil, nil, fmt.Errorf("memmy: read current schema version: %w", err)
		}
		if got != want {
			_ = storage.Close()
			return nil, nil, fmt.Errorf("memmy: schema version mismatch (database is at v%d, this build requires v%d). Call memmy.Migrate() or run `memmy migrate` first", got, want)
		}
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

// Migrate applies every embedded migration whose version is greater
// than the database's current applied version. Idempotent: re-running
// against an up-to-date database is a no-op.
//
// opts.Dim sets the dimensionality the native vector index is created
// with on first migration; subsequent calls with a different dim are
// silently fine because the index is only created once. Callers that
// later switch embedder dims must drop the existing index manually
// (see DESIGN.md §13.1) and re-Migrate to re-create it.
func Migrate(ctx context.Context, opts MigrationOptions) error {
	if opts.Neo4j.URI == "" {
		return errors.New("memmy: MigrationOptions.Neo4j.URI required")
	}
	if opts.Neo4j.User == "" {
		return errors.New("memmy: MigrationOptions.Neo4j.User required")
	}
	if opts.Neo4j.Password == "" {
		return errors.New("memmy: MigrationOptions.Neo4j.Password required")
	}
	if opts.Dim < 1 {
		return errors.New("memmy: MigrationOptions.Dim must be >= 1")
	}
	storage, err := neo4jstore.Open(ctx, neo4jstore.Options{
		URI:            opts.Neo4j.URI,
		Username:       opts.Neo4j.User,
		Password:       opts.Neo4j.Password,
		Database:       opts.Neo4j.Database,
		Dim:            opts.Dim,
		ConnectTimeout: opts.Neo4j.ConnectTimeout,
	})
	if err != nil {
		return fmt.Errorf("memmy: open for migrate: %w", err)
	}
	defer storage.Close()
	return storage.Migrate(ctx)
}
