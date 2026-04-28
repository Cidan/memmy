package bboltstore_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	gport "github.com/Cidan/memmy/internal/graph"
	"github.com/Cidan/memmy/internal/service"
	bboltstore "github.com/Cidan/memmy/internal/storage/bbolt"
	"github.com/Cidan/memmy/internal/types"
)

// TestCounters_MatchBruteForce exercises a randomized mix of node and
// edge mutations (insert / replace / weight-update / delete) and after
// each batch asserts that the O(1) per-tenant counter equals the
// counts and weight sums computed via a fresh full-bucket walk.
func TestCounters_MatchBruteForce(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	ctx := context.Background()
	tenant := "t"
	r := rand.New(rand.NewPCG(20260427, 7))

	// Track an in-memory shadow so the brute-force reconciliation has a
	// straightforward expected state.
	type nodeShadow struct {
		ID     string
		Weight float64
	}
	type edgeShadow struct {
		From, To string
		Weight   float64
	}
	nodes := map[string]nodeShadow{}
	edges := map[[2]string]edgeShadow{}

	const ops = 400
	for i := 0; i < ops; i++ {
		switch r.IntN(7) {
		case 0, 1: // PutNode (new or replace)
			id := fmt.Sprintf("n-%03d", r.IntN(80))
			weight := r.Float64() * 10
			n := types.Node{
				ID:          id,
				TenantID:    tenant,
				Weight:      weight,
				CreatedAt:   time.Unix(0, 0),
				LastTouched: time.Unix(0, 0),
			}
			if err := g.PutNode(ctx, n); err != nil {
				t.Fatalf("PutNode: %v", err)
			}
			nodes[id] = nodeShadow{id, weight}

		case 2: // UpdateNode (weight bump)
			if len(nodes) == 0 {
				continue
			}
			id := pickRandKey(r, nodes)
			delta := r.Float64() * 4
			err := g.UpdateNode(ctx, tenant, id, func(n *types.Node) error {
				n.Weight += delta
				return nil
			})
			if err != nil {
				t.Fatalf("UpdateNode: %v", err)
			}
			s := nodes[id]
			s.Weight += delta
			nodes[id] = s

		case 3: // DeleteNode
			if len(nodes) == 0 {
				continue
			}
			id := pickRandKey(r, nodes)
			if err := g.DeleteNode(ctx, tenant, id); err != nil {
				t.Fatalf("DeleteNode: %v", err)
			}
			delete(nodes, id)
			// Adjacent edges remain in the bucket — Graph does NOT cascade.
			// The shadow must mirror that exact behavior.

		case 4, 5: // PutEdge (new or replace)
			if len(nodes) < 2 {
				continue
			}
			from := pickRandKey(r, nodes)
			to := pickRandKey(r, nodes)
			if from == to {
				continue
			}
			weight := r.Float64() * 5
			e := types.MemoryEdge{
				From: from, To: to, TenantID: tenant,
				Kind:        types.EdgeStructural,
				Weight:      weight,
				CreatedAt:   time.Unix(0, 0),
				LastTouched: time.Unix(0, 0),
			}
			if err := g.PutEdge(ctx, e); err != nil {
				t.Fatalf("PutEdge: %v", err)
			}
			edges[[2]string{from, to}] = edgeShadow{from, to, weight}

		case 6: // DeleteEdge
			if len(edges) == 0 {
				continue
			}
			var key [2]string
			n := r.IntN(len(edges))
			i := 0
			for k := range edges {
				if i == n {
					key = k
					break
				}
				i++
			}
			if err := g.DeleteEdge(ctx, tenant, key[0], key[1]); err != nil {
				t.Fatalf("DeleteEdge: %v", err)
			}
			delete(edges, key)
		}
	}

	// Brute-force expected from the shadow.
	var expected service.TenantStats
	expected.NodeCount = len(nodes)
	expected.EdgeCount = len(edges)
	for _, n := range nodes {
		expected.SumNodeWeight += n.Weight
	}
	for _, e := range edges {
		expected.SumEdgeWeight += e.Weight
	}

	// Walk the bbolt buckets directly as a second oracle (catches drift
	// between shadow and what's actually persisted).
	bucketWalk := walkBucketStats(t, st, tenant)
	if bucketWalk.NodeCount != expected.NodeCount {
		t.Fatalf("bucket NodeCount=%d, shadow=%d", bucketWalk.NodeCount, expected.NodeCount)
	}
	if bucketWalk.EdgeCount != expected.EdgeCount {
		t.Fatalf("bucket EdgeCount=%d, shadow=%d", bucketWalk.EdgeCount, expected.EdgeCount)
	}
	if !nearlyEqual(bucketWalk.SumNodeWeight, expected.SumNodeWeight) {
		t.Fatalf("bucket SumNodeWeight=%v, shadow=%v", bucketWalk.SumNodeWeight, expected.SumNodeWeight)
	}
	if !nearlyEqual(bucketWalk.SumEdgeWeight, expected.SumEdgeWeight) {
		t.Fatalf("bucket SumEdgeWeight=%v, shadow=%v", bucketWalk.SumEdgeWeight, expected.SumEdgeWeight)
	}

	// Now the counter-backed Stats must match.
	scanner, ok := g.(interface {
		TenantStats(ctx context.Context, tenant string) (service.TenantStats, error)
	})
	if !ok {
		t.Fatal("graph does not expose TenantStats")
	}
	got, err := scanner.TenantStats(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeCount != expected.NodeCount {
		t.Errorf("counter NodeCount=%d, want %d", got.NodeCount, expected.NodeCount)
	}
	if got.EdgeCount != expected.EdgeCount {
		t.Errorf("counter EdgeCount=%d, want %d", got.EdgeCount, expected.EdgeCount)
	}
	if !nearlyEqual(got.SumNodeWeight, expected.SumNodeWeight) {
		t.Errorf("counter SumNodeWeight=%v, want %v", got.SumNodeWeight, expected.SumNodeWeight)
	}
	if !nearlyEqual(got.SumEdgeWeight, expected.SumEdgeWeight) {
		t.Errorf("counter SumEdgeWeight=%v, want %v", got.SumEdgeWeight, expected.SumEdgeWeight)
	}
}

// TestCounters_DeleteNonexistent verifies absent-target deletes don't
// drift the counter.
func TestCounters_DeleteNonexistent(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	ctx := context.Background()
	if err := g.PutNode(ctx, types.Node{ID: "a", TenantID: "t", Weight: 1.0}); err != nil {
		t.Fatal(err)
	}
	if err := g.DeleteNode(ctx, "t", "missing"); err != nil {
		t.Fatal(err)
	}
	if err := g.DeleteEdge(ctx, "t", "missing", "alsomissing"); err != nil {
		t.Fatal(err)
	}
	scanner := g.(interface {
		TenantStats(ctx context.Context, tenant string) (service.TenantStats, error)
	})
	got, err := scanner.TenantStats(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeCount != 1 || !nearlyEqual(got.SumNodeWeight, 1.0) {
		t.Fatalf("after no-op deletes: %+v", got)
	}
}

// pickRandKey returns a uniformly random key from a map (deterministic
// per the supplied rand).
func pickRandKey[T any](r *rand.Rand, m map[string]T) string {
	if len(m) == 0 {
		return ""
	}
	n := r.IntN(len(m))
	i := 0
	for k := range m {
		if i == n {
			return k
		}
		i++
	}
	return ""
}

// walkBucketStats opens a fresh read tx and walks the nodes + outbound-
// edges buckets, computing counts and weight sums. This is the
// independent oracle the counter test reconciles against.
func walkBucketStats(t *testing.T, st bboltDB, tenant string) service.TenantStats {
	t.Helper()
	var ts service.TenantStats
	err := st.Raw().View(func(tx *bbolt.Tx) error {
		root := tx.Bucket([]byte("t"))
		if root == nil {
			return nil
		}
		tb := root.Bucket([]byte(tenant))
		if tb == nil {
			return nil
		}
		if nb := tb.Bucket([]byte("nodes")); nb != nil {
			if err := nb.ForEach(func(_, v []byte) error {
				var n types.Node
				if err := bboltstore.DecodeNodeForTest(v, &n); err != nil {
					return err
				}
				ts.NodeCount++
				ts.SumNodeWeight += n.Weight
				return nil
			}); err != nil {
				return err
			}
		}
		if eo := tb.Bucket([]byte("eout")); eo != nil {
			return eo.ForEachBucket(func(k []byte) error {
				return eo.Bucket(k).ForEach(func(_, v []byte) error {
					var e types.MemoryEdge
					if err := bboltstore.DecodeEdgeForTest(v, &e); err != nil {
						return err
					}
					ts.EdgeCount++
					ts.SumEdgeWeight += e.Weight
					return nil
				})
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return ts
}

// bboltDB is the small contract walkBucketStats needs from Storage.
type bboltDB interface {
	Raw() *bbolt.DB
}

func nearlyEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}

// silence unused imports in case test build conditions exclude callers
var _ = gport.ErrNotFound
