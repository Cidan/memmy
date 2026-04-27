package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Cidan/memmy/internal/graph"
	"github.com/Cidan/memmy/internal/types"
)

// Forget removes either a single message (and all derived nodes/vectors/
// HNSW records/edges) by ID, or all messages created strictly before a
// given timestamp.
func (s *Service) Forget(ctx context.Context, req types.ForgetRequest) (types.ForgetResult, error) {
	tenant := types.TenantID(req.Tenant)
	if tenant == "" {
		return types.ForgetResult{}, errors.New("service: tenant required")
	}
	if strings.TrimSpace(req.MessageID) == "" && req.Before.IsZero() {
		return types.ForgetResult{}, errors.New("service: forget requires MessageID or Before")
	}

	var (
		out     types.ForgetResult
		msgIDs  []string
	)
	if req.MessageID != "" {
		msgIDs = []string{req.MessageID}
	} else {
		ids, err := s.messagesBefore(ctx, tenant, req.Before)
		if err != nil {
			return types.ForgetResult{}, err
		}
		msgIDs = ids
	}

	for _, msgID := range msgIDs {
		nodes, err := s.nodesForMessage(ctx, tenant, msgID)
		if err != nil {
			return out, err
		}
		for _, n := range nodes {
			edgesDeleted, err := s.purgeNodeEdges(ctx, tenant, n.ID)
			if err != nil {
				return out, err
			}
			out.DeletedEdges += edgesDeleted

			if err := s.vidx.Delete(ctx, tenant, n.ID); err != nil {
				return out, err
			}
			out.DeletedVectors++
			if err := s.graph.DeleteNode(ctx, tenant, n.ID); err != nil {
				return out, err
			}
			out.DeletedNodes++
		}
		if err := s.graph.DeleteMessage(ctx, tenant, msgID); err != nil {
			return out, err
		}
	}
	return out, nil
}

// purgeNodeEdges deletes every memory edge incident to nodeID (in either
// direction) and returns the count.
func (s *Service) purgeNodeEdges(ctx context.Context, tenant, nodeID string) (int, error) {
	out, err := s.graph.Neighbors(ctx, tenant, nodeID)
	if err != nil {
		return 0, err
	}
	in, err := s.graph.InboundNeighbors(ctx, tenant, nodeID)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, e := range out {
		if err := s.graph.DeleteEdge(ctx, tenant, e.From, e.To); err != nil {
			return deleted, err
		}
		deleted++
	}
	for _, e := range in {
		// May already be deleted by the outbound pass on the same pair;
		// DeleteEdge is idempotent — ignore "no such edge" semantics.
		if err := s.graph.DeleteEdge(ctx, tenant, e.From, e.To); err != nil && !errors.Is(err, graph.ErrNotFound) {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// nodesForMessage returns all nodes derived from a given message ID. We
// look this up by scanning the tenant's nodes; bbolt backend exposes a
// helper for it. Backends may implement messageNodeScanner for an
// efficient query path.
func (s *Service) nodesForMessage(ctx context.Context, tenant, msgID string) ([]types.Node, error) {
	if scanner, ok := s.graph.(messageNodeScanner); ok {
		return scanner.NodesForMessage(ctx, tenant, msgID)
	}
	return nil, errors.New("service: graph backend does not support NodesForMessage")
}

// messagesBefore returns all message IDs created strictly before t.
func (s *Service) messagesBefore(ctx context.Context, tenant string, t time.Time) ([]string, error) {
	if scanner, ok := s.graph.(messageScanner); ok {
		return scanner.MessageIDsBefore(ctx, tenant, t)
	}
	return nil, errors.New("service: graph backend does not support MessageIDsBefore")
}

// messageNodeScanner is an optional backend capability for finding all
// nodes derived from a message.
type messageNodeScanner interface {
	NodesForMessage(ctx context.Context, tenant, msgID string) ([]types.Node, error)
}

// messageScanner is an optional backend capability for time-bounded
// message enumeration.
type messageScanner interface {
	MessageIDsBefore(ctx context.Context, tenant string, before time.Time) ([]string, error)
}
