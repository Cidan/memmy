package neo4jstore_test

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/Cidan/memmy/internal/storage/neo4j/neo4jtest"
	"github.com/Cidan/memmy/internal/types"
)

// TestVectorIndex_FlatScan_TopK seeds N vectors into a tenant under
// the flat-scan threshold, runs a Search, and confirms the returned
// top-K matches the brute-force cosine ranking. The flat-scan path is
// the correctness oracle for the native vector index path tested in
// oracle_test.go.
func TestVectorIndex_FlatScan_TopK(t *testing.T) {
	st, _, prefix := neo4jtest.Open(t, testDim, neo4jtest.WithFlatScanThreshold(100000))
	g := st.Graph()
	v := st.VectorIndex()
	ctx := context.Background()
	tenant := prefix

	const N = 100
	r := rand.New(rand.NewPCG(1, 2))
	vecs := make(map[string][]float32, N)
	for i := 0; i < N; i++ {
		id := fmt.Sprintf("n-%04d", i)
		vec := randVec(r, testDim)
		vecs[id] = vec
		if err := g.PutNode(ctx, types.Node{ID: id, TenantID: tenant, Weight: 1}); err != nil {
			t.Fatal(err)
		}
		if err := v.Insert(ctx, tenant, id, vec); err != nil {
			t.Fatal(err)
		}
	}

	q := randVec(r, testDim)
	got, err := v.Search(ctx, tenant, q, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("got %d hits, want 10", len(got))
	}

	type pair struct {
		id  string
		sim float64
	}
	qn := normalize(q)
	all := make([]pair, 0, N)
	for id, vec := range vecs {
		all = append(all, pair{id, dotF(qn, normalize(vec))})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].sim > all[j].sim })

	for i := 0; i < 10; i++ {
		if got[i].NodeID != all[i].id {
			t.Fatalf("rank %d mismatch: got %s, want %s (sim got=%v want=%v)",
				i, got[i].NodeID, all[i].id, got[i].Sim, all[i].sim)
		}
		if math.Abs(got[i].Sim-all[i].sim) > 1e-4 {
			t.Fatalf("rank %d sim mismatch: got %v want %v", i, got[i].Sim, all[i].sim)
		}
	}
}

func TestVectorIndex_FlatScan_RespectsK(t *testing.T) {
	st, _, prefix := neo4jtest.Open(t, testDim, neo4jtest.WithFlatScanThreshold(100000))
	g := st.Graph()
	v := st.VectorIndex()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(7, 13))
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("n-%d", i)
		_ = g.PutNode(ctx, types.Node{ID: id, TenantID: prefix, Weight: 1})
		_ = v.Insert(ctx, prefix, id, randVec(r, testDim))
	}
	got, _ := v.Search(ctx, prefix, randVec(r, testDim), 100)
	if len(got) != 5 {
		t.Fatalf("len=%d, want 5 (corpus size)", len(got))
	}
	got, _ = v.Search(ctx, prefix, randVec(r, testDim), 2)
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
}

func TestVectorIndex_FlatScan_EmptyTenant(t *testing.T) {
	st, _, _ := neo4jtest.Open(t, testDim, neo4jtest.WithFlatScanThreshold(100000))
	v := st.VectorIndex()
	got, err := v.Search(context.Background(), "no-such-tenant", make([]float32, testDim), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty results, got %v", got)
	}
}

func TestVectorIndex_Insert_DimMismatch(t *testing.T) {
	st, _, prefix := neo4jtest.Open(t, testDim)
	v := st.VectorIndex()
	if err := v.Insert(context.Background(), prefix, "n", make([]float32, testDim/2)); err == nil {
		t.Fatal("expected dim-mismatch error")
	}
}

func TestVectorIndex_Size(t *testing.T) {
	st, _, prefix := neo4jtest.Open(t, testDim)
	g := st.Graph()
	v := st.VectorIndex()
	ctx := context.Background()

	r := rand.New(rand.NewPCG(99, 99))
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("n-%d", i)
		_ = g.PutNode(ctx, types.Node{ID: id, TenantID: prefix, Weight: 1})
		_ = v.Insert(ctx, prefix, id, randVec(r, testDim))
	}
	sz, _ := v.Size(ctx, prefix)
	if sz != 7 {
		t.Fatalf("Size=%d, want 7", sz)
	}
}

func TestVectorIndex_Replace(t *testing.T) {
	st, _, prefix := neo4jtest.Open(t, testDim)
	g := st.Graph()
	v := st.VectorIndex()
	ctx := context.Background()

	r := rand.New(rand.NewPCG(11, 11))
	_ = g.PutNode(ctx, types.Node{ID: "x", TenantID: prefix, Weight: 1})
	_ = v.Insert(ctx, prefix, "x", randVec(r, testDim))
	sz0, _ := v.Size(ctx, prefix)
	_ = v.Insert(ctx, prefix, "x", randVec(r, testDim)) // upsert
	sz1, _ := v.Size(ctx, prefix)
	if sz0 != 1 || sz1 != 1 {
		t.Fatalf("expected size 1 both times: sz0=%d sz1=%d", sz0, sz1)
	}
}

// TestVectorIndex_Delete tombstones the node — Size drops, and the
// tombstoned node never appears in subsequent search results.
func TestVectorIndex_Delete(t *testing.T) {
	st, _, prefix := neo4jtest.Open(t, testDim, neo4jtest.WithFlatScanThreshold(100000))
	g := st.Graph()
	v := st.VectorIndex()
	ctx := context.Background()

	r := rand.New(rand.NewPCG(2, 3))
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("n-%d", i)
		_ = g.PutNode(ctx, types.Node{ID: id, TenantID: prefix, Weight: 1})
		_ = v.Insert(ctx, prefix, id, randVec(r, testDim))
	}
	if err := v.Delete(ctx, prefix, "n-2"); err != nil {
		t.Fatal(err)
	}
	sz, _ := v.Size(ctx, prefix)
	if sz != 4 {
		t.Fatalf("Size after delete = %d, want 4", sz)
	}
	hits, _ := v.Search(ctx, prefix, randVec(r, testDim), 10)
	for _, h := range hits {
		if h.NodeID == "n-2" {
			t.Fatalf("tombstoned node still in results: %v", hits)
		}
	}
}

// ----- helpers -----

func randVec(r *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(r.Float64()*2 - 1)
	}
	return v
}

func normalize(v []float32) []float32 {
	out := make([]float32, len(v))
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return out
	}
	inv := float32(1.0 / math.Sqrt(s))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

func dotF(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
