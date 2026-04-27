package bboltstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"go.etcd.io/bbolt"

	vidx "github.com/Cidan/memmy/internal/vectorindex"
)

// vectorAdapter exposes Storage as the vectorindex.VectorIndex interface.
type vectorAdapter struct{ s *Storage }

// VectorIndex returns the VectorIndex view over this Storage.
func (s *Storage) VectorIndex() vidx.VectorIndex { return vectorAdapter{s: s} }

func (a vectorAdapter) Dim() int { return a.s.dim }

func (a vectorAdapter) Close() error { return a.s.Close() }

// Insert adds (or replaces) the vector for nodeID and its HNSW record.
// All writes commit in a single transaction.
func (a vectorAdapter) Insert(_ context.Context, tenant, nodeID string, vec []float32) error {
	if tenant == "" {
		return errors.New("vectorindex: tenant required")
	}
	if nodeID == "" {
		return errors.New("vectorindex: nodeID required")
	}
	if len(vec) != a.s.dim {
		return fmt.Errorf("vectorindex: vector dim=%d, want %d", len(vec), a.s.dim)
	}
	norm := l2Normalize(vec)
	return a.s.db.Update(func(tx *bbolt.Tx) error {
		if err := putVectorTx(tx, tenant, nodeID, norm); err != nil {
			return err
		}
		return a.s.hnswInsertTx(tx, tenant, nodeID, norm)
	})
}

// Delete hard-removes the vector and HNSW record for nodeID and repairs
// neighbor lists. If nodeID was the HNSW entry point, a new entry point
// is chosen.
func (a vectorAdapter) Delete(_ context.Context, tenant, nodeID string) error {
	return a.s.db.Update(func(tx *bbolt.Tx) error {
		if err := a.s.hnswDeleteTx(tx, tenant, nodeID); err != nil {
			return err
		}
		return deleteVectorTx(tx, tenant, nodeID)
	})
}

func (a vectorAdapter) Size(_ context.Context, tenant string) (int, error) {
	var size int
	err := a.s.db.View(func(tx *bbolt.Tx) error {
		meta, ok, err := readHNSWMeta(tx, tenant)
		if err != nil || !ok {
			return err
		}
		size = meta.Size
		return nil
	})
	return size, err
}

// Search runs flat scan when tenant size is below the threshold, else HNSW.
func (a vectorAdapter) Search(_ context.Context, tenant string, qVec []float32, n int) ([]vidx.Hit, error) {
	if len(qVec) != a.s.dim {
		return nil, fmt.Errorf("vectorindex: query dim=%d, want %d", len(qVec), a.s.dim)
	}
	if n <= 0 {
		return nil, nil
	}
	qNorm := l2Normalize(qVec)
	var hits []vidx.Hit
	err := a.s.db.View(func(tx *bbolt.Tx) error {
		meta, ok, err := readHNSWMeta(tx, tenant)
		if err != nil {
			return err
		}
		if !ok || meta.Size == 0 {
			return nil
		}
		if meta.Size < a.s.flatScanThreshold {
			hits = a.flatScanTx(tx, tenant, qNorm, n)
			return nil
		}
		hits = a.s.hnswSearchTx(tx, tenant, &meta, qNorm, n)
		return nil
	})
	return hits, err
}

// flatScanTx streams every vector in the tenant via a bbolt cursor and
// keeps a bounded top-N heap. Memory is O(N), independent of corpus size.
func (a vectorAdapter) flatScanTx(tx *bbolt.Tx, tenant string, q []float32, n int) []vidx.Hit {
	vb, err := vecBucket(tx, tenant, false)
	if err != nil || vb == nil {
		return nil
	}
	heap := newTopNSim(n)
	buf := make([]float32, a.s.dim)
	cur := vb.Cursor()
	for k, v := cur.First(); k != nil; k, v = cur.Next() {
		if len(v) != a.s.dim*4 {
			continue
		}
		decodeVectorInto(v, buf)
		s := float64(dot(q, buf))
		heap.push(string(k), s)
	}
	return heap.sorted()
}

// vecBucket returns the t/<tenant>/vec bucket.
func vecBucket(tx *bbolt.Tx, tenant string, create bool) (*bbolt.Bucket, error) {
	t, err := tenantBucket(tx, tenant, create)
	if err != nil {
		return nil, err
	}
	return subBucket(t, bktVec, create)
}

// putVectorTx writes raw LE float32 bytes for a vector inside an existing tx.
func putVectorTx(tx *bbolt.Tx, tenant, nodeID string, normVec []float32) error {
	vb, err := vecBucket(tx, tenant, true)
	if err != nil {
		return err
	}
	return vb.Put([]byte(nodeID), encodeVector(normVec))
}

func deleteVectorTx(tx *bbolt.Tx, tenant, nodeID string) error {
	vb, err := vecBucket(tx, tenant, false)
	if err != nil {
		return err
	}
	if vb == nil {
		return nil
	}
	return vb.Delete([]byte(nodeID))
}

// readVectorTx loads a vector by nodeID.
func readVectorTx(tx *bbolt.Tx, tenant, nodeID string, dim int, out []float32) (bool, error) {
	vb, err := vecBucket(tx, tenant, false)
	if err != nil {
		return false, err
	}
	if vb == nil {
		return false, nil
	}
	raw := vb.Get([]byte(nodeID))
	if raw == nil {
		return false, nil
	}
	if len(raw) != dim*4 {
		return false, fmt.Errorf("vector dim mismatch for %s: have %d bytes, want %d", nodeID, len(raw), dim*4)
	}
	decodeVectorInto(raw, out)
	return true, nil
}

// ----- math helpers -----

func l2Normalize(v []float32) []float32 {
	out := make([]float32, len(v))
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	if sumSq == 0 {
		return out
	}
	inv := float32(1.0 / math.Sqrt(sumSq))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

func dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// ----- top-N similarity heap -----
//
// Bounded heap that keeps the top-n elements by similarity (larger = better).
// Internally a min-heap (root is the smallest sim, which is the eviction
// candidate when a new better element arrives).

type simEntry struct {
	id  string
	sim float64
}

type topNSim struct {
	cap  int
	heap []simEntry // min-heap
}

func newTopNSim(cap int) *topNSim { return &topNSim{cap: cap} }

func (t *topNSim) push(id string, sim float64) {
	if len(t.heap) < t.cap {
		t.heap = append(t.heap, simEntry{id, sim})
		t.siftUp(len(t.heap) - 1)
		return
	}
	if sim <= t.heap[0].sim {
		return
	}
	t.heap[0] = simEntry{id, sim}
	t.siftDown(0)
}

func (t *topNSim) sorted() []vidx.Hit {
	out := make([]simEntry, len(t.heap))
	copy(out, t.heap)
	sort.Slice(out, func(i, j int) bool { return out[i].sim > out[j].sim })
	hits := make([]vidx.Hit, len(out))
	for i, e := range out {
		hits[i] = vidx.Hit{NodeID: e.id, Sim: e.sim}
	}
	return hits
}

func (t *topNSim) siftUp(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if t.heap[p].sim <= t.heap[i].sim {
			return
		}
		t.heap[p], t.heap[i] = t.heap[i], t.heap[p]
		i = p
	}
}

func (t *topNSim) siftDown(i int) {
	n := len(t.heap)
	for {
		l, r := 2*i+1, 2*i+2
		small := i
		if l < n && t.heap[l].sim < t.heap[small].sim {
			small = l
		}
		if r < n && t.heap[r].sim < t.heap[small].sim {
			small = r
		}
		if small == i {
			return
		}
		t.heap[i], t.heap[small] = t.heap[small], t.heap[i]
		i = small
	}
}
