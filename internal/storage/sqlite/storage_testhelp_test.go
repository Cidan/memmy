package sqlitestore_test

import (
	"path/filepath"
	"testing"

	sqlitestore "github.com/Cidan/memmy/internal/storage/sqlite"
)

// openTestStorage opens a Storage in a per-test temp directory and
// arranges for clean shutdown. The returned Storage uses a fixed
// RandSeed so HNSW behavior is deterministic across runs.
func openTestStorage(t *testing.T, dim int, opts ...func(*sqlitestore.Options)) *sqlitestore.Storage {
	t.Helper()
	o := sqlitestore.Options{
		Path:              filepath.Join(t.TempDir(), "memmy.db"),
		Dim:               dim,
		RandSeed:          42,
		FlatScanThreshold: 5000,
	}
	for _, fn := range opts {
		fn(&o)
	}
	st, err := sqlitestore.Open(o)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// withFlatScanThreshold sets a custom threshold for backend selection.
func withFlatScanThreshold(n int) func(*sqlitestore.Options) {
	return func(o *sqlitestore.Options) { o.FlatScanThreshold = n }
}

// withHNSW sets a custom HNSW configuration for the test.
func withHNSW(cfg sqlitestore.HNSWConfig) func(*sqlitestore.Options) {
	return func(o *sqlitestore.Options) { o.HNSW = cfg }
}

// withRandSeed overrides the default deterministic seed.
func withRandSeed(seed uint64) func(*sqlitestore.Options) {
	return func(o *sqlitestore.Options) { o.RandSeed = seed }
}
