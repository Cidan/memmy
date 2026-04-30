package neo4jstore_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/Cidan/memmy/internal/storage/neo4j/neo4jtest"
	"github.com/Cidan/memmy/internal/types"
)

// TestMultiHandle_ConcurrentReadWrite proves that two Storage handles
// open against the same Neo4j database simultaneously can see each
// other's writes — the property that lets multiple processes embed
// memmy against the same db without lock contention. Bolt's session
// model handles cross-handle visibility natively.
func TestMultiHandle_ConcurrentReadWrite(t *testing.T) {
	stA, _, prefix := neo4jtest.Open(t, testDim)
	stB, _, _ := neo4jtest.OpenSharedTenant(t, testDim, prefix)

	ctx := context.Background()
	tenant := prefix

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
		if err := stA.VectorIndex().Insert(ctx, tenant, ids[i], randVec(r, testDim)); err != nil {
			t.Fatalf("A.Insert %s: %v", ids[i], err)
		}
	}

	// Handle B reads each one back. Both handles point at the same
	// database and Bolt gives B a fresh transaction snapshot that
	// includes A's commits.
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
	hitsA, err := stA.VectorIndex().Search(ctx, tenant, randVec(r, testDim), 3)
	if err != nil {
		t.Fatalf("A.Search: %v", err)
	}
	hitsB, err := stB.VectorIndex().Search(ctx, tenant, randVec(r, testDim), 3)
	if err != nil {
		t.Fatalf("B.Search: %v", err)
	}
	if len(hitsA) == 0 || len(hitsB) == 0 {
		t.Fatalf("empty search hits: A=%v B=%v", hitsA, hitsB)
	}
}
