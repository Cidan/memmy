package sqlitestore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/Cidan/memmy/internal/service"
	"github.com/Cidan/memmy/internal/types"
)

// The methods below implement the optional scanner capabilities defined
// in internal/service. They live on graphAdapter (the public Graph view)
// so non-sqlite backends can choose not to implement them.

// RecentNodeIDs returns up to maxN node IDs created at-or-after `since`,
// in DESCENDING chronological order, excluding nodes whose SourceMsgID
// matches excludeMsgID. ULIDs are lex-sortable, so we lower-bound the
// scan with a synthetic ULID derived from `since`.
func (g graphAdapter) RecentNodeIDs(ctx context.Context, tenant string, since time.Time, excludeMsgID string, maxN int) ([]string, error) {
	if maxN <= 0 {
		return nil, nil
	}
	out := make([]string, 0, maxN)
	err := g.s.withReadTx(ctx, func(tx *sql.Tx) error {
		sinceULID, err := ulid.New(uint64(since.UnixMilli()), zeroEntropy)
		if err != nil {
			return err
		}
		sinceKey := sinceULID.String()
		// Over-fetch so the excludeMsgID filter never causes a short read.
		const sqlOverfetch = 64
		rows, err := tx.QueryContext(ctx, `
			SELECT id, blob FROM nodes
			WHERE tenant = ? AND id >= ?
			ORDER BY id DESC
			LIMIT ?
		`, tenant, sinceKey, maxN+sqlOverfetch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var raw []byte
			if err := rows.Scan(&id, &raw); err != nil {
				return err
			}
			// Guard against sub-ms skew between the monotonic ULID
			// generator and `since`.
			if bytes.Compare([]byte(id), []byte(sinceKey)) < 0 {
				continue
			}
			var n types.Node
			if err := decodeNode(raw, &n); err != nil {
				return err
			}
			if n.SourceMsgID == excludeMsgID {
				continue
			}
			out = append(out, id)
			if len(out) >= maxN {
				break
			}
		}
		return rows.Err()
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return nil, err
	}
	return out, nil
}

// NodesForMessage returns every node whose SourceMsgID matches msgID.
// Full-tenant scan is acceptable here because Forget is a rare admin op.
func (g graphAdapter) NodesForMessage(ctx context.Context, tenant, msgID string) ([]types.Node, error) {
	var out []types.Node
	err := g.s.withReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT blob FROM nodes WHERE tenant = ?`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var n types.Node
			if err := decodeNode(raw, &n); err != nil {
				return err
			}
			if n.SourceMsgID == msgID {
				out = append(out, n)
			}
		}
		return rows.Err()
	})
	return out, err
}

// MessageIDsBefore returns every message ID with CreatedAt strictly
// before `before`.
func (g graphAdapter) MessageIDsBefore(ctx context.Context, tenant string, before time.Time) ([]string, error) {
	var out []string
	err := g.s.withReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT blob FROM messages WHERE tenant = ?`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var m types.Message
			if err := decodeMessage(raw, &m); err != nil {
				return err
			}
			if m.CreatedAt.Before(before) {
				out = append(out, m.ID)
			}
		}
		return rows.Err()
	})
	return out, err
}

// TenantStats reads the per-tenant counter row (maintained
// transactionally by every Graph mutation) plus HNSWMeta. O(1) — does
// NOT walk the edges or nodes tables.
func (g graphAdapter) TenantStats(ctx context.Context, tenant string) (service.TenantStats, error) {
	var ts service.TenantStats
	err := g.s.withReadTx(ctx, func(tx *sql.Tx) error {
		c, err := readCountersTx(ctx, tx, tenant)
		if err != nil {
			return err
		}
		ts.NodeCount = c.NodeCount
		ts.EdgeCount = c.EdgeCount
		ts.SumNodeWeight = c.SumNodeWeight
		ts.SumEdgeWeight = c.SumEdgeWeight
		meta, ok, err := readHNSWMetaTx(ctx, tx, tenant)
		if err != nil {
			return err
		}
		if ok {
			ts.HNSWSize = meta.Size
		}
		return nil
	})
	return ts, err
}

// zeroEntropy is a fixed entropy source used to construct lower-bound
// ULIDs from a millisecond timestamp.
var zeroEntropy = ulid.Monotonic(zeroReader{}, 0)

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = 0
	}
	return len(b), nil
}
