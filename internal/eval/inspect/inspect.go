// Package inspect is a read-only window into a memmy Neo4j database
// used by the eval harness to capture per-node state (weight, last
// touch, edge degree) before and after a query.
//
// It opens its own bolt driver against the same Neo4j the live memmy
// service writes to. Reads run as managed read transactions and never
// interfere with the writer's session pool. This separation keeps
// memmy.MemoryService out of the harness's path and preserves the
// stateless-service contract (CLAUDE.md §0 #3).
package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// Connection bundles the Neo4j connection settings.
type Connection struct {
	URI      string
	User     string
	Password string
	Database string
}

// Reader is the read-only window. Open one against the same Neo4j
// instance the live service writes to.
type Reader struct {
	driver   neo4j.DriverWithContext
	database string
}

// Open returns a Reader pointing at conn.
func Open(conn Connection) (*Reader, error) {
	if conn.URI == "" {
		return nil, errors.New("inspect: Connection.URI required")
	}
	if conn.User == "" {
		return nil, errors.New("inspect: Connection.User required")
	}
	if conn.Password == "" {
		return nil, errors.New("inspect: Connection.Password required")
	}
	db := conn.Database
	if db == "" {
		db = "neo4j"
	}
	driver, err := neo4j.NewDriverWithContext(conn.URI, neo4j.BasicAuth(conn.User, conn.Password, ""))
	if err != nil {
		return nil, fmt.Errorf("inspect: open driver: %w", err)
	}
	verifyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := driver.VerifyConnectivity(verifyCtx); err != nil {
		_ = driver.Close(context.Background())
		return nil, fmt.Errorf("inspect: verify: %w", err)
	}
	return &Reader{driver: driver, database: db}, nil
}

// Close releases the read-side driver pool.
func (r *Reader) Close() error {
	if r.driver == nil {
		return nil
	}
	err := r.driver.Close(context.Background())
	r.driver = nil
	return err
}

// NodeState is the slice of per-node metadata the harness cares about.
type NodeState struct {
	NodeID       string
	TenantID     string
	Weight       float64
	LastTouched  time.Time
	AccessCount  uint64
	EdgeCountOut int
	EdgeCountIn  int
}

// Tenant describes one tenant the db has seen.
type Tenant struct {
	ID    string
	Tuple map[string]string
}

// ListTenants returns every tenant registered in the db.
func (r *Reader) ListTenants(ctx context.Context) ([]Tenant, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		rec, err := tx.Run(ctx, `MATCH (t:TenantInfo) RETURN t.id AS id, t.tuple_json AS tuple ORDER BY t.id ASC`, nil)
		if err != nil {
			return nil, err
		}
		var out []Tenant
		for rec.Next(ctx) {
			r := rec.Record()
			id, _ := r.Get("id")
			tupleRaw, _ := r.Get("tuple")
			tuple := decodeTupleJSON(asString(tupleRaw))
			out = append(out, Tenant{ID: asString(id), Tuple: tuple})
		}
		return out, rec.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("inspect: list tenants: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	return res.([]Tenant), nil
}

// ListNodes returns every node ID for tenant in storage order.
func (r *Reader) ListNodes(ctx context.Context, tenant string) ([]string, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		rec, err := tx.Run(ctx, `
			MATCH (n:Node {tenant: $tenant})
			RETURN n.id AS id
			ORDER BY n.id ASC
		`, map[string]any{"tenant": tenant})
		if err != nil {
			return nil, err
		}
		var out []string
		for rec.Next(ctx) {
			r := rec.Record()
			id, _ := r.Get("id")
			out = append(out, asString(id))
		}
		return out, rec.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("inspect: list nodes: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	return res.([]string), nil
}

// NodeStates fetches state for the given node IDs in one read tx.
// Missing nodes are silently omitted.
func (r *Reader) NodeStates(ctx context.Context, tenant string, ids []string) ([]NodeState, error) {
	out := make([]NodeState, 0, len(ids))
	for _, id := range ids {
		st, ok, err := r.NodeState(ctx, tenant, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, st)
		}
	}
	return out, nil
}

// NodeState reads a single node's state. (state, false, nil) when absent.
func (r *Reader) NodeState(ctx context.Context, tenant, id string) (NodeState, bool, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		rec, err := tx.Run(ctx, `
			MATCH (n:Node {tenant: $tenant, id: $id})
			OPTIONAL MATCH (n)-[r_out:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]->()
			WITH n, count(r_out) AS oc
			OPTIONAL MATCH (n)<-[r_in:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]-()
			RETURN n.id AS id, n.tenant AS tenant, n.weight AS w,
			       n.last_touched_unix_ms AS lt, n.access_count AS ac,
			       oc, count(r_in) AS ic
		`, map[string]any{"tenant": tenant, "id": id})
		if err != nil {
			return nil, err
		}
		r, err := rec.Single(ctx)
		if err != nil {
			return nil, nil
		}
		st := NodeState{}
		raw, _ := r.Get("id")
		st.NodeID = asString(raw)
		raw, _ = r.Get("tenant")
		st.TenantID = asString(raw)
		raw, _ = r.Get("w")
		st.Weight = asFloat(raw)
		raw, _ = r.Get("lt")
		ms := asInt64(raw)
		if ms != 0 {
			st.LastTouched = time.UnixMilli(ms).UTC()
		}
		raw, _ = r.Get("ac")
		st.AccessCount = uint64(asInt64(raw))
		raw, _ = r.Get("oc")
		st.EdgeCountOut = int(asInt64(raw))
		raw, _ = r.Get("ic")
		st.EdgeCountIn = int(asInt64(raw))
		return &st, nil
	})
	if err != nil {
		return NodeState{}, false, fmt.Errorf("inspect: get node: %w", err)
	}
	if res == nil {
		return NodeState{}, false, nil
	}
	return *res.(*NodeState), true, nil
}

func (r *Reader) read(ctx context.Context, fn func(tx neo4j.ManagedTransaction) (any, error)) (any, error) {
	sess := r.driver.NewSession(ctx, neo4j.SessionConfig{
		DatabaseName: r.database,
		AccessMode:   neo4j.AccessModeRead,
	})
	defer sess.Close(ctx)
	return sess.ExecuteRead(ctx, fn)
}

func decodeTupleJSON(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// asString / asInt64 / asFloat coerce bolt-driver results that may
// arrive as multiple Go types (int / int64 / float32 / float64).

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
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
