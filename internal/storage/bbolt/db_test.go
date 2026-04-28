package bboltstore_test

import (
	"os"
	"path/filepath"
	"testing"

	bboltstore "github.com/Cidan/memmy/internal/storage/bbolt"
)

// TestOpen_CreatesMissingParentDirs verifies that Open materializes
// the directory chain leading up to the configured database file.
// This was a v1 ergonomics bug: a fresh install with a path like
// ~/.local/share/memmy/memmy.db would fail because bbolt won't
// create parent directories.
func TestOpen_CreatesMissingParentDirs(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "deep", "nested", "tree", "memmy.db")
	if _, err := os.Stat(filepath.Dir(dbPath)); err == nil {
		t.Fatal("test precondition broken: parent already exists")
	}
	st, err := bboltstore.Open(bboltstore.Options{Path: dbPath, Dim: 8, RandSeed: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("DB file not created: %v", err)
	}
}

// TestOpen_ExpandsTildePath confirms `~/` in the configured path is
// resolved against the user's home directory rather than passed
// literally to bbolt. We point HOME at a temp dir so the test
// doesn't pollute the developer's actual home.
func TestOpen_ExpandsTildePath(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	st, err := bboltstore.Open(bboltstore.Options{
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
	st, err := bboltstore.Open(bboltstore.Options{Path: dbPath, Dim: 8, RandSeed: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("DB file not created at %q: %v", dbPath, err)
	}
}
