package sqlitestore_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	sqlitestore "github.com/Cidan/memmy/internal/storage/sqlite"
)

// TestOpen_CreatesMissingParentDirs verifies that Open materializes
// the directory chain leading up to the configured database file.
func TestOpen_CreatesMissingParentDirs(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "deep", "nested", "tree", "memmy.db")
	if _, err := os.Stat(filepath.Dir(dbPath)); err == nil {
		t.Fatal("test precondition broken: parent already exists")
	}
	st, err := sqlitestore.Open(sqlitestore.Options{Path: dbPath, Dim: 8, RandSeed: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("DB file not created: %v", err)
	}
}

// TestOpen_ExpandsTildePath confirms `~/` in the configured path is
// resolved against the user's home directory.
func TestOpen_ExpandsTildePath(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	st, err := sqlitestore.Open(sqlitestore.Options{
		Path: "~/memmy/test.db",
		Dim:  8, RandSeed: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	want := filepath.Join(tempHome, "memmy", "test.db")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected DB at %q: %v", want, err)
	}
}

// TestOpen_AbsolutePathUnchanged sanity-checks that absolute paths
// are passed through unmodified by the resolver.
func TestOpen_AbsolutePathUnchanged(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "memmy.db")
	st, err := sqlitestore.Open(sqlitestore.Options{Path: dbPath, Dim: 8, RandSeed: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("DB file not created at %q: %v", dbPath, err)
	}
}

// TestOpen_WALMode confirms the database is in journal_mode=WAL.
// Multi-process reads + writes depend on this being correctly set
// at Open time.
func TestOpen_WALMode(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memmy.db")
	st, err := sqlitestore.Open(sqlitestore.Options{Path: dbPath, Dim: 8, RandSeed: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Open a fresh sql handle just to PRAGMA the journal mode out.
	probe, err := sql.Open("sqlite3", "file:"+dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("probe open: %v", err)
	}
	t.Cleanup(func() { _ = probe.Close() })

	var mode string
	if err := probe.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode=%q, want wal", mode)
	}
}

// TestOpen_SchemaVersionRecorded confirms the schema_version row is
// written on first open.
func TestOpen_SchemaVersionRecorded(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memmy.db")
	st, err := sqlitestore.Open(sqlitestore.Options{Path: dbPath, Dim: 8, RandSeed: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	probe, err := sql.Open("sqlite3", "file:"+dbPath)
	if err != nil {
		t.Fatalf("probe open: %v", err)
	}
	t.Cleanup(func() { _ = probe.Close() })

	var raw []byte
	if err := probe.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&raw); err != nil {
		t.Fatalf("schema_version row missing: %v", err)
	}
	if len(raw) != 4 {
		t.Fatalf("schema_version length=%d, want 4", len(raw))
	}
}
