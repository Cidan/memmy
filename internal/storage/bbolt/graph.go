package bboltstore

import (
	"context"
	"errors"
	"fmt"

	"go.etcd.io/bbolt"

	gport "github.com/Cidan/memmy/internal/graph"
	"github.com/Cidan/memmy/internal/types"
)

// graphAdapter adapts *Storage to the graph.Graph interface.
type graphAdapter struct{ s *Storage }

// Graph returns the graph.Graph view over this Storage.
func (s *Storage) Graph() gport.Graph { return graphAdapter{s: s} }

// ----- nodes -----

func (g graphAdapter) PutNode(_ context.Context, n types.Node) error {
	if err := validateNode(n); err != nil {
		return err
	}
	return g.s.db.Update(func(tx *bbolt.Tx) error {
		return putNodeTxWithCounters(tx, n)
	})
}

func (g graphAdapter) GetNode(_ context.Context, tenant, id string) (types.Node, error) {
	var out types.Node
	err := g.s.db.View(func(tx *bbolt.Tx) error {
		b, err := nodesBucket(tx, tenant, false)
		if err != nil {
			return err
		}
		if b == nil {
			return gport.ErrNotFound
		}
		raw := b.Get([]byte(id))
		if raw == nil {
			return gport.ErrNotFound
		}
		return decodeNode(raw, &out)
	})
	return out, err
}

func (g graphAdapter) UpdateNode(_ context.Context, tenant, id string, fn func(*types.Node) error) error {
	return g.s.db.Update(func(tx *bbolt.Tx) error {
		b, err := nodesBucket(tx, tenant, false)
		if err != nil {
			return err
		}
		if b == nil {
			return gport.ErrNotFound
		}
		raw := b.Get([]byte(id))
		if raw == nil {
			return gport.ErrNotFound
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
		if err := b.Put([]byte(id), buf); err != nil {
			return err
		}
		return adjustCountersTx(tx, tenant, tenantCounters{SumNodeWeight: n.Weight - oldWeight})
	})
}

func (g graphAdapter) DeleteNode(_ context.Context, tenant, id string) error {
	return g.s.db.Update(func(tx *bbolt.Tx) error {
		b, err := nodesBucket(tx, tenant, false)
		if err != nil {
			return err
		}
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(id))
		if raw == nil {
			return nil
		}
		var n types.Node
		if err := decodeNode(raw, &n); err != nil {
			return err
		}
		if err := b.Delete([]byte(id)); err != nil {
			return err
		}
		return adjustCountersTx(tx, tenant, tenantCounters{NodeCount: -1, SumNodeWeight: -n.Weight})
	})
}

// ----- messages -----

func (g graphAdapter) PutMessage(_ context.Context, m types.Message) error {
	if m.ID == "" {
		return errors.New("graph: message ID required")
	}
	if m.TenantID == "" {
		return errors.New("graph: message TenantID required")
	}
	return g.s.db.Update(func(tx *bbolt.Tx) error {
		b, err := msgsBucket(tx, m.TenantID, true)
		if err != nil {
			return err
		}
		buf, err := encodeMessage(&m)
		if err != nil {
			return err
		}
		return b.Put([]byte(m.ID), buf)
	})
}

func (g graphAdapter) GetMessage(_ context.Context, tenant, id string) (types.Message, error) {
	var out types.Message
	err := g.s.db.View(func(tx *bbolt.Tx) error {
		b, err := msgsBucket(tx, tenant, false)
		if err != nil {
			return err
		}
		if b == nil {
			return gport.ErrNotFound
		}
		raw := b.Get([]byte(id))
		if raw == nil {
			return gport.ErrNotFound
		}
		return decodeMessage(raw, &out)
	})
	return out, err
}

func (g graphAdapter) DeleteMessage(_ context.Context, tenant, id string) error {
	return g.s.db.Update(func(tx *bbolt.Tx) error {
		b, err := msgsBucket(tx, tenant, false)
		if err != nil {
			return err
		}
		if b == nil {
			return nil
		}
		return b.Delete([]byte(id))
	})
}

// ----- edges -----

func (g graphAdapter) PutEdge(_ context.Context, e types.MemoryEdge) error {
	if err := validateEdge(e); err != nil {
		return err
	}
	return g.s.db.Update(func(tx *bbolt.Tx) error {
		return putEdgeTxWithCounters(tx, e)
	})
}

func (g graphAdapter) GetEdge(_ context.Context, tenant, from, to string) (types.MemoryEdge, bool, error) {
	var out types.MemoryEdge
	var found bool
	err := g.s.db.View(func(tx *bbolt.Tx) error {
		eo, err := eoutBucket(tx, tenant, false)
		if err != nil {
			return err
		}
		if eo == nil {
			return nil
		}
		fb := eo.Bucket([]byte(from))
		if fb == nil {
			return nil
		}
		raw := fb.Get([]byte(to))
		if raw == nil {
			return nil
		}
		if err := decodeEdge(raw, &out); err != nil {
			return err
		}
		found = true
		return nil
	})
	return out, found, err
}

func (g graphAdapter) UpdateEdge(_ context.Context, tenant, from, to string, fn func(*types.MemoryEdge) error) error {
	return g.s.db.Update(func(tx *bbolt.Tx) error {
		eo, err := eoutBucket(tx, tenant, false)
		if err != nil {
			return err
		}
		if eo == nil {
			return gport.ErrNotFound
		}
		fb := eo.Bucket([]byte(from))
		if fb == nil {
			return gport.ErrNotFound
		}
		raw := fb.Get([]byte(to))
		if raw == nil {
			return gport.ErrNotFound
		}
		var e types.MemoryEdge
		if err := decodeEdge(raw, &e); err != nil {
			return err
		}
		oldWeight := e.Weight
		if err := fn(&e); err != nil {
			return err
		}
		if err := writeEdgeMirrorsTx(tx, e); err != nil {
			return err
		}
		return adjustCountersTx(tx, tenant, tenantCounters{SumEdgeWeight: e.Weight - oldWeight})
	})
}

func (g graphAdapter) DeleteEdge(_ context.Context, tenant, from, to string) error {
	return g.s.db.Update(func(tx *bbolt.Tx) error {
		return deleteEdgeTxWithCounters(tx, tenant, from, to)
	})
}

// ----- neighbors -----

func (g graphAdapter) Neighbors(_ context.Context, tenant, id string) ([]types.MemoryEdge, error) {
	var out []types.MemoryEdge
	err := g.s.db.View(func(tx *bbolt.Tx) error {
		eo, err := eoutBucket(tx, tenant, false)
		if err != nil {
			return err
		}
		if eo == nil {
			return nil
		}
		fb := eo.Bucket([]byte(id))
		if fb == nil {
			return nil
		}
		return fb.ForEach(func(_, v []byte) error {
			var e types.MemoryEdge
			if err := decodeEdge(v, &e); err != nil {
				return err
			}
			out = append(out, e)
			return nil
		})
	})
	return out, err
}

func (g graphAdapter) InboundNeighbors(_ context.Context, tenant, id string) ([]types.MemoryEdge, error) {
	var out []types.MemoryEdge
	err := g.s.db.View(func(tx *bbolt.Tx) error {
		ei, err := einBucket(tx, tenant, false)
		if err != nil {
			return err
		}
		if ei == nil {
			return nil
		}
		tb := ei.Bucket([]byte(id))
		if tb == nil {
			return nil
		}
		return tb.ForEach(func(_, v []byte) error {
			var e types.MemoryEdge
			if err := decodeEdge(v, &e); err != nil {
				return err
			}
			out = append(out, e)
			return nil
		})
	})
	return out, err
}

// ----- tenants -----

func (g graphAdapter) UpsertTenant(_ context.Context, info types.TenantInfo) error {
	if info.ID == "" {
		return errors.New("graph: TenantInfo.ID required")
	}
	return g.s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bktTenants))
		buf, err := encodeTenantInfo(&info)
		if err != nil {
			return err
		}
		return b.Put([]byte(info.ID), buf)
	})
}

func (g graphAdapter) GetTenant(_ context.Context, id string) (types.TenantInfo, error) {
	var out types.TenantInfo
	err := g.s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bktTenants))
		raw := b.Get([]byte(id))
		if raw == nil {
			return gport.ErrNotFound
		}
		return decodeTenantInfo(raw, &out)
	})
	return out, err
}

func (g graphAdapter) ListTenants(_ context.Context) ([]types.TenantInfo, error) {
	var out []types.TenantInfo
	err := g.s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bktTenants))
		return b.ForEach(func(_, v []byte) error {
			var t types.TenantInfo
			if err := decodeTenantInfo(v, &t); err != nil {
				return err
			}
			out = append(out, t)
			return nil
		})
	})
	return out, err
}

func (g graphAdapter) Close() error { return g.s.Close() }

// ----- helpers -----

func nodesBucket(tx *bbolt.Tx, tenant string, create bool) (*bbolt.Bucket, error) {
	t, err := tenantBucket(tx, tenant, create)
	if err != nil {
		return nil, err
	}
	return subBucket(t, bktNodes, create)
}

func msgsBucket(tx *bbolt.Tx, tenant string, create bool) (*bbolt.Bucket, error) {
	t, err := tenantBucket(tx, tenant, create)
	if err != nil {
		return nil, err
	}
	return subBucket(t, bktMsgs, create)
}

func eoutBucket(tx *bbolt.Tx, tenant string, create bool) (*bbolt.Bucket, error) {
	t, err := tenantBucket(tx, tenant, create)
	if err != nil {
		return nil, err
	}
	return subBucket(t, bktEout, create)
}

func einBucket(tx *bbolt.Tx, tenant string, create bool) (*bbolt.Bucket, error) {
	t, err := tenantBucket(tx, tenant, create)
	if err != nil {
		return nil, err
	}
	return subBucket(t, bktEin, create)
}

// writeNodeRecordTx writes a node record without touching counters. Used
// by helpers that maintain counters explicitly.
func writeNodeRecordTx(tx *bbolt.Tx, n types.Node) error {
	b, err := nodesBucket(tx, n.TenantID, true)
	if err != nil {
		return err
	}
	buf, err := encodeNode(&n)
	if err != nil {
		return err
	}
	return b.Put([]byte(n.ID), buf)
}

// putNodeTxWithCounters inserts (or replaces) a node and updates the
// per-tenant counter atomically. Used by graphAdapter.PutNode.
func putNodeTxWithCounters(tx *bbolt.Tx, n types.Node) error {
	b, err := nodesBucket(tx, n.TenantID, true)
	if err != nil {
		return err
	}
	var oldWeight float64
	isNew := true
	if raw := b.Get([]byte(n.ID)); raw != nil {
		var old types.Node
		if err := decodeNode(raw, &old); err != nil {
			return err
		}
		oldWeight = old.Weight
		isNew = false
	}
	if err := writeNodeRecordTx(tx, n); err != nil {
		return err
	}
	delta := tenantCounters{SumNodeWeight: n.Weight - oldWeight}
	if isNew {
		delta.NodeCount = 1
	}
	return adjustCountersTx(tx, n.TenantID, delta)
}

// writeEdgeMirrorsTx writes both eout/from/to and ein/to/from for an
// edge without touching counters. Callers maintain counters explicitly.
func writeEdgeMirrorsTx(tx *bbolt.Tx, e types.MemoryEdge) error {
	buf, err := encodeEdge(&e)
	if err != nil {
		return err
	}
	eo, err := eoutBucket(tx, e.TenantID, true)
	if err != nil {
		return err
	}
	fb, err := eo.CreateBucketIfNotExists([]byte(e.From))
	if err != nil {
		return fmt.Errorf("eout/%s: %w", e.From, err)
	}
	if err := fb.Put([]byte(e.To), buf); err != nil {
		return err
	}
	ei, err := einBucket(tx, e.TenantID, true)
	if err != nil {
		return err
	}
	tb, err := ei.CreateBucketIfNotExists([]byte(e.To))
	if err != nil {
		return fmt.Errorf("ein/%s: %w", e.To, err)
	}
	return tb.Put([]byte(e.From), buf)
}

// putEdgeTxWithCounters upserts an edge in both mirrors and updates the
// counter delta atomically.
func putEdgeTxWithCounters(tx *bbolt.Tx, e types.MemoryEdge) error {
	var oldWeight float64
	isNew := true
	if eo, _ := eoutBucket(tx, e.TenantID, false); eo != nil {
		if fb := eo.Bucket([]byte(e.From)); fb != nil {
			if raw := fb.Get([]byte(e.To)); raw != nil {
				var old types.MemoryEdge
				if err := decodeEdge(raw, &old); err != nil {
					return err
				}
				oldWeight = old.Weight
				isNew = false
			}
		}
	}
	if err := writeEdgeMirrorsTx(tx, e); err != nil {
		return err
	}
	delta := tenantCounters{SumEdgeWeight: e.Weight - oldWeight}
	if isNew {
		delta.EdgeCount = 1
	}
	return adjustCountersTx(tx, e.TenantID, delta)
}

// deleteEdgeTxWithCounters removes both edge mirrors and updates the
// counter (decrement count, subtract the deleted edge's weight).
// Absent-edge calls are silent no-ops.
func deleteEdgeTxWithCounters(tx *bbolt.Tx, tenant, from, to string) error {
	var oldWeight float64
	existed := false
	eo, err := eoutBucket(tx, tenant, false)
	if err != nil {
		return err
	}
	if eo != nil {
		if fb := eo.Bucket([]byte(from)); fb != nil {
			if raw := fb.Get([]byte(to)); raw != nil {
				var old types.MemoryEdge
				if err := decodeEdge(raw, &old); err != nil {
					return err
				}
				oldWeight = old.Weight
				existed = true
				if err := fb.Delete([]byte(to)); err != nil {
					return err
				}
			}
		}
	}
	ei, err := einBucket(tx, tenant, false)
	if err != nil {
		return err
	}
	if ei != nil {
		if tb := ei.Bucket([]byte(to)); tb != nil {
			if err := tb.Delete([]byte(from)); err != nil {
				return err
			}
		}
	}
	if !existed {
		return nil
	}
	return adjustCountersTx(tx, tenant, tenantCounters{EdgeCount: -1, SumEdgeWeight: -oldWeight})
}

// validateNode checks required fields.
func validateNode(n types.Node) error {
	switch {
	case n.ID == "":
		return errors.New("graph: node ID required")
	case n.TenantID == "":
		return errors.New("graph: node TenantID required")
	}
	return nil
}

// validateEdge checks required fields.
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
