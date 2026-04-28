package memmy_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cidan/memmy"
)

const testDim = 32

func openTestService(t *testing.T, opts ...func(*memmy.Options)) (memmy.Service, *memmy.FakeClock) {
	t.Helper()
	cl := memmy.NewFakeClock(time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC))
	o := memmy.Options{
		DBPath:            filepath.Join(t.TempDir(), "memmy.db"),
		Embedder:          memmy.NewFakeEmbedder(testDim),
		Clock:             cl,
		FlatScanThreshold: 100000,
		HNSWRandSeed:      42,
	}
	for _, fn := range opts {
		fn(&o)
	}
	svc, closer, err := memmy.Open(o)
	if err != nil {
		t.Fatalf("memmy.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := closer.Close(); err != nil {
			t.Errorf("closer.Close: %v", err)
		}
	})
	return svc, cl
}

func TestOpen_RequiresDBPath(t *testing.T) {
	_, _, err := memmy.Open(memmy.Options{Embedder: memmy.NewFakeEmbedder(testDim)})
	if err == nil {
		t.Fatal("expected error for missing DBPath")
	}
}

func TestOpen_RequiresEmbedder(t *testing.T) {
	_, _, err := memmy.Open(memmy.Options{DBPath: filepath.Join(t.TempDir(), "memmy.db")})
	if err == nil {
		t.Fatal("expected error for missing Embedder")
	}
}

func TestOpen_RejectsZeroDimEmbedder(t *testing.T) {
	_, _, err := memmy.Open(memmy.Options{
		DBPath:   filepath.Join(t.TempDir(), "memmy.db"),
		Embedder: zeroDimEmbedder{},
	})
	if err == nil {
		t.Fatal("expected error for zero-dim embedder")
	}
}

type zeroDimEmbedder struct{}

func (zeroDimEmbedder) Dim() int { return 0 }
func (zeroDimEmbedder) Embed(_ context.Context, _ memmy.EmbedTask, _ []string) ([][]float32, error) {
	return nil, nil
}

func TestOpen_DefaultsApplied(t *testing.T) {
	svc, _ := openTestService(t)
	if svc == nil {
		t.Fatal("nil Service from Open")
	}
}

func TestService_WriteAndRecall_RoundTrip(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}

	w, err := svc.Write(ctx, memmy.WriteRequest{
		Tenant:  tenant,
		Message: "S1. S2. S3. S4. S5. S6. S7. S8. S9. S10.",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if w.MessageID == "" {
		t.Fatal("empty MessageID")
	}
	if len(w.NodeIDs) == 0 {
		t.Fatal("no nodes produced")
	}

	r, err := svc.Recall(ctx, memmy.RecallRequest{
		Tenant: tenant,
		Query:  "S1. S2. S3.",
		K:      3,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(r.Results) == 0 {
		t.Fatal("no recall hits")
	}
	for i, hit := range r.Results {
		if hit.NodeID == "" {
			t.Errorf("hit %d has empty NodeID", i)
		}
		if hit.Text == "" {
			t.Errorf("hit %d has empty Text", i)
		}
	}
}

func TestService_ReinforceDemoteMark(t *testing.T) {
	svc, cl := openTestService(t)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}

	w, err := svc.Write(ctx, memmy.WriteRequest{
		Tenant:  tenant,
		Message: "Alpha. Beta. Gamma. Delta. Epsilon.",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(w.NodeIDs) == 0 {
		t.Fatal("no nodes produced")
	}
	target := w.NodeIDs[0]

	// Advance past the default 60s refractory window since Write set
	// LastTouched to "now"; otherwise the first explicit bump is gated.
	cl.Advance(2 * time.Minute)

	// Reinforce.
	rr, err := svc.Reinforce(ctx, memmy.ReinforceRequest{Tenant: tenant, NodeID: target})
	if err != nil {
		t.Fatalf("Reinforce: %v", err)
	}
	if rr.NodeID != target {
		t.Fatalf("Reinforce returned NodeID %q, want %q", rr.NodeID, target)
	}
	if rr.SkippedRefractory {
		t.Fatal("first Reinforce should not be refractory-skipped")
	}
	weightAfterReinforce := rr.NewWeight

	// Advance past the refractory window so the next explicit bump applies.
	cl.Advance(2 * time.Minute)

	// Demote drops the weight.
	dr, err := svc.Demote(ctx, memmy.DemoteRequest{Tenant: tenant, NodeID: target})
	if err != nil {
		t.Fatalf("Demote: %v", err)
	}
	if dr.SkippedRefractory {
		t.Fatal("Demote skipped due to refractory period after explicit advance")
	}
	if dr.NewWeight >= weightAfterReinforce {
		t.Fatalf("Demote did not reduce weight: before=%v after=%v", weightAfterReinforce, dr.NewWeight)
	}

	// Mark walks recent nodes inside the [Since, now] window.
	cl.Advance(2 * time.Minute)
	mr, err := svc.Mark(ctx, memmy.MarkRequest{
		Tenant:   tenant,
		Since:    cl.Now().Add(-10 * time.Minute),
		Strength: 1.0,
	})
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if mr.NodesAffected+mr.NodesSkippedRefractory == 0 {
		t.Fatalf("Mark walked 0 nodes; want > 0")
	}
}

func TestService_StatsReflectsWrites(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}

	if _, err := svc.Write(ctx, memmy.WriteRequest{Tenant: tenant, Message: "S1. S2. S3. S4. S5."}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	st, err := svc.Stats(ctx, memmy.StatsRequest{Tenant: tenant})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.NodeCount == 0 {
		t.Fatalf("Stats.NodeCount=0 after Write; want > 0")
	}
	if st.HNSWSize == 0 {
		t.Fatalf("Stats.HNSWSize=0 after Write; want > 0")
	}
}

func TestService_ForgetByMessageID(t *testing.T) {
	svc, _ := openTestService(t)
	ctx := context.Background()
	tenant := map[string]string{"agent": "ada"}

	w, err := svc.Write(ctx, memmy.WriteRequest{Tenant: tenant, Message: "Alpha. Beta. Gamma."})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	fr, err := svc.Forget(ctx, memmy.ForgetRequest{Tenant: tenant, MessageID: w.MessageID})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if fr.DeletedNodes != len(w.NodeIDs) {
		t.Fatalf("Forget.DeletedNodes=%d, want %d", fr.DeletedNodes, len(w.NodeIDs))
	}
}

func TestTenantSchema_AcceptsValidTuple(t *testing.T) {
	schema, err := memmy.NewTenantSchema(memmy.TenantSchemaConfig{
		Description: "single-user agent",
		Keys: map[string]memmy.TenantKeyConfig{
			"user":  {Required: true, Pattern: `^[a-zA-Z0-9_.-]+$`},
			"scope": {Enum: []string{"chat", "code"}},
		},
	})
	if err != nil {
		t.Fatalf("NewTenantSchema: %v", err)
	}
	if schema == nil {
		t.Fatal("schema unexpectedly nil")
	}

	svc, _ := openTestService(t, func(o *memmy.Options) {
		o.TenantSchema = schema
	})
	ctx := context.Background()

	if _, err := svc.Write(ctx, memmy.WriteRequest{
		Tenant:  map[string]string{"user": "alice", "scope": "chat"},
		Message: "Hello world. Goodbye world.",
	}); err != nil {
		t.Fatalf("valid tuple rejected: %v", err)
	}
}

func TestTenantSchema_RejectsInvalidTuple(t *testing.T) {
	schema, err := memmy.NewTenantSchema(memmy.TenantSchemaConfig{
		Keys: map[string]memmy.TenantKeyConfig{
			"user":  {Required: true},
			"scope": {Enum: []string{"chat", "code"}},
		},
	})
	if err != nil {
		t.Fatalf("NewTenantSchema: %v", err)
	}

	svc, _ := openTestService(t, func(o *memmy.Options) {
		o.TenantSchema = schema
	})
	ctx := context.Background()

	cases := []struct {
		name   string
		tenant map[string]string
		code   string
	}{
		{"missing required", map[string]string{"scope": "chat"}, "tenant_missing_required"},
		{"unknown key", map[string]string{"user": "a", "weird": "x"}, "tenant_unknown_key"},
		{"enum mismatch", map[string]string{"user": "a", "scope": "nope"}, "tenant_enum_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Write(ctx, memmy.WriteRequest{Tenant: tc.tenant, Message: "x. y. z."})
			if err == nil {
				t.Fatalf("expected rejection for %v", tc.tenant)
			}
			var te *memmy.ErrTenantInvalid
			if !errors.As(err, &te) {
				t.Fatalf("expected ErrTenantInvalid, got %T: %v", err, err)
			}
			if te.Code != tc.code {
				t.Errorf("Code=%q, want %q", te.Code, tc.code)
			}
		})
	}
}

func TestTenantSchema_NilConfigReturnsNilSchema(t *testing.T) {
	schema, err := memmy.NewTenantSchema(memmy.TenantSchemaConfig{})
	if err != nil {
		t.Fatalf("NewTenantSchema(empty): %v", err)
	}
	if schema != nil {
		t.Fatalf("empty schema config should yield nil schema, got %#v", schema)
	}
}

func TestClose_ReleasesDBLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memmy.db")
	openOnce := func() error {
		_, closer, err := memmy.Open(memmy.Options{
			DBPath:       dbPath,
			Embedder:     memmy.NewFakeEmbedder(testDim),
			OpenTimeout:  500 * time.Millisecond,
			HNSWRandSeed: 7,
		})
		if err != nil {
			return err
		}
		return closer.Close()
	}

	if err := openOnce(); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := openOnce(); err != nil {
		t.Fatalf("second Open after Close: %v", err)
	}
}

// Confirms a custom Embedder (any type implementing memmy.Embedder)
// flows through Open without needing a built-in constructor.
func TestOpen_AcceptsCustomEmbedder(t *testing.T) {
	svc, closer, err := memmy.Open(memmy.Options{
		DBPath:   filepath.Join(t.TempDir(), "memmy.db"),
		Embedder: constantEmbedder{dim: testDim},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	if _, err := svc.Write(context.Background(), memmy.WriteRequest{
		Tenant:  map[string]string{"agent": "ada"},
		Message: "one. two. three.",
	}); err != nil {
		t.Fatalf("Write with custom embedder: %v", err)
	}
}

// Verifies the documented partial-override pattern: start from
// DefaultServiceConfig(), mutate one field, take the address. The
// pointer-typed Options.ServiceConfig prevents the silent partial-zero
// trap that a value-typed field would have.
func TestOpen_PartialServiceConfigOverride(t *testing.T) {
	cfg := memmy.DefaultServiceConfig()
	cfg.NodeDelta = 2.0

	svc, closer, err := memmy.Open(memmy.Options{
		DBPath:        filepath.Join(t.TempDir(), "memmy.db"),
		Embedder:      memmy.NewFakeEmbedder(testDim),
		Clock:         memmy.NewFakeClock(time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)),
		ServiceConfig: &cfg,
		HNSWRandSeed:  11,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	if _, err := svc.Write(context.Background(), memmy.WriteRequest{
		Tenant:  map[string]string{"agent": "ada"},
		Message: "alpha. beta. gamma. delta.",
	}); err != nil {
		t.Fatalf("Write with partial ServiceConfig override: %v", err)
	}
}

// Verifies the documented partial-override pattern for HNSW: start from
// DefaultHNSWConfig(), mutate, take address.
func TestOpen_PartialHNSWConfigOverride(t *testing.T) {
	hnsw := memmy.DefaultHNSWConfig()
	hnsw.EfSearch = 64

	svc, closer, err := memmy.Open(memmy.Options{
		DBPath:       filepath.Join(t.TempDir(), "memmy.db"),
		Embedder:     memmy.NewFakeEmbedder(testDim),
		HNSW:         &hnsw,
		HNSWRandSeed: 13,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	if _, err := svc.Write(context.Background(), memmy.WriteRequest{
		Tenant:  map[string]string{"agent": "ada"},
		Message: "alpha. beta. gamma.",
	}); err != nil {
		t.Fatalf("Write with partial HNSW override: %v", err)
	}
}

// bbolt's Close() is idempotent (returns nil on the second call); the
// facade's io.Closer surface inherits that, and library callers that
// rely on the standard `defer closer.Close()` pattern shouldn't get
// surprised by paired manual closes.
func TestClose_IsIdempotent(t *testing.T) {
	_, closer, err := memmy.Open(memmy.Options{
		DBPath:       filepath.Join(t.TempDir(), "memmy.db"),
		Embedder:     memmy.NewFakeEmbedder(testDim),
		HNSWRandSeed: 17,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

type constantEmbedder struct{ dim int }

func (c constantEmbedder) Dim() int { return c.dim }
func (c constantEmbedder) Embed(_ context.Context, _ memmy.EmbedTask, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, c.dim)
		for j := range v {
			// Deterministic but distinct enough across positions to
			// avoid degenerate identical-vector behavior in HNSW.
			v[j] = float32(i+1) / float32(c.dim+j+1)
		}
		out[i] = v
	}
	return out, nil
}
