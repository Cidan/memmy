package neo4jstore

import (
	"context"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// tenantCounters tracks per-tenant aggregate counts. Maintained
// transactionally with every Graph mutation so TenantStats is O(1).
type tenantCounters struct {
	NodeCount     int64
	EdgeCount     int64
	SumNodeWeight float64
	SumEdgeWeight float64
}

// adjustCountersTx applies a delta to the per-tenant Counter node
// inside an existing managed write transaction. MERGE creates the
// node on first touch with zero baseline so the SET delta is safe.
func adjustCountersTx(ctx context.Context, tx neo4j.ManagedTransaction, tenant string, delta tenantCounters) error {
	_, err := tx.Run(ctx, `
		MERGE (c:Counter {tenant: $tenant})
		ON CREATE SET c.node_count = 0, c.edge_count = 0,
		              c.sum_node_weight = 0.0, c.sum_edge_weight = 0.0
		SET c.node_count       = c.node_count       + $dn,
		    c.edge_count       = c.edge_count       + $de,
		    c.sum_node_weight  = c.sum_node_weight  + $dnw,
		    c.sum_edge_weight  = c.sum_edge_weight  + $dew
	`, map[string]any{
		"tenant": tenant,
		"dn":     delta.NodeCount,
		"de":     delta.EdgeCount,
		"dnw":    delta.SumNodeWeight,
		"dew":    delta.SumEdgeWeight,
	})
	return err
}

// readCountersTx reads the current Counter node inside a tx. Missing
// node returns zeros (no error).
func readCountersTx(ctx context.Context, tx neo4j.ManagedTransaction, tenant string) (tenantCounters, error) {
	r, err := tx.Run(ctx, `
		MATCH (c:Counter {tenant: $tenant})
		RETURN c.node_count AS nc, c.edge_count AS ec,
		       c.sum_node_weight AS nw, c.sum_edge_weight AS ew
	`, map[string]any{"tenant": tenant})
	if err != nil {
		return tenantCounters{}, err
	}
	rec, err := r.Single(ctx)
	if err != nil {
		return tenantCounters{}, nil
	}
	nc, _ := rec.Get("nc")
	ec, _ := rec.Get("ec")
	nw, _ := rec.Get("nw")
	ew, _ := rec.Get("ew")
	return tenantCounters{
		NodeCount:     asInt64(nc),
		EdgeCount:     asInt64(ec),
		SumNodeWeight: asFloat(nw),
		SumEdgeWeight: asFloat(ew),
	}, nil
}
