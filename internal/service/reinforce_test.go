package service_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/Cidan/memmy/internal/service"
	"github.com/Cidan/memmy/internal/types"
)

func TestService_Reinforce_BumpsWeight(t *testing.T) {
	f := newFixture(t, 32)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}
	tenantID := types.TenantID(tenant)

	res, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "alpha sentence. beta sentence. gamma sentence.",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := res.NodeIDs[0]

	// Advance past the refractory window so the bump is applied.
	f.cl.Advance(2 * f.cfg.RefractoryPeriod)

	out, err := f.svc.Reinforce(ctx, types.ReinforceRequest{
		Tenant: tenant, NodeID: id,
	})
	if err != nil {
		t.Fatalf("Reinforce: %v", err)
	}
	if out.SkippedRefractory {
		t.Fatal("first Reinforce was unexpectedly refractory-skipped")
	}
	if out.NewWeight <= 1.0 {
		t.Fatalf("expected weight > 1.0 after Reinforce, got %v", out.NewWeight)
	}

	// Cross-check the persisted weight matches.
	n, err := f.store.Graph().GetNode(ctx, tenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(n.Weight-out.NewWeight) > 1e-9 {
		t.Fatalf("persisted weight %v != reported %v", n.Weight, out.NewWeight)
	}
}

func TestService_Reinforce_RefractoryBlocksRepeatedCalls(t *testing.T) {
	f := newFixture(t, 32)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}

	res, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "alpha. beta. gamma.",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := res.NodeIDs[0]

	// Advance past refractory so the first bump lands.
	f.cl.Advance(2 * f.cfg.RefractoryPeriod)
	first, err := f.svc.Reinforce(ctx, types.ReinforceRequest{Tenant: tenant, NodeID: id})
	if err != nil {
		t.Fatal(err)
	}
	if first.SkippedRefractory {
		t.Fatal("first Reinforce should not have been refractory-skipped")
	}

	// Within the refractory window: the second call should be skipped.
	f.cl.Advance(f.cfg.RefractoryPeriod / 2)
	second, err := f.svc.Reinforce(ctx, types.ReinforceRequest{Tenant: tenant, NodeID: id})
	if err != nil {
		t.Fatal(err)
	}
	if !second.SkippedRefractory {
		t.Fatal("second Reinforce within refractory window should have been skipped")
	}
	if math.Abs(second.NewWeight-first.NewWeight) > 1e-6 {
		// Weight may differ slightly due to lazy-decay over the half-window;
		// it must NOT be a fresh bump (which would add ~NodeDelta * dampening).
		// Permit only sub-delta drift.
		if math.Abs(second.NewWeight-first.NewWeight) > 0.5 {
			t.Fatalf("refractory-skipped Reinforce changed weight by more than expected drift: first=%v second=%v",
				first.NewWeight, second.NewWeight)
		}
	}

	// Past the refractory window: the third call should land.
	f.cl.Advance(f.cfg.RefractoryPeriod + time.Second)
	third, err := f.svc.Reinforce(ctx, types.ReinforceRequest{Tenant: tenant, NodeID: id})
	if err != nil {
		t.Fatal(err)
	}
	if third.SkippedRefractory {
		t.Fatal("third Reinforce past refractory window should NOT have been skipped")
	}
	if third.NewWeight <= second.NewWeight {
		t.Fatalf("third Reinforce did not increase weight: second=%v third=%v",
			second.NewWeight, third.NewWeight)
	}
}

func TestService_Reinforce_LogDampeningNearCap(t *testing.T) {
	f := newFixture(t, 32, func(c *service.Config) {
		// Large refractory so manual UpdateNode setup isn't disturbed; the
		// test exercises log-dampening on a single Reinforce call.
		c.RefractoryPeriod = 0
		c.LogDampening = true
	})
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}
	tenantID := types.TenantID(tenant)

	res, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "one. two. three.",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := res.NodeIDs[0]

	// Pump weight close to WeightCap by direct UpdateNode (no decay
	// between this and the Reinforce call thanks to FakeClock).
	target := f.cfg.WeightCap - 1.0 // i.e. 99.0 for default cap=100
	err = f.store.Graph().UpdateNode(ctx, tenantID, id, func(n *types.Node) error {
		n.Weight = target
		n.LastTouched = f.cl.Now()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := f.svc.Reinforce(ctx, types.ReinforceRequest{Tenant: tenant, NodeID: id})
	if err != nil {
		t.Fatal(err)
	}
	if out.SkippedRefractory {
		t.Fatal("unexpected refractory skip with RefractoryPeriod=0")
	}
	added := out.NewWeight - target
	// With LogDampening, effective_delta = NodeDelta * (1 - w/cap)
	// = 1.0 * (1 - 99/100) = 0.01. The bump should be MUCH less than 1.0.
	if added >= 0.5 {
		t.Fatalf("log-dampening near cap expected sub-0.5 bump, got %v (target=%v new=%v)",
			added, target, out.NewWeight)
	}
	if added < 0 {
		t.Fatalf("expected positive bump even with dampening, got %v", added)
	}
}

func TestService_Reinforce_LogDampeningOffMatchesNodeDelta(t *testing.T) {
	f := newFixture(t, 32, func(c *service.Config) {
		c.RefractoryPeriod = 0
		c.LogDampening = false
	})
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}
	tenantID := types.TenantID(tenant)

	res, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "one. two. three.",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := res.NodeIDs[0]

	// Set weight to a low value; LogDampening=false should yield a
	// near-NodeDelta bump regardless of distance from cap.
	err = f.store.Graph().UpdateNode(ctx, tenantID, id, func(n *types.Node) error {
		n.Weight = 5.0
		n.LastTouched = f.cl.Now()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := f.svc.Reinforce(ctx, types.ReinforceRequest{Tenant: tenant, NodeID: id})
	if err != nil {
		t.Fatal(err)
	}
	added := out.NewWeight - 5.0
	if math.Abs(added-f.cfg.NodeDelta) > 1e-6 {
		t.Fatalf("expected bump = NodeDelta (%v) with dampening off, got %v",
			f.cfg.NodeDelta, added)
	}
}

func TestService_Demote_ClampsAtNodeFloor(t *testing.T) {
	f := newFixture(t, 32, func(c *service.Config) {
		c.RefractoryPeriod = 0
		c.NodeFloor = 0.05
	})
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}
	tenantID := types.TenantID(tenant)

	res, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "x. y. z.",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := res.NodeIDs[0]

	// Drop weight below cap to make decay arithmetic predictable.
	err = f.store.Graph().UpdateNode(ctx, tenantID, id, func(n *types.Node) error {
		n.Weight = 0.5 // expected to drop toward 0.05 in one Demote
		n.LastTouched = f.cl.Now()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// First demote: 0.5 - 1.0 = -0.5 → clamps at NodeFloor (0.05).
	out, err := f.svc.Demote(ctx, types.DemoteRequest{Tenant: tenant, NodeID: id})
	if err != nil {
		t.Fatal(err)
	}
	if out.NewWeight < f.cfg.NodeFloor-1e-9 {
		t.Fatalf("Demote went below NodeFloor: %v < %v", out.NewWeight, f.cfg.NodeFloor)
	}
	if math.Abs(out.NewWeight-f.cfg.NodeFloor) > 1e-9 {
		t.Fatalf("Demote did not clamp at NodeFloor: got %v want %v", out.NewWeight, f.cfg.NodeFloor)
	}

	// Repeated demote stays at floor.
	out, err = f.svc.Demote(ctx, types.DemoteRequest{Tenant: tenant, NodeID: id})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(out.NewWeight-f.cfg.NodeFloor) > 1e-9 {
		t.Fatalf("repeated Demote moved off NodeFloor: got %v", out.NewWeight)
	}
}

func TestService_Mark_BoostsRecentWindow(t *testing.T) {
	f := newFixture(t, 32, func(c *service.Config) {
		// Disable refractory so the per-node bump always lands.
		c.RefractoryPeriod = 0
	})
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}
	tenantID := types.TenantID(tenant)

	// Older message — created BEFORE the mark window.
	resOld, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "old. message. content.",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Advance well past where the window will start.
	f.cl.Advance(2 * time.Hour)
	windowStart := f.cl.Now() // window opens here, BEFORE the in-window write

	// Move forward inside the window so the new nodes have age << window.
	f.cl.Advance(30 * time.Minute)
	resNew, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "new. message. content.",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Snapshot pre-mark weights.
	preNew := make(map[string]float64, len(resNew.NodeIDs))
	for _, id := range resNew.NodeIDs {
		n, err := f.store.Graph().GetNode(ctx, tenantID, id)
		if err != nil {
			t.Fatal(err)
		}
		preNew[id] = n.Weight
	}
	preOld := make(map[string]float64, len(resOld.NodeIDs))
	for _, id := range resOld.NodeIDs {
		n, err := f.store.Graph().GetNode(ctx, tenantID, id)
		if err != nil {
			t.Fatal(err)
		}
		preOld[id] = n.Weight
	}

	// Advance further so the new nodes are past their LastTouched
	// refractory floor and the recency factor is meaningful.
	f.cl.Advance(30 * time.Minute) // total window now = 1h, new-node age = 30m → recency ≈ 0.5

	out, err := f.svc.Mark(ctx, types.MarkRequest{
		Tenant: tenant, Since: windowStart, Strength: 1.0,
	})
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if out.NodesAffected == 0 {
		t.Fatalf("Mark affected zero nodes despite in-window content (skipped=%d)", out.NodesSkippedRefractory)
	}

	// In-window nodes should have been bumped.
	for _, id := range resNew.NodeIDs {
		n, err := f.store.Graph().GetNode(ctx, tenantID, id)
		if err != nil {
			t.Fatal(err)
		}
		if n.Weight <= preNew[id] {
			t.Fatalf("in-window node %s not boosted: pre=%v post=%v", id, preNew[id], n.Weight)
		}
	}
	// Out-of-window nodes should NOT have been bumped (their weights only
	// decay or remain the same — never exceed pre weight).
	for _, id := range resOld.NodeIDs {
		n, err := f.store.Graph().GetNode(ctx, tenantID, id)
		if err != nil {
			t.Fatal(err)
		}
		if n.Weight > preOld[id]+1e-6 {
			t.Fatalf("out-of-window node %s was boosted: pre=%v post=%v", id, preOld[id], n.Weight)
		}
	}
}

func TestService_Mark_RefractorySkipsRecentlyTouched(t *testing.T) {
	f := newFixture(t, 32) // default RefractoryPeriod=60s
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}

	windowStart := f.cl.Now()
	// Open a wide window before any writes, so recency factor is well
	// above zero for the in-window nodes regardless of where they land.
	f.cl.Advance(30 * time.Minute)
	res, err := f.svc.Write(ctx, types.WriteRequest{
		Tenant: tenant, Message: "alpha. beta. gamma.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NodeIDs) == 0 {
		t.Fatal("expected nodes")
	}

	// Mark within the refractory window from the Write — every node was
	// just LastTouched on creation, so the bumps must be refractory-skipped.
	f.cl.Advance(5 * time.Second) // < 60s since LastTouched
	out, err := f.svc.Mark(ctx, types.MarkRequest{
		Tenant: tenant, Since: windowStart, Strength: 1.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.NodesAffected != 0 {
		t.Fatalf("expected all in-window nodes to be refractory-skipped, got affected=%d skipped=%d",
			out.NodesAffected, out.NodesSkippedRefractory)
	}
	if out.NodesSkippedRefractory == 0 {
		t.Fatalf("expected refractory skips, got affected=%d skipped=%d",
			out.NodesAffected, out.NodesSkippedRefractory)
	}
}

func TestService_Reinforce_UnknownNodeReturnsError(t *testing.T) {
	f := newFixture(t, 32)
	ctx := context.Background()
	_, err := f.svc.Reinforce(ctx, types.ReinforceRequest{
		Tenant: map[string]string{"agent": "ada"},
		NodeID: "01J0000000000000000000NOPE",
	})
	if err == nil {
		t.Fatal("expected error for unknown node, got nil")
	}
}

func TestService_Demote_UnknownNodeReturnsError(t *testing.T) {
	f := newFixture(t, 32)
	ctx := context.Background()
	_, err := f.svc.Demote(ctx, types.DemoteRequest{
		Tenant: map[string]string{"agent": "ada"},
		NodeID: "01J0000000000000000000NOPE",
	})
	if err == nil {
		t.Fatal("expected error for unknown node, got nil")
	}
}

func TestService_Mark_RejectsBadInputs(t *testing.T) {
	f := newFixture(t, 32)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}

	if _, err := f.svc.Mark(ctx, types.MarkRequest{
		Tenant: tenant, Since: time.Time{}, Strength: 1.0,
	}); err == nil {
		t.Fatal("expected error for zero Since")
	}
	if _, err := f.svc.Mark(ctx, types.MarkRequest{
		Tenant: tenant, Since: f.cl.Now().Add(time.Hour), Strength: 1.0,
	}); err == nil {
		t.Fatal("expected error for future Since")
	}
	if _, err := f.svc.Mark(ctx, types.MarkRequest{
		Tenant: tenant, Since: f.cl.Now().Add(-time.Hour), Strength: 0,
	}); err == nil {
		t.Fatal("expected error for non-positive Strength")
	}
}
