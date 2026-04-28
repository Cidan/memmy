package service

import (
	"context"

	"github.com/Cidan/memmy/internal/types"
)

// Stats aggregates counts for one tenant or all tenants.
func (s *Service) Stats(ctx context.Context, req types.StatsRequest) (types.StatsResult, error) {
	if scanner, ok := s.graph.(statsScanner); ok {
		var tenants []string
		if len(req.Tenant) > 0 {
			id, err := s.requireValidTenant(req.Tenant)
			if err != nil {
				return types.StatsResult{}, err
			}
			tenants = []string{id}
		} else {
			ts, err := s.graph.ListTenants(ctx)
			if err != nil {
				return types.StatsResult{}, err
			}
			for _, t := range ts {
				tenants = append(tenants, t.ID)
			}
		}

		var out types.StatsResult
		var nodeWeightSum, edgeWeightSum float64
		for _, t := range tenants {
			ts, err := scanner.TenantStats(ctx, t)
			if err != nil {
				return types.StatsResult{}, err
			}
			out.NodeCount += ts.NodeCount
			out.MemoryEdgeCount += ts.EdgeCount
			out.HNSWSize += ts.HNSWSize
			nodeWeightSum += ts.SumNodeWeight
			edgeWeightSum += ts.SumEdgeWeight
		}
		if out.NodeCount > 0 {
			out.AvgNodeWeight = nodeWeightSum / float64(out.NodeCount)
		}
		if out.MemoryEdgeCount > 0 {
			out.AvgEdgeWeight = edgeWeightSum / float64(out.MemoryEdgeCount)
		}
		return out, nil
	}
	return types.StatsResult{}, nil
}

// TenantStats is the per-tenant breakdown returned by a backend implementing
// statsScanner.
type TenantStats struct {
	NodeCount     int
	EdgeCount     int
	HNSWSize      int
	SumNodeWeight float64
	SumEdgeWeight float64
}

// statsScanner is an optional backend capability for stats aggregation.
type statsScanner interface {
	TenantStats(ctx context.Context, tenant string) (TenantStats, error)
}
