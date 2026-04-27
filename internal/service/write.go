package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/Cidan/memmy/internal/chunker"
	"github.com/Cidan/memmy/internal/types"
)

// Write ingests a message: chunk it, embed each chunk, persist nodes +
// vectors + HNSW records, and create structural memory edges.
//
// Order matters:
//  1. Embed BEFORE any storage write (DESIGN.md §5).
//  2. Persist message + nodes + vectors + HNSW.
//  3. Create structural edges (sequential within message + recent within tenant).
func (s *Service) Write(ctx context.Context, req types.WriteRequest) (types.WriteResult, error) {
	if strings.TrimSpace(req.Message) == "" {
		return types.WriteResult{}, errors.New("service: message text required")
	}
	tenant, err := s.resolveTenant(ctx, req.Tenant)
	if err != nil {
		return types.WriteResult{}, err
	}

	chunks := chunker.Chunkify(req.Message, s.cfg.ChunkWindowSize, s.cfg.ChunkStride)
	if len(chunks) == 0 {
		return types.WriteResult{}, errors.New("service: message produced no chunks")
	}

	// Step 1 — embed all chunks in one batched call (no storage tx open yet).
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}
	vecs, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		return types.WriteResult{}, err
	}
	if len(vecs) != len(chunks) {
		return types.WriteResult{}, errors.New("service: embedder returned mismatched vector count")
	}

	now := s.clock.Now()
	msgID := s.newID()

	// Step 2 — persist message.
	if err := s.graph.PutMessage(ctx, types.Message{
		ID:        msgID,
		TenantID:  tenant,
		Text:      req.Message,
		Metadata:  req.Metadata,
		CreatedAt: now,
	}); err != nil {
		return types.WriteResult{}, err
	}

	// Step 3 — for each chunk: PutNode, VectorIndex.Insert.
	nodeIDs := make([]string, len(chunks))
	for i, c := range chunks {
		id := s.newID()
		nodeIDs[i] = id

		node := types.Node{
			ID:           id,
			TenantID:     tenant,
			SourceMsgID:  msgID,
			SentenceSpan: c.SentenceSpan,
			Text:         c.Text,
			EmbeddingDim: len(vecs[i]),
			CreatedAt:    now,
			LastTouched:  now,
			Weight:       1.0,
		}
		if err := s.graph.PutNode(ctx, node); err != nil {
			return types.WriteResult{}, err
		}
		if err := s.vidx.Insert(ctx, tenant, id, vecs[i]); err != nil {
			return types.WriteResult{}, err
		}
	}

	// Step 4 — sequential edges within this message (chunks[i] ↔ chunks[i+1]).
	for i := 0; i+1 < len(nodeIDs); i++ {
		if err := s.putStructuralEdgePair(ctx, tenant, nodeIDs[i], nodeIDs[i+1], s.cfg.EdgeStructuralWeight, now); err != nil {
			return types.WriteResult{}, err
		}
	}

	// Step 5 — recent-within-tenant: link the FIRST chunk of this message
	// to the most recent chunks of OTHER messages in the same tenant
	// within Δt. This avoids quadratic blowup but still seeds cross-
	// message associations.
	if s.cfg.StructuralRecentN > 0 && s.cfg.StructuralRecentDelta > 0 {
		recent, err := s.recentNodeIDsInTenant(ctx, tenant, now.Add(-s.cfg.StructuralRecentDelta), msgID, s.cfg.StructuralRecentN)
		if err != nil {
			return types.WriteResult{}, err
		}
		for _, recentID := range recent {
			if err := s.putStructuralEdgePair(ctx, tenant, nodeIDs[0], recentID, s.cfg.EdgeStructuralTemporalWeight, now); err != nil {
				return types.WriteResult{}, err
			}
		}
	}

	return types.WriteResult{MessageID: msgID, NodeIDs: nodeIDs}, nil
}

// putStructuralEdgePair writes a symmetric pair of structural edges
// between a and b at the given weight.
func (s *Service) putStructuralEdgePair(ctx context.Context, tenant, a, b string, weight float64, now time.Time) error {
	if a == b {
		return nil
	}
	for _, pair := range [2][2]string{{a, b}, {b, a}} {
		e := types.MemoryEdge{
			From:        pair[0],
			To:          pair[1],
			TenantID:    tenant,
			Kind:        types.EdgeStructural,
			Weight:      weight,
			LastTouched: now,
			CreatedAt:   now,
		}
		if err := s.graph.PutEdge(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// recentNodeIDsInTenant returns up to maxN node IDs created at-or-after
// `since`, in DESCENDING chronological order, excluding nodes whose
// SourceMsgID == excludeMsgID.
//
// We rely on the bbolt-internal layout (ULIDs are lex-sortable) by going
// through a backend-aware helper. For non-bbolt backends, this would
// translate to an ORDER BY CreatedAt DESC LIMIT N query.
func (s *Service) recentNodeIDsInTenant(ctx context.Context, tenant string, since time.Time, excludeMsgID string, maxN int) ([]string, error) {
	scanner, ok := s.graph.(recentNodeScanner)
	if !ok {
		return nil, nil
	}
	return scanner.RecentNodeIDs(ctx, tenant, since, excludeMsgID, maxN)
}

// recentNodeScanner is an optional capability that storage backends MAY
// implement to expose efficient "most recent N nodes since T" queries.
// The bbolt backend implements it via a reverse cursor scan on the
// nodes/ bucket (ULIDs are lex-sortable).
type recentNodeScanner interface {
	RecentNodeIDs(ctx context.Context, tenant string, since time.Time, excludeMsgID string, maxN int) ([]string, error)
}

// silence unused import in case bbolt isn't referenced in this file's
// final form; the import is required by some platforms' build tooling.
var _ = bbolt.DefaultOptions
