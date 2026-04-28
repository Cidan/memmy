package service

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/Cidan/memmy/internal/embed"
	"github.com/Cidan/memmy/internal/graph"
	"github.com/Cidan/memmy/internal/types"
)

// embedTaskRetrievalQuery is a small alias to keep the import surface
// of recall.go tight and the call site readable.
const embedTaskRetrievalQuery = embed.EmbedTaskRetrievalQuery

// Recall implements the full retrieval pipeline (DESIGN.md §6).
//
// Phases:
//  1. Embed query, normalize.
//  2. Vector search with oversample.
//  3. For each candidate: lazy-decay-then-reinforce node weight.
//  4. Re-rank by sim^α × weight^β; take top-K → seeds.
//  5. Hebbian co-retrieval: reinforce edges between every seed pair.
//  6. BFS expansion via memory edges, with edge-floor pruning + depth penalty.
//  7. Co-traversal reinforcement on edges that delivered nodes into the
//     final returned result set.
//  8. Build provenance hits with score breakdowns + path.
func (s *Service) Recall(ctx context.Context, req types.RecallRequest) (types.RecallResult, error) {
	if strings.TrimSpace(req.Query) == "" {
		return types.RecallResult{}, errors.New("service: query required")
	}
	tenant, err := s.requireValidTenant(req.Tenant)
	if err != nil {
		return types.RecallResult{}, err
	}
	k := req.K
	if k <= 0 {
		k = s.cfg.DefaultK
	}
	hops := req.Hops
	if hops < 0 {
		hops = 0
	}
	if req.Hops == 0 {
		hops = s.cfg.DefaultHops
	}
	overN := req.OversampleN
	if overN <= 0 {
		overN = s.cfg.DefaultOversample
	}
	if overN < k {
		overN = k
	}

	// Phase 1 — embed query.
	// Queries get RetrievalQuery so the model tunes the vector for the
	// "search input" side of the asymmetric pair (writes use
	// RetrievalDocument — see write.go).
	embed, err := s.embedder.Embed(ctx, embedTaskRetrievalQuery, []string{req.Query})
	if err != nil {
		return types.RecallResult{}, err
	}
	if len(embed) == 0 {
		return types.RecallResult{}, errors.New("service: embedder returned 0 vectors")
	}

	// Phase 2 — vector search.
	hits, err := s.vidx.Search(ctx, tenant, embed[0], overN)
	if err != nil {
		return types.RecallResult{}, err
	}
	if len(hits) == 0 {
		return types.RecallResult{}, nil
	}

	// Phase 3 — for each candidate: lazy decay+reinforce; build a scored list.
	type candidate struct {
		nodeID         string
		sim            float64
		nodeWeight     float64
		combinedScore  float64
		node           types.Node
		alive          bool
	}
	cands := make([]candidate, 0, len(hits))
	for _, h := range hits {
		n, err := s.applyNodeDecayReinforce(ctx, tenant, h.NodeID, s.cfg.NodeDelta)
		if err != nil {
			if errors.Is(err, graph.ErrNotFound) {
				continue
			}
			return types.RecallResult{}, err
		}
		score := math.Pow(simScore(h.Sim), s.cfg.SimAlpha) * math.Pow(n.Weight, s.cfg.WeightBeta)
		cands = append(cands, candidate{
			nodeID:        h.NodeID,
			sim:           h.Sim,
			nodeWeight:    n.Weight,
			combinedScore: score,
			node:          n,
			alive:         true,
		})
	}
	if len(cands) == 0 {
		return types.RecallResult{}, nil
	}

	// Phase 4 — sort by combined score; take top-K seeds.
	sort.Slice(cands, func(i, j int) bool { return cands[i].combinedScore > cands[j].combinedScore })
	seedCount := k
	if seedCount > len(cands) {
		seedCount = len(cands)
	}
	seeds := cands[:seedCount]

	// Phase 5 — Hebbian co-retrieval. For each unordered seed pair (i, j)
	// reinforce the directed edges in both directions so graph expansion
	// works regardless of seed origin.
	for i := 0; i < len(seeds); i++ {
		for j := i + 1; j < len(seeds); j++ {
			delta := s.cfg.EdgeCoRetrievalBase * math.Min(simScore(seeds[i].sim), simScore(seeds[j].sim))
			if delta <= 0 {
				continue
			}
			if _, _, err := s.applyEdgeDecayReinforce(ctx, tenant, seeds[i].nodeID, seeds[j].nodeID, types.EdgeCoRetrieval, delta, true); err != nil {
				return types.RecallResult{}, err
			}
			if _, _, err := s.applyEdgeDecayReinforce(ctx, tenant, seeds[j].nodeID, seeds[i].nodeID, types.EdgeCoRetrieval, delta, true); err != nil {
				return types.RecallResult{}, err
			}
		}
	}

	// Phase 6 — BFS expansion via memory edges from each seed.
	type visited struct {
		bestPath  []string  // chain from a seed to this node
		bestScore float64   // path_score (not yet multiplied by node weight)
		seedID    string    // origin seed
		depth     int
		viaEdge   bool      // true if reached via expansion (not a seed itself)
	}
	visit := make(map[string]*visited, len(seeds))
	type frontier struct {
		nodeID    string
		seedID    string
		pathScore float64
		path      []string
		depth     int
	}
	queue := make([]frontier, 0, len(seeds))
	for _, sd := range seeds {
		visit[sd.nodeID] = &visited{
			bestPath:  []string{sd.nodeID},
			bestScore: sd.combinedScore,
			seedID:    sd.nodeID,
			depth:     0,
			viaEdge:   false,
		}
		queue = append(queue, frontier{
			nodeID:    sd.nodeID,
			seedID:    sd.nodeID,
			pathScore: sd.combinedScore,
			path:      []string{sd.nodeID},
			depth:     0,
		})
	}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		if f.depth >= hops {
			continue
		}
		neighbors, err := s.graph.Neighbors(ctx, tenant, f.nodeID)
		if err != nil {
			return types.RecallResult{}, err
		}
		for _, ne := range neighbors {
			edge, alive, err := s.readDecayedEdge(ctx, tenant, ne.From, ne.To)
			if err != nil {
				return types.RecallResult{}, err
			}
			if !alive {
				continue
			}
			nextDepth := f.depth + 1
			penalty := math.Pow(s.cfg.DepthPenaltyFactor, float64(nextDepth))
			contribution := f.pathScore * edge.Weight / penalty
			if contribution <= 0 {
				continue
			}
			existing, seen := visit[edge.To]
			if seen && existing.bestScore >= contribution {
				continue
			}
			newPath := append(append([]string{}, f.path...), edge.To)
			visit[edge.To] = &visited{
				bestPath:  newPath,
				bestScore: contribution,
				seedID:    f.seedID,
				depth:     nextDepth,
				viaEdge:   true,
			}
			queue = append(queue, frontier{
				nodeID:    edge.To,
				seedID:    f.seedID,
				pathScore: contribution,
				path:      newPath,
				depth:     nextDepth,
			})
		}
	}

	// Phase 7 — score all visited nodes and pick top-K.
	type result struct {
		nodeID    string
		score     float64
		breakdown types.ScoreBreakdown
		path      []string
		viaEdge   bool
	}
	candByID := make(map[string]candidate, len(cands))
	for _, c := range cands {
		candByID[c.nodeID] = c
	}
	results := make([]result, 0, len(visit))
	for nodeID, v := range visit {
		var nodeWeight float64
		var sim float64
		if c, ok := candByID[nodeID]; ok {
			nodeWeight = c.nodeWeight
			sim = c.sim
		} else {
			n, err := s.graph.GetNode(ctx, tenant, nodeID)
			if err != nil {
				if errors.Is(err, graph.ErrNotFound) {
					continue
				}
				return types.RecallResult{}, err
			}
			nodeWeight = n.Weight
		}
		score := v.bestScore * math.Pow(nodeWeight, s.cfg.WeightBeta)
		results = append(results, result{
			nodeID: nodeID,
			score:  score,
			breakdown: types.ScoreBreakdown{
				Sim:        sim,
				NodeWeight: nodeWeight,
				GraphMult:  v.bestScore,
				Depth:      v.depth,
			},
			path:    v.bestPath,
			viaEdge: v.viaEdge,
		})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	if len(results) > k {
		results = results[:k]
	}

	// Phase 8 — co-traversal reinforcement on edges that delivered an
	// expansion-reached node into the final result set.
	for _, r := range results {
		if !r.viaEdge {
			continue
		}
		// Reinforce the last hop along the path that delivered this node.
		if len(r.path) < 2 {
			continue
		}
		for i := 0; i+1 < len(r.path); i++ {
			from := r.path[i]
			to := r.path[i+1]
			delta := s.cfg.EdgeCoTraversalMultiplier * s.cfg.EdgeCoRetrievalBase
			if _, _, err := s.applyEdgeDecayReinforce(ctx, tenant, from, to, types.EdgeCoTraversal, delta, true); err != nil {
				return types.RecallResult{}, err
			}
		}
	}

	// Phase 9 — build response with provenance.
	hitsOut := make([]types.RecallHit, 0, len(results))
	for _, r := range results {
		node, err := s.graph.GetNode(ctx, tenant, r.nodeID)
		if err != nil {
			if errors.Is(err, graph.ErrNotFound) {
				continue
			}
			return types.RecallResult{}, err
		}
		var sourceText string
		if msg, err := s.graph.GetMessage(ctx, tenant, node.SourceMsgID); err == nil {
			sourceText = msg.Text
		}
		hitsOut = append(hitsOut, types.RecallHit{
			NodeID:         r.nodeID,
			Text:           node.Text,
			SourceMsgID:    node.SourceMsgID,
			SourceText:     sourceText,
			Score:          r.score,
			ScoreBreakdown: r.breakdown,
			Path:           r.path,
		})
	}

	return types.RecallResult{Results: hitsOut}, nil
}

// simScore maps cosine similarity from [-1, 1] to [0, 1] so it can be
// used in `sim^α × weight^β` without zeroing out negative-but-relevant
// scores and discarding the weight contribution. Anti-correlated
// vectors map to 0; orthogonal to 0.5; identical to 1.
func simScore(s float64) float64 {
	v := (s + 1) / 2
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
