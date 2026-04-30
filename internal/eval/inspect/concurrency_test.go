package inspect_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Cidan/memmy"
	"github.com/Cidan/memmy/internal/eval/inspect"
)

// inspect opens the memmy SQLite db with mode=ro through a SECOND
// connection pool. The whole architectural reason this is allowed is
// SQLite's WAL mode: many readers + one writer concurrently. This test
// exercises that contract end-to-end — write through the live service
// while inspect is open, then confirm inspect sees the new state.
func TestInspect_SeesWritesFromConcurrentService(t *testing.T) {
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

	r, err := inspect.Open(dbPath)
	if err != nil {
		t.Fatalf("inspect.Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}

	var (
		writeWG sync.WaitGroup
		nodeIDs []string
		writeMu sync.Mutex
	)
	for i := range 4 {
		writeWG.Go(func() {
			w, err := svc.Write(ctx, memmy.WriteRequest{
				Tenant:  tenant,
				Message: "Alpha. Beta. Gamma. Delta. Epsilon.",
				Metadata: map[string]string{
					"i": "msg-" + itoa(i),
				},
			})
			if err != nil {
				t.Errorf("Write %d: %v", i, err)
				return
			}
			writeMu.Lock()
			nodeIDs = append(nodeIDs, w.NodeIDs...)
			writeMu.Unlock()
		})
	}
	writeWG.Wait()
	if len(nodeIDs) == 0 {
		t.Fatal("no nodes written")
	}

	tenants, err := r.ListTenants(ctx)
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	if len(tenants) != 1 {
		t.Fatalf("got %d tenants, want 1", len(tenants))
	}
	tenantID := tenants[0].ID

	states, err := r.NodeStates(ctx, tenantID, nodeIDs)
	if err != nil {
		t.Fatalf("NodeStates: %v", err)
	}
	if len(states) != len(nodeIDs) {
		t.Errorf("inspect saw %d states; expected %d (concurrent writes)", len(states), len(nodeIDs))
	}
}

// NodeStates(unknown ids...) silently omits unknown rather than
// erroring — important contract for the harness which can pass a hit
// list that includes nodes that aged out of the db.
func TestInspect_NodeStatesSilentlyOmitsUnknownIDs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memmy.db")
	svc, closer, err := memmy.Open(memmy.Options{
		DBPath:       dbPath,
		Embedder:     memmy.NewFakeEmbedder(32),
		HNSWRandSeed: 99,
	})
	if err != nil {
		t.Fatalf("memmy.Open: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}
	w, err := svc.Write(ctx, memmy.WriteRequest{Tenant: tenant, Message: "S1. S2. S3. S4. S5."})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	r, err := inspect.Open(dbPath)
	if err != nil {
		t.Fatalf("inspect.Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	tenants, _ := r.ListTenants(ctx)
	tenantID := tenants[0].ID

	mixed := append([]string{"no-such", "also-missing"}, w.NodeIDs[0])
	states, err := r.NodeStates(ctx, tenantID, mixed)
	if err != nil {
		t.Fatalf("NodeStates: %v", err)
	}
	if len(states) != 1 {
		t.Errorf("got %d states for [unknown, unknown, real]; want 1", len(states))
	}
	if states[0].NodeID != w.NodeIDs[0] {
		t.Errorf("returned wrong node: %v", states[0])
	}
}

func TestInspect_ListNodes_EmptyTenantReturnsEmpty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memmy.db")
	svc, closer, err := memmy.Open(memmy.Options{
		DBPath:       dbPath,
		Embedder:     memmy.NewFakeEmbedder(32),
		HNSWRandSeed: 11,
	})
	if err != nil {
		t.Fatalf("memmy.Open: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	// Materialize a tenant so the db has the tenant row but zero nodes.
	if _, err := svc.Stats(context.Background(), memmy.StatsRequest{
		Tenant: map[string]string{"agent": "noone"},
	}); err != nil {
		t.Fatalf("Stats: %v", err)
	}

	r, err := inspect.Open(dbPath)
	if err != nil {
		t.Fatalf("inspect.Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	ids, err := r.ListNodes(context.Background(), "no-such-tenant-id")
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("got %d ids, want 0", len(ids))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
