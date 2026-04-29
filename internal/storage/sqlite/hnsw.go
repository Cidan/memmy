package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"sync"

	"github.com/Cidan/memmy/internal/types"
	vidx "github.com/Cidan/memmy/internal/vectorindex"
)

// hnswRand wraps *rand.Rand with a mutex so concurrent writers cannot
// race on the layer-sampling source.
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

// ----- HNSW row helpers -----

func readHNSWMetaTx(ctx context.Context, tx *sql.Tx, tenant string) (types.HNSWMeta, bool, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT blob FROM hnsw_meta WHERE tenant = ?`, tenant).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return types.HNSWMeta{}, false, nil
	}
	if err != nil {
		return types.HNSWMeta{}, false, err
	}
	var m types.HNSWMeta
	if err := decodeHNSWMeta(raw, &m); err != nil {
		return types.HNSWMeta{}, false, err
	}
	return m, true, nil
}

func writeHNSWMetaTx(ctx context.Context, tx *sql.Tx, tenant string, m *types.HNSWMeta) error {
	buf, err := encodeHNSWMeta(m)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO hnsw_meta(tenant, blob) VALUES(?, ?)
		ON CONFLICT(tenant) DO UPDATE SET blob = excluded.blob
	`, tenant, buf)
	return err
}

func readHNSWRecordTx(ctx context.Context, tx *sql.Tx, tenant, nodeID string) (types.HNSWRecord, bool, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT blob FROM hnsw_records WHERE tenant = ? AND node_id = ?`, tenant, nodeID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return types.HNSWRecord{}, false, nil
	}
	if err != nil {
		return types.HNSWRecord{}, false, err
	}
	var r types.HNSWRecord
	if err := decodeHNSWRecord(raw, &r); err != nil {
		return types.HNSWRecord{}, false, err
	}
	return r, true, nil
}

func writeHNSWRecordTx(ctx context.Context, tx *sql.Tx, tenant string, r *types.HNSWRecord) error {
	buf, err := encodeHNSWRecord(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO hnsw_records(tenant, node_id, blob) VALUES(?, ?, ?)
		ON CONFLICT(tenant, node_id) DO UPDATE SET blob = excluded.blob
	`, tenant, r.NodeID, buf)
	return err
}

func deleteHNSWRecordTx(ctx context.Context, tx *sql.Tx, tenant, nodeID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM hnsw_records WHERE tenant = ? AND node_id = ?`, tenant, nodeID)
	return err
}

// ----- HNSW insert (Malkov §4 Alg.1) -----

// hnswInsertTx links a new node into the HNSW graph inside the given tx.
// The vector must already be persisted to vectors by the caller. All
// HNSW reads and writes happen here within the same tx.
func (s *Storage) hnswInsertTx(ctx context.Context, tx *sql.Tx, tenant, nodeID string, vec []float32) error {
	if s.rand == nil {
		// Defensive — Open should always set this.
		s.rand = newHNSWRand(1)
	}

	// Upsert path: if nodeID already exists, fully remove it from the
	// graph (cleaning neighbors, decrementing Size, replacing entry
	// point if needed) so we re-insert into a consistent state.
	if _, ok, err := readHNSWRecordTx(ctx, tx, tenant, nodeID); err != nil {
		return err
	} else if ok {
		if err := s.hnswDeleteTx(ctx, tx, tenant, nodeID); err != nil {
			return err
		}
	}

	meta, found, err := readHNSWMetaTx(ctx, tx, tenant)
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

	L := s.rand.sampleLayer(meta.ML)

	if meta.Size == 0 {
		rec := types.HNSWRecord{
			NodeID:    nodeID,
			Layer:     L,
			Neighbors: emptyNeighbors(L),
		}
		if err := writeHNSWRecordTx(ctx, tx, tenant, &rec); err != nil {
			return err
		}
		meta.EntryPoint = nodeID
		meta.MaxLayer = L
		meta.Size = 1
		return writeHNSWMetaTx(ctx, tx, tenant, &meta)
	}

	// Phase 1 — greedy descent from MaxLayer down to L+1 with ef=1.
	cur := meta.EntryPoint
	curDist, err := s.distToTx(ctx, tx, tenant, cur, vec)
	if err != nil {
		return err
	}
	for ℓ := meta.MaxLayer; ℓ > L; ℓ-- {
		cur, curDist, err = s.greedyDescentTx(ctx, tx, tenant, cur, curDist, vec, ℓ)
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
		W, err := s.searchLayerTx(ctx, tx, tenant, vec, entryPoints, meta.EfConstruction, ℓ)
		if err != nil {
			return err
		}
		mTarget := meta.M
		if ℓ == 0 {
			mTarget = meta.M0
		}
		chosen := selectNeighborsHeuristic(W, mTarget)

		ids := make([]string, len(chosen))
		for i, c := range chosen {
			ids[i] = c.id
		}
		rec.Neighbors[ℓ] = ids

		for _, c := range chosen {
			if err := s.linkAndPruneTx(ctx, tx, tenant, c.id, nodeID, vec, ℓ, mTarget); err != nil {
				return err
			}
		}

		entryPoints = W
	}

	if err := writeHNSWRecordTx(ctx, tx, tenant, &rec); err != nil {
		return err
	}

	if L > meta.MaxLayer {
		meta.MaxLayer = L
		meta.EntryPoint = nodeID
	}
	meta.Size++
	return writeHNSWMetaTx(ctx, tx, tenant, &meta)
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
func (s *Storage) linkAndPruneTx(ctx context.Context, tx *sql.Tx, tenant, n, nodeID string, nodeVec []float32, ℓ, mTarget int) error {
	rec, ok, err := readHNSWRecordTx(ctx, tx, tenant, n)
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
		nVec := make([]float32, s.dim)
		ok, err := readVectorTx(ctx, tx, tenant, n, s.dim, nVec)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("hnsw: vector for neighbor %q missing during prune", n)
		}
		cands := make([]candidate, 0, len(rec.Neighbors[ℓ]))
		buf := make([]float32, s.dim)
		for _, id := range rec.Neighbors[ℓ] {
			var v []float32
			if id == nodeID {
				v = nodeVec
			} else {
				ok, err := readVectorTx(ctx, tx, tenant, id, s.dim, buf)
				if err != nil {
					return err
				}
				if !ok {
					continue
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
	return writeHNSWRecordTx(ctx, tx, tenant, &rec)
}

// ----- HNSW search (Malkov §4 Alg.5) -----

// hnswSearchTx returns the top-n similarity hits using HNSW navigation.
func (s *Storage) hnswSearchTx(ctx context.Context, tx *sql.Tx, tenant string, meta *types.HNSWMeta, q []float32, n int) []vidx.Hit {
	if meta.Size == 0 || meta.EntryPoint == "" {
		return nil
	}
	cur := meta.EntryPoint
	curDist, err := s.distToTx(ctx, tx, tenant, cur, q)
	if err != nil {
		return nil
	}
	for ℓ := meta.MaxLayer; ℓ > 0; ℓ-- {
		var newCur string
		newCur, curDist, err = s.greedyDescentTx(ctx, tx, tenant, cur, curDist, q, ℓ)
		if err != nil {
			return nil
		}
		cur = newCur
	}
	ef := max(meta.EfSearch, n)
	W, err := s.searchLayerTx(ctx, tx, tenant, q, []candidate{{id: cur, dist: curDist}}, ef, 0)
	if err != nil {
		return nil
	}
	if n > len(W) {
		n = len(W)
	}
	hits := make([]vidx.Hit, n)
	for i := 0; i < n; i++ {
		hits[i] = vidx.Hit{NodeID: W[i].id, Sim: 1 - W[i].dist}
	}
	return hits
}

// greedyDescentTx walks the layer from cur, replacing it with any
// neighbor strictly closer to q. Returns the local optimum.
func (s *Storage) greedyDescentTx(ctx context.Context, tx *sql.Tx, tenant, cur string, curDist float64, q []float32, ℓ int) (string, float64, error) {
	for {
		rec, ok, err := readHNSWRecordTx(ctx, tx, tenant, cur)
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
			ok, err := readVectorTx(ctx, tx, tenant, id, s.dim, buf)
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

// searchLayerTx returns up to ef closest nodes to q at layer ℓ,
// starting from the given entry-point candidates.
func (s *Storage) searchLayerTx(ctx context.Context, tx *sql.Tx, tenant string, q []float32, eps []candidate, ef, ℓ int) ([]candidate, error) {
	if ef < 1 {
		ef = 1
	}
	visited := make(map[string]struct{}, ef*4)
	candidatesPQ := newCandMinPQ()
	resultsPQ := newCandMaxPQ(ef)
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
		if resultsPQ.len() >= ef && c.dist > resultsPQ.worst().dist {
			break
		}
		rec, ok, err := readHNSWRecordTx(ctx, tx, tenant, c.id)
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
			ok, err := readVectorTx(ctx, tx, tenant, id, s.dim, buf)
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
	return resultsPQ.sortedAsc(), nil
}

// distToTx computes 1 - dot(q, vec(node)).
func (s *Storage) distToTx(ctx context.Context, tx *sql.Tx, tenant, nodeID string, q []float32) (float64, error) {
	buf := make([]float32, s.dim)
	ok, err := readVectorTx(ctx, tx, tenant, nodeID, s.dim, buf)
	if err != nil {
		return 0, err
	}
	if !ok {
		return math.Inf(1), nil
	}
	return 1 - float64(dot(q, buf)), nil
}

// ----- HNSW delete -----

func (s *Storage) hnswDeleteTx(ctx context.Context, tx *sql.Tx, tenant, nodeID string) error {
	meta, found, err := readHNSWMetaTx(ctx, tx, tenant)
	if err != nil {
		return err
	}
	if !found || meta.Size == 0 {
		return nil
	}
	rec, ok, err := readHNSWRecordTx(ctx, tx, tenant, nodeID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := s.hnswDetachTx(ctx, tx, tenant, &meta, rec); err != nil {
		return err
	}
	if err := deleteHNSWRecordTx(ctx, tx, tenant, nodeID); err != nil {
		return err
	}
	meta.Size--
	if meta.Size == 0 {
		meta.EntryPoint = ""
		meta.MaxLayer = 0
		return writeHNSWMetaTx(ctx, tx, tenant, &meta)
	}
	if meta.EntryPoint == nodeID {
		newEP, newLayer, err := pickEntryPointTx(ctx, tx, tenant)
		if err != nil {
			return err
		}
		meta.EntryPoint = newEP
		meta.MaxLayer = newLayer
	}
	return writeHNSWMetaTx(ctx, tx, tenant, &meta)
}

// hnswDetachTx removes nodeID from every neighbor's neighbor list
// across all layers.
func (s *Storage) hnswDetachTx(ctx context.Context, tx *sql.Tx, tenant string, _ *types.HNSWMeta, rec types.HNSWRecord) error {
	touched := make(map[string]struct{})
	for _, nbrs := range rec.Neighbors {
		for _, n := range nbrs {
			touched[n] = struct{}{}
		}
	}
	for n := range touched {
		nrec, ok, err := readHNSWRecordTx(ctx, tx, tenant, n)
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
			if err := writeHNSWRecordTx(ctx, tx, tenant, &nrec); err != nil {
				return err
			}
		}
	}
	return nil
}

// pickEntryPointTx scans hnsw_records for the tenant and returns an ID
// at the highest remaining layer.
func pickEntryPointTx(ctx context.Context, tx *sql.Tx, tenant string) (string, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT node_id, blob FROM hnsw_records WHERE tenant = ?`, tenant)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	bestLayer := -1
	var bestID string
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return "", 0, err
		}
		var r types.HNSWRecord
		if err := decodeHNSWRecord(raw, &r); err != nil {
			return "", 0, err
		}
		if r.Layer > bestLayer {
			bestLayer = r.Layer
			bestID = id
		}
	}
	if err := rows.Err(); err != nil {
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

type candMinPQ struct{ data []candidate }

func newCandMinPQ() *candMinPQ { return &candMinPQ{} }

func (h *candMinPQ) len() int { return len(h.data) }
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

// candMaxPQ pops the candidate with the largest distance.
type candMaxPQ struct {
	data []candidate
	cap  int
}

func newCandMaxPQ(cap int) *candMaxPQ { return &candMaxPQ{cap: cap} }

func (h *candMaxPQ) len() int { return len(h.data) }
func (h *candMaxPQ) push(c candidate) {
	if h.cap > 0 && len(h.data) >= h.cap {
		if c.dist >= h.data[0].dist {
			return
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

// selectNeighborsHeuristic picks up to m candidates that are diverse
// w.r.t. the implicit query point q. Implements Malkov & Yashunin (2018)
// Algorithm 4 with extendCandidates=false and keepPrunedConnections=true.
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
	for _, d := range discarded {
		if len(chosen) >= m {
			break
		}
		chosen = append(chosen, d)
	}
	return chosen
}

func contains(s []string, v string) bool { return slices.Contains(s, v) }
