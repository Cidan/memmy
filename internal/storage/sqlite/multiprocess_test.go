package sqlitestore_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"testing"

	sqlitestore "github.com/Cidan/memmy/internal/storage/sqlite"
	"github.com/Cidan/memmy/internal/types"
)

// TestMultiHandle_ConcurrentReadWrite proves the scenario the bbolt
// backend could not satisfy: two *Storage handles open against the
// same on-disk database simultaneously, with reads from handle B
// observing writes committed via handle A. This is the property that
// lets multiple processes (e.g. several `ask` instances) embed memmy
// against the same DB without lock contention beyond the WAL writer
// gate.
func TestMultiHandle_ConcurrentReadWrite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memmy.db")

	openHandle := func(seed uint64) *sqlitestore.Storage {
		t.Helper()
		st, err := sqlitestore.Open(sqlitestore.Options{
			Path:     dbPath,
			Dim:      8,
			RandSeed: seed,
		})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return st
	}

	stA := openHandle(11)
	t.Cleanup(func() { _ = stA.Close() })
	stB := openHandle(13)
	t.Cleanup(func() { _ = stB.Close() })

	ctx := context.Background()
	tenant := "shared-tenant"

	// Handle A inserts a node + vector; both writers can hold the file
	// open simultaneously thanks to WAL.
	r := rand.New(rand.NewPCG(1, 1))
	const N = 5
	ids := make([]string, N)
	for i := 0; i < N; i++ {
		ids[i] = fmt.Sprintf("n-%02d", i)
		if err := stA.Graph().PutNode(ctx, types.Node{
			ID:       ids[i],
			TenantID: tenant,
			Text:     fmt.Sprintf("text %d", i),
			Weight:   1.0,
		}); err != nil {
			t.Fatalf("A.PutNode %s: %v", ids[i], err)
		}
		if err := stA.VectorIndex().Insert(ctx, tenant, ids[i], randVec(r, 8)); err != nil {
			t.Fatalf("A.Insert %s: %v", ids[i], err)
		}
	}

	// Handle B reads each one back. Both handles point at the same file
	// and SQLite WAL gives B a fresh snapshot that includes A's commits.
	for _, id := range ids {
		got, err := stB.Graph().GetNode(ctx, tenant, id)
		if err != nil {
			t.Fatalf("B.GetNode %s: %v", id, err)
		}
		if got.ID != id {
			t.Fatalf("B saw stale snapshot: got %+v, want id %s", got, id)
		}
	}

	// Cross-write: handle B updates a node, handle A observes it.
	if err := stB.Graph().UpdateNode(ctx, tenant, ids[0], func(n *types.Node) error {
		n.Weight = 42.0
		return nil
	}); err != nil {
		t.Fatalf("B.UpdateNode: %v", err)
	}
	got, err := stA.Graph().GetNode(ctx, tenant, ids[0])
	if err != nil {
		t.Fatalf("A.GetNode after B-write: %v", err)
	}
	if got.Weight != 42.0 {
		t.Fatalf("A did not see B's commit: weight=%v", got.Weight)
	}

	// Searches succeed on both handles.
	hitsA, err := stA.VectorIndex().Search(ctx, tenant, randVec(r, 8), 3)
	if err != nil {
		t.Fatalf("A.Search: %v", err)
	}
	hitsB, err := stB.VectorIndex().Search(ctx, tenant, randVec(r, 8), 3)
	if err != nil {
		t.Fatalf("B.Search: %v", err)
	}
	if len(hitsA) == 0 || len(hitsB) == 0 {
		t.Fatalf("empty search hits: A=%v B=%v", hitsA, hitsB)
	}
}
