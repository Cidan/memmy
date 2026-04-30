package neo4jstore_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/Cidan/memmy/internal/storage/neo4j/neo4jtest"
	"github.com/Cidan/memmy/internal/types"
	vidx "github.com/Cidan/memmy/internal/vectorindex"
)

// TestNativeVectorIndex_RecallFloorVsFlatScan is the load-bearing
// correctness test for the Neo4j vector-index path: for a non-trivial
// corpus, top-K returned by the native HNSW index must agree with
// top-K returned by flat scan above a recall floor.
//
// Both storages point at the same Neo4j database so the underlying
// data is identical; they differ only in FlatScanThreshold (which
// chooses the search code path).
func TestNativeVectorIndex_RecallFloorVsFlatScan(t *testing.T) {
	if testing.Short() {
		t.Skip("native vector index recall test is slow; rerun without -short")
	}
	const (
		corpus      = 500
		queries     = 25
		k           = 8
		recallFloor = 0.85 // Neo4j's native vector index is approximate
	)

	stHNSW, _, prefix := neo4jtest.Open(t, testDim, neo4jtest.WithFlatScanThreshold(1))
	stFlat, _, _ := neo4jtest.OpenSharedTenant(t, testDim, prefix, neo4jtest.WithFlatScanThreshold(corpus+1))

	g := stHNSW.Graph()
	v := stHNSW.VectorIndex()
	ctx := context.Background()

	r := rand.New(rand.NewPCG(20260427, 1))
	for i := 0; i < corpus; i++ {
		id := fmt.Sprintf("n-%05d", i)
		if err := g.PutNode(ctx, types.Node{ID: id, TenantID: prefix, Weight: 1}); err != nil {
			t.Fatal(err)
		}
		if err := v.Insert(ctx, prefix, id, randVec(r, testDim)); err != nil {
			t.Fatal(err)
		}
	}

	flatV := stFlat.VectorIndex()

	var totalRecall float64
	for q := 0; q < queries; q++ {
		qVec := randVec(r, testDim)
		flatHits, err := flatV.Search(ctx, prefix, qVec, k)
		if err != nil {
			t.Fatal(err)
		}
		hnswHits, err := v.Search(ctx, prefix, qVec, k)
		if err != nil {
			t.Fatal(err)
		}

		gold := make(map[string]struct{}, k)
		for _, h := range truncateHits(flatHits, k) {
			gold[h.NodeID] = struct{}{}
		}
		hits := 0
		for _, h := range truncateHits(hnswHits, k) {
			if _, ok := gold[h.NodeID]; ok {
				hits++
			}
		}
		totalRecall += float64(hits) / float64(k)
	}
	mean := totalRecall / float64(queries)
	if mean < recallFloor {
		t.Fatalf("native index recall@%d = %.3f, want ≥ %.3f", k, mean, recallFloor)
	}
}

func truncateHits(hits []vidx.Hit, k int) []vidx.Hit {
	if len(hits) > k {
		return hits[:k]
	}
	return hits
}

// TestNativeVectorIndex_Search_RespectsK confirms the native index
// path returns at most K hits even when the corpus exceeds K.
func TestNativeVectorIndex_Search_RespectsK(t *testing.T) {
	st, _, prefix := neo4jtest.Open(t, testDim, neo4jtest.WithFlatScanThreshold(1))
	g := st.Graph()
	v := st.VectorIndex()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(3, 7))
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("n-%d", i)
		_ = g.PutNode(ctx, types.Node{ID: id, TenantID: prefix, Weight: 1})
		_ = v.Insert(ctx, prefix, id, randVec(r, testDim))
	}
	got, err := v.Search(ctx, prefix, randVec(r, testDim), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("len=%d, want 5", len(got))
	}
}

// TestNativeVectorIndex_Delete_ExcludesTombstoned proves the native
// index path excludes tombstoned nodes from results.
func TestNativeVectorIndex_Delete_ExcludesTombstoned(t *testing.T) {
	st, _, prefix := neo4jtest.Open(t, testDim, neo4jtest.WithFlatScanThreshold(1))
	g := st.Graph()
	v := st.VectorIndex()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(5, 5))
	const N = 60
	ids := make([]string, N)
	for i := 0; i < N; i++ {
		ids[i] = fmt.Sprintf("n-%02d", i)
		_ = g.PutNode(ctx, types.Node{ID: ids[i], TenantID: prefix, Weight: 1})
		_ = v.Insert(ctx, prefix, ids[i], randVec(r, testDim))
	}
	for i := 0; i < N/2; i++ {
		if err := v.Delete(ctx, prefix, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	sz, _ := v.Size(ctx, prefix)
	if sz != N/2 {
		t.Fatalf("Size=%d, want %d", sz, N/2)
	}
	got, _ := v.Search(ctx, prefix, randVec(r, testDim), 10)
	tombstoned := make(map[string]struct{}, N/2)
	for i := 0; i < N/2; i++ {
		tombstoned[ids[i]] = struct{}{}
	}
	for _, h := range got {
		if _, bad := tombstoned[h.NodeID]; bad {
			t.Fatalf("search returned tombstoned node: %s", h.NodeID)
		}
	}
}

func TestNativeVectorIndex_Empty(t *testing.T) {
	st, _, prefix := neo4jtest.Open(t, testDim, neo4jtest.WithFlatScanThreshold(1))
	v := st.VectorIndex()
	got, err := v.Search(context.Background(), prefix, make([]float32, testDim), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
	sz, _ := v.Size(context.Background(), prefix)
	if sz != 0 {
		t.Fatalf("Size on empty = %d", sz)
	}
}
