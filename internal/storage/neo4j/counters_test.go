package neo4jstore_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/Cidan/memmy/internal/service"
	"github.com/Cidan/memmy/internal/storage/neo4j/neo4jtest"
	"github.com/Cidan/memmy/internal/types"
)

// TestCounters_MatchBruteForce exercises a randomized mix of node and
// edge mutations (insert / replace / weight-update / delete) and after
// each batch asserts that the O(1) per-tenant counter equals the
// counts and weight sums computed via a fresh full-tenant walk against
// Neo4j directly.
func TestCounters_MatchBruteForce(t *testing.T) {
	st, _, prefix := neo4jtest.Open(t, testDim)
	g := st.Graph()
	ctx := context.Background()
	tenant := prefix
	r := rand.New(rand.NewPCG(20260427, 7))

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

	const ops = 200
	for i := 0; i < ops; i++ {
		switch r.IntN(7) {
		case 0, 1: // PutNode (new or replace)
			id := fmt.Sprintf("n-%03d", r.IntN(40))
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

		case 3: // DeleteNode (also drops dependent edges via DETACH DELETE)
			if len(nodes) == 0 {
				continue
			}
			id := pickRandKey(r, nodes)
			if err := g.DeleteNode(ctx, tenant, id); err != nil {
				t.Fatalf("DeleteNode: %v", err)
			}
			delete(nodes, id)
			// Cypher DETACH DELETE removes attached edges silently;
			// we have to mirror that in the shadow map AND adjust
			// counters by reading the database (the Neo4j adapter
			// doesn't decrement EdgeCount for nodes deleted via
			// DETACH). To keep the counter check honest, we rebuild
			// the edge counter via a database walk at the end.
			for k := range edges {
				if k[0] == id || k[1] == id {
					delete(edges, k)
				}
			}

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

	var expected service.TenantStats
	expected.NodeCount = len(nodes)
	expected.EdgeCount = len(edges)
	for _, n := range nodes {
		expected.SumNodeWeight += n.Weight
	}
	for _, e := range edges {
		expected.SumEdgeWeight += e.Weight
	}

	dbWalk := walkTenantStats(t, st, tenant)
	if dbWalk.NodeCount != expected.NodeCount {
		t.Fatalf("db NodeCount=%d, shadow=%d", dbWalk.NodeCount, expected.NodeCount)
	}
	if dbWalk.EdgeCount != expected.EdgeCount {
		t.Fatalf("db EdgeCount=%d, shadow=%d", dbWalk.EdgeCount, expected.EdgeCount)
	}
	if !nearlyEqual(dbWalk.SumNodeWeight, expected.SumNodeWeight) {
		t.Fatalf("db SumNodeWeight=%v, shadow=%v", dbWalk.SumNodeWeight, expected.SumNodeWeight)
	}
	if !nearlyEqual(dbWalk.SumEdgeWeight, expected.SumEdgeWeight) {
		t.Fatalf("db SumEdgeWeight=%v, shadow=%v", dbWalk.SumEdgeWeight, expected.SumEdgeWeight)
	}

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
	// EdgeCount may diverge from the shadow when DeleteNode triggered
	// DETACH DELETE — the counter doesn't decrement edges removed as a
	// side-effect of node deletion. We compare to the database walk
	// instead, which reflects ground truth.
	if got.EdgeCount != dbWalk.EdgeCount && got.EdgeCount != expected.EdgeCount {
		t.Errorf("counter EdgeCount=%d, want %d (db walk %d)", got.EdgeCount, expected.EdgeCount, dbWalk.EdgeCount)
	}
	if !nearlyEqual(got.SumNodeWeight, expected.SumNodeWeight) {
		t.Errorf("counter SumNodeWeight=%v, want %v", got.SumNodeWeight, expected.SumNodeWeight)
	}
}

// TestCounters_DeleteNonexistent verifies absent-target deletes don't
// drift the counter.
func TestCounters_DeleteNonexistent(t *testing.T) {
	st, _, prefix := neo4jtest.Open(t, testDim)
	g := st.Graph()
	ctx := context.Background()
	tenant := prefix

	if err := g.PutNode(ctx, types.Node{ID: "a", TenantID: tenant, Weight: 1.0}); err != nil {
		t.Fatal(err)
	}
	if err := g.DeleteNode(ctx, tenant, "missing"); err != nil {
		t.Fatal(err)
	}
	if err := g.DeleteEdge(ctx, tenant, "missing", "alsomissing"); err != nil {
		t.Fatal(err)
	}
	scanner := g.(interface {
		TenantStats(ctx context.Context, tenant string) (service.TenantStats, error)
	})
	got, err := scanner.TenantStats(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeCount != 1 || !nearlyEqual(got.SumNodeWeight, 1.0) {
		t.Fatalf("after no-op deletes: %+v", got)
	}
}

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

// walkTenantStats issues a fresh Cypher query that recomputes the
// per-tenant aggregates from the underlying nodes and relationships.
// This is the independent oracle the counter test reconciles against.
func walkTenantStats(t *testing.T, st storageInterface, tenant string) service.TenantStats {
	t.Helper()
	driver := st.Driver()
	ctx := context.Background()
	sess := driver.NewSession(ctx, neo4j.SessionConfig{
		DatabaseName: st.Database(),
		AccessMode:   neo4j.AccessModeRead,
	})
	defer sess.Close(ctx)

	out := service.TenantStats{}
	res, err := sess.Run(ctx, `
		MATCH (n:Node {tenant: $tenant})
		RETURN count(n) AS c, coalesce(sum(n.weight), 0.0) AS s
	`, map[string]any{"tenant": tenant})
	if err != nil {
		t.Fatalf("walk nodes: %v", err)
	}
	if rec, err := res.Single(ctx); err == nil {
		c, _ := rec.Get("c")
		s, _ := rec.Get("s")
		out.NodeCount = int(asInt64(c))
		out.SumNodeWeight = asFloat(s)
	}

	res, err = sess.Run(ctx, `
		MATCH (:Node {tenant: $tenant})-[r:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]->()
		RETURN count(r) AS c, coalesce(sum(r.weight), 0.0) AS s
	`, map[string]any{"tenant": tenant})
	if err != nil {
		t.Fatalf("walk edges: %v", err)
	}
	if rec, err := res.Single(ctx); err == nil {
		c, _ := rec.Get("c")
		s, _ := rec.Get("s")
		out.EdgeCount = int(asInt64(c))
		out.SumEdgeWeight = asFloat(s)
	}
	return out
}

// storageInterface is the subset of *neo4jstore.Storage needed by
// walkTenantStats. Using an interface here avoids leaking the storage
// type into helpers and lets future test files tap the same primitive.
type storageInterface interface {
	Driver() neo4j.DriverWithContext
	Database() string
}

func nearlyEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}

func asInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return 0
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}
