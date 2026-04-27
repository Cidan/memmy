package bboltstore

import (
	"bytes"
	"context"
	"time"

	"github.com/oklog/ulid/v2"
	"go.etcd.io/bbolt"

	"github.com/Cidan/memmy/internal/service"
	"github.com/Cidan/memmy/internal/types"
)

// The methods below implement the optional scanner capabilities defined
// in internal/service. They live on *graphAdapter (the public Graph view)
// rather than on the interface itself, so non-bbolt backends can choose
// not to implement them and the service falls back to portable paths.

// RecentNodeIDs returns up to maxN node IDs created at-or-after `since`,
// in DESCENDING chronological order, excluding nodes whose SourceMsgID
// matches excludeMsgID. ULIDs are lex-sortable, so we lower-bound the
// scan with a synthetic ULID derived from `since`.
func (g graphAdapter) RecentNodeIDs(_ context.Context, tenant string, since time.Time, excludeMsgID string, maxN int) ([]string, error) {
	if maxN <= 0 {
		return nil, nil
	}
	out := make([]string, 0, maxN)
	err := g.s.db.View(func(tx *bbolt.Tx) error {
		nb, err := nodesBucket(tx, tenant, false)
		if err != nil || nb == nil {
			return err
		}
		// Iterate the bucket in REVERSE (most recent ULIDs last in
		// lexicographic order) and stop when we hit `since`.
		sinceULID, _ := ulid.New(uint64(since.UnixMilli()), zeroEntropy)
		sinceKey := []byte(sinceULID.String())
		cur := nb.Cursor()
		for k, v := cur.Last(); k != nil; k, v = cur.Prev() {
			// Stop once we've crossed `since` in reverse order.
			if bytes.Compare(k, sinceKey) < 0 {
				break
			}
			var n types.Node
			if err := decodeNode(v, &n); err != nil {
				return err
			}
			if n.SourceMsgID == excludeMsgID {
				continue
			}
			out = append(out, string(k))
			if len(out) >= maxN {
				break
			}
		}
		return nil
	})
	return out, err
}

// NodesForMessage returns every node whose SourceMsgID matches msgID.
// Implementation: scan the tenant's nodes bucket; v1 acceptable cost
// because Forget is a rare admin op.
func (g graphAdapter) NodesForMessage(_ context.Context, tenant, msgID string) ([]types.Node, error) {
	var out []types.Node
	err := g.s.db.View(func(tx *bbolt.Tx) error {
		nb, err := nodesBucket(tx, tenant, false)
		if err != nil || nb == nil {
			return err
		}
		return nb.ForEach(func(_, v []byte) error {
			var n types.Node
			if err := decodeNode(v, &n); err != nil {
				return err
			}
			if n.SourceMsgID == msgID {
				out = append(out, n)
			}
			return nil
		})
	})
	return out, err
}

// MessageIDsBefore returns every message ID with CreatedAt strictly
// before `before`.
func (g graphAdapter) MessageIDsBefore(_ context.Context, tenant string, before time.Time) ([]string, error) {
	var out []string
	err := g.s.db.View(func(tx *bbolt.Tx) error {
		mb, err := msgsBucket(tx, tenant, false)
		if err != nil || mb == nil {
			return err
		}
		return mb.ForEach(func(_, v []byte) error {
			var m types.Message
			if err := decodeMessage(v, &m); err != nil {
				return err
			}
			if m.CreatedAt.Before(before) {
				out = append(out, m.ID)
			}
			return nil
		})
	})
	return out, err
}

// TenantStats aggregates counts and weight sums across the tenant's
// nodes and memory edges.
func (g graphAdapter) TenantStats(_ context.Context, tenant string) (service.TenantStats, error) {
	var ts service.TenantStats
	err := g.s.db.View(func(tx *bbolt.Tx) error {
		// Nodes
		if nb, err := nodesBucket(tx, tenant, false); err == nil && nb != nil {
			if err := nb.ForEach(func(_, v []byte) error {
				var n types.Node
				if err := decodeNode(v, &n); err != nil {
					return err
				}
				ts.NodeCount++
				ts.SumNodeWeight += n.Weight
				return nil
			}); err != nil {
				return err
			}
		}
		// Edges (count outbound mirror only to avoid double-count).
		if eo, err := eoutBucket(tx, tenant, false); err == nil && eo != nil {
			if err := eo.ForEach(func(_, _ []byte) error {
				// Each top-level key is a from-id sub-bucket.
				return nil
			}); err != nil {
				return err
			}
			if err := eo.ForEachBucket(func(k []byte) error {
				fb := eo.Bucket(k)
				return fb.ForEach(func(_, v []byte) error {
					var e types.MemoryEdge
					if err := decodeEdge(v, &e); err != nil {
						return err
					}
					ts.EdgeCount++
					ts.SumEdgeWeight += e.Weight
					return nil
				})
			}); err != nil {
				return err
			}
		}
		// HNSW size
		if meta, ok, err := readHNSWMeta(tx, tenant); err == nil && ok {
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

