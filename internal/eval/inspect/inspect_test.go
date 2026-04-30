package inspect_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cidan/memmy"
	"github.com/Cidan/memmy/internal/eval/inspect"
)

func TestInspect_RoundTripsThroughMemmyFacade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memmy.db")
	svc, closer, err := memmy.Open(memmy.Options{
		DBPath:       dbPath,
		Embedder:     memmy.NewFakeEmbedder(32),
		Clock:        memmy.NewFakeClock(time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)),
		HNSWRandSeed: 42,
	})
	if err != nil {
		t.Fatalf("memmy.Open: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}
	w, err := svc.Write(ctx, memmy.WriteRequest{
		Tenant:  tenant,
		Message: "Alpha. Beta. Gamma. Delta. Epsilon.",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(w.NodeIDs) == 0 {
		t.Fatal("no nodes produced")
	}

	r, err := inspect.Open(dbPath)
	if err != nil {
		t.Fatalf("inspect.Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	tenants, err := r.ListTenants(ctx)
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	if len(tenants) != 1 {
		t.Fatalf("got %d tenants, want 1", len(tenants))
	}
	if tenants[0].Tuple["agent"] != "ada" {
		t.Errorf("tuple=%v", tenants[0].Tuple)
	}

	tenantID := tenants[0].ID
	ids, err := r.ListNodes(ctx, tenantID)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(ids) != len(w.NodeIDs) {
		t.Errorf("ListNodes count=%d, want %d", len(ids), len(w.NodeIDs))
	}

	states, err := r.NodeStates(ctx, tenantID, w.NodeIDs)
	if err != nil {
		t.Fatalf("NodeStates: %v", err)
	}
	if len(states) != len(w.NodeIDs) {
		t.Fatalf("got %d states, want %d", len(states), len(w.NodeIDs))
	}
	for _, st := range states {
		if st.Weight <= 0 {
			t.Errorf("node %s weight=%v, want >0", st.NodeID, st.Weight)
		}
		if st.LastTouched.IsZero() {
			t.Errorf("node %s zero LastTouched", st.NodeID)
		}
	}

	st, ok, err := r.NodeState(ctx, tenantID, "no-such-node")
	if err != nil {
		t.Fatalf("NodeState missing: %v", err)
	}
	if ok {
		t.Errorf("ok=true for missing id; got %+v", st)
	}
}

func TestInspect_OpenRejectsEmptyPath(t *testing.T) {
	if _, err := inspect.Open(""); err == nil {
		t.Error("expected error for empty path")
	}
}
