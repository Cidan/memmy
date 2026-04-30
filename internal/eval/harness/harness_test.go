package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cidan/memmy"
	"github.com/Cidan/memmy/internal/embed/fake"
	"github.com/Cidan/memmy/internal/eval/corpus"
	"github.com/Cidan/memmy/internal/eval/harness"
	"github.com/Cidan/memmy/internal/eval/metrics"
	"github.com/Cidan/memmy/internal/eval/queries"
)

// Synthetic JSONL with three user turns, each on a distinct topic.
// Spans 30 minutes of simulated time so decay/reinforcement math has
// non-zero deltas to chew on between turns.
const synthJSONL = `{"type":"user","uuid":"t1","sessionId":"s1","timestamp":"2026-04-27T12:00:00.000Z","message":{"role":"user","content":"Apples are sweet fruits. They grow on trees. Many cultivars exist worldwide."}}
{"type":"user","uuid":"t2","sessionId":"s1","timestamp":"2026-04-27T12:15:00.000Z","message":{"role":"user","content":"Mountains are tall rocky landforms. Some have permanent snowcaps. Climbers attempt them each summer."}}
{"type":"user","uuid":"t3","sessionId":"s1","timestamp":"2026-04-27T12:30:00.000Z","message":{"role":"user","content":"Whales are marine mammals. Blue whales are the largest animals to ever live. They migrate vast distances."}}
`

func TestEndToEnd_IngestReplayRunMetrics(t *testing.T) {
	root := t.TempDir()
	sessionsPath := filepath.Join(root, "sessions.jsonl")
	if err := os.WriteFile(sessionsPath, []byte(synthJSONL), 0o600); err != nil {
		t.Fatalf("write JSONL: %v", err)
	}

	ctx := context.Background()
	const dim = 32
	embedder := fake.New(dim)
	modelID := harness.FakeEmbedderModelID(dim)

	// 1) Ingest.
	ingestRes, err := harness.Ingest(ctx, "alpha", harness.IngestOptions{
		SessionsPath:    sessionsPath,
		CorpusStorePath: filepath.Join(root, "corpus.sqlite"),
		EmbedCachePath:  filepath.Join(root, "embedcache.sqlite"),
		Embedder:        embedder,
		EmbedderModelID: modelID,
		EmbedderKind:    "fake",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if ingestRes.TurnsAdded != 3 {
		t.Fatalf("TurnsAdded=%d, want 3", ingestRes.TurnsAdded)
	}
	if ingestRes.ChunksEmbedded == 0 {
		t.Fatalf("no chunks embedded")
	}

	// Re-run ingest; should be a complete no-op (file dedup).
	ingestRes2, err := harness.Ingest(ctx, "alpha", harness.IngestOptions{
		SessionsPath:    sessionsPath,
		CorpusStorePath: filepath.Join(root, "corpus.sqlite"),
		EmbedCachePath:  filepath.Join(root, "embedcache.sqlite"),
		Embedder:        embedder,
		EmbedderModelID: modelID,
		EmbedderKind:    "fake",
	})
	if err != nil {
		t.Fatalf("Ingest 2: %v", err)
	}
	if ingestRes2.FilesSkippedDup != 1 {
		t.Errorf("FilesSkippedDup=%d, want 1 on second ingest", ingestRes2.FilesSkippedDup)
	}
	if ingestRes2.TurnsAdded != 0 {
		t.Errorf("re-ingest added %d turns, want 0", ingestRes2.TurnsAdded)
	}

	// 2) Replay into a fresh memmy db.
	replayRes, err := harness.Replay(ctx, harness.ReplayOptions{
		CorpusStorePath: filepath.Join(root, "corpus.sqlite"),
		EmbedCachePath:  filepath.Join(root, "embedcache.sqlite"),
		MemmyDBPath:     filepath.Join(root, "memmy.db"),
		Embedder:        embedder,
		EmbedderModelID: modelID,
		HNSWRandSeed:    42,
		DatasetName:     "alpha",
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	t.Cleanup(func() { _ = replayRes.Close() })
	if replayRes.TurnsReplayed != 3 {
		t.Fatalf("TurnsReplayed=%d, want 3", replayRes.TurnsReplayed)
	}
	if replayRes.NodesWritten == 0 {
		t.Fatal("no nodes written")
	}

	// 3) Generate queries from the corpus.
	cstore, err := corpus.OpenStore(filepath.Join(root, "corpus.sqlite"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = cstore.Close() })
	var turns []corpus.StoredTurn
	if err := cstore.IterateTurns(ctx, func(st corpus.StoredTurn) error {
		turns = append(turns, st)
		return nil
	}); err != nil {
		t.Fatalf("IterateTurns: %v", err)
	}

	gen := queries.NewFakeGenerator()
	qs, err := gen.Generate(ctx, turns, queries.GenerateRequest{
		Categories: []queries.Category{queries.CategoryParaphrase, queries.CategoryDistractor},
		TargetN:    2,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(qs) == 0 {
		t.Fatal("no queries generated")
	}

	// 4) RunQueries.
	results, err := harness.RunQueries(ctx, qs, harness.RunQueriesOptions{
		Service:     replayRes.Service,
		Tenant:      replayRes.Tenant,
		InspectPath: filepath.Join(root, "memmy.db"),
		K:           5,
		Hops:        1,
		FakeClock:   replayRes.FakeClock,
		AdvanceClock: 2 * time.Minute, // step past the 60s default refractory window between queries
	})
	if err != nil {
		t.Fatalf("RunQueries: %v", err)
	}
	if len(results) != len(qs) {
		t.Fatalf("got %d results, want %d", len(results), len(qs))
	}

	// 5) Resolve hits to source-turn UUIDs via substring match against
	// the original turn texts. memmy chunks each turn into windows so
	// every hit text is a substring of exactly one source turn.
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

	// 6) Metrics.
	rows := make([]metrics.QueryRow, 0, len(results))
	for _, qr := range results {
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
	summary := metrics.Aggregate("test-run-1", "alpha", rows)
	outDir := filepath.Join(root, "runs", "test-run-1")
	if err := metrics.WriteRun(outDir, rows, summary); err != nil {
		t.Fatalf("WriteRun: %v", err)
	}
	if summary.QueriesExecuted != len(qs) {
		t.Errorf("QueriesExecuted=%d, want %d", summary.QueriesExecuted, len(qs))
	}
	if summary.OverallRecallAt5 < 0 || summary.OverallRecallAt5 > 1 {
		t.Errorf("OverallRecallAt5 out of range: %v", summary.OverallRecallAt5)
	}
	if _, err := os.Stat(filepath.Join(outDir, "summary.json")); err != nil {
		t.Errorf("summary.json missing: %v", err)
	}
	// The implicit Recall reinforcement path must fire at least once
	// across the battery — otherwise the dynamics measurement surface
	// is silently broken even when IR metrics look fine.
	saw := false
	for _, qr := range results {
		for _, post := range qr.PostState {
			for _, pre := range qr.PreState {
				if pre.NodeID == post.NodeID && post.Weight > pre.Weight {
					saw = true
				}
			}
		}
	}
	if !saw {
		t.Errorf("no node weight increased pre→post across %d queries; reinforcement path is not firing", len(results))
	}
}

func TestReplay_AdvancesClockToTurnTimestamps(t *testing.T) {
	root := t.TempDir()
	sessionsPath := filepath.Join(root, "s.jsonl")
	if err := os.WriteFile(sessionsPath, []byte(synthJSONL), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx := context.Background()
	const dim = 32
	embedder := fake.New(dim)
	modelID := harness.FakeEmbedderModelID(dim)
	if _, err := harness.Ingest(ctx, "alpha", harness.IngestOptions{
		SessionsPath:    sessionsPath,
		CorpusStorePath: filepath.Join(root, "corpus.sqlite"),
		EmbedCachePath:  filepath.Join(root, "embedcache.sqlite"),
		Embedder:        embedder,
		EmbedderModelID: modelID,
		EmbedderKind:    "fake",
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	res, err := harness.Replay(ctx, harness.ReplayOptions{
		CorpusStorePath: filepath.Join(root, "corpus.sqlite"),
		EmbedCachePath:  filepath.Join(root, "embedcache.sqlite"),
		MemmyDBPath:     filepath.Join(root, "memmy.db"),
		Embedder:        embedder,
		EmbedderModelID: modelID,
		HNSWRandSeed:    42,
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	want := time.Date(2026, 4, 27, 12, 30, 0, 0, time.UTC)
	if !res.FakeClock.Now().Equal(want) {
		t.Errorf("clock at end of replay = %s, want %s (last turn timestamp)", res.FakeClock.Now(), want)
	}

	// Smoke: a Recall using the live memmy facade returns hits.
	rec, err := res.Service.Recall(ctx, memmy.RecallRequest{
		Tenant: res.Tenant,
		Query:  "Apples sweet fruits trees cultivars",
		K:      3,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(rec.Results) == 0 {
		t.Errorf("Recall returned no hits")
	}
}

