package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	gport "github.com/Cidan/memmy/internal/graph"
	"github.com/Cidan/memmy/internal/types"
)

// graphAdapter exposes Storage as the graph.Graph interface.
type graphAdapter struct{ s *Storage }

// Graph returns the graph.Graph view over this Storage.
func (s *Storage) Graph() gport.Graph { return graphAdapter{s: s} }

// ----- nodes -----

func (g graphAdapter) PutNode(ctx context.Context, n types.Node) error {
	if err := validateNode(n); err != nil {
		return err
	}
	return g.s.withWriteTx(ctx, func(tx *sql.Tx) error {
		return putNodeTxWithCounters(ctx, tx, n)
	})
}

func (g graphAdapter) GetNode(ctx context.Context, tenant, id string) (types.Node, error) {
	var out types.Node
	err := g.s.withReadTx(ctx, func(tx *sql.Tx) error {
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT blob FROM nodes WHERE tenant = ? AND id = ?`, tenant, id).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return gport.ErrNotFound
		}
		if err != nil {
			return err
		}
		return decodeNode(raw, &out)
	})
	return out, err
}

func (g graphAdapter) UpdateNode(ctx context.Context, tenant, id string, fn func(*types.Node) error) error {
	return g.s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT blob FROM nodes WHERE tenant = ? AND id = ?`, tenant, id).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return gport.ErrNotFound
		}
		if err != nil {
			return err
		}
		var n types.Node
		if err := decodeNode(raw, &n); err != nil {
			return err
		}
		oldWeight := n.Weight
		if err := fn(&n); err != nil {
			return err
		}
		buf, err := encodeNode(&n)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO nodes(tenant, id, blob) VALUES(?, ?, ?)
			ON CONFLICT(tenant, id) DO UPDATE SET blob = excluded.blob
		`, n.TenantID, n.ID, buf); err != nil {
			return err
		}
		return adjustCountersTx(ctx, tx, tenant, tenantCounters{SumNodeWeight: n.Weight - oldWeight})
	})
}

func (g graphAdapter) DeleteNode(ctx context.Context, tenant, id string) error {
	return g.s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT blob FROM nodes WHERE tenant = ? AND id = ?`, tenant, id).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var n types.Node
		if err := decodeNode(raw, &n); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE tenant = ? AND id = ?`, tenant, id); err != nil {
			return err
		}
		return adjustCountersTx(ctx, tx, tenant, tenantCounters{NodeCount: -1, SumNodeWeight: -n.Weight})
	})
}

// ----- messages -----

func (g graphAdapter) PutMessage(ctx context.Context, m types.Message) error {
	if m.ID == "" {
		return errors.New("graph: message ID required")
	}
	if m.TenantID == "" {
		return errors.New("graph: message TenantID required")
	}
	return g.s.withWriteTx(ctx, func(tx *sql.Tx) error {
		buf, err := encodeMessage(&m)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO messages(tenant, id, blob) VALUES(?, ?, ?)
			ON CONFLICT(tenant, id) DO UPDATE SET blob = excluded.blob
		`, m.TenantID, m.ID, buf)
		return err
	})
}

func (g graphAdapter) GetMessage(ctx context.Context, tenant, id string) (types.Message, error) {
	var out types.Message
	err := g.s.withReadTx(ctx, func(tx *sql.Tx) error {
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT blob FROM messages WHERE tenant = ? AND id = ?`, tenant, id).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return gport.ErrNotFound
		}
		if err != nil {
			return err
		}
		return decodeMessage(raw, &out)
	})
	return out, err
}

func (g graphAdapter) DeleteMessage(ctx context.Context, tenant, id string) error {
	return g.s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE tenant = ? AND id = ?`, tenant, id)
		return err
	})
}

// ----- edges -----

func (g graphAdapter) PutEdge(ctx context.Context, e types.MemoryEdge) error {
	if err := validateEdge(e); err != nil {
		return err
	}
	return g.s.withWriteTx(ctx, func(tx *sql.Tx) error {
		return putEdgeTxWithCounters(ctx, tx, e)
	})
}

func (g graphAdapter) GetEdge(ctx context.Context, tenant, from, to string) (types.MemoryEdge, bool, error) {
	var out types.MemoryEdge
	var found bool
	err := g.s.withReadTx(ctx, func(tx *sql.Tx) error {
		var raw []byte
		err := tx.QueryRowContext(ctx, `
			SELECT blob FROM edges_out WHERE tenant = ? AND from_id = ? AND to_id = ?
		`, tenant, from, to).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := decodeEdge(raw, &out); err != nil {
			return err
		}
		found = true
		return nil
	})
	return out, found, err
}

func (g graphAdapter) UpdateEdge(ctx context.Context, tenant, from, to string, fn func(*types.MemoryEdge) error) error {
	return g.s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var raw []byte
		err := tx.QueryRowContext(ctx, `
			SELECT blob FROM edges_out WHERE tenant = ? AND from_id = ? AND to_id = ?
		`, tenant, from, to).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return gport.ErrNotFound
		}
		if err != nil {
			return err
		}
		var e types.MemoryEdge
		if err := decodeEdge(raw, &e); err != nil {
			return err
		}
		oldWeight := e.Weight
		if err := fn(&e); err != nil {
			return err
		}
		if err := writeEdgeMirrorsTx(ctx, tx, e); err != nil {
			return err
		}
		return adjustCountersTx(ctx, tx, tenant, tenantCounters{SumEdgeWeight: e.Weight - oldWeight})
	})
}

func (g graphAdapter) DeleteEdge(ctx context.Context, tenant, from, to string) error {
	return g.s.withWriteTx(ctx, func(tx *sql.Tx) error {
		return deleteEdgeTxWithCounters(ctx, tx, tenant, from, to)
	})
}

// ----- neighbors -----

func (g graphAdapter) Neighbors(ctx context.Context, tenant, id string) ([]types.MemoryEdge, error) {
	var out []types.MemoryEdge
	err := g.s.withReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT blob FROM edges_out WHERE tenant = ? AND from_id = ?
		`, tenant, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var e types.MemoryEdge
			if err := decodeEdge(raw, &e); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

func (g graphAdapter) InboundNeighbors(ctx context.Context, tenant, id string) ([]types.MemoryEdge, error) {
	var out []types.MemoryEdge
	err := g.s.withReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT blob FROM edges_in WHERE tenant = ? AND to_id = ?
		`, tenant, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var e types.MemoryEdge
			if err := decodeEdge(raw, &e); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// ----- tenants -----

func (g graphAdapter) UpsertTenant(ctx context.Context, info types.TenantInfo) error {
	if info.ID == "" {
		return errors.New("graph: TenantInfo.ID required")
	}
	return g.s.withWriteTx(ctx, func(tx *sql.Tx) error {
		buf, err := encodeTenantInfo(&info)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO tenants(id, info) VALUES(?, ?)
			ON CONFLICT(id) DO UPDATE SET info = excluded.info
		`, info.ID, buf)
		return err
	})
}

func (g graphAdapter) GetTenant(ctx context.Context, id string) (types.TenantInfo, error) {
	var out types.TenantInfo
	err := g.s.withReadTx(ctx, func(tx *sql.Tx) error {
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT info FROM tenants WHERE id = ?`, id).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return gport.ErrNotFound
		}
		if err != nil {
			return err
		}
		return decodeTenantInfo(raw, &out)
	})
	return out, err
}

func (g graphAdapter) ListTenants(ctx context.Context) ([]types.TenantInfo, error) {
	var out []types.TenantInfo
	err := g.s.withReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT info FROM tenants ORDER BY id ASC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var t types.TenantInfo
			if err := decodeTenantInfo(raw, &t); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

func (g graphAdapter) Close() error { return g.s.Close() }

// ----- helpers -----

// writeNodeRecordTx writes a node record without touching counters.
// Used by HNSW maintenance paths that maintain their own bookkeeping.
func writeNodeRecordTx(ctx context.Context, tx *sql.Tx, n types.Node) error {
	buf, err := encodeNode(&n)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO nodes(tenant, id, blob) VALUES(?, ?, ?)
		ON CONFLICT(tenant, id) DO UPDATE SET blob = excluded.blob
	`, n.TenantID, n.ID, buf)
	return err
}

// putNodeTxWithCounters inserts (or replaces) a node and updates the
// per-tenant counter atomically.
func putNodeTxWithCounters(ctx context.Context, tx *sql.Tx, n types.Node) error {
	var oldWeight float64
	isNew := true
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT blob FROM nodes WHERE tenant = ? AND id = ?`, n.TenantID, n.ID).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var old types.Node
		if err := decodeNode(raw, &old); err != nil {
			return err
		}
		oldWeight = old.Weight
		isNew = false
	}
	if err := writeNodeRecordTx(ctx, tx, n); err != nil {
		return err
	}
	delta := tenantCounters{SumNodeWeight: n.Weight - oldWeight}
	if isNew {
		delta.NodeCount = 1
	}
	return adjustCountersTx(ctx, tx, n.TenantID, delta)
}

// writeEdgeMirrorsTx writes both edges_out (tenant, from, to) and
// edges_in (tenant, to, from) for an edge without touching counters.
// Callers maintain counters explicitly.
func writeEdgeMirrorsTx(ctx context.Context, tx *sql.Tx, e types.MemoryEdge) error {
	buf, err := encodeEdge(&e)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO edges_out(tenant, from_id, to_id, blob) VALUES(?, ?, ?, ?)
		ON CONFLICT(tenant, from_id, to_id) DO UPDATE SET blob = excluded.blob
	`, e.TenantID, e.From, e.To, buf); err != nil {
		return fmt.Errorf("eout/%s: %w", e.From, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO edges_in(tenant, to_id, from_id, blob) VALUES(?, ?, ?, ?)
		ON CONFLICT(tenant, to_id, from_id) DO UPDATE SET blob = excluded.blob
	`, e.TenantID, e.To, e.From, buf); err != nil {
		return fmt.Errorf("ein/%s: %w", e.To, err)
	}
	return nil
}

// putEdgeTxWithCounters upserts an edge in both mirrors and updates
// the counter delta atomically.
func putEdgeTxWithCounters(ctx context.Context, tx *sql.Tx, e types.MemoryEdge) error {
	var oldWeight float64
	isNew := true
	var raw []byte
	err := tx.QueryRowContext(ctx, `
		SELECT blob FROM edges_out WHERE tenant = ? AND from_id = ? AND to_id = ?
	`, e.TenantID, e.From, e.To).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var old types.MemoryEdge
		if err := decodeEdge(raw, &old); err != nil {
			return err
		}
		oldWeight = old.Weight
		isNew = false
	}
	if err := writeEdgeMirrorsTx(ctx, tx, e); err != nil {
		return err
	}
	delta := tenantCounters{SumEdgeWeight: e.Weight - oldWeight}
	if isNew {
		delta.EdgeCount = 1
	}
	return adjustCountersTx(ctx, tx, e.TenantID, delta)
}

// deleteEdgeTxWithCounters removes both edge mirrors and updates the
// counter (decrement count, subtract the deleted edge's weight).
// Absent-edge calls are silent no-ops. The eout mirror is treated as
// authoritative for "did this edge exist?"; ein delete is best-effort
// cleanup. The public Graph API never produces asymmetric mirror state
// because Put / Update / Delete all touch both mirrors inside one tx.
func deleteEdgeTxWithCounters(ctx context.Context, tx *sql.Tx, tenant, from, to string) error {
	var oldWeight float64
	existed := false
	var raw []byte
	err := tx.QueryRowContext(ctx, `
		SELECT blob FROM edges_out WHERE tenant = ? AND from_id = ? AND to_id = ?
	`, tenant, from, to).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var old types.MemoryEdge
		if err := decodeEdge(raw, &old); err != nil {
			return err
		}
		oldWeight = old.Weight
		existed = true
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM edges_out WHERE tenant = ? AND from_id = ? AND to_id = ?
		`, tenant, from, to); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM edges_in WHERE tenant = ? AND to_id = ? AND from_id = ?
	`, tenant, to, from); err != nil {
		return err
	}
	if !existed {
		return nil
	}
	return adjustCountersTx(ctx, tx, tenant, tenantCounters{EdgeCount: -1, SumEdgeWeight: -oldWeight})
}

func validateNode(n types.Node) error {
	switch {
	case n.ID == "":
		return errors.New("graph: node ID required")
	case n.TenantID == "":
		return errors.New("graph: node TenantID required")
	}
	return nil
}

func validateEdge(e types.MemoryEdge) error {
	switch {
	case e.From == "":
		return errors.New("graph: edge From required")
	case e.To == "":
		return errors.New("graph: edge To required")
	case e.From == e.To:
		return errors.New("graph: self-loop edges not permitted")
	case e.TenantID == "":
		return errors.New("graph: edge TenantID required")
	}
	return nil
}
