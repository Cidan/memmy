// Package inspect is a read-only window into a memmy SQLite database
// used by the eval harness to capture per-node state (weight, last
// touch, edge degree) before and after a query.
//
// It opens the same db file as the live memmy service in `mode=ro`
// (independent connection pool; never touches the writer's lock) and
// decodes the gob-encoded record blobs the SQLite backend produces.
// This separation keeps memmy.MemoryService out of the harness's path
// and preserves the stateless-service contract (CLAUDE.md §0 #3).
package inspect

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/Cidan/memmy/internal/types"
)

// Reader is the read-only window. Open one against the same .sqlite
// file the live service writes to.
type Reader struct {
	db *sql.DB
}

// Open returns a Reader pointing at path.
func Open(path string) (*Reader, error) {
	if path == "" {
		return nil, errors.New("inspect: path required")
	}
	v := url.Values{}
	v.Set("mode", "ro")
	v.Set("_journal_mode", "WAL")
	v.Set("_busy_timeout", "5000")
	v.Set("_query_only", "1")
	dsn := "file:" + path + "?" + v.Encode()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("inspect: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("inspect: ping: %w", err)
	}
	db.SetMaxOpenConns(2)
	return &Reader{db: db}, nil
}

// Close releases the read handle.
func (r *Reader) Close() error {
	if r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
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
	rows, err := r.db.QueryContext(ctx, `SELECT id, info FROM tenants ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("inspect: list tenants: %w", err)
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var (
			id  string
			raw []byte
		)
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var info types.TenantInfo
		if err := gobDecode(raw, &info); err != nil {
			return nil, fmt.Errorf("inspect: decode tenant: %w", err)
		}
		out = append(out, Tenant{ID: id, Tuple: info.Tuple})
	}
	return out, rows.Err()
}

// ListNodes returns every node ID for tenant in storage order.
func (r *Reader) ListNodes(ctx context.Context, tenant string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM nodes WHERE tenant=? ORDER BY id ASC`, tenant)
	if err != nil {
		return nil, fmt.Errorf("inspect: list nodes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
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
	var raw []byte
	err := r.db.QueryRowContext(ctx, `SELECT blob FROM nodes WHERE tenant=? AND id=?`, tenant, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeState{}, false, nil
	}
	if err != nil {
		return NodeState{}, false, fmt.Errorf("inspect: get node: %w", err)
	}
	var n types.Node
	if err := gobDecode(raw, &n); err != nil {
		return NodeState{}, false, fmt.Errorf("inspect: decode node: %w", err)
	}
	st := NodeState{
		NodeID:      n.ID,
		TenantID:    n.TenantID,
		Weight:      n.Weight,
		LastTouched: n.LastTouched,
		AccessCount: n.AccessCount,
	}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges_out WHERE tenant=? AND from_id=?`, tenant, id).Scan(&st.EdgeCountOut); err != nil {
		return NodeState{}, false, fmt.Errorf("inspect: count out edges: %w", err)
	}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges_in WHERE tenant=? AND to_id=?`, tenant, id).Scan(&st.EdgeCountIn); err != nil {
		return NodeState{}, false, fmt.Errorf("inspect: count in edges: %w", err)
	}
	return st, true, nil
}

func gobDecode(b []byte, out any) error {
	return gob.NewDecoder(bytes.NewReader(b)).Decode(out)
}
