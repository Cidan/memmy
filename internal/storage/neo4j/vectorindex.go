package neo4jstore

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	vport "github.com/Cidan/memmy/internal/vectorindex"
)

// vectorIndexAdapter exposes Storage as the vectorindex.VectorIndex
// interface. Embeddings live as a `LIST<FLOAT>` property on the
// :Node label; the native vector index `node_embedding_idx` (created
// by migration 002) handles HNSW navigation.
type vectorIndexAdapter struct{ s *Storage }

// VectorIndex returns the vectorindex.VectorIndex view over Storage.
func (s *Storage) VectorIndex() vport.VectorIndex { return vectorIndexAdapter{s: s} }

// Insert sets the embedding on an existing :Node row, L2-normalizing
// at write time so cosine similarity reduces to a dot product on the
// native index. The Node row must already exist (memmy's service
// always writes the Node before its embedding).
func (v vectorIndexAdapter) Insert(ctx context.Context, tenant, nodeID string, vec []float32) error {
	if len(vec) != v.s.dim {
		return fmt.Errorf("neo4jstore: insert vector len %d, want %d", len(vec), v.s.dim)
	}
	normalized := l2Normalize(vec)
	values := float32SliceToAny(normalized)
	_, err := v.s.withWriteSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(ctx, `
			MERGE (n:Node {tenant: $tenant, id: $id})
			SET n.embedding = $vec, n.embedding_dim = $dim
		`, map[string]any{
			"tenant": tenant,
			"id":     nodeID,
			"vec":    values,
			"dim":    int64(v.s.dim),
		})
		return nil, err
	})
	return err
}

// Delete tombstones the node. memmy uses tombstones so the vector
// index doesn't have to be rebuilt every time a chunk is forgotten;
// search results must filter out tombstoned nodes.
func (v vectorIndexAdapter) Delete(ctx context.Context, tenant, nodeID string) error {
	_, err := v.s.withWriteSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(ctx, `
			MATCH (n:Node {tenant: $tenant, id: $id})
			SET n.tombstoned = true
		`, map[string]any{"tenant": tenant, "id": nodeID})
		return nil, err
	})
	return err
}

// Search returns the top-n hits by similarity. Tenants below the
// FlatScanThreshold use a Cypher flat scan (deterministic — also
// serves as the oracle for the vector-index recall test). Larger
// tenants delegate to the native vector index, oversampling to
// compensate for the post-call tenant filter.
func (v vectorIndexAdapter) Search(ctx context.Context, tenant string, qVec []float32, n int) ([]vport.Hit, error) {
	if len(qVec) != v.s.dim {
		return nil, fmt.Errorf("neo4jstore: query vector len %d, want %d", len(qVec), v.s.dim)
	}
	if n <= 0 {
		return nil, nil
	}
	size, err := v.Size(ctx, tenant)
	if err != nil {
		return nil, err
	}
	normQ := l2Normalize(qVec)
	queryVec := float32SliceToAny(normQ)
	if size <= v.s.flatScanThreshold {
		return v.flatScan(ctx, tenant, queryVec, n)
	}
	return v.nativeIndexSearch(ctx, tenant, queryVec, n)
}

// Size returns the count of non-tombstoned nodes in the tenant.
func (v vectorIndexAdapter) Size(ctx context.Context, tenant string) (int, error) {
	res, err := v.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `
			MATCH (n:Node {tenant: $tenant})
			WHERE coalesce(n.tombstoned, false) = false AND n.embedding IS NOT NULL
			RETURN count(n) AS c
		`, map[string]any{"tenant": tenant})
		if err != nil {
			return 0, err
		}
		rec, err := r.Single(ctx)
		if err != nil {
			return 0, nil
		}
		raw, _ := rec.Get("c")
		return asInt(raw), nil
	})
	if err != nil {
		return 0, err
	}
	return res.(int), nil
}

func (v vectorIndexAdapter) Dim() int { return v.s.dim }

func (v vectorIndexAdapter) Close() error { return nil }

// flatScan computes cosine similarity over every non-tombstoned node
// in the tenant. With L2-normalized vectors at write+query time, the
// flat-scan score equals the dot product. Used as the oracle for the
// native index recall test.
func (v vectorIndexAdapter) flatScan(ctx context.Context, tenant string, qVec []any, n int) ([]vport.Hit, error) {
	res, err := v.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `
			MATCH (m:Node {tenant: $tenant})
			WHERE coalesce(m.tombstoned, false) = false AND m.embedding IS NOT NULL
			WITH m, vector.similarity.cosine(m.embedding, $q) AS sim
			ORDER BY sim DESC
			LIMIT $n
			RETURN m.id AS id, sim
		`, map[string]any{"tenant": tenant, "q": qVec, "n": int64(n)})
		if err != nil {
			return nil, err
		}
		var out []vport.Hit
		for r.Next(ctx) {
			rec := r.Record()
			idRaw, _ := rec.Get("id")
			simRaw, _ := rec.Get("sim")
			out = append(out, vport.Hit{NodeID: asString(idRaw), Sim: asFloat(simRaw)})
		}
		return out, r.Err()
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	return res.([]vport.Hit), nil
}

// nativeIndexSearch consults the global vector index and filters the
// results down to the requested tenant + non-tombstoned set. Index
// queries are global across tenants in Neo4j; we oversample by 4x to
// cushion the tenant filter loss.
func (v vectorIndexAdapter) nativeIndexSearch(ctx context.Context, tenant string, qVec []any, n int) ([]vport.Hit, error) {
	const oversampleFactor = 4
	probe := int64(n * oversampleFactor)
	if probe < int64(n) {
		probe = int64(n)
	}
	res, err := v.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `
			CALL db.index.vector.queryNodes('node_embedding_idx', $probe, $q)
			YIELD node, score
			WHERE node.tenant = $tenant AND coalesce(node.tombstoned, false) = false
			RETURN node.id AS id, score AS sim
			ORDER BY sim DESC
			LIMIT $n
		`, map[string]any{
			"tenant": tenant,
			"q":      qVec,
			"probe":  probe,
			"n":      int64(n),
		})
		if err != nil {
			return nil, err
		}
		var out []vport.Hit
		for r.Next(ctx) {
			rec := r.Record()
			idRaw, _ := rec.Get("id")
			simRaw, _ := rec.Get("sim")
			out = append(out, vport.Hit{NodeID: asString(idRaw), Sim: asFloat(simRaw)})
		}
		return out, r.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("neo4jstore: vector index query: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	return res.([]vport.Hit), nil
}

// l2Normalize returns a copy of vec scaled to unit length. memmy's
// VectorIndex contract requires L2 normalization at write time so
// cosine reduces to dot product on the native index.
func l2Normalize(vec []float32) []float32 {
	var sum float64
	for _, x := range vec {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return append([]float32(nil), vec...)
	}
	inv := float32(1.0 / math.Sqrt(sum))
	out := make([]float32, len(vec))
	for i, x := range vec {
		out[i] = x * inv
	}
	return out
}

// float32SliceToAny copies a []float32 into a []any. The bolt driver
// expects parameter slices as []any.
func float32SliceToAny(in []float32) []any {
	out := make([]any, len(in))
	for i, x := range in {
		out[i] = float64(x)
	}
	return out
}

// GetVector returns the stored embedding for nodeID, or
// ErrNotFound if absent. Useful for tests + the inspect API.
func (v vectorIndexAdapter) GetVector(ctx context.Context, tenant, nodeID string) ([]float32, error) {
	res, err := v.s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, `
			MATCH (n:Node {tenant: $tenant, id: $id})
			RETURN n.embedding AS v
		`, map[string]any{"tenant": tenant, "id": nodeID})
		if err != nil {
			return nil, err
		}
		rec, err := r.Single(ctx)
		if err != nil {
			return nil, vport.ErrNotFound
		}
		raw, _ := rec.Get("v")
		if raw == nil {
			return nil, vport.ErrNotFound
		}
		list, ok := raw.([]any)
		if !ok {
			return nil, errors.New("neo4jstore: unexpected embedding type")
		}
		out := make([]float32, len(list))
		for i, x := range list {
			out[i] = float32(asFloat(x))
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return res.([]float32), nil
}
