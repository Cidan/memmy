package sqlitestore_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/Cidan/memmy/internal/service"
	sqlitestore "github.com/Cidan/memmy/internal/storage/sqlite"
	"github.com/Cidan/memmy/internal/types"
)

// TestCounters_MatchBruteForce exercises a randomized mix of node and
// edge mutations (insert / replace / weight-update / delete) and after
// each batch asserts that the O(1) per-tenant counter equals the
// counts and weight sums computed via a fresh full-table walk.
func TestCounters_MatchBruteForce(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	ctx := context.Background()
	tenant := "t"
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

	tableWalk := walkTableStats(t, st, tenant)
	if tableWalk.NodeCount != expected.NodeCount {
		t.Fatalf("table NodeCount=%d, shadow=%d", tableWalk.NodeCount, expected.NodeCount)
	}
	if tableWalk.EdgeCount != expected.EdgeCount {
		t.Fatalf("table EdgeCount=%d, shadow=%d", tableWalk.EdgeCount, expected.EdgeCount)
	}
	if !nearlyEqual(tableWalk.SumNodeWeight, expected.SumNodeWeight) {
		t.Fatalf("table SumNodeWeight=%v, shadow=%v", tableWalk.SumNodeWeight, expected.SumNodeWeight)
	}
	if !nearlyEqual(tableWalk.SumEdgeWeight, expected.SumEdgeWeight) {
		t.Fatalf("table SumEdgeWeight=%v, shadow=%v", tableWalk.SumEdgeWeight, expected.SumEdgeWeight)
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

// walkTableStats opens a fresh sql.DB at the same path and walks the
// nodes + edges_out tables, computing counts and weight sums. This is
// the independent oracle the counter test reconciles against.
func walkTableStats(t *testing.T, st *sqlitestore.Storage, tenant string) service.TenantStats {
	t.Helper()
	probe, err := sql.Open("sqlite3", "file:"+st.Path()+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("probe open: %v", err)
	}
	defer probe.Close()

	var ts service.TenantStats
	rows, err := probe.Query(`SELECT blob FROM nodes WHERE tenant = ?`, tenant)
	if err != nil {
		t.Fatalf("nodes query: %v", err)
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		var n types.Node
		if err := sqlitestore.DecodeNodeForTest(raw, &n); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		ts.NodeCount++
		ts.SumNodeWeight += n.Weight
	}
	rows.Close()

	rows, err = probe.Query(`SELECT blob FROM edges_out WHERE tenant = ?`, tenant)
	if err != nil {
		t.Fatalf("edges_out query: %v", err)
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		var e types.MemoryEdge
		if err := sqlitestore.DecodeEdgeForTest(raw, &e); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		ts.EdgeCount++
		ts.SumEdgeWeight += e.Weight
	}
	rows.Close()
	return ts
}

func nearlyEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}
