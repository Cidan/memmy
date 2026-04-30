package sweep_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cidan/memmy"
	"github.com/Cidan/memmy/internal/embed/fake"
	"github.com/Cidan/memmy/internal/eval/corpus"
	"github.com/Cidan/memmy/internal/eval/harness"
	"github.com/Cidan/memmy/internal/eval/inspect"
	"github.com/Cidan/memmy/internal/eval/metrics"
	"github.com/Cidan/memmy/internal/eval/queries"
	"github.com/Cidan/memmy/internal/eval/sweep"
	"github.com/Cidan/memmy/internal/storage/neo4j/neo4jtest"
)

// Synthetic JSONL with three turns spread over an hour so the
// reinforcement deltas have non-zero room.
const sweepSynthJSONL = `{"type":"user","uuid":"t1","sessionId":"s1","timestamp":"2026-04-27T12:00:00.000Z","message":{"role":"user","content":"Apples are sweet fruits. They grow on trees."}}
{"type":"user","uuid":"t2","sessionId":"s1","timestamp":"2026-04-27T12:30:00.000Z","message":{"role":"user","content":"Mountains are tall rocky landforms. Some have permanent snowcaps."}}
{"type":"user","uuid":"t3","sessionId":"s1","timestamp":"2026-04-27T13:00:00.000Z","message":{"role":"user","content":"Whales are marine mammals. Blue whales are the largest animals to ever live."}}
`

// Drives a 2-entry sweep through the harness + sweep package APIs (no
// binary), confirms each entry produces its own runs/<id>/summary.json
// pair, and confirms the override actually flowed through to the live
// service by comparing per-entry OverallReinforcementMean.
func TestSweep_TwoEntriesEndToEnd(t *testing.T) {
	root := t.TempDir()
	sessionsPath := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(sessionsPath, []byte(sweepSynthJSONL), 0o600); err != nil {
		t.Fatalf("write JSONL: %v", err)
	}

	ctx := context.Background()
	const dim = 32
	embedder := fake.New(dim)
	modelID := harness.FakeEmbedderModelID(dim)

	corpusPath := filepath.Join(root, "corpus.sqlite")
	cachePath := filepath.Join(root, "embedcache.sqlite")
	queriesPath := filepath.Join(root, "queries.sqlite")

	if _, err := harness.Ingest(ctx, "alpha", harness.IngestOptions{
		SessionsPath:    sessionsPath,
		CorpusStorePath: corpusPath,
		EmbedCachePath:  cachePath,
		Embedder:        embedder,
		EmbedderModelID: modelID,
		EmbedderKind:    "fake",
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	cs, err := corpus.OpenStore(corpusPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	corpusHash, err := cs.SnapshotHash(ctx)
	if err != nil {
		t.Fatalf("SnapshotHash: %v", err)
	}
	var turns []corpus.StoredTurn
	if err := cs.IterateTurns(ctx, func(st corpus.StoredTurn) error {
		turns = append(turns, st)
		return nil
	}); err != nil {
		t.Fatalf("IterateTurns: %v", err)
	}

	qstore, err := queries.OpenStore(queriesPath)
	if err != nil {
		t.Fatalf("queries.OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = qstore.Close() })
	gen := queries.NewFakeGenerator()
	qs, err := gen.Generate(ctx, turns, queries.GenerateRequest{
		Categories: []queries.Category{queries.CategoryParaphrase},
		TargetN:    2,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, q := range qs {
		if err := qstore.Put(ctx, q, gen.Version(), corpusHash); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	allQs, err := qstore.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(allQs) == 0 {
		t.Fatal("no queries to drive sweep")
	}

	// Two sweep entries with materially different overrides.
	matrix := []sweep.Entry{
		{Name: "low-delta", Overrides: map[string]any{"NodeDelta": 0.1}},
		{Name: "high-delta", Overrides: map[string]any{"NodeDelta": 5.0}},
	}

	type runReport struct {
		name      string
		summary   metrics.Summary
		summaryFp string
	}
	results := make([]runReport, 0, len(matrix))

	turnByText := map[string]string{}
	for _, tn := range turns {
		turnByText[tn.Text] = tn.UUID
	}
	resolveByHitText := func(hitText string) string {
		for original, uuid := range turnByText {
			if strings.Contains(original, hitText) {
				return uuid
			}
		}
		return ""
	}

	baseCfg := memmy.DefaultServiceConfig()
	for _, entry := range matrix {
		cfg, err := sweep.ApplyServiceOverrides(baseCfg, entry.Overrides)
		if err != nil {
			t.Fatalf("ApplyServiceOverrides %s: %v", entry.Name, err)
		}
		cfgCopy := cfg
		runDir := filepath.Join(root, "runs", entry.Name)
		if err := os.MkdirAll(runDir, 0o700); err != nil {
			t.Fatalf("mkdir run: %v", err)
		}
		_, ntConn, prefix := neo4jtest.Open(t, 32)
		conn := inspect.Connection{URI: ntConn.URI, User: ntConn.User, Password: ntConn.Password, Database: ntConn.Database}
		tenant := map[string]string{"agent": prefix, "entry": entry.Name}
		replay, err := harness.Replay(ctx, harness.ReplayOptions{
			CorpusStorePath: corpusPath,
			EmbedCachePath:  cachePath,
			Embedder:        embedder,
			EmbedderModelID: modelID,
			ServiceConfig:   &cfgCopy,
			DatasetName:     "alpha",
			Neo4j:           conn,
			TenantTuple:     tenant,
		})
		if err != nil {
			t.Fatalf("Replay %s: %v", entry.Name, err)
		}
		runResults, err := harness.RunQueries(ctx, allQs, harness.RunQueriesOptions{
			Service:      replay.Service,
			Tenant:       replay.Tenant,
			InspectConn:  conn,
			K:            3,
			Hops:         1,
			FakeClock:    replay.FakeClock,
			AdvanceClock: 2 * time.Minute,
		})
		if err != nil {
			t.Fatalf("RunQueries %s: %v", entry.Name, err)
		}
		rows := make([]metrics.QueryRow, 0, len(runResults))
		for _, qr := range runResults {
			row := metrics.Compute(qr, func(nodeID string) string {
				for _, h := range qr.Hits {
					if h.NodeID == nodeID {
						return resolveByHitText(h.Text)
					}
				}
				return ""
			})
			rows = append(rows, row)
		}
		summary := metrics.Aggregate(entry.Name, "alpha", rows)
		if err := metrics.WriteRun(runDir, rows, summary); err != nil {
			t.Fatalf("WriteRun %s: %v", entry.Name, err)
		}
		_ = replay.Close()
		results = append(results, runReport{
			name:      entry.Name,
			summary:   summary,
			summaryFp: filepath.Join(runDir, "summary.json"),
		})
	}

	for _, r := range results {
		st, err := os.Stat(r.summaryFp)
		if err != nil {
			t.Fatalf("missing summary for %s: %v", r.name, err)
		}
		if st.Size() == 0 {
			t.Errorf("summary %s is empty", r.summaryFp)
		}
		raw, err := os.ReadFile(r.summaryFp)
		if err != nil {
			t.Fatalf("read summary %s: %v", r.name, err)
		}
		var s metrics.Summary
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("decode summary %s: %v", r.name, err)
		}
		if s.QueriesExecuted == 0 {
			t.Errorf("%s: QueriesExecuted=0", r.name)
		}
	}

	// The whole point of a sweep: different overrides must produce
	// different dynamics. NodeDelta=5 should produce strictly larger
	// reinforcement than NodeDelta=0.1.
	low, high := results[0].summary.OverallReinforcementMean, results[1].summary.OverallReinforcementMean
	if !(high > low) {
		t.Errorf("override didn't flow through: low-delta reinforce_mean=%v >= high-delta reinforce_mean=%v", low, high)
	}
}
