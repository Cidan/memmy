package bboltstore_test

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"testing"

	vidx "github.com/Cidan/memmy/internal/vectorindex"
)

func TestVectorIndex_Flat_TopK(t *testing.T) {
	const dim = 16
	st := openTestStorage(t, dim, withFlatScanThreshold(100000)) // force flat scan
	v := st.VectorIndex()
	ctx := context.Background()
	tenant := "t"

	const N = 200
	r := rand.New(rand.NewPCG(1, 2))
	vecs := make(map[string][]float32, N)
	ids := make([]string, 0, N)
	for i := 0; i < N; i++ {
		id := fmt.Sprintf("n-%04d", i)
		vec := randVec(r, dim)
		ids = append(ids, id)
		vecs[id] = vec
		if err := v.Insert(ctx, tenant, id, vec); err != nil {
			t.Fatal(err)
		}
	}

	q := randVec(r, dim)
	got, err := v.Search(ctx, tenant, q, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("got %d hits, want 10", len(got))
	}

	// Brute-force oracle: sort all known vectors by sim with q (both normalized).
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
		if math.Abs(got[i].Sim-all[i].sim) > 1e-5 {
			t.Fatalf("rank %d sim mismatch: got %v want %v", i, got[i].Sim, all[i].sim)
		}
	}
}

func TestVectorIndex_Flat_RespectsK(t *testing.T) {
	st := openTestStorage(t, 8, withFlatScanThreshold(100000))
	v := st.VectorIndex()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(7, 13))
	for i := 0; i < 5; i++ {
		_ = v.Insert(ctx, "t", fmt.Sprintf("n-%d", i), randVec(r, 8))
	}
	got, _ := v.Search(ctx, "t", randVec(r, 8), 100)
	if len(got) != 5 {
		t.Fatalf("len=%d, want 5 (corpus size)", len(got))
	}
	got, _ = v.Search(ctx, "t", randVec(r, 8), 2)
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
}

func TestVectorIndex_Flat_EmptyTenant(t *testing.T) {
	st := openTestStorage(t, 8, withFlatScanThreshold(100000))
	v := st.VectorIndex()
	got, err := v.Search(context.Background(), "no-such-tenant", make([]float32, 8), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty results, got %v", got)
	}
}

func TestVectorIndex_Insert_DimMismatch(t *testing.T) {
	st := openTestStorage(t, 16)
	v := st.VectorIndex()
	if err := v.Insert(context.Background(), "t", "n", make([]float32, 8)); err == nil {
		t.Fatal("expected dim-mismatch error")
	}
}

func TestVectorIndex_Size(t *testing.T) {
	st := openTestStorage(t, 4)
	v := st.VectorIndex()
	ctx := context.Background()

	r := rand.New(rand.NewPCG(99, 99))
	for i := 0; i < 7; i++ {
		_ = v.Insert(ctx, "t", fmt.Sprintf("n-%d", i), randVec(r, 4))
	}
	sz, _ := v.Size(ctx, "t")
	if sz != 7 {
		t.Fatalf("Size=%d, want 7", sz)
	}
}

func TestVectorIndex_Replace(t *testing.T) {
	st := openTestStorage(t, 4)
	v := st.VectorIndex()
	ctx := context.Background()

	r := rand.New(rand.NewPCG(11, 11))
	_ = v.Insert(ctx, "t", "x", randVec(r, 4))
	sz0, _ := v.Size(ctx, "t")
	_ = v.Insert(ctx, "t", "x", randVec(r, 4)) // upsert
	sz1, _ := v.Size(ctx, "t")
	if sz0 != 1 || sz1 != 1 {
		t.Fatalf("expected size 1 both times: sz0=%d sz1=%d", sz0, sz1)
	}
}

func TestVectorIndex_Delete(t *testing.T) {
	st := openTestStorage(t, 4)
	v := st.VectorIndex()
	ctx := context.Background()

	r := rand.New(rand.NewPCG(2, 3))
	for i := 0; i < 5; i++ {
		_ = v.Insert(ctx, "t", fmt.Sprintf("n-%d", i), randVec(r, 4))
	}
	if err := v.Delete(ctx, "t", "n-2"); err != nil {
		t.Fatal(err)
	}
	sz, _ := v.Size(ctx, "t")
	if sz != 4 {
		t.Fatalf("Size after delete = %d, want 4", sz)
	}
	hits, _ := v.Search(ctx, "t", randVec(r, 4), 10)
	for _, h := range hits {
		if h.NodeID == "n-2" {
			t.Fatalf("deleted node still in results: %v", hits)
		}
	}
}

// ----- helpers -----

func randVec(r *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		// Centered around 0, range roughly [-1, 1].
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

// silence unused import when tests are filtered
var _ = vidx.ErrNotFound
