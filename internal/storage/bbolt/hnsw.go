package bboltstore

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"sync"

	"go.etcd.io/bbolt"

	vidx "github.com/Cidan/memmy/internal/vectorindex"
	"github.com/Cidan/memmy/internal/types"
)

// hnswRand is a small wrapper that protects a *rand.Rand with a mutex so
// concurrent writers cannot race on the layer-sampling source.
type hnswRand struct {
	mu sync.Mutex
	r  *rand.Rand
}

func newHNSWRand(seed uint64) *hnswRand {
	return &hnswRand{r: rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))}
}

// uniform returns a number in (0, 1].
func (h *hnswRand) uniform() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	for {
		x := h.r.Float64()
		if x > 0 {
			return x
		}
	}
}

// sampleLayer picks an insertion layer by `floor(-ln(u) * mL)` (Malkov §4 Alg.1).
func (h *hnswRand) sampleLayer(mL float64) int {
	u := h.uniform()
	return int(math.Floor(-math.Log(u) * mL))
}

// ----- bucket helpers -----

func hnswRoot(tx *bbolt.Tx, tenant string, create bool) (*bbolt.Bucket, error) {
	t, err := tenantBucket(tx, tenant, create)
	if err != nil {
		return nil, err
	}
	return subBucket(t, bktHNSW, create)
}

func hnswRecords(tx *bbolt.Tx, tenant string, create bool) (*bbolt.Bucket, error) {
	root, err := hnswRoot(tx, tenant, create)
	if err != nil || root == nil {
		return root, err
	}
	return subBucket(root, bktHNSWRecords, create)
}

// readHNSWMeta returns (meta, found, error).
func readHNSWMeta(tx *bbolt.Tx, tenant string) (types.HNSWMeta, bool, error) {
	root, err := hnswRoot(tx, tenant, false)
	if err != nil || root == nil {
		return types.HNSWMeta{}, false, err
	}
	raw := root.Get([]byte(keyHNSWMeta))
	if raw == nil {
		return types.HNSWMeta{}, false, nil
	}
	var m types.HNSWMeta
	if err := decodeHNSWMeta(raw, &m); err != nil {
		return types.HNSWMeta{}, false, err
	}
	return m, true, nil
}

func writeHNSWMeta(tx *bbolt.Tx, tenant string, m *types.HNSWMeta) error {
	root, err := hnswRoot(tx, tenant, true)
	if err != nil {
		return err
	}
	buf, err := encodeHNSWMeta(m)
	if err != nil {
		return err
	}
	return root.Put([]byte(keyHNSWMeta), buf)
}

func readHNSWRecord(tx *bbolt.Tx, tenant, nodeID string) (types.HNSWRecord, bool, error) {
	rb, err := hnswRecords(tx, tenant, false)
	if err != nil || rb == nil {
		return types.HNSWRecord{}, false, err
	}
	raw := rb.Get([]byte(nodeID))
	if raw == nil {
		return types.HNSWRecord{}, false, nil
	}
	var r types.HNSWRecord
	if err := decodeHNSWRecord(raw, &r); err != nil {
		return types.HNSWRecord{}, false, err
	}
	return r, true, nil
}

func writeHNSWRecord(tx *bbolt.Tx, tenant string, r *types.HNSWRecord) error {
	rb, err := hnswRecords(tx, tenant, true)
	if err != nil {
		return err
	}
	buf, err := encodeHNSWRecord(r)
	if err != nil {
		return err
	}
	return rb.Put([]byte(r.NodeID), buf)
}

func deleteHNSWRecord(tx *bbolt.Tx, tenant, nodeID string) error {
	rb, err := hnswRecords(tx, tenant, false)
	if err != nil || rb == nil {
		return err
	}
	return rb.Delete([]byte(nodeID))
}

// ----- HNSW insert (Malkov §4 Alg.1) -----

// hnswInsertTx links a new node into the HNSW graph inside the given tx.
// The vector must already be persisted to vec/ by the caller. All HNSW
// reads and writes happen here within the same tx.
func (s *Storage) hnswInsertTx(tx *bbolt.Tx, tenant, nodeID string, vec []float32) error {
	if s.rand == nil {
		// Defensive — Open should always set this.
		s.rand = newHNSWRand(1)
	}

	// Upsert path: if nodeID already exists, fully remove it from the
	// graph (cleaning neighbors, decrementing Size, replacing entry
	// point if needed) so we re-insert into a consistent state.
	if _, ok, err := readHNSWRecord(tx, tenant, nodeID); err != nil {
		return err
	} else if ok {
		if err := s.hnswDeleteTx(tx, tenant, nodeID); err != nil {
			return err
		}
	}

	meta, found, err := readHNSWMeta(tx, tenant)
	if err != nil {
		return err
	}
	if !found {
		meta = types.HNSWMeta{
			Dim:            s.dim,
			M:              s.hnsw.M,
			M0:             s.hnsw.M0,
			EfConstruction: s.hnsw.EfConstruction,
			EfSearch:       s.hnsw.EfSearch,
			ML:             s.hnsw.ML,
		}
	}

	// Sample insertion layer.
	L := s.rand.sampleLayer(meta.ML)

	// First node ever (or first after full deletion): trivial.
	if meta.Size == 0 {
		rec := types.HNSWRecord{
			NodeID:    nodeID,
			Layer:     L,
			Neighbors: emptyNeighbors(L),
		}
		if err := writeHNSWRecord(tx, tenant, &rec); err != nil {
			return err
		}
		meta.EntryPoint = nodeID
		meta.MaxLayer = L
		meta.Size = 1
		return writeHNSWMeta(tx, tenant, &meta)
	}

	// Phase 1 — greedy descent from MaxLayer down to L+1 with ef=1.
	cur := meta.EntryPoint
	curDist, err := s.distTo(tx, tenant, cur, vec)
	if err != nil {
		return err
	}
	for ℓ := meta.MaxLayer; ℓ > L; ℓ-- {
		cur, curDist, err = s.greedyDescentTx(tx, tenant, cur, curDist, vec, ℓ)
		if err != nil {
			return err
		}
	}

	// Phase 2 — at each layer min(L, MaxLayer) ... 0, ef-search and link.
	rec := types.HNSWRecord{
		NodeID:    nodeID,
		Layer:     L,
		Neighbors: emptyNeighbors(L),
	}

	startLayer := min(L, meta.MaxLayer)

	entryPoints := []candidate{{id: cur, dist: curDist}}
	for ℓ := startLayer; ℓ >= 0; ℓ-- {
		W, err := s.searchLayerTx(tx, tenant, vec, entryPoints, meta.EfConstruction, ℓ)
		if err != nil {
			return err
		}
		mTarget := meta.M
		if ℓ == 0 {
			mTarget = meta.M0
		}
		chosen := selectNeighborsHeuristic(W, mTarget)

		// Set rec.Neighbors[ℓ] from chosen (in dist-ascending order).
		ids := make([]string, len(chosen))
		for i, c := range chosen {
			ids[i] = c.id
		}
		rec.Neighbors[ℓ] = ids

		// Bidirectional link + prune neighbors of each chosen.
		for _, c := range chosen {
			if err := s.linkAndPruneTx(tx, tenant, c.id, nodeID, vec, ℓ, mTarget); err != nil {
				return err
			}
		}

		// Carry the search frontier forward as entry points for the next layer.
		entryPoints = W
	}

	if err := writeHNSWRecord(tx, tenant, &rec); err != nil {
		return err
	}

	// Phase 3 — extend top.
	if L > meta.MaxLayer {
		meta.MaxLayer = L
		meta.EntryPoint = nodeID
	}
	meta.Size++
	return writeHNSWMeta(tx, tenant, &meta)
}

// emptyNeighbors returns Neighbors map with empty slices for layers 0..topLayer.
func emptyNeighbors(topLayer int) map[int][]string {
	m := make(map[int][]string, topLayer+1)
	for ℓ := 0; ℓ <= topLayer; ℓ++ {
		m[ℓ] = nil
	}
	return m
}

// linkAndPruneTx adds nodeID to neighbor n's neighbor list at layer ℓ,
// then prunes n's neighbors to mTarget if necessary using the heuristic.
func (s *Storage) linkAndPruneTx(tx *bbolt.Tx, tenant, n, nodeID string, nodeVec []float32, ℓ, mTarget int) error {
	rec, ok, err := readHNSWRecord(tx, tenant, n)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("hnsw: neighbor %q vanished during link", n)
	}
	if rec.Neighbors == nil {
		rec.Neighbors = map[int][]string{}
	}
	if !contains(rec.Neighbors[ℓ], nodeID) {
		rec.Neighbors[ℓ] = append(rec.Neighbors[ℓ], nodeID)
	}
	if len(rec.Neighbors[ℓ]) > mTarget {
		// Need n's vector to compute distances during pruning.
		nVec := make([]float32, s.dim)
		ok, err := readVectorTx(tx, tenant, n, s.dim, nVec)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("hnsw: vector for neighbor %q missing during prune", n)
		}
		// Build candidate list from current neighbors. Each candidate
		// carries its vector so the heuristic can compute pairwise
		// distances during the diversity check.
		cands := make([]candidate, 0, len(rec.Neighbors[ℓ]))
		buf := make([]float32, s.dim)
		for _, id := range rec.Neighbors[ℓ] {
			var v []float32
			if id == nodeID {
				v = nodeVec
			} else {
				ok, err := readVectorTx(tx, tenant, id, s.dim, buf)
				if err != nil {
					return err
				}
				if !ok {
					continue // missing neighbor vector — drop
				}
				v = make([]float32, s.dim)
				copy(v, buf)
			}
			cands = append(cands, candidate{id: id, dist: 1 - float64(dot(v, nVec)), vec: v})
		}
		chosen := selectNeighborsHeuristic(cands, mTarget)
		ids := make([]string, len(chosen))
		for i, c := range chosen {
			ids[i] = c.id
		}
		rec.Neighbors[ℓ] = ids
	}
	return writeHNSWRecord(tx, tenant, &rec)
}

// ----- HNSW search (Malkov §4 Alg.5) -----

// hnswSearchTx returns the top-n similarity hits using HNSW navigation.
// The vector is normalized; meta is fetched fresh from the backend.
func (s *Storage) hnswSearchTx(tx *bbolt.Tx, tenant string, meta *types.HNSWMeta, q []float32, n int) []vidx.Hit {
	if meta.Size == 0 || meta.EntryPoint == "" {
		return nil
	}
	cur := meta.EntryPoint
	curDist, err := s.distTo(tx, tenant, cur, q)
	if err != nil {
		return nil
	}
	for ℓ := meta.MaxLayer; ℓ > 0; ℓ-- {
		var newCur string
		newCur, curDist, err = s.greedyDescentTx(tx, tenant, cur, curDist, q, ℓ)
		if err != nil {
			return nil
		}
		cur = newCur
	}
	ef := max(meta.EfSearch, n)
	W, err := s.searchLayerTx(tx, tenant, q, []candidate{{id: cur, dist: curDist}}, ef, 0)
	if err != nil {
		return nil
	}
	if n > len(W) {
		n = len(W)
	}
	// Already distance-ascending; convert to similarity-descending hits.
	hits := make([]vidx.Hit, n)
	for i := 0; i < n; i++ {
		hits[i] = vidx.Hit{NodeID: W[i].id, Sim: 1 - W[i].dist}
	}
	return hits
}

// greedyDescentTx walks the layer from cur, replacing it with any neighbor
// strictly closer to q. Returns the local optimum.
func (s *Storage) greedyDescentTx(tx *bbolt.Tx, tenant, cur string, curDist float64, q []float32, ℓ int) (string, float64, error) {
	for {
		rec, ok, err := readHNSWRecord(tx, tenant, cur)
		if err != nil {
			return cur, curDist, err
		}
		if !ok {
			return cur, curDist, nil
		}
		nbrs := rec.Neighbors[ℓ]
		if len(nbrs) == 0 {
			return cur, curDist, nil
		}
		bestID := cur
		bestDist := curDist
		buf := make([]float32, s.dim)
		for _, id := range nbrs {
			ok, err := readVectorTx(tx, tenant, id, s.dim, buf)
			if err != nil {
				return cur, curDist, err
			}
			if !ok {
				continue
			}
			d := 1 - float64(dot(q, buf))
			if d < bestDist {
				bestID, bestDist = id, d
			}
		}
		if bestID == cur {
			return cur, curDist, nil
		}
		cur, curDist = bestID, bestDist
	}
}

// searchLayerTx returns up to ef closest nodes to q at layer ℓ, starting
// from the given entry-point candidates.
func (s *Storage) searchLayerTx(tx *bbolt.Tx, tenant string, q []float32, eps []candidate, ef, ℓ int) ([]candidate, error) {
	if ef < 1 {
		ef = 1
	}
	visited := make(map[string]struct{}, ef*4)
	candidatesPQ := newCandMinPQ() // closest first
	resultsPQ := newCandMaxPQ(ef)  // furthest at root, bounded
	for _, ep := range eps {
		if _, seen := visited[ep.id]; seen {
			continue
		}
		visited[ep.id] = struct{}{}
		candidatesPQ.push(ep)
		resultsPQ.push(ep)
	}

	buf := make([]float32, s.dim)
	for candidatesPQ.len() > 0 {
		c := candidatesPQ.pop()
		// If the closest unexplored candidate is further than the worst
		// kept result and we already have ef results, stop.
		if resultsPQ.len() >= ef && c.dist > resultsPQ.worst().dist {
			break
		}
		rec, ok, err := readHNSWRecord(tx, tenant, c.id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		for _, id := range rec.Neighbors[ℓ] {
			if _, seen := visited[id]; seen {
				continue
			}
			visited[id] = struct{}{}
			ok, err := readVectorTx(tx, tenant, id, s.dim, buf)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			d := 1 - float64(dot(q, buf))
			if resultsPQ.len() < ef || d < resultsPQ.worst().dist {
				vec := make([]float32, s.dim)
				copy(vec, buf)
				cand := candidate{id: id, dist: d, vec: vec}
				candidatesPQ.push(cand)
				resultsPQ.push(cand)
			}
		}
	}
	out := resultsPQ.sortedAsc()
	return out, nil
}

// distTo computes 1 - dot(q, vec(node)) and returns +Inf when the vector
// is missing (callers should treat as unreachable rather than failing).
func (s *Storage) distTo(tx *bbolt.Tx, tenant, nodeID string, q []float32) (float64, error) {
	buf := make([]float32, s.dim)
	ok, err := readVectorTx(tx, tenant, nodeID, s.dim, buf)
	if err != nil {
		return 0, err
	}
	if !ok {
		return math.Inf(1), nil
	}
	return 1 - float64(dot(q, buf)), nil
}

// ----- HNSW delete -----

// hnswDeleteTx removes nodeID from the HNSW graph: cleans neighbor lists,
// deletes the record, picks a new entry point if needed.
func (s *Storage) hnswDeleteTx(tx *bbolt.Tx, tenant, nodeID string) error {
	meta, found, err := readHNSWMeta(tx, tenant)
	if err != nil {
		return err
	}
	if !found || meta.Size == 0 {
		return nil
	}
	rec, ok, err := readHNSWRecord(tx, tenant, nodeID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := s.hnswDetachTx(tx, tenant, &meta, rec); err != nil {
		return err
	}
	if err := deleteHNSWRecord(tx, tenant, nodeID); err != nil {
		return err
	}
	meta.Size--
	if meta.Size == 0 {
		meta.EntryPoint = ""
		meta.MaxLayer = 0
		return writeHNSWMeta(tx, tenant, &meta)
	}
	if meta.EntryPoint == nodeID {
		newEP, newLayer, err := pickEntryPoint(tx, tenant)
		if err != nil {
			return err
		}
		meta.EntryPoint = newEP
		meta.MaxLayer = newLayer
	}
	return writeHNSWMeta(tx, tenant, &meta)
}

// hnswDetachTx removes nodeID from every neighbor's neighbor list across
// all layers. It does NOT delete the node's own HNSW record (caller does).
func (s *Storage) hnswDetachTx(tx *bbolt.Tx, tenant string, _ *types.HNSWMeta, rec types.HNSWRecord) error {
	touched := make(map[string]struct{})
	for _, nbrs := range rec.Neighbors {
		for _, n := range nbrs {
			touched[n] = struct{}{}
		}
	}
	for n := range touched {
		nrec, ok, err := readHNSWRecord(tx, tenant, n)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		changed := false
		for ℓ, ids := range nrec.Neighbors {
			filtered := ids[:0]
			for _, id := range ids {
				if id == rec.NodeID {
					changed = true
					continue
				}
				filtered = append(filtered, id)
			}
			nrec.Neighbors[ℓ] = filtered
		}
		if changed {
			if err := writeHNSWRecord(tx, tenant, &nrec); err != nil {
				return err
			}
		}
	}
	return nil
}

// pickEntryPoint scans hnsw_records and returns an ID at the highest
// remaining layer. If the bucket is empty, returns ("", 0, nil).
func pickEntryPoint(tx *bbolt.Tx, tenant string) (string, int, error) {
	rb, err := hnswRecords(tx, tenant, false)
	if err != nil || rb == nil {
		return "", 0, err
	}
	bestLayer := -1
	var bestID string
	if err := rb.ForEach(func(k, v []byte) error {
		var r types.HNSWRecord
		if err := decodeHNSWRecord(v, &r); err != nil {
			return err
		}
		if r.Layer > bestLayer {
			bestLayer = r.Layer
			bestID = string(k)
		}
		return nil
	}); err != nil {
		return "", 0, err
	}
	if bestLayer < 0 {
		return "", 0, nil
	}
	return bestID, bestLayer, nil
}

// ----- candidate priority queues -----

// candidate is one node-with-distance carried through HNSW search and
// neighbor selection. The optional vec is populated by callers that
// need pairwise distance computations (Malkov §4 Algorithm 4 diversity
// check). Search-only callers may leave vec nil.
type candidate struct {
	id   string
	dist float64
	vec  []float32
}

// candMinPQ pops the candidate with the smallest distance.
type candMinPQ struct{ data []candidate }

func newCandMinPQ() *candMinPQ { return &candMinPQ{} }

func (h *candMinPQ) len() int    { return len(h.data) }
func (h *candMinPQ) push(c candidate) {
	h.data = append(h.data, c)
	h.siftUp(len(h.data) - 1)
}
func (h *candMinPQ) pop() candidate {
	out := h.data[0]
	n := len(h.data)
	h.data[0] = h.data[n-1]
	h.data = h.data[:n-1]
	if len(h.data) > 0 {
		h.siftDown(0)
	}
	return out
}
func (h *candMinPQ) siftUp(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if h.data[p].dist <= h.data[i].dist {
			return
		}
		h.data[p], h.data[i] = h.data[i], h.data[p]
		i = p
	}
}
func (h *candMinPQ) siftDown(i int) {
	n := len(h.data)
	for {
		l, r := 2*i+1, 2*i+2
		small := i
		if l < n && h.data[l].dist < h.data[small].dist {
			small = l
		}
		if r < n && h.data[r].dist < h.data[small].dist {
			small = r
		}
		if small == i {
			return
		}
		h.data[i], h.data[small] = h.data[small], h.data[i]
		i = small
	}
}

// candMaxPQ pops the candidate with the largest distance. Bounded; pushing
// past cap evicts the largest after insertion to keep the cap.
type candMaxPQ struct {
	data []candidate
	cap  int
}

func newCandMaxPQ(cap int) *candMaxPQ { return &candMaxPQ{cap: cap} }

func (h *candMaxPQ) len() int { return len(h.data) }
func (h *candMaxPQ) push(c candidate) {
	if h.cap > 0 && len(h.data) >= h.cap {
		if c.dist >= h.data[0].dist {
			return // worse than worst-kept; skip
		}
		h.data[0] = c
		h.siftDownMax(0)
		return
	}
	h.data = append(h.data, c)
	h.siftUpMax(len(h.data) - 1)
}
func (h *candMaxPQ) worst() candidate { return h.data[0] }

func (h *candMaxPQ) sortedAsc() []candidate {
	out := make([]candidate, len(h.data))
	copy(out, h.data)
	sort.Slice(out, func(i, j int) bool { return out[i].dist < out[j].dist })
	return out
}

func (h *candMaxPQ) siftUpMax(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if h.data[p].dist >= h.data[i].dist {
			return
		}
		h.data[p], h.data[i] = h.data[i], h.data[p]
		i = p
	}
}
func (h *candMaxPQ) siftDownMax(i int) {
	n := len(h.data)
	for {
		l, r := 2*i+1, 2*i+2
		large := i
		if l < n && h.data[l].dist > h.data[large].dist {
			large = l
		}
		if r < n && h.data[r].dist > h.data[large].dist {
			large = r
		}
		if large == i {
			return
		}
		h.data[i], h.data[large] = h.data[large], h.data[i]
		i = large
	}
}

// ----- neighbor-selection heuristic (Malkov §4 Alg.4) -----

// selectNeighborsHeuristic picks up to m candidates that are diverse
// with respect to the implicit query point q (whatever the candidates'
// dist field measures distance to). Implements Algorithm 4 of Malkov
// & Yashunin (2018) with extendCandidates=false and
// keepPrunedConnections=true.
//
// Pull candidates closest-to-q first. Admit c iff for every already-
// chosen r, c is closer to q than to r — i.e., dist(c, q) < dist(c, r).
// Otherwise hold c in the discarded set. Once the main loop finishes,
// fill any remaining slots from the discarded set in dist-to-q order so
// we always return min(|W|, m) candidates.
//
// Each candidate must carry vec; callers that lack vectors get the
// same closest-first behavior as before since the diversity check
// silently degrades (no veci → no rejection).
func selectNeighborsHeuristic(W []candidate, m int) []candidate {
	if m <= 0 || len(W) == 0 {
		return nil
	}
	cands := make([]candidate, len(W))
	copy(cands, W)
	sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })

	if len(cands) <= m {
		return cands
	}

	chosen := make([]candidate, 0, m)
	discarded := make([]candidate, 0, len(cands))

	for _, c := range cands {
		if len(chosen) >= m {
			discarded = append(discarded, c)
			continue
		}
		admit := true
		if c.vec != nil {
			for _, r := range chosen {
				if r.vec == nil {
					continue
				}
				rdist := 1 - float64(dot(c.vec, r.vec))
				if rdist <= c.dist {
					admit = false
					break
				}
			}
		}
		if admit {
			chosen = append(chosen, c)
		} else {
			discarded = append(discarded, c)
		}
	}
	// keepPrunedConnections — fill remaining slots with the closest of
	// the discarded set (already in dist-ascending order).
	for _, d := range discarded {
		if len(chosen) >= m {
			break
		}
		chosen = append(chosen, d)
	}
	return chosen
}

// ----- helpers -----

func contains(s []string, v string) bool { return slices.Contains(s, v) }

// ErrHNSWMissingVector is returned when an HNSW operation requires a
// vector that is not present (corruption or partial delete).
var ErrHNSWMissingVector = errors.New("hnsw: vector missing")
