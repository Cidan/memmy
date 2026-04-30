// Package neo4jtest is the test helper that opens a per-test
// Storage handle against the developer's local Neo4j and registers
// a t.Cleanup that DETACH DELETEs every node carrying the test's
// unique tenant prefix. Tests skip cleanly when NEO4J_PASSWORD is
// not set so `go test ./...` succeeds on hosts without a Neo4j.
package neo4jtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/Cidan/memmy/internal/clock"
	neo4jstore "github.com/Cidan/memmy/internal/storage/neo4j"
)

// Default connection settings; overridable via env.
const (
	envURI      = "NEO4J_URI"
	envUser     = "NEO4J_USER"
	envPassword = "NEO4J_PASSWORD"
	envDatabase = "NEO4J_DATABASE"

	defaultURI      = "bolt://localhost:7687"
	defaultUser     = "neo4j"
	defaultDatabase = "neo4j"

	tenantPrefixHead = "Test_"
)

// Connection bundles the env-resolved Neo4j connection settings so
// tests can also pass them into eval.inspect or any other consumer
// that needs to reach the same database.
type Connection struct {
	URI      string
	User     string
	Password string
	Database string
}

// SkipIfUnset checks for NEO4J_PASSWORD and t.Skips with a helpful
// message when unset. Other env vars fall back to documented defaults.
func SkipIfUnset(t *testing.T) Connection {
	t.Helper()
	pw := os.Getenv(envPassword)
	if pw == "" {
		t.Skipf("Neo4j integration test skipped: set %s (and optionally %s, %s, %s) to run", envPassword, envURI, envUser, envDatabase)
	}
	return Connection{
		URI:      envOr(envURI, defaultURI),
		User:     envOr(envUser, defaultUser),
		Password: pw,
		Database: envOr(envDatabase, defaultDatabase),
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// TenantPrefix returns a unique prefix derived from the test name so
// concurrent tests can't see each other's data. The prefix starts
// with `Test_` so the cleanup matcher is unambiguous.
func TenantPrefix(t *testing.T) string {
	t.Helper()
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return tenantPrefixHead + hex.EncodeToString(buf[:])
}

// migrationsApplied serializes Migrate calls across the process so
// concurrent t.Run invocations don't race against each other when
// applying schema. The Migrate function itself is idempotent; this
// gate just avoids "constraint already exists" surfacing as a flaky
// test failure when many goroutines try to create the same constraint
// at once.
var migrationsApplied sync.Map

// OpenOption mutates the neo4jstore.Options before Open is called.
type OpenOption func(*neo4jstore.Options)

// WithFlatScanThreshold sets the Storage.FlatScanThreshold. Tests that
// want to force the flat-scan code path use a large value; tests that
// want to force the native vector-index path use a small value.
func WithFlatScanThreshold(n int) OpenOption {
	return func(o *neo4jstore.Options) { o.FlatScanThreshold = n }
}

// Open returns a Storage opened with dim, with migrations applied.
// Registers a t.Cleanup that DETACH DELETEs every Node, Message,
// TenantInfo, and Counter whose tenant property starts with the
// returned tenant prefix.
//
// Note: the native Neo4j vector index dim is fixed at migration time
// (the first call to Open in the process sets it). Tests that exercise
// the native vector index must therefore agree on a single dim across
// the whole run; convention is dim=32. Tests that only use the flat-
// scan path (FlatScanThreshold ≥ corpus size) can pick any dim, but
// they still share the same migrated index dim, so be consistent.
func Open(t *testing.T, dim int, opts ...OpenOption) (*neo4jstore.Storage, Connection, string) {
	t.Helper()
	conn := SkipIfUnset(t)
	prefix := TenantPrefix(t)

	o := neo4jstore.Options{
		URI:            conn.URI,
		Username:       conn.User,
		Password:       conn.Password,
		Database:       conn.Database,
		Dim:            dim,
		Clock:          clock.Real{},
		ConnectTimeout: 10 * time.Second,
	}
	for _, fn := range opts {
		fn(&o)
	}

	ctx := context.Background()
	storage, err := neo4jstore.Open(ctx, o)
	if err != nil {
		t.Fatalf("neo4jtest: open: %v", err)
	}
	t.Cleanup(func() {
		cleanup(t, storage, prefix)
		_ = storage.Close()
	})
	migrationKey := conn.URI + "|" + conn.Database
	if _, loaded := migrationsApplied.LoadOrStore(migrationKey, true); !loaded {
		if err := storage.Migrate(ctx); err != nil {
			t.Fatalf("neo4jtest: migrate: %v", err)
		}
	}
	return storage, conn, prefix
}

// OpenSharedTenant opens a second Storage handle that shares the
// tenant prefix from a previous Open. Useful for tests that need two
// configurations (e.g. flat-scan vs native-index path) operating on
// the same data set. The shared prefix means the original Open's
// t.Cleanup catches data created by either handle.
func OpenSharedTenant(t *testing.T, dim int, prefix string, opts ...OpenOption) (*neo4jstore.Storage, Connection, string) {
	t.Helper()
	conn := SkipIfUnset(t)

	o := neo4jstore.Options{
		URI:            conn.URI,
		Username:       conn.User,
		Password:       conn.Password,
		Database:       conn.Database,
		Dim:            dim,
		Clock:          clock.Real{},
		ConnectTimeout: 10 * time.Second,
	}
	for _, fn := range opts {
		fn(&o)
	}

	ctx := context.Background()
	storage, err := neo4jstore.Open(ctx, o)
	if err != nil {
		t.Fatalf("neo4jtest: open shared: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	return storage, conn, prefix
}

func cleanup(t *testing.T, storage *neo4jstore.Storage, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	driver := storage.Driver()
	if driver == nil {
		return
	}
	sess := driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: storage.Database()})
	defer sess.Close(ctx)
	// Two cleanup paths cover both ways tests assign tenants:
	//   - storage-layer tests pass the prefix value as the literal
	//     TenantID, so n.tenant STARTS WITH $p matches.
	//   - service-layer tests pass a tuple containing the prefix; the
	//     service hashes the tuple to a SHA-256 TenantID, so we have
	//     to look up TenantInfo rows whose tuple_json contains the
	//     prefix and delete every node carrying that hashed ID.
	queries := []string{
		`MATCH (t:TenantInfo) WHERE t.tuple_json CONTAINS $p
		 WITH collect(t.id) AS ids
		 MATCH (n) WHERE n.tenant IN ids DETACH DELETE n`,
		`MATCH (t:TenantInfo) WHERE t.tuple_json CONTAINS $p DETACH DELETE t`,
		`MATCH (n) WHERE n.tenant STARTS WITH $p DETACH DELETE n`,
		`MATCH (t:TenantInfo) WHERE t.id STARTS WITH $p DETACH DELETE t`,
	}
	for _, q := range queries {
		if _, err := sess.Run(ctx, q, map[string]any{"p": prefix}); err != nil {
			t.Logf("neo4jtest cleanup: %s: %v", q, err)
		}
	}
}
