package harness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Cidan/memmy"
	"github.com/Cidan/memmy/internal/eval/inspect"
	"github.com/Cidan/memmy/internal/eval/queries"
)

// Hit is one returned chunk from a Recall call, joined with the
// originating turn UUID we stamped into the Write metadata.
type Hit struct {
	Rank        int
	NodeID      string
	Text        string
	Score       float64
	Sim         float64
	NodeWeight  float64
	GraphMult   float64
	Depth       int
	SourceMsgID string
	TurnUUID    string // resolved from metadata when available
}

// QueryResult records everything needed to score one query.
type QueryResult struct {
	Query        queries.LabeledQuery
	StartedAt    time.Time
	FinishedAt   time.Time
	Hits         []Hit
	PreState     []inspect.NodeState // top-K node state captured before Recall
	PostState    []inspect.NodeState // same nodes after Recall
	Error        string
}

// RunQueriesOptions configures one query battery.
type RunQueriesOptions struct {
	Service       memmy.Service
	Tenant        map[string]string
	InspectPath   string // path to the SAME memmy db the service writes
	K             int
	Hops          int
	OversampleN   int
	AdvanceClock  time.Duration // FakeClock advance between queries (0 = none)
	FakeClock     *memmy.FakeClock
}

// RunQueries executes each labeled query against the live service,
// captures rankings + score breakdowns, and snapshots the node state
// of the top-K hits before and after Recall via the inspect reader.
//
// We snapshot the full corpus state ONCE at the start (O(corpus)) into
// a rolling baseline, then per query we read post-state for only the
// top-K hits (O(K)) and compute the delta against the rolling baseline.
// The baseline gets refreshed for hit nodes after each query so that
// each query's "pre" reflects the state immediately before that
// query's Recall, not the state at battery start.
func RunQueries(ctx context.Context, qs []queries.LabeledQuery, opts RunQueriesOptions) ([]QueryResult, error) {
	if opts.Service == nil {
		return nil, errors.New("harness: Service required")
	}
	if opts.InspectPath == "" {
		return nil, errors.New("harness: InspectPath required")
	}
	if len(opts.Tenant) == 0 {
		return nil, errors.New("harness: Tenant required")
	}
	if opts.K <= 0 {
		opts.K = 8
	}

	r, err := inspect.Open(opts.InspectPath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	tenants, err := r.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := ""
	for _, t := range tenants {
		if mapEqual(t.Tuple, opts.Tenant) {
			tenantID = t.ID
			break
		}
	}
	if tenantID == "" {
		return nil, fmt.Errorf("harness: tenant %v not present in inspect db", opts.Tenant)
	}

	allIDs, err := r.ListNodes(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("harness: list nodes for baseline: %w", err)
	}
	baseline := make(map[string]inspect.NodeState, len(allIDs))
	allStates, err := r.NodeStates(ctx, tenantID, allIDs)
	if err != nil {
		return nil, fmt.Errorf("harness: baseline snapshot: %w", err)
	}
	for _, st := range allStates {
		baseline[st.NodeID] = st
	}

	out := make([]QueryResult, 0, len(qs))
	for _, q := range qs {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if opts.AdvanceClock > 0 && opts.FakeClock != nil {
			opts.FakeClock.Advance(opts.AdvanceClock)
		}
		qr := QueryResult{Query: q, StartedAt: time.Now().UTC()}

		recallRes, rerr := opts.Service.Recall(ctx, memmy.RecallRequest{
			Tenant:      opts.Tenant,
			Query:       q.Text,
			K:           opts.K,
			Hops:        opts.Hops,
			OversampleN: opts.OversampleN,
		})
		if rerr != nil {
			qr.Error = rerr.Error()
			out = append(out, qr)
			continue
		}

		hits := make([]Hit, 0, len(recallRes.Results))
		topNodeIDs := make([]string, 0, len(recallRes.Results))
		for i, h := range recallRes.Results {
			hits = append(hits, Hit{
				Rank:        i + 1,
				NodeID:      h.NodeID,
				Text:        h.Text,
				Score:       h.Score,
				Sim:         h.ScoreBreakdown.Sim,
				NodeWeight:  h.ScoreBreakdown.NodeWeight,
				GraphMult:   h.ScoreBreakdown.GraphMult,
				Depth:       h.ScoreBreakdown.Depth,
				SourceMsgID: h.SourceMsgID,
			})
			topNodeIDs = append(topNodeIDs, h.NodeID)
		}
		preTop := make([]inspect.NodeState, 0, len(topNodeIDs))
		for _, id := range topNodeIDs {
			if st, ok := baseline[id]; ok {
				preTop = append(preTop, st)
			}
		}
		postTop, err := r.NodeStates(ctx, tenantID, topNodeIDs)
		if err != nil {
			qr.Error = err.Error()
			qr.Hits = hits
			out = append(out, qr)
			continue
		}
		// Refresh baseline for the hit nodes so the next query's "pre"
		// reflects today's post-state, not the start-of-battery snapshot.
		for _, st := range postTop {
			baseline[st.NodeID] = st
		}
		qr.Hits = hits
		qr.PreState = preTop
		qr.PostState = postTop
		qr.FinishedAt = time.Now().UTC()
		out = append(out, qr)
	}
	return out, nil
}

func mapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
