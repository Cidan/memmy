package service

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/Cidan/memmy/internal/graph"
	"github.com/Cidan/memmy/internal/types"
)

// applyNodeDecayReinforce runs lazy-decay-then-reinforce on a node within
// a single Graph.UpdateNode tx. Returns the post-update Node so callers
// can use the fresh weight for ranking without a separate read.
func (s *Service) applyNodeDecayReinforce(ctx context.Context, tenant, nodeID string, delta float64) (types.Node, error) {
	var out types.Node
	err := s.graph.UpdateNode(ctx, tenant, nodeID, func(n *types.Node) error {
		now := s.clock.Now()
		n.Weight = capWeight(decay(n.Weight, s.cfg.NodeLambda, now.Sub(n.LastTouched))+delta, s.cfg.WeightCap)
		n.LastTouched = now
		n.AccessCount++
		out = *n
		return nil
	})
	return out, err
}

// applyEdgeDecayReinforce runs lazy-decay-then-reinforce on an edge,
// creating it if absent (with the supplied kind and weight as initial).
// Returns the post-update edge.
//
// If the decayed weight (after reinforcement) is below s.cfg.EdgeFloor,
// the edge is deleted. The returned (edge, true) indicates the edge
// survived; (zero, false) indicates it was pruned.
func (s *Service) applyEdgeDecayReinforce(
	ctx context.Context,
	tenant, from, to string,
	kind types.EdgeKind,
	delta float64,
	co bool,
) (types.MemoryEdge, bool, error) {
	now := s.clock.Now()
	lambda := s.lambdaForKind(kind)

	existing, found, err := s.graph.GetEdge(ctx, tenant, from, to)
	if err != nil {
		return types.MemoryEdge{}, false, err
	}
	if !found {
		// Symmetric Hebbian edges: when forming co-retrieval/co-traversal
		// edges we always store both directions of the association so
		// graph expansion works in either direction. Structural edges
		// are also written as undirected pairs by the caller.
		e := types.MemoryEdge{
			From:        from,
			To:          to,
			TenantID:    tenant,
			Kind:        kind,
			Weight:      capWeight(delta, s.cfg.WeightCap),
			LastTouched: now,
			CreatedAt:   now,
		}
		if co && kind == types.EdgeCoRetrieval {
			e.AccessCount = 1
		}
		if co && kind == types.EdgeCoTraversal {
			e.TraverseCount = 1
		}
		if e.Weight < s.cfg.EdgeFloor {
			return types.MemoryEdge{}, false, nil
		}
		if err := s.graph.PutEdge(ctx, e); err != nil {
			return types.MemoryEdge{}, false, err
		}
		return e, true, nil
	}
	// Update existing.
	var out types.MemoryEdge
	pruned := false
	err = s.graph.UpdateEdge(ctx, tenant, from, to, func(e *types.MemoryEdge) error {
		newWeight := capWeight(decay(e.Weight, lambda, now.Sub(e.LastTouched))+delta, s.cfg.WeightCap)
		e.Weight = newWeight
		e.LastTouched = now
		// Promote kind if the operation indicates a stronger linkage.
		// CoTraversal > CoRetrieval > Structural in informational value.
		if kindRank(kind) > kindRank(e.Kind) {
			e.Kind = kind
		}
		if co && kind == types.EdgeCoRetrieval {
			e.AccessCount++
		}
		if co && kind == types.EdgeCoTraversal {
			e.TraverseCount++
		}
		if newWeight < s.cfg.EdgeFloor {
			pruned = true
			return nil
		}
		out = *e
		return nil
	})
	if err != nil {
		return types.MemoryEdge{}, false, err
	}
	if pruned {
		if delErr := s.graph.DeleteEdge(ctx, tenant, from, to); delErr != nil {
			return types.MemoryEdge{}, false, delErr
		}
		return types.MemoryEdge{}, false, nil
	}
	_ = existing
	return out, true, nil
}

// readDecayedEdge fetches an edge and writes its decayed weight back.
// Used during graph expansion when we need an up-to-date view without
// reinforcement. Prunes the edge if it falls below EdgeFloor.
//
// Returns (edge, alive, error). alive=false means the edge was pruned
// or never existed.
func (s *Service) readDecayedEdge(ctx context.Context, tenant, from, to string) (types.MemoryEdge, bool, error) {
	now := s.clock.Now()
	var out types.MemoryEdge
	pruned := false
	err := s.graph.UpdateEdge(ctx, tenant, from, to, func(e *types.MemoryEdge) error {
		lambda := s.lambdaForKind(e.Kind)
		e.Weight = decay(e.Weight, lambda, now.Sub(e.LastTouched))
		e.LastTouched = now
		if e.Weight < s.cfg.EdgeFloor {
			pruned = true
			return nil
		}
		out = *e
		return nil
	})
	if errors.Is(err, graph.ErrNotFound) {
		return types.MemoryEdge{}, false, nil
	}
	if err != nil {
		return types.MemoryEdge{}, false, err
	}
	if pruned {
		if delErr := s.graph.DeleteEdge(ctx, tenant, from, to); delErr != nil {
			return types.MemoryEdge{}, false, delErr
		}
		return types.MemoryEdge{}, false, nil
	}
	return out, true, nil
}

func (s *Service) lambdaForKind(k types.EdgeKind) float64 {
	switch k {
	case types.EdgeStructural:
		return s.cfg.EdgeStructuralLambda
	case types.EdgeCoRetrieval:
		return s.cfg.EdgeCoRetrievalLambda
	case types.EdgeCoTraversal:
		return s.cfg.EdgeCoTraversalLambda
	default:
		return s.cfg.EdgeStructuralLambda
	}
}

// kindRank orders edge kinds by informational rank. Higher = stronger.
func kindRank(k types.EdgeKind) int {
	switch k {
	case types.EdgeCoTraversal:
		return 3
	case types.EdgeCoRetrieval:
		return 2
	case types.EdgeStructural:
		return 1
	default:
		return 0
	}
}

// decay applies exponential decay over dt and returns the new weight.
// Returns 0 for negative weights / dt to keep arithmetic clean.
func decay(weight, lambda float64, dt time.Duration) float64 {
	if weight <= 0 {
		return 0
	}
	if dt <= 0 {
		return weight
	}
	return weight * math.Exp(-lambda*dt.Seconds())
}

func capWeight(w, cap float64) float64 {
	if w < 0 {
		return 0
	}
	if cap > 0 && w > cap {
		return cap
	}
	return w
}
