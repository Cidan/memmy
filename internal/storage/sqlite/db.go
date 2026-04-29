// Package sqlitestore implements both the Graph and VectorIndex ports
// (DESIGN.md §9.2) over a single SQLite database file in WAL mode.
//
// Why two *sql.DB handles? SQLite WAL mode permits many readers and one
// writer concurrently across processes, but database/sql doesn't expose
// per-tx lock-mode selection (`BEGIN IMMEDIATE` vs `BEGIN DEFERRED`).
// We therefore open the same file twice: a writer DSN with
// `_txlock=immediate` (every write tx grabs the RESERVED lock upfront,
// avoiding SQLITE_BUSY upgrade races) limited to one open connection,
// and a reader DSN with deferred default that allows many concurrent
// snapshot reads. Both honour `_journal_mode=WAL`, `_busy_timeout`,
// `_foreign_keys=ON`, `_synchronous=NORMAL`.
//
// Statelessness is preserved: the Storage struct holds connection
// pools and read-only config — no caches, no in-memory shadow indexes
// (DESIGN.md §0 #3, §13.2).
package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/Cidan/memmy/internal/clock"
)

// Storage exposes Graph + VectorIndex over a SQLite database. The two
// adapters share the same physical store but own disjoint table sets
// (DESIGN.md §0 #6 / §4.6).
type Storage struct {
	writeDB *sql.DB
	readDB  *sql.DB
	path    string
	dim     int
	hnsw    HNSWConfig
	clock   clock.Clock
	rand    *hnswRand

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
	Dim  int
	HNSW HNSWConfig
	// FlatScanThreshold — tenants below this size use flat scan.
	FlatScanThreshold int
	// Clock used by service-layer decay math.
	Clock clock.Clock
	// RandSeed seeds the HNSW layer-assignment RNG. If 0, a time-derived
	// seed is used (production); tests pass a fixed seed for determinism.
	RandSeed uint64
	// BusyTimeout is the SQLite `_busy_timeout` window — how long a
	// blocked writer waits for the reserved lock before failing. 0 →
	// 5 seconds default.
	BusyTimeout time.Duration
}

const defaultBusyTimeout = 5 * time.Second

// Open creates or opens the SQLite database at opts.Path and ensures
// the schema is bootstrapped.
func Open(opts Options) (*Storage, error) {
	if opts.Path == "" {
		return nil, errors.New("sqlitestore: opts.Path is required")
	}
	if opts.Dim < 1 {
		return nil, errors.New("sqlitestore: opts.Dim must be >= 1")
	}
	if opts.HNSW == (HNSWConfig{}) {
		opts.HNSW = DefaultHNSWConfig()
	}
	if opts.FlatScanThreshold < 0 {
		return nil, errors.New("sqlitestore: opts.FlatScanThreshold must be >= 0")
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
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = defaultBusyTimeout
	}

	resolvedPath, err := resolvePath(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("sqlite resolve %q: %w", opts.Path, err)
	}
	if err := os.MkdirAll(filepath.Dir(resolvedPath), 0o700); err != nil {
		return nil, fmt.Errorf("sqlite mkdir for %q: %w", resolvedPath, err)
	}

	writeDB, err := openHandle(resolvedPath, busy, true)
	if err != nil {
		return nil, fmt.Errorf("open write handle: %w", err)
	}
	readDB, err := openHandle(resolvedPath, busy, false)
	if err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("open read handle: %w", err)
	}

	s := &Storage{
		writeDB:           writeDB,
		readDB:            readDB,
		path:              resolvedPath,
		dim:               opts.Dim,
		hnsw:              opts.HNSW,
		clock:             opts.Clock,
		rand:              newHNSWRand(seed),
		flatScanThreshold: opts.FlatScanThreshold,
	}
	if err := s.bootstrap(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// openHandle opens one *sql.DB pointed at path. write=true uses
// `_txlock=immediate` and caps the pool at a single connection; read
// handles use deferred default and permit many connections.
func openHandle(path string, busy time.Duration, write bool) (*sql.DB, error) {
	v := url.Values{}
	v.Set("_journal_mode", "WAL")
	v.Set("_synchronous", "NORMAL")
	v.Set("_foreign_keys", "ON")
	v.Set("_busy_timeout", fmt.Sprintf("%d", busy.Milliseconds()))
	if write {
		v.Set("_txlock", "immediate")
	} else {
		v.Set("_txlock", "deferred")
	}
	dsn := "file:" + path + "?" + v.Encode()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	if write {
		// Single in-process writer. SQLite serializes writers at the
		// file level anyway; capping the pool avoids SQLITE_BUSY storms
		// when multiple goroutines start txs at once.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(8)
		db.SetMaxIdleConns(4)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// bootstrap creates the schema and writes the schema_version marker.
// Idempotent.
func (s *Storage) bootstrap() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.writeDB.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, keySchemaVer).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			buf := make([]byte, 4)
			putUint32LE(buf, schemaVersion)
			if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES(?, ?)`, keySchemaVer, buf); err != nil {
				return err
			}
		}
		return nil
	})
}

// Close shuts down both *sql.DB handles. Idempotent.
func (s *Storage) Close() error {
	var first error
	if s.writeDB != nil {
		if err := s.writeDB.Close(); err != nil {
			first = err
		}
		s.writeDB = nil
	}
	if s.readDB != nil {
		if err := s.readDB.Close(); err != nil && first == nil {
			first = err
		}
		s.readDB = nil
	}
	return first
}

// Dim returns the configured vector dimensionality.
func (s *Storage) Dim() int { return s.dim }

// Path returns the resolved filesystem path of the database.
func (s *Storage) Path() string { return s.path }

// withWriteTx runs fn inside a write tx. `_txlock=immediate` makes
// every BEGIN take the RESERVED lock upfront, avoiding the deferred-tx
// upgrade race.
func (s *Storage) withWriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin write tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit write tx: %w", err)
	}
	return nil
}

// withReadTx runs fn inside a snapshot-read tx. Multiple readers can
// run concurrently; readers do not block on the writer in WAL mode.
func (s *Storage) withReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin read tx: %w", err)
	}
	defer tx.Rollback()
	return fn(tx)
}

// resolvePath expands a leading `~/` to the current user's home
// directory. We deliberately handle ONLY the leading `~/` form.
func resolvePath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}
