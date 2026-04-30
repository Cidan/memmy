// Package neo4jstore implements the Graph + VectorIndex ports
// (DESIGN.md §9.2) over a Neo4j database via the Bolt protocol.
//
// memmy treats Neo4j's native vector index as the HNSW navigation
// graph and uses explicit relationships for the Hebbian memory graph.
// Both graphs share the same database; the interface ownership rule
// (Graph owns nodes/messages/edges, VectorIndex owns the vector
// index) is preserved at the package boundary.
//
// Statelessness: Storage holds the driver pool, configured database
// name, dim, clock, and flat-scan threshold. No caches.
package neo4jstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/Cidan/memmy/internal/clock"
)

// Storage is the Bolt-backed Graph + VectorIndex implementation.
type Storage struct {
	driver   neo4j.DriverWithContext
	database string
	dim      int
	clock    clock.Clock

	flatScanThreshold int
}

// Options configures Open.
type Options struct {
	URI            string // bolt:// or neo4j+s:// — required
	Username       string // required
	Password       string // required
	Database       string // default "neo4j"
	Dim            int    // required, must match the configured embedder
	ConnectTimeout time.Duration

	// FlatScanThreshold — tenants below this size use a Cypher flat
	// scan (cosine over every node) instead of the vector index.
	// Useful so tiny tenants don't pay vector-index call overhead.
	FlatScanThreshold int

	// Clock used by service-layer decay math.
	Clock clock.Clock
}

const (
	defaultDatabase    = "neo4j"
	defaultFlatScan    = 5000
	defaultConnectWait = 10 * time.Second
)

// Open dials the Neo4j driver, verifies connectivity, and returns a
// usable Storage. The schema is NOT auto-migrated — call Migrate (or
// the binary's `memmy migrate` subcommand) explicitly.
func Open(ctx context.Context, opts Options) (*Storage, error) {
	if opts.URI == "" {
		return nil, errors.New("neo4jstore: opts.URI required")
	}
	if opts.Username == "" {
		return nil, errors.New("neo4jstore: opts.Username required")
	}
	if opts.Password == "" {
		return nil, errors.New("neo4jstore: opts.Password required")
	}
	if opts.Dim < 1 {
		return nil, errors.New("neo4jstore: opts.Dim must be >= 1")
	}
	if opts.Database == "" {
		opts.Database = defaultDatabase
	}
	if opts.FlatScanThreshold < 0 {
		return nil, errors.New("neo4jstore: opts.FlatScanThreshold must be >= 0")
	}
	if opts.FlatScanThreshold == 0 {
		opts.FlatScanThreshold = defaultFlatScan
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = defaultConnectWait
	}

	driver, err := neo4j.NewDriverWithContext(opts.URI, neo4j.BasicAuth(opts.Username, opts.Password, ""))
	if err != nil {
		return nil, fmt.Errorf("neo4jstore: NewDriver: %w", err)
	}
	verifyCtx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer cancel()
	if err := driver.VerifyConnectivity(verifyCtx); err != nil {
		_ = driver.Close(ctx)
		return nil, fmt.Errorf("neo4jstore: VerifyConnectivity: %w", err)
	}

	return &Storage{
		driver:            driver,
		database:          opts.Database,
		dim:               opts.Dim,
		clock:             opts.Clock,
		flatScanThreshold: opts.FlatScanThreshold,
	}, nil
}

// Close releases the driver pool. Idempotent.
func (s *Storage) Close() error {
	if s.driver == nil {
		return nil
	}
	err := s.driver.Close(context.Background())
	s.driver = nil
	return err
}

// Dim returns the configured vector dimensionality.
func (s *Storage) Dim() int { return s.dim }

// Database returns the configured Neo4j database name.
func (s *Storage) Database() string { return s.database }

// Driver returns the underlying driver. Exposed so the migration
// helper can run schema Cypher against the same connection pool.
func (s *Storage) Driver() neo4j.DriverWithContext { return s.driver }

// withWriteSession runs fn in a managed write transaction against the
// configured database. The driver retries fn on transient errors per
// the standard Bolt managed-transaction pattern.
func (s *Storage) withWriteSession(ctx context.Context, fn func(tx neo4j.ManagedTransaction) (any, error)) (any, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{
		DatabaseName: s.database,
		AccessMode:   neo4j.AccessModeWrite,
	})
	defer sess.Close(ctx)
	return sess.ExecuteWrite(ctx, fn)
}

// withReadSession runs fn in a managed read transaction.
func (s *Storage) withReadSession(ctx context.Context, fn func(tx neo4j.ManagedTransaction) (any, error)) (any, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{
		DatabaseName: s.database,
		AccessMode:   neo4j.AccessModeRead,
	})
	defer sess.Close(ctx)
	return sess.ExecuteRead(ctx, fn)
}
