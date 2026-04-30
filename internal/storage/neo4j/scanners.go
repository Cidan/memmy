package neo4jstore

import (
	"context"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/Cidan/memmy/internal/service"
	"github.com/Cidan/memmy/internal/types"
)

// The methods below implement the optional scanner capabilities the
// service package duck-types against the graph adapter. They live on
// graphAdapter (the Graph interface view) so non-Neo4j backends can
// choose not to implement them.

// RecentNodeIDs returns up to maxN node IDs created at-or-after
// `since`, in descending chronological order, excluding nodes whose
// SourceMsgID matches excludeMsgID.
func (g graphAdapter) RecentNodeIDs(ctx context.Context, tenant string, since time.Time, excludeMsgID string, maxN int) ([]string, error) {
	if maxN <= 0 {
		return nil, nil
	}
	res, err := g.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `
			MATCH (n:Node {tenant: $tenant})
			WHERE n.created_at_unix_ms >= $since
			  AND (n.source_msg_id IS NULL OR n.source_msg_id <> $exclude)
			RETURN n.id AS id
			ORDER BY n.created_at_unix_ms DESC, n.id DESC
			LIMIT $maxN
		`, map[string]any{
			"tenant":  tenant,
			"since":   since.UnixMilli(),
			"exclude": excludeMsgID,
			"maxN":    int64(maxN),
		})
		if err != nil {
			return nil, err
		}
		var out []string
		for r.Next(ctx) {
			rec := r.Record()
			raw, _ := rec.Get("id")
			out = append(out, asString(raw))
		}
		return out, r.Err()
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	return res.([]string), nil
}

// NodesForMessage returns every node whose SourceMsgID matches msgID.
// Used by Forget to cascade-delete a message's chunks.
func (g graphAdapter) NodesForMessage(ctx context.Context, tenant, msgID string) ([]types.Node, error) {
	res, err := g.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `
			MATCH (n:Node {tenant: $tenant, source_msg_id: $msg})
			RETURN n
		`, map[string]any{"tenant": tenant, "msg": msgID})
		if err != nil {
			return nil, err
		}
		var out []types.Node
		for r.Next(ctx) {
			rec := r.Record()
			raw, _ := rec.Get("n")
			node, ok := raw.(neo4j.Node)
			if !ok {
				continue
			}
			out = append(out, decodeNodeProps(node.Props))
		}
		return out, r.Err()
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	return res.([]types.Node), nil
}

// MessageIDsBefore returns every message ID with CreatedAt strictly
// before `before`. Used by Forget(Before:) to cascade-delete an
// entire time slice of memory.
func (g graphAdapter) MessageIDsBefore(ctx context.Context, tenant string, before time.Time) ([]string, error) {
	res, err := g.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `
			MATCH (m:Message {tenant: $tenant})
			WHERE m.created_at_unix_ms < $before
			RETURN m.id AS id
		`, map[string]any{"tenant": tenant, "before": before.UnixMilli()})
		if err != nil {
			return nil, err
		}
		var out []string
		for r.Next(ctx) {
			rec := r.Record()
			raw, _ := rec.Get("id")
			out = append(out, asString(raw))
		}
		return out, r.Err()
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	return res.([]string), nil
}

// TenantStats reads the per-tenant Counter node in O(1).
func (g graphAdapter) TenantStats(ctx context.Context, tenant string) (service.TenantStats, error) {
	var ts service.TenantStats
	_, err := g.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		c, err := readCountersTx(ctx, tx, tenant)
		if err != nil {
			return nil, err
		}
		ts.NodeCount = int(c.NodeCount)
		ts.EdgeCount = int(c.EdgeCount)
		ts.SumNodeWeight = c.SumNodeWeight
		ts.SumEdgeWeight = c.SumEdgeWeight
		// HNSWSize is the live count of non-tombstoned, embedded nodes
		// — Neo4j has no separate HNSW size; we report the same number
		// the vector index would search over.
		r, err := tx.Run(ctx, `
			MATCH (n:Node {tenant: $tenant})
			WHERE coalesce(n.tombstoned, false) = false AND n.embedding IS NOT NULL
			RETURN count(n) AS c
		`, map[string]any{"tenant": tenant})
		if err != nil {
			return nil, err
		}
		rec, err := r.Single(ctx)
		if err == nil {
			raw, _ := rec.Get("c")
			ts.HNSWSize = asInt(raw)
		}
		return nil, nil
	})
	return ts, err
}
