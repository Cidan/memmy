package types_test

import (
	"testing"

	"github.com/Cidan/memmy/internal/types"
)

func TestCanonicalTenant_StableAcrossKeyOrder(t *testing.T) {
	a := map[string]string{"agent": "ada", "user": "u_42"}
	b := map[string]string{"user": "u_42", "agent": "ada"}
	if types.CanonicalTenant(a) != types.CanonicalTenant(b) {
		t.Fatal("canonical form differs by insertion order")
	}
}

func TestCanonicalTenant_TrimsValues(t *testing.T) {
	a := map[string]string{"agent": "ada", "user": "u_42"}
	b := map[string]string{"agent": "  ada  ", "user": "\tu_42\n"}
	if types.CanonicalTenant(a) != types.CanonicalTenant(b) {
		t.Fatalf("trim failed: %q vs %q", types.CanonicalTenant(a), types.CanonicalTenant(b))
	}
}

func TestCanonicalTenant_TrimsKeys(t *testing.T) {
	a := map[string]string{"agent": "ada"}
	b := map[string]string{" agent ": "ada"}
	if types.CanonicalTenant(a) != types.CanonicalTenant(b) {
		t.Fatalf("key trim failed: %q vs %q", types.CanonicalTenant(a), types.CanonicalTenant(b))
	}
}

func TestCanonicalTenant_Empty(t *testing.T) {
	if got := types.CanonicalTenant(nil); got != "" {
		t.Fatalf("nil tuple = %q, want empty", got)
	}
	if got := types.CanonicalTenant(map[string]string{}); got != "" {
		t.Fatalf("empty tuple = %q, want empty", got)
	}
}

func TestTenantID_Deterministic(t *testing.T) {
	a := map[string]string{"agent": "ada", "user": "u_42"}
	id1 := types.TenantID(a)
	id2 := types.TenantID(a)
	if id1 != id2 {
		t.Fatalf("non-deterministic: %q vs %q", id1, id2)
	}
	if len(id1) != 32 {
		t.Fatalf("len = %d, want 32 hex chars", len(id1))
	}
}

func TestTenantID_DistinguishesTuples(t *testing.T) {
	a := types.TenantID(map[string]string{"agent": "ada"})
	b := types.TenantID(map[string]string{"agent": "bda"})
	if a == b {
		t.Fatal("different tuples produced same id")
	}
}

func TestEdgeKindString(t *testing.T) {
	cases := []struct {
		k    types.EdgeKind
		want string
	}{
		{types.EdgeStructural, "structural"},
		{types.EdgeCoRetrieval, "coretrieval"},
		{types.EdgeCoTraversal, "cotraversal"},
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.want {
			t.Errorf("EdgeKind(%d).String()=%q, want %q", c.k, got, c.want)
		}
	}
}
