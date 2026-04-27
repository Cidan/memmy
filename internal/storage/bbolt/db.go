// Package bboltstore implements both the Graph and VectorIndex ports
// (DESIGN.md §9.2) over a single bbolt database file. It owns all of:
// nodes, messages, memory_edges_out, memory_edges_in, vectors,
// hnsw_records, hnsw_meta, tenants, schema_version.
package bboltstore

import (
	"errors"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"github.com/Cidan/memmy/internal/clock"
)

// Storage is a single bbolt-backed handle that exposes both the Graph
// and VectorIndex interfaces. The two interfaces own disjoint bucket
// sets within the same file (DESIGN.md §0 #6).
type Storage struct {
	db    *bbolt.DB
	dim   int
	hnsw  HNSWConfig
	clock clock.Clock
	rand  *hnswRand

	// flatScanThreshold: tenants with size below this use linear scan;
	// at or above, the HNSW navigation graph is used (DESIGN.md §6.1).
	flatScanThreshold int
}

// HNSWConfig holds the index hyperparameters. See DESIGN.md §12.
type HNSWConfig struct {
	M              int
	M0             int
	EfConstruction int
	EfSearch       int
	ML             float64
}

// DefaultHNSWConfig returns the documented defaults.
func DefaultHNSWConfig() HNSWConfig {
	return HNSWConfig{
		M:              16,
		M0:             32,
		EfConstruction: 200,
		EfSearch:       100,
		ML:             0.36,
	}
}

// Options are passed to Open.
type Options struct {
	Path string
	// Dim is the embedding vector dimensionality. Required.
	Dim int
	// HNSW configuration. If zero, DefaultHNSWConfig() is used.
	HNSW HNSWConfig
	// FlatScanThreshold — tenants below this size use flat scan.
	FlatScanThreshold int
	// Clock used by service-layer decay math; the storage layer itself
	// does not consult the clock.
	Clock clock.Clock
	// RandSeed seeds the HNSW layer-assignment RNG. If 0, a time-derived
	// seed is used (production); tests pass a fixed seed for determinism.
	RandSeed uint64
	// Timeout for acquiring the file lock.
	Timeout time.Duration
}

// Open creates or opens a bbolt database at opts.Path and ensures the
// root buckets and schema marker are present.
func Open(opts Options) (*Storage, error) {
	if opts.Path == "" {
		return nil, errors.New("bboltstore: opts.Path is required")
	}
	if opts.Dim < 1 {
		return nil, errors.New("bboltstore: opts.Dim must be >= 1")
	}
	if opts.HNSW == (HNSWConfig{}) {
		opts.HNSW = DefaultHNSWConfig()
	}
	if opts.FlatScanThreshold < 0 {
		return nil, errors.New("bboltstore: opts.FlatScanThreshold must be >= 0")
	}
	if opts.FlatScanThreshold == 0 {
		opts.FlatScanThreshold = 5000
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	seed := opts.RandSeed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}

	bopts := *bbolt.DefaultOptions
	if opts.Timeout > 0 {
		bopts.Timeout = opts.Timeout
	}

	db, err := bbolt.Open(opts.Path, 0o600, &bopts)
	if err != nil {
		return nil, fmt.Errorf("bbolt open %q: %w", opts.Path, err)
	}
	if err := db.Update(initRoots); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Storage{
		db:                db,
		dim:               opts.Dim,
		hnsw:              opts.HNSW,
		clock:             opts.Clock,
		rand:              newHNSWRand(seed),
		flatScanThreshold: opts.FlatScanThreshold,
	}, nil
}

// Close shuts down the underlying bbolt database.
func (s *Storage) Close() error { return s.db.Close() }

// Raw returns the underlying *bbolt.DB. Used by tests and by the
// VectorIndex/Graph adapters within this package; do not export
// outside the package.
func (s *Storage) Raw() *bbolt.DB { return s.db }

// Dim returns the configured vector dimensionality.
func (s *Storage) Dim() int { return s.dim }

// FlatScanThreshold returns the configured threshold.
func (s *Storage) FlatScanThreshold() int { return s.flatScanThreshold }

func initRoots(tx *bbolt.Tx) error {
	for _, name := range []string{bktTenants, bktT, bktMeta} {
		if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
			return fmt.Errorf("create root %q: %w", name, err)
		}
	}
	mb := tx.Bucket([]byte(bktMeta))
	if mb.Get([]byte(keySchemaVer)) == nil {
		buf := make([]byte, 4)
		putUint32LE(buf, schemaVersion)
		if err := mb.Put([]byte(keySchemaVer), buf); err != nil {
			return fmt.Errorf("put schema_version: %w", err)
		}
	}
	return nil
}

// tenantBucket returns the t/<tenantID> bucket. If create is true it is
// upserted. If create is false and the tenant has no bucket, returns nil
// (with no error) so callers can no-op naturally on absent tenants.
func tenantBucket(tx *bbolt.Tx, tenantID string, create bool) (*bbolt.Bucket, error) {
	root := tx.Bucket([]byte(bktT))
	if root == nil {
		return nil, fmt.Errorf("missing root bucket %q", bktT)
	}
	if create {
		b, err := root.CreateBucketIfNotExists([]byte(tenantID))
		if err != nil {
			return nil, fmt.Errorf("create tenant bucket %q: %w", tenantID, err)
		}
		return b, nil
	}
	return root.Bucket([]byte(tenantID)), nil
}

// subBucket returns parent.<name>, or nil when absent and create==false.
func subBucket(parent *bbolt.Bucket, name string, create bool) (*bbolt.Bucket, error) {
	if parent == nil {
		return nil, nil
	}
	if create {
		b, err := parent.CreateBucketIfNotExists([]byte(name))
		if err != nil {
			return nil, fmt.Errorf("create sub-bucket %q: %w", name, err)
		}
		return b, nil
	}
	return parent.Bucket([]byte(name)), nil
}
