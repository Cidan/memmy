package inspect_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Cidan/memmy"
	"github.com/Cidan/memmy/internal/eval/inspect"
	"github.com/Cidan/memmy/internal/storage/neo4j/neo4jtest"
)

// inspect opens its own driver against the same Neo4j the live memmy
// service writes to. Bolt allows many concurrent readers and writers
// against one database; this test exercises that contract end-to-end —
// write through the live service while inspect is open, then confirm
// inspect sees the new state.
func TestInspect_SeesWritesFromConcurrentService(t *testing.T) {
	_, conn, prefix := neo4jtest.Open(t, 32)
	svc, closer, err := memmy.Open(context.Background(), memmy.Options{
		Neo4j: memmy.Neo4jOptions{
			URI:      conn.URI,
			User:     conn.User,
			Password: conn.Password,
			Database: conn.Database,
		},
		Embedder:           memmy.NewFakeEmbedder(32),
		Clock:              memmy.NewFakeClock(time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)),
		SkipMigrationCheck: true,
	})
	if err != nil {
		t.Fatalf("memmy.Open: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	r, err := inspect.Open(inspect.Connection{
		URI: conn.URI, User: conn.User, Password: conn.Password, Database: conn.Database,
	})
	if err != nil {
		t.Fatalf("inspect.Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	ctx := context.Background()
	tenant := map[string]string{"agent": prefix}

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
	var tenantID string
	for _, te := range tenants {
		if te.Tuple["agent"] == prefix {
			tenantID = te.ID
			break
		}
	}
	if tenantID == "" {
		t.Fatalf("did not find our tenant in %d listed", len(tenants))
	}

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
	_, conn, prefix := neo4jtest.Open(t, 32)
	svc, closer, err := memmy.Open(context.Background(), memmy.Options{
		Neo4j: memmy.Neo4jOptions{
			URI:      conn.URI,
			User:     conn.User,
			Password: conn.Password,
			Database: conn.Database,
		},
		Embedder:           memmy.NewFakeEmbedder(32),
		SkipMigrationCheck: true,
	})
	if err != nil {
		t.Fatalf("memmy.Open: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	ctx := context.Background()
	tenant := map[string]string{"agent": prefix}
	w, err := svc.Write(ctx, memmy.WriteRequest{Tenant: tenant, Message: "S1. S2. S3. S4. S5."})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	r, err := inspect.Open(inspect.Connection{
		URI: conn.URI, User: conn.User, Password: conn.Password, Database: conn.Database,
	})
	if err != nil {
		t.Fatalf("inspect.Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	tenants, _ := r.ListTenants(ctx)
	var tenantID string
	for _, te := range tenants {
		if te.Tuple["agent"] == prefix {
			tenantID = te.ID
			break
		}
	}

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
	_, conn, _ := neo4jtest.Open(t, 32)
	r, err := inspect.Open(inspect.Connection{
		URI: conn.URI, User: conn.User, Password: conn.Password, Database: conn.Database,
	})
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
