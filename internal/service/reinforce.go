package service

import (
	"context"
	"errors"
	"strings"

	"github.com/Cidan/memmy/internal/graph"
	"github.com/Cidan/memmy/internal/types"
)

// Reinforce applies an explicit caller-driven LTP bump to a single node.
// Used to record "this hit was useful in my answer."
//
// Bump magnitude is the configured NodeDelta. Repeated bumps approach
// WeightCap asymptotically (LogDampening). A per-node refractory window
// (RefractoryPeriod) prevents double-counting when a single retrieval
// is reinforced multiple times in quick succession; the call still
// updates LastTouched and AccessCount so the system knows the access
// happened. See DESIGN.md §8.2.
func (s *Service) Reinforce(ctx context.Context, req types.ReinforceRequest) (types.ReinforceResult, error) {
	tenant, err := s.requireValidTenant(req.Tenant)
	if err != nil {
		return types.ReinforceResult{}, err
	}
	if strings.TrimSpace(req.NodeID) == "" {
		return types.ReinforceResult{}, errors.New("service: reinforce requires NodeID")
	}

	node, skipped, err := s.applyExplicitNodeBump(ctx, tenant, req.NodeID, s.cfg.NodeDelta)
	if err != nil {
		return types.ReinforceResult{}, err
	}
	return types.ReinforceResult{
		NodeID:            node.ID,
		NewWeight:         node.Weight,
		SkippedRefractory: skipped,
	}, nil
}

// Demote applies an explicit caller-driven inhibition to a single node.
// Used to record "this hit was misleading or wrong."
//
// Bump magnitude is -NodeDelta. The post-update weight is clamped at
// NodeFloor — Demote never deletes the node. The same refractory window
// applies (a single retrieval can't be repeatedly demoted within the
// window). Demote bypasses LogDampening so its effect is not damped
// near WeightCap. See DESIGN.md §8.2.
func (s *Service) Demote(ctx context.Context, req types.DemoteRequest) (types.DemoteResult, error) {
	tenant, err := s.requireValidTenant(req.Tenant)
	if err != nil {
		return types.DemoteResult{}, err
	}
	if strings.TrimSpace(req.NodeID) == "" {
		return types.DemoteResult{}, errors.New("service: demote requires NodeID")
	}

	node, skipped, err := s.applyExplicitNodeBump(ctx, tenant, req.NodeID, -s.cfg.NodeDelta)
	if err != nil {
		return types.DemoteResult{}, err
	}
	return types.DemoteResult{
		NodeID:            node.ID,
		NewWeight:         node.Weight,
		SkippedRefractory: skipped,
	}, nil
}

// Mark retroactively boosts every node in the tenant whose CreatedAt
// falls within [Since, now]. Per-node delta is `Strength * NodeDelta`,
// scaled linearly by recency: most recently created nodes within the
// window get the full bump; nodes near the edge of the window get a
// fractional bump.
//
// Each per-node application goes through the same refractory and
// log-dampening path as Reinforce.
//
// See DESIGN.md §8.2 (synaptic-tag capture analog).
func (s *Service) Mark(ctx context.Context, req types.MarkRequest) (types.MarkResult, error) {
	tenant, err := s.requireValidTenant(req.Tenant)
	if err != nil {
		return types.MarkResult{}, err
	}
	if req.Strength <= 0 {
		return types.MarkResult{}, errors.New("service: mark strength must be > 0")
	}
	now := s.clock.Now()
	if req.Since.IsZero() || !req.Since.Before(now) {
		return types.MarkResult{}, errors.New("service: mark since must be a past timestamp")
	}

	scanner, ok := s.graph.(recentNodeScanner)
	if !ok {
		return types.MarkResult{}, errors.New("service: graph backend does not support RecentNodeIDs")
	}
	maxN := s.cfg.MarkMaxNodes
	if maxN <= 0 {
		maxN = 256
	}
	ids, err := scanner.RecentNodeIDs(ctx, tenant, req.Since, "", maxN)
	if err != nil {
		return types.MarkResult{}, err
	}

	windowSecs := now.Sub(req.Since).Seconds()
	var out types.MarkResult
	for _, id := range ids {
		n, err := s.graph.GetNode(ctx, tenant, id)
		if err != nil {
			if errors.Is(err, graph.ErrNotFound) {
				continue
			}
			return out, err
		}
		// Recency-weighted: most-recent → full bump, oldest → ~0 bump.
		recency := 1.0
		if windowSecs > 0 {
			age := now.Sub(n.CreatedAt).Seconds()
			recency = 1.0 - age/windowSecs
		}
		if recency < 0 {
			recency = 0
		}
		if recency > 1 {
			recency = 1
		}
		delta := req.Strength * s.cfg.NodeDelta * recency
		if delta <= 0 {
			continue
		}
		_, skipped, err := s.applyExplicitNodeBump(ctx, tenant, id, delta)
		if err != nil {
			if errors.Is(err, graph.ErrNotFound) {
				continue
			}
			return out, err
		}
		if skipped {
			out.NodesSkippedRefractory++
		} else {
			out.NodesAffected++
		}
	}
	return out, nil
}
