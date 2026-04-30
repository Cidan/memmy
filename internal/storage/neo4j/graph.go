package neo4jstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	gport "github.com/Cidan/memmy/internal/graph"
	"github.com/Cidan/memmy/internal/types"
)

// graphAdapter exposes Storage as the graph.Graph interface. The
// adapter owns Node, Message, TenantInfo, Counter, and the
// :STRUCTURAL/:CORETRIEVAL/:COTRAVERSAL relationship types. The
// vector index ownership lives in vectorindex.go.
type graphAdapter struct{ s *Storage }

// Graph returns the graph.Graph view over this Storage.
func (s *Storage) Graph() gport.Graph { return graphAdapter{s: s} }

// ----- nodes -----

func (g graphAdapter) PutNode(ctx context.Context, n types.Node) error {
	if err := validateNode(n); err != nil {
		return err
	}
	_, err := g.s.withWriteSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return nil, putNodeTx(ctx, tx, n)
	})
	return err
}

func (g graphAdapter) GetNode(ctx context.Context, tenant, id string) (types.Node, error) {
	res, err := g.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `
			MATCH (n:Node {tenant: $tenant, id: $id})
			RETURN n
		`, map[string]any{"tenant": tenant, "id": id})
		if err != nil {
			return types.Node{}, err
		}
		rec, err := r.Single(ctx)
		if err != nil {
			return types.Node{}, gport.ErrNotFound
		}
		raw, _ := rec.Get("n")
		node, ok := raw.(neo4j.Node)
		if !ok {
			return types.Node{}, fmt.Errorf("neo4jstore: unexpected node type %T", raw)
		}
		return decodeNodeProps(node.Props), nil
	})
	if err != nil {
		return types.Node{}, err
	}
	return res.(types.Node), nil
}

func (g graphAdapter) UpdateNode(ctx context.Context, tenant, id string, fn func(*types.Node) error) error {
	_, err := g.s.withWriteSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `
			MATCH (n:Node {tenant: $tenant, id: $id})
			RETURN n
		`, map[string]any{"tenant": tenant, "id": id})
		if err != nil {
			return nil, err
		}
		rec, err := r.Single(ctx)
		if err != nil {
			return nil, gport.ErrNotFound
		}
		raw, _ := rec.Get("n")
		nodeRec, ok := raw.(neo4j.Node)
		if !ok {
			return nil, fmt.Errorf("neo4jstore: unexpected node type %T", raw)
		}
		current := decodeNodeProps(nodeRec.Props)
		oldWeight := current.Weight
		if err := fn(&current); err != nil {
			return nil, err
		}
		props := encodeNodePropsForSet(current)
		if _, err := tx.Run(ctx, `
			MATCH (n:Node {tenant: $tenant, id: $id})
			SET n += $props
		`, map[string]any{
			"tenant": tenant,
			"id":     id,
			"props":  props,
		}); err != nil {
			return nil, err
		}
		return nil, adjustCountersTx(ctx, tx, tenant, tenantCounters{
			SumNodeWeight: current.Weight - oldWeight,
		})
	})
	return err
}

func (g graphAdapter) DeleteNode(ctx context.Context, tenant, id string) error {
	_, err := g.s.withWriteSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		// Read the node weight plus the count and weight-sum of every
		// edge attached to it so we can drop them from the counter
		// before the DETACH DELETE silently removes the relationships.
		// An undirected pattern matches each attached edge exactly once
		// (self-loops are forbidden, so no double-count risk).
		r, err := tx.Run(ctx, `
			MATCH (n:Node {tenant: $tenant, id: $id})
			OPTIONAL MATCH (n)-[r:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]-()
			RETURN n.weight AS w,
			       count(r) AS ec,
			       coalesce(sum(r.weight), 0.0) AS esw
		`, map[string]any{"tenant": tenant, "id": id})
		if err != nil {
			return nil, err
		}
		rec, err := r.Single(ctx)
		if err != nil {
			return nil, nil // absent -> silent
		}
		wRaw, _ := rec.Get("w")
		ecRaw, _ := rec.Get("ec")
		eswRaw, _ := rec.Get("esw")
		w := asFloat(wRaw)
		ec := asInt64(ecRaw)
		esw := asFloat(eswRaw)

		if _, err := tx.Run(ctx, `
			MATCH (n:Node {tenant: $tenant, id: $id})
			DETACH DELETE n
		`, map[string]any{"tenant": tenant, "id": id}); err != nil {
			return nil, err
		}
		return nil, adjustCountersTx(ctx, tx, tenant, tenantCounters{
			NodeCount:     -1,
			SumNodeWeight: -w,
			EdgeCount:     -ec,
			SumEdgeWeight: -esw,
		})
	})
	return err
}

// putNodeTx implements PutNode inside an existing tx. Counter delta
// covers both the new-row and the upsert paths.
func putNodeTx(ctx context.Context, tx neo4j.ManagedTransaction, n types.Node) error {
	r, err := tx.Run(ctx, `
		MATCH (n:Node {tenant: $tenant, id: $id})
		RETURN n.weight AS w
	`, map[string]any{"tenant": n.TenantID, "id": n.ID})
	if err != nil {
		return err
	}
	rec, err := r.Single(ctx)
	var oldWeight float64
	isNew := true
	if err == nil {
		isNew = false
		wRaw, _ := rec.Get("w")
		oldWeight = asFloat(wRaw)
	}
	props := encodeNodePropsForSet(n)
	if _, err := tx.Run(ctx, `
		MERGE (n:Node {tenant: $tenant, id: $id})
		SET n += $props
	`, map[string]any{
		"tenant": n.TenantID,
		"id":     n.ID,
		"props":  props,
	}); err != nil {
		return err
	}
	delta := tenantCounters{SumNodeWeight: n.Weight - oldWeight}
	if isNew {
		delta.NodeCount = 1
	}
	return adjustCountersTx(ctx, tx, n.TenantID, delta)
}

// ----- messages -----

func (g graphAdapter) PutMessage(ctx context.Context, m types.Message) error {
	if m.ID == "" {
		return errors.New("graph: message ID required")
	}
	if m.TenantID == "" {
		return errors.New("graph: message TenantID required")
	}
	metaRaw, err := json.Marshal(m.Metadata)
	if err != nil {
		return fmt.Errorf("neo4jstore: marshal metadata: %w", err)
	}
	_, err = g.s.withWriteSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(ctx, `
			MERGE (m:Message {tenant: $tenant, id: $id})
			SET m.text = $text,
			    m.metadata_json = $metadata,
			    m.created_at_unix_ms = $created
		`, map[string]any{
			"tenant":   m.TenantID,
			"id":       m.ID,
			"text":     m.Text,
			"metadata": string(metaRaw),
			"created":  m.CreatedAt.UnixMilli(),
		})
		return nil, err
	})
	return err
}

func (g graphAdapter) GetMessage(ctx context.Context, tenant, id string) (types.Message, error) {
	res, err := g.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `
			MATCH (m:Message {tenant: $tenant, id: $id}) RETURN m
		`, map[string]any{"tenant": tenant, "id": id})
		if err != nil {
			return types.Message{}, err
		}
		rec, err := r.Single(ctx)
		if err != nil {
			return types.Message{}, gport.ErrNotFound
		}
		raw, _ := rec.Get("m")
		node, ok := raw.(neo4j.Node)
		if !ok {
			return types.Message{}, fmt.Errorf("neo4jstore: unexpected node type %T", raw)
		}
		return decodeMessageProps(node.Props), nil
	})
	if err != nil {
		return types.Message{}, err
	}
	return res.(types.Message), nil
}

func (g graphAdapter) DeleteMessage(ctx context.Context, tenant, id string) error {
	_, err := g.s.withWriteSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(ctx, `
			MATCH (m:Message {tenant: $tenant, id: $id})
			DETACH DELETE m
		`, map[string]any{"tenant": tenant, "id": id})
		return nil, err
	})
	return err
}

// ----- edges -----
//
// memmy's MemoryEdge.Kind picks the relationship type. Single
// directed edges (no dual mirror), so Neighbors is an outbound MATCH
// and InboundNeighbors is an inbound MATCH. Concurrent updates use
// MERGE on the rel pattern.

func (g graphAdapter) PutEdge(ctx context.Context, e types.MemoryEdge) error {
	if err := validateEdge(e); err != nil {
		return err
	}
	_, err := g.s.withWriteSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return nil, putEdgeTx(ctx, tx, e)
	})
	return err
}

func (g graphAdapter) GetEdge(ctx context.Context, tenant, from, to string) (types.MemoryEdge, bool, error) {
	res, err := g.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		// LIMIT 1: putEdgeTx enforces single-edge-per-pair, but a
		// limit shields the read side against any historical drift.
		r, err := tx.Run(ctx, `
			MATCH (a:Node {tenant: $tenant, id: $from})-[r:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]->(b:Node {tenant: $tenant, id: $to})
			RETURN r, type(r) AS rel_type LIMIT 1
		`, map[string]any{"tenant": tenant, "from": from, "to": to})
		if err != nil {
			return nil, err
		}
		rec, err := r.Single(ctx)
		if err != nil {
			return nil, nil
		}
		rRaw, _ := rec.Get("r")
		typeRaw, _ := rec.Get("rel_type")
		rel, ok := rRaw.(neo4j.Relationship)
		if !ok {
			return nil, fmt.Errorf("neo4jstore: unexpected rel type %T", rRaw)
		}
		edge := decodeEdgeProps(rel.Props, asString(typeRaw))
		edge.From, edge.To, edge.TenantID = from, to, tenant
		return edge, nil
	})
	if err != nil {
		return types.MemoryEdge{}, false, err
	}
	if res == nil {
		return types.MemoryEdge{}, false, nil
	}
	return res.(types.MemoryEdge), true, nil
}

func (g graphAdapter) UpdateEdge(ctx context.Context, tenant, from, to string, fn func(*types.MemoryEdge) error) error {
	_, err := g.s.withWriteSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `
			MATCH (a:Node {tenant: $tenant, id: $from})-[r:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]->(b:Node {tenant: $tenant, id: $to})
			RETURN r, type(r) AS rel_type LIMIT 1
		`, map[string]any{"tenant": tenant, "from": from, "to": to})
		if err != nil {
			return nil, err
		}
		rec, err := r.Single(ctx)
		if err != nil {
			return nil, gport.ErrNotFound
		}
		rRaw, _ := rec.Get("r")
		typeRaw, _ := rec.Get("rel_type")
		rel, ok := rRaw.(neo4j.Relationship)
		if !ok {
			return nil, fmt.Errorf("neo4jstore: unexpected rel type %T", rRaw)
		}
		oldRelType := asString(typeRaw)
		current := decodeEdgeProps(rel.Props, oldRelType)
		current.From, current.To, current.TenantID = from, to, tenant
		oldWeight := current.Weight
		if err := fn(&current); err != nil {
			return nil, err
		}

		// If the closure promoted the edge Kind (e.g. STRUCTURAL →
		// CORETRIEVAL during Phase 5 of recall), Cypher's relType is
		// immutable so we drop the old rel and create the new typed
		// rel with the mutated props. The (from, to) endpoints stay.
		newRelType := edgeKindRelType(current.Kind)
		props := encodeEdgePropsForSet(current)
		if newRelType != oldRelType {
			if _, err := tx.Run(ctx, `
				MATCH (a:Node {tenant: $tenant, id: $from})-[r:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]->(b:Node {tenant: $tenant, id: $to})
				DELETE r
			`, map[string]any{"tenant": tenant, "from": from, "to": to}); err != nil {
				return nil, err
			}
			if _, err := tx.Run(ctx, fmt.Sprintf(`
				MATCH (a:Node {tenant: $tenant, id: $from})
				MATCH (b:Node {tenant: $tenant, id: $to})
				MERGE (a)-[r:%s]->(b)
				SET r += $props
			`, newRelType), map[string]any{
				"tenant": tenant, "from": from, "to": to, "props": props,
			}); err != nil {
				return nil, err
			}
		} else {
			if _, err := tx.Run(ctx, `
				MATCH (a:Node {tenant: $tenant, id: $from})-[r:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]->(b:Node {tenant: $tenant, id: $to})
				SET r += $props
			`, map[string]any{
				"tenant": tenant, "from": from, "to": to, "props": props,
			}); err != nil {
				return nil, err
			}
		}
		return nil, adjustCountersTx(ctx, tx, tenant, tenantCounters{
			SumEdgeWeight: current.Weight - oldWeight,
		})
	})
	return err
}

func (g graphAdapter) DeleteEdge(ctx context.Context, tenant, from, to string) error {
	_, err := g.s.withWriteSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `
			MATCH (a:Node {tenant: $tenant, id: $from})-[r:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]->(b:Node {tenant: $tenant, id: $to})
			RETURN r.weight AS w
		`, map[string]any{"tenant": tenant, "from": from, "to": to})
		if err != nil {
			return nil, err
		}
		rec, err := r.Single(ctx)
		if err != nil {
			return nil, nil // absent -> silent
		}
		wRaw, _ := rec.Get("w")
		w := asFloat(wRaw)
		if _, err := tx.Run(ctx, `
			MATCH (a:Node {tenant: $tenant, id: $from})-[r:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]->(b:Node {tenant: $tenant, id: $to})
			DELETE r
		`, map[string]any{"tenant": tenant, "from": from, "to": to}); err != nil {
			return nil, err
		}
		return nil, adjustCountersTx(ctx, tx, tenant, tenantCounters{
			EdgeCount:     -1,
			SumEdgeWeight: -w,
		})
	})
	return err
}

// putEdgeTx implements PutEdge inside an existing tx. memmy's
// contract is one edge per (from, to) pair regardless of Kind, but
// Cypher relationship types are part of the rel identity so storing
// each Kind as a different relType would let two distinct rels share
// one (from, to) pair — breaking that contract. We enforce
// single-edge-per-pair at write time: any existing rel with a
// different type is deleted before the requested type is MERGEd.
func putEdgeTx(ctx context.Context, tx neo4j.ManagedTransaction, e types.MemoryEdge) error {
	relType := edgeKindRelType(e.Kind)

	// Read the current state of any edge between these endpoints
	// (regardless of relType) so we can update counters correctly
	// when a same-pair, different-kind rel is being replaced.
	r, err := tx.Run(ctx, `
		MATCH (a:Node {tenant: $tenant, id: $from})-[r:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]->(b:Node {tenant: $tenant, id: $to})
		RETURN r.weight AS w, type(r) AS t
	`, map[string]any{
		"tenant": e.TenantID, "from": e.From, "to": e.To,
	})
	if err != nil {
		return err
	}
	var oldWeight float64
	isNew := true
	oldRelType := ""
	if r.Next(ctx) {
		isNew = false
		rec := r.Record()
		wRaw, _ := rec.Get("w")
		oldWeight = asFloat(wRaw)
		tRaw, _ := rec.Get("t")
		oldRelType = asString(tRaw)
	}
	if err := r.Err(); err != nil {
		return err
	}

	// If the existing rel has a different Kind, drop it so MERGE
	// below doesn't end up with two rels between the same pair.
	if !isNew && oldRelType != relType {
		if _, err := tx.Run(ctx, `
			MATCH (a:Node {tenant: $tenant, id: $from})-[r:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]->(b:Node {tenant: $tenant, id: $to})
			DELETE r
		`, map[string]any{"tenant": e.TenantID, "from": e.From, "to": e.To}); err != nil {
			return err
		}
	}

	// Both endpoints must exist as :Node before edge create. memmy's
	// service guarantees this — Write inserts nodes before edges.
	props := encodeEdgePropsForSet(e)
	if _, err := tx.Run(ctx, fmt.Sprintf(`
		MATCH (a:Node {tenant: $tenant, id: $from})
		MATCH (b:Node {tenant: $tenant, id: $to})
		MERGE (a)-[r:%s]->(b)
		SET r += $props
	`, relType), map[string]any{
		"tenant": e.TenantID, "from": e.From, "to": e.To, "props": props,
	}); err != nil {
		return err
	}
	delta := tenantCounters{SumEdgeWeight: e.Weight - oldWeight}
	if isNew {
		delta.EdgeCount = 1
	}
	return adjustCountersTx(ctx, tx, e.TenantID, delta)
}

// ----- neighbors -----

func (g graphAdapter) Neighbors(ctx context.Context, tenant, id string) ([]types.MemoryEdge, error) {
	return g.neighborQuery(ctx, tenant, id, true)
}

func (g graphAdapter) InboundNeighbors(ctx context.Context, tenant, id string) ([]types.MemoryEdge, error) {
	return g.neighborQuery(ctx, tenant, id, false)
}

func (g graphAdapter) neighborQuery(ctx context.Context, tenant, id string, outbound bool) ([]types.MemoryEdge, error) {
	pattern := `MATCH (a:Node {tenant: $tenant, id: $id})-[r:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]->(b:Node)`
	if !outbound {
		pattern = `MATCH (a:Node)-[r:STRUCTURAL|CORETRIEVAL|COTRAVERSAL]->(b:Node {tenant: $tenant, id: $id})`
	}
	res, err := g.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, pattern+`
			RETURN r, type(r) AS rel_type, a.id AS from_id, b.id AS to_id
		`, map[string]any{"tenant": tenant, "id": id})
		if err != nil {
			return nil, err
		}
		var out []types.MemoryEdge
		for r.Next(ctx) {
			rec := r.Record()
			rRaw, _ := rec.Get("r")
			typeRaw, _ := rec.Get("rel_type")
			fromRaw, _ := rec.Get("from_id")
			toRaw, _ := rec.Get("to_id")
			rel, ok := rRaw.(neo4j.Relationship)
			if !ok {
				return nil, fmt.Errorf("neo4jstore: unexpected rel type %T", rRaw)
			}
			edge := decodeEdgeProps(rel.Props, asString(typeRaw))
			edge.From = asString(fromRaw)
			edge.To = asString(toRaw)
			edge.TenantID = tenant
			out = append(out, edge)
		}
		return out, r.Err()
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	return res.([]types.MemoryEdge), nil
}

// ----- tenants -----

func (g graphAdapter) UpsertTenant(ctx context.Context, info types.TenantInfo) error {
	if info.ID == "" {
		return errors.New("graph: TenantInfo.ID required")
	}
	tupleRaw, err := json.Marshal(info.Tuple)
	if err != nil {
		return fmt.Errorf("neo4jstore: marshal tuple: %w", err)
	}
	_, err = g.s.withWriteSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(ctx, `
			MERGE (t:TenantInfo {id: $id})
			SET t.tuple_json = $tuple,
			    t.created_at_unix_ms = $created
		`, map[string]any{
			"id":      info.ID,
			"tuple":   string(tupleRaw),
			"created": info.CreatedAt.UnixMilli(),
		})
		return nil, err
	})
	return err
}

func (g graphAdapter) GetTenant(ctx context.Context, id string) (types.TenantInfo, error) {
	res, err := g.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `MATCH (t:TenantInfo {id: $id}) RETURN t`, map[string]any{"id": id})
		if err != nil {
			return types.TenantInfo{}, err
		}
		rec, err := r.Single(ctx)
		if err != nil {
			return types.TenantInfo{}, gport.ErrNotFound
		}
		raw, _ := rec.Get("t")
		node, ok := raw.(neo4j.Node)
		if !ok {
			return types.TenantInfo{}, fmt.Errorf("neo4jstore: unexpected tenant node type %T", raw)
		}
		return decodeTenantProps(node.Props), nil
	})
	if err != nil {
		return types.TenantInfo{}, err
	}
	return res.(types.TenantInfo), nil
}

func (g graphAdapter) ListTenants(ctx context.Context) ([]types.TenantInfo, error) {
	res, err := g.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `MATCH (t:TenantInfo) RETURN t ORDER BY t.id ASC`, nil)
		if err != nil {
			return nil, err
		}
		var out []types.TenantInfo
		for r.Next(ctx) {
			rec := r.Record()
			raw, _ := rec.Get("t")
			node, ok := raw.(neo4j.Node)
			if !ok {
				return nil, fmt.Errorf("neo4jstore: unexpected tenant node type %T", raw)
			}
			out = append(out, decodeTenantProps(node.Props))
		}
		return out, r.Err()
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	return res.([]types.TenantInfo), nil
}

func (g graphAdapter) Close() error { return g.s.Close() }

// ----- validation -----

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

// ----- prop encoders / decoders -----
//
// memmy's domain types map cleanly to Neo4j properties. We pick stable
// snake_case property names; ints stored as int64 unix-ms; sentence
// span as two ints; embedding as a LIST<FLOAT> handled by VectorIndex.

func encodeNodePropsForSet(n types.Node) map[string]any {
	return map[string]any{
		"tenant":               n.TenantID,
		"id":                   n.ID,
		"source_msg_id":        n.SourceMsgID,
		"sentence_span_start":  int64(n.SentenceSpan[0]),
		"sentence_span_end":    int64(n.SentenceSpan[1]),
		"text":                 n.Text,
		"embedding_dim":        int64(n.EmbeddingDim),
		"created_at_unix_ms":   n.CreatedAt.UnixMilli(),
		"last_touched_unix_ms": n.LastTouched.UnixMilli(),
		"weight":               n.Weight,
		"access_count":         int64(n.AccessCount),
		"tombstoned":           n.Tombstoned,
	}
}

func decodeNodeProps(p map[string]any) types.Node {
	return types.Node{
		ID:           asString(p["id"]),
		TenantID:     asString(p["tenant"]),
		SourceMsgID:  asString(p["source_msg_id"]),
		SentenceSpan: [2]int{asInt(p["sentence_span_start"]), asInt(p["sentence_span_end"])},
		Text:         asString(p["text"]),
		EmbeddingDim: asInt(p["embedding_dim"]),
		CreatedAt:    asUnixMs(p["created_at_unix_ms"]),
		LastTouched:  asUnixMs(p["last_touched_unix_ms"]),
		Weight:       asFloat(p["weight"]),
		AccessCount:  uint64(asInt64(p["access_count"])),
		Tombstoned:   asBool(p["tombstoned"]),
	}
}

func decodeMessageProps(p map[string]any) types.Message {
	var meta map[string]string
	if raw := asString(p["metadata_json"]); raw != "" {
		_ = json.Unmarshal([]byte(raw), &meta)
	}
	return types.Message{
		ID:        asString(p["id"]),
		TenantID:  asString(p["tenant"]),
		Text:      asString(p["text"]),
		Metadata:  meta,
		CreatedAt: asUnixMs(p["created_at_unix_ms"]),
	}
}

func decodeTenantProps(p map[string]any) types.TenantInfo {
	var tup map[string]string
	if raw := asString(p["tuple_json"]); raw != "" {
		_ = json.Unmarshal([]byte(raw), &tup)
	}
	return types.TenantInfo{
		ID:        asString(p["id"]),
		Tuple:     tup,
		CreatedAt: asUnixMs(p["created_at_unix_ms"]),
	}
}

func encodeEdgePropsForSet(e types.MemoryEdge) map[string]any {
	return map[string]any{
		"tenant":               e.TenantID,
		"weight":               e.Weight,
		"created_at_unix_ms":   e.CreatedAt.UnixMilli(),
		"last_touched_unix_ms": e.LastTouched.UnixMilli(),
		"access_count":         int64(e.AccessCount),
		"traverse_count":       int64(e.TraverseCount),
	}
}

func decodeEdgeProps(p map[string]any, relType string) types.MemoryEdge {
	return types.MemoryEdge{
		Kind:          relTypeEdgeKind(relType),
		Weight:        asFloat(p["weight"]),
		CreatedAt:     asUnixMs(p["created_at_unix_ms"]),
		LastTouched:   asUnixMs(p["last_touched_unix_ms"]),
		AccessCount:   uint64(asInt64(p["access_count"])),
		TraverseCount: uint64(asInt64(p["traverse_count"])),
	}
}

func edgeKindRelType(k types.EdgeKind) string {
	switch k {
	case types.EdgeStructural:
		return "STRUCTURAL"
	case types.EdgeCoRetrieval:
		return "CORETRIEVAL"
	case types.EdgeCoTraversal:
		return "COTRAVERSAL"
	default:
		return "STRUCTURAL"
	}
}

func relTypeEdgeKind(t string) types.EdgeKind {
	switch t {
	case "STRUCTURAL":
		return types.EdgeStructural
	case "CORETRIEVAL":
		return types.EdgeCoRetrieval
	case "COTRAVERSAL":
		return types.EdgeCoTraversal
	default:
		return types.EdgeStructural
	}
}
