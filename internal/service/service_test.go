package service_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cidan/memmy/internal/clock"
	"github.com/Cidan/memmy/internal/embed/fake"
	"github.com/Cidan/memmy/internal/service"
	bboltstore "github.com/Cidan/memmy/internal/storage/bbolt"
	"github.com/Cidan/memmy/internal/types"
)

// fixture builds a real bbolt-backed Service with a Fake clock and Fake
// embedder. All state lives in t.TempDir().
type fixture struct {
	svc   *service.Service
	store *bboltstore.Storage
	cl    *clock.Fake
	emb   *fake.Embedder
	cfg   service.Config
}

func newFixture(t *testing.T, dim int, opts ...func(*service.Config)) *fixture {
	t.Helper()
	store, err := bboltstore.Open(bboltstore.Options{
		Path:              filepath.Join(t.TempDir(), "memmy.db"),
		Dim:               dim,
		RandSeed:          42,
		FlatScanThreshold: 100000, // force flat scan in service tests for stability
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cl := clock.NewFake(time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC))
	emb := fake.New(dim)

	cfg := service.DefaultConfig()
	for _, fn := range opts {
		fn(&cfg)
	}
	svc, err := service.New(store.Graph(), store.VectorIndex(), emb, cl, cfg)
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	return &fixture{svc: svc, store: store, cl: cl, emb: emb, cfg: cfg}
}

func TestService_Write_CreatesNodesVectorsEdges(t *testing.T) {
	f := newFixture(t, 32)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}

	res, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant:  tenant,
		Message: "S1. S2. S3. S4. S5. S6. S7. S8. S9. S10.",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.MessageID == "" {
		t.Fatal("empty message id")
	}
	if got := len(res.NodeIDs); got != 5 {
		t.Fatalf("len(node_ids)=%d, want 5", got)
	}
	tenantID := types.TenantID(tenant)

	// Each node should be retrievable.
	for i, id := range res.NodeIDs {
		n, err := f.store.Graph().GetNode(ctx, tenantID, id)
		if err != nil {
			t.Fatalf("GetNode #%d: %v", i, err)
		}
		if n.SourceMsgID != res.MessageID {
			t.Fatalf("node %d source=%s, want %s", i, n.SourceMsgID, res.MessageID)
		}
		if n.Weight != 1.0 {
			t.Fatalf("node %d weight=%v, want 1.0", i, n.Weight)
		}
	}

	// HNSW size should be 5.
	if sz, _ := f.store.VectorIndex().Size(ctx, tenantID); sz != 5 {
		t.Fatalf("vector size=%d, want 5", sz)
	}

	// Sequential structural edges between adjacent chunks.
	for i := 0; i+1 < len(res.NodeIDs); i++ {
		_, ok, err := f.store.Graph().GetEdge(ctx, tenantID, res.NodeIDs[i], res.NodeIDs[i+1])
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("missing forward structural edge %d→%d", i, i+1)
		}
		_, ok, err = f.store.Graph().GetEdge(ctx, tenantID, res.NodeIDs[i+1], res.NodeIDs[i])
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("missing reverse structural edge %d→%d", i+1, i)
		}
	}
}

func TestService_Recall_ReturnsExactMatch(t *testing.T) {
	f := newFixture(t, 32)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}

	if _, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant:  tenant,
		Message: "the cat sat on the mat. it was a very fluffy cat.",
	}); err != nil {
		t.Fatal(err)
	}

	// The fake embedder is hash-based and deterministic, so an exact
	// query string match should produce a near-perfect similarity score.
	res, err := f.svc.Recall(ctx, types.RecallRequest{
		Tenant: tenant,
		Query:  "the cat sat on the mat. it was a very fluffy cat.",
		K:      3,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Results) == 0 {
		t.Fatal("no results")
	}
	top := res.Results[0]
	if top.Text == "" || top.SourceMsgID == "" || top.SourceText == "" {
		t.Fatalf("missing provenance: %+v", top)
	}
	if top.ScoreBreakdown.Sim <= 0 {
		t.Fatalf("non-positive sim breakdown: %+v", top.ScoreBreakdown)
	}
}

func TestService_Recall_HotMemoryRanksAboveStale(t *testing.T) {
	f := newFixture(t, 32, func(c *service.Config) {
		c.WeightBeta = 1.5 // amplify weight effect for the test
		c.SimAlpha = 0.5   // dampen sim effect
	})
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}

	// Two candidate memories with the SAME text → identical embeddings,
	// so raw similarity to a query is identical. We then bump the access
	// count of one (making it hotter) and confirm it ranks above the
	// stale one for a query that overlaps.
	resA, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant:  tenant,
		Message: "the meeting is about widgets. the widgets are blue.",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.cl.Advance(time.Hour)
	resB, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant:  tenant,
		Message: "the meeting is about widgets. the widgets are blue.",
	})
	if err != nil {
		t.Fatal(err)
	}
	tenantID := types.TenantID(tenant)

	// Reinforce node B several times directly via UpdateNode to make it hot.
	for _, id := range resB.NodeIDs {
		for i := 0; i < 10; i++ {
			err := f.store.Graph().UpdateNode(ctx, tenantID, id, func(n *types.Node) error {
				n.Weight += 5
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	res, err := f.svc.Recall(ctx, types.RecallRequest{
		Tenant: tenant,
		Query:  "widgets meeting",
		K:     10,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Results) == 0 {
		t.Fatal("no results")
	}

	// The first occurrence of any B-node in the ranked results must come
	// before the first occurrence of any A-node.
	bSet := map[string]struct{}{}
	for _, id := range resB.NodeIDs {
		bSet[id] = struct{}{}
	}
	aSet := map[string]struct{}{}
	for _, id := range resA.NodeIDs {
		aSet[id] = struct{}{}
	}
	firstA, firstB := -1, -1
	for i, r := range res.Results {
		if _, ok := bSet[r.NodeID]; ok && firstB < 0 {
			firstB = i
		}
		if _, ok := aSet[r.NodeID]; ok && firstA < 0 {
			firstA = i
		}
	}
	if firstB < 0 || firstA < 0 {
		t.Fatalf("results did not include both A and B groups: %+v", res.Results)
	}
	if firstB > firstA {
		t.Fatalf("hot memory B (firstAt=%d) ranked below stale A (firstAt=%d)", firstB, firstA)
	}
}

func TestService_Recall_FormsCoRetrievalEdges(t *testing.T) {
	f := newFixture(t, 32)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}

	resA, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "alpha beta gamma. delta epsilon.",
	})
	if err != nil {
		t.Fatal(err)
	}
	resB, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "alpha beta gamma. delta epsilon.",
	})
	if err != nil {
		t.Fatal(err)
	}
	tenantID := types.TenantID(tenant)

	// Recall with query that should pull seeds from both A and B (they
	// share text → similar vectors).
	if _, err := f.svc.Recall(ctx, types.RecallRequest{
		Tenant: tenant,
		Query:  "alpha beta gamma",
		K:      4,
	}); err != nil {
		t.Fatal(err)
	}

	// At least one CoRetrieval edge should now exist between any node in
	// resA and any node in resB.
	bSet := map[string]struct{}{}
	for _, id := range resB.NodeIDs {
		bSet[id] = struct{}{}
	}
	found := false
	for _, aID := range resA.NodeIDs {
		neigh, err := f.store.Graph().Neighbors(ctx, tenantID, aID)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range neigh {
			if _, ok := bSet[e.To]; ok && e.Kind == types.EdgeCoRetrieval {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("no CoRetrieval edge formed between A-message and B-message nodes after Recall")
	}
}

func TestService_Recall_ExpandsViaMemoryEdges(t *testing.T) {
	f := newFixture(t, 32)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}
	tenantID := types.TenantID(tenant)

	resA, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "weather report sunny today.",
	})
	if err != nil {
		t.Fatal(err)
	}
	resB, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "completely unrelated topic about programming languages.",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Manually wire a strong memory edge from resA[0] → resB[0].
	now := f.cl.Now()
	if err := f.store.Graph().PutEdge(ctx, types.MemoryEdge{
		From: resA.NodeIDs[0], To: resB.NodeIDs[0], TenantID: tenantID,
		Kind: types.EdgeCoRetrieval, Weight: 5.0,
		LastTouched: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Query something that strongly matches A but NOT B. B should still
	// surface via graph expansion.
	res, err := f.svc.Recall(ctx, types.RecallRequest{
		Tenant: tenant,
		Query:  "weather report sunny today.",
		K:     5,
		Hops:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	bSet := map[string]struct{}{}
	for _, id := range resB.NodeIDs {
		bSet[id] = struct{}{}
	}
	expandedHit := false
	for _, r := range res.Results {
		if _, ok := bSet[r.NodeID]; ok && len(r.Path) >= 2 {
			expandedHit = true
			break
		}
	}
	if !expandedHit {
		t.Fatalf("expected B-node to surface via graph expansion; results=%+v", res.Results)
	}
}

func TestService_Recall_PrunesEdgeBelowFloor(t *testing.T) {
	f := newFixture(t, 32, func(c *service.Config) {
		c.EdgeFloor = 0.5
		c.EdgeCoRetrievalLambda = 1e-3 // strong decay so a long Δt drops it fast
	})
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}
	tenantID := types.TenantID(tenant)

	res, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant:  tenant,
		Message: "first sentence. second sentence. third sentence. fourth sentence. fifth sentence.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NodeIDs) < 2 {
		t.Fatalf("expected ≥ 2 chunks, got %d", len(res.NodeIDs))
	}
	a, b := res.NodeIDs[0], res.NodeIDs[1]

	// Manually create a fragile co-retrieval edge.
	now := f.cl.Now()
	err = f.store.Graph().PutEdge(ctx, types.MemoryEdge{
		From: a, To: b, TenantID: tenantID,
		Kind: types.EdgeCoRetrieval, Weight: 0.6,
		LastTouched: now, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Advance clock past decay horizon: 0.6 * exp(-1e-3 * 3600) ≈ 0.0164 → < 0.5
	f.cl.Advance(time.Hour)

	if _, err := f.svc.Recall(ctx, types.RecallRequest{
		Tenant: tenant, Query: "first message", K: 4, Hops: 2,
	}); err != nil {
		t.Fatal(err)
	}
	_, ok, err := f.store.Graph().GetEdge(ctx, tenantID, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("edge below floor should have been pruned by Recall expansion")
	}
}

func TestService_Forget_ByMessageID(t *testing.T) {
	f := newFixture(t, 32)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}
	tenantID := types.TenantID(tenant)

	resA, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "first message body.",
	})
	if err != nil {
		t.Fatal(err)
	}
	resB, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "second message body.",
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := f.svc.Forget(ctx, types.ForgetRequest{
		Tenant:    tenant,
		MessageID: resA.MessageID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.DeletedNodes != len(resA.NodeIDs) {
		t.Fatalf("DeletedNodes=%d, want %d", out.DeletedNodes, len(resA.NodeIDs))
	}
	if out.DeletedVectors != len(resA.NodeIDs) {
		t.Fatalf("DeletedVectors=%d, want %d", out.DeletedVectors, len(resA.NodeIDs))
	}
	// resA nodes are gone.
	for _, id := range resA.NodeIDs {
		_, err := f.store.Graph().GetNode(ctx, tenantID, id)
		if err == nil {
			t.Fatalf("node %s still present after Forget", id)
		}
	}
	// resB nodes survive.
	for _, id := range resB.NodeIDs {
		if _, err := f.store.Graph().GetNode(ctx, tenantID, id); err != nil {
			t.Fatalf("node %s missing after Forget(other): %v", id, err)
		}
	}
}

func TestService_Forget_BeforeTimestamp(t *testing.T) {
	f := newFixture(t, 32)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}
	tenantID := types.TenantID(tenant)

	old, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "this is an old message.",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.cl.Advance(time.Hour)
	newer, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "this is a newer message.",
	})
	if err != nil {
		t.Fatal(err)
	}
	cutoff := f.cl.Now().Add(-30 * time.Minute) // between old and newer

	if _, err := f.svc.Forget(ctx, types.ForgetRequest{
		Tenant: tenant,
		Before: cutoff,
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range old.NodeIDs {
		_, err := f.store.Graph().GetNode(ctx, tenantID, id)
		if err == nil {
			t.Fatalf("old node %s still present", id)
		}
	}
	for _, id := range newer.NodeIDs {
		if _, err := f.store.Graph().GetNode(ctx, tenantID, id); err != nil {
			t.Fatalf("newer node %s missing: %v", id, err)
		}
	}
}

func TestService_Stats(t *testing.T) {
	f := newFixture(t, 32)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}

	if _, err := f.svc.Write(ctx, types.WriteRequest{Tenant: tenant, Message: "S1. S2. S3."}); err != nil {
		t.Fatal(err)
	}
	st, err := f.svc.Stats(ctx, types.StatsRequest{Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	if st.NodeCount == 0 {
		t.Fatal("NodeCount=0")
	}
	if st.HNSWSize == 0 {
		t.Fatal("HNSWSize=0")
	}
	if st.AvgNodeWeight <= 0 {
		t.Fatalf("AvgNodeWeight=%v", st.AvgNodeWeight)
	}
}
