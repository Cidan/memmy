package sqlitestore_test

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	gport "github.com/Cidan/memmy/internal/graph"
	"github.com/Cidan/memmy/internal/types"
)

func TestGraph_NodeCRUD(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	ctx := context.Background()

	tenant := "tenant-a"
	n := types.Node{
		ID:           "node-1",
		TenantID:     tenant,
		SourceMsgID:  "msg-1",
		SentenceSpan: [2]int{0, 3},
		Text:         "hello world",
		EmbeddingDim: 8,
		CreatedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		LastTouched:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Weight:       1.0,
	}
	if err := g.PutNode(ctx, n); err != nil {
		t.Fatalf("PutNode: %v", err)
	}

	got, err := g.GetNode(ctx, tenant, n.ID)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.Text != "hello world" || got.SentenceSpan != [2]int{0, 3} {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	if err := g.UpdateNode(ctx, tenant, n.ID, func(n *types.Node) error {
		n.Weight = 2.5
		n.AccessCount = 7
		return nil
	}); err != nil {
		t.Fatalf("UpdateNode: %v", err)
	}
	got, _ = g.GetNode(ctx, tenant, n.ID)
	if got.Weight != 2.5 || got.AccessCount != 7 {
		t.Fatalf("update did not persist: %+v", got)
	}

	if err := g.DeleteNode(ctx, tenant, n.ID); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	if _, err := g.GetNode(ctx, tenant, n.ID); !errors.Is(err, gport.ErrNotFound) {
		t.Fatalf("after delete err = %v, want ErrNotFound", err)
	}
}

func TestGraph_NodeUpdate_NotFound(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	err := g.UpdateNode(context.Background(), "t", "missing", func(*types.Node) error { return nil })
	if !errors.Is(err, gport.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGraph_Message(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	ctx := context.Background()
	tenant := "tenant-a"

	m := types.Message{
		ID:        "msg-1",
		TenantID:  tenant,
		Text:      "hello world",
		Metadata:  map[string]string{"src": "test"},
		CreatedAt: time.Now().UTC(),
	}
	if err := g.PutMessage(ctx, m); err != nil {
		t.Fatalf("PutMessage: %v", err)
	}
	got, err := g.GetMessage(ctx, tenant, m.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Text != m.Text || got.Metadata["src"] != "test" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestGraph_Edge_DualMirrorAtomic(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	ctx := context.Background()
	tenant := "tenant-a"

	e := types.MemoryEdge{
		From:        "a",
		To:          "b",
		TenantID:    tenant,
		Kind:        types.EdgeStructural,
		Weight:      1.0,
		LastTouched: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		CreatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := g.PutEdge(ctx, e); err != nil {
		t.Fatalf("PutEdge: %v", err)
	}

	out, err := g.Neighbors(ctx, tenant, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].To != "b" {
		t.Fatalf("outbound from a = %+v", out)
	}

	in, err := g.InboundNeighbors(ctx, tenant, "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 1 || in[0].From != "a" {
		t.Fatalf("inbound to b = %+v", in)
	}

	if err := g.UpdateEdge(ctx, tenant, "a", "b", func(e *types.MemoryEdge) error {
		e.Weight = 5.0
		e.AccessCount = 3
		return nil
	}); err != nil {
		t.Fatalf("UpdateEdge: %v", err)
	}
	out, _ = g.Neighbors(ctx, tenant, "a")
	in, _ = g.InboundNeighbors(ctx, tenant, "b")
	if out[0].Weight != 5.0 || in[0].Weight != 5.0 {
		t.Fatalf("weights diverged: out=%+v in=%+v", out, in)
	}
	if out[0].AccessCount != 3 || in[0].AccessCount != 3 {
		t.Fatalf("counts diverged")
	}

	if err := g.DeleteEdge(ctx, tenant, "a", "b"); err != nil {
		t.Fatal(err)
	}
	out, _ = g.Neighbors(ctx, tenant, "a")
	in, _ = g.InboundNeighbors(ctx, tenant, "b")
	if len(out) != 0 || len(in) != 0 {
		t.Fatalf("after delete: out=%v in=%v", out, in)
	}
}

func TestGraph_Edge_GetEdge(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	ctx := context.Background()
	tenant := "tenant-a"

	if _, ok, err := g.GetEdge(ctx, tenant, "x", "y"); err != nil || ok {
		t.Fatalf("missing edge: ok=%v err=%v", ok, err)
	}

	e := types.MemoryEdge{From: "x", To: "y", TenantID: tenant, Weight: 1, Kind: types.EdgeCoRetrieval}
	if err := g.PutEdge(ctx, e); err != nil {
		t.Fatal(err)
	}
	got, ok, err := g.GetEdge(ctx, tenant, "x", "y")
	if err != nil || !ok {
		t.Fatalf("after put: ok=%v err=%v", ok, err)
	}
	if got.Kind != types.EdgeCoRetrieval {
		t.Fatalf("kind=%v", got.Kind)
	}
}

func TestGraph_Edge_UpdateEdge_NotFound(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	err := g.UpdateEdge(context.Background(), "t", "a", "b", func(*types.MemoryEdge) error { return nil })
	if !errors.Is(err, gport.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGraph_Edge_RejectsSelfLoop(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	err := g.PutEdge(context.Background(), types.MemoryEdge{From: "a", To: "a", TenantID: "t"})
	if err == nil {
		t.Fatal("expected error for self-loop")
	}
}

func TestGraph_Neighbors_MultipleEdges(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	ctx := context.Background()
	tenant := "tenant-a"

	for _, to := range []string{"b", "c", "d"} {
		err := g.PutEdge(ctx, types.MemoryEdge{From: "a", To: to, TenantID: tenant, Weight: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	out, err := g.Neighbors(ctx, tenant, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("len=%d, want 3; %v", len(out), out)
	}
	tos := make([]string, len(out))
	for i, e := range out {
		tos[i] = e.To
	}
	sort.Strings(tos)
	if tos[0] != "b" || tos[1] != "c" || tos[2] != "d" {
		t.Fatalf("tos=%v", tos)
	}
}

func TestGraph_Tenants(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	ctx := context.Background()

	if _, err := g.GetTenant(ctx, "missing"); !errors.Is(err, gport.ErrNotFound) {
		t.Fatalf("missing tenant err = %v", err)
	}

	for _, id := range []string{"t-a", "t-b", "t-c"} {
		err := g.UpsertTenant(ctx, types.TenantInfo{
			ID:        id,
			Tuple:     map[string]string{"agent": id},
			CreatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := g.ListTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
}

// TestGraph_UpdateNode_ClosureErrorAborts asserts that returning a
// non-nil error from the UpdateNode closure rolls back the underlying
// SQLite transaction so the previous state is preserved.
func TestGraph_UpdateNode_ClosureErrorAborts(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	ctx := context.Background()
	tenant := "t"

	n := types.Node{ID: "n", TenantID: tenant, Weight: 1.0}
	if err := g.PutNode(ctx, n); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("boom")
	err := g.UpdateNode(ctx, tenant, "n", func(n *types.Node) error {
		n.Weight = 999 // would-be change
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want boom", err)
	}
	got, _ := g.GetNode(ctx, tenant, "n")
	if got.Weight != 1.0 {
		t.Fatalf("closure error did not abort: weight=%v", got.Weight)
	}
}

// TestGraph_Edge_UpdateEdge_ClosureErrorAborts confirms the same
// rollback semantics for UpdateEdge — both edges_out and edges_in
// must remain at their pre-closure state.
func TestGraph_Edge_UpdateEdge_ClosureErrorAborts(t *testing.T) {
	st := openTestStorage(t, 8)
	g := st.Graph()
	ctx := context.Background()
	tenant := "t"

	if err := g.PutEdge(ctx, types.MemoryEdge{
		From: "a", To: "b", TenantID: tenant, Weight: 1.0, Kind: types.EdgeStructural,
	}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("nope")
	err := g.UpdateEdge(ctx, tenant, "a", "b", func(e *types.MemoryEdge) error {
		e.Weight = 999
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want nope", err)
	}
	out, _ := g.Neighbors(ctx, tenant, "a")
	in, _ := g.InboundNeighbors(ctx, tenant, "b")
	if out[0].Weight != 1.0 || in[0].Weight != 1.0 {
		t.Fatalf("closure error did not abort: out=%v in=%v", out[0].Weight, in[0].Weight)
	}
}
