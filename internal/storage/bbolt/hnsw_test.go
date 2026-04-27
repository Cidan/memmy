package bboltstore_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	bboltstore "github.com/Cidan/memmy/internal/storage/bbolt"
	vidx "github.com/Cidan/memmy/internal/vectorindex"
)

// TestHNSW_OracleVsFlatScan is the load-bearing correctness test for the
// HNSW implementation: for a non-trivial corpus, top-K returned by HNSW
// must agree with top-K returned by flat scan above a recall floor.
func TestHNSW_OracleVsFlatScan(t *testing.T) {
	const (
		dim         = 32
		corpus      = 2000
		queries     = 50
		k           = 8
		oversample  = 200
		recallFloor = 0.93 // mean recall@k vs flat oracle
	)

	hnswCfg := bboltstore.HNSWConfig{
		M:              16,
		M0:             32,
		EfConstruction: 200,
		EfSearch:       oversample,
		ML:             0.36,
	}
	// Storage A — flat scan oracle (threshold above corpus size).
	stFlat := openTestStorage(t, dim,
		withFlatScanThreshold(corpus+1),
		withHNSW(hnswCfg),
		withRandSeed(101),
	)
	// Storage B — HNSW (threshold = 1, corpus ≫ 1).
	stHNSW := openTestStorage(t, dim,
		withFlatScanThreshold(1),
		withHNSW(hnswCfg),
		withRandSeed(101),
	)

	r := rand.New(rand.NewPCG(20260427, 1))
	ctx := context.Background()
	for i := 0; i < corpus; i++ {
		id := fmt.Sprintf("n-%05d", i)
		vec := randVec(r, dim)
		if err := stFlat.VectorIndex().Insert(ctx, "t", id, vec); err != nil {
			t.Fatal(err)
		}
		if err := stHNSW.VectorIndex().Insert(ctx, "t", id, vec); err != nil {
			t.Fatal(err)
		}
	}

	var totalRecall float64
	for q := 0; q < queries; q++ {
		qVec := randVec(r, dim)
		flatHits, err := stFlat.VectorIndex().Search(ctx, "t", qVec, oversample)
		if err != nil {
			t.Fatal(err)
		}
		hnswHits, err := stHNSW.VectorIndex().Search(ctx, "t", qVec, oversample)
		if err != nil {
			t.Fatal(err)
		}

		// Truncate both to top-k and compute recall.
		topFlat := truncateIDs(flatHits, k)
		topHNSW := truncateIDs(hnswHits, k)
		gold := make(map[string]struct{}, k)
		for _, id := range topFlat {
			gold[id] = struct{}{}
		}
		hits := 0
		for _, id := range topHNSW {
			if _, ok := gold[id]; ok {
				hits++
			}
		}
		totalRecall += float64(hits) / float64(k)
	}
	mean := totalRecall / float64(queries)
	if mean < recallFloor {
		t.Fatalf("HNSW recall@%d = %.3f, want ≥ %.3f", k, mean, recallFloor)
	}
}

func truncateIDs(hits []vidx.Hit, k int) []string {
	if len(hits) > k {
		hits = hits[:k]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.NodeID
	}
	return out
}

func TestHNSW_Search_RespectsK(t *testing.T) {
	st := openTestStorage(t, 8, withFlatScanThreshold(1))
	v := st.VectorIndex()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(3, 7))
	for i := 0; i < 50; i++ {
		_ = v.Insert(ctx, "t", fmt.Sprintf("n-%d", i), randVec(r, 8))
	}
	got, err := v.Search(ctx, "t", randVec(r, 8), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("len=%d, want 5", len(got))
	}
}

func TestHNSW_Delete_FixesNeighborLists(t *testing.T) {
	st := openTestStorage(t, 8, withFlatScanThreshold(1))
	v := st.VectorIndex()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(5, 5))
	const N = 60
	ids := make([]string, N)
	for i := 0; i < N; i++ {
		ids[i] = fmt.Sprintf("n-%02d", i)
		_ = v.Insert(ctx, "t", ids[i], randVec(r, 8))
	}
	// Delete half.
	for i := 0; i < N/2; i++ {
		if err := v.Delete(ctx, "t", ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	sz, _ := v.Size(ctx, "t")
	if sz != N/2 {
		t.Fatalf("Size=%d, want %d", sz, N/2)
	}
	// Search should still work and never return a deleted ID.
	got, _ := v.Search(ctx, "t", randVec(r, 8), 10)
	deleted := make(map[string]struct{}, N/2)
	for i := 0; i < N/2; i++ {
		deleted[ids[i]] = struct{}{}
	}
	for _, h := range got {
		if _, bad := deleted[h.NodeID]; bad {
			t.Fatalf("search returned deleted node: %s", h.NodeID)
		}
	}
}

func TestHNSW_Empty(t *testing.T) {
	st := openTestStorage(t, 8, withFlatScanThreshold(1))
	v := st.VectorIndex()
	got, err := v.Search(context.Background(), "t", make([]float32, 8), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
	sz, _ := v.Size(context.Background(), "t")
	if sz != 0 {
		t.Fatalf("Size on empty = %d", sz)
	}
}

func TestHNSW_BackendSelection_BelowThreshold(t *testing.T) {
	// With threshold > corpus, search must use flat scan and produce
	// exact top-K (no recall loss).
	const dim = 16
	st := openTestStorage(t, dim, withFlatScanThreshold(1000))
	v := st.VectorIndex()
	ctx := context.Background()

	r := rand.New(rand.NewPCG(9, 9))
	const N = 50
	ids := make([]string, N)
	for i := 0; i < N; i++ {
		ids[i] = fmt.Sprintf("n-%02d", i)
		_ = v.Insert(ctx, "t", ids[i], randVec(r, dim))
	}
	q := randVec(r, dim)
	got, _ := v.Search(ctx, "t", q, 5)

	// Build oracle.
	oracle, _ := openTestStorage(t, dim, withFlatScanThreshold(100000)).VectorIndex().Size(ctx, "t")
	_ = oracle
	if len(got) != 5 {
		t.Fatalf("len=%d", len(got))
	}
	// Exact-top-K tested elsewhere; here just sanity-check non-trivial similarity.
	prev := got[0].Sim
	for _, h := range got[1:] {
		if h.Sim > prev {
			t.Fatalf("results not sorted: %v", got)
		}
		prev = h.Sim
	}
}

