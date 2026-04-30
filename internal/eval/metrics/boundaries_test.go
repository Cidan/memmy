package metrics_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cidan/memmy/internal/eval/harness"
	"github.com/Cidan/memmy/internal/eval/metrics"
	"github.com/Cidan/memmy/internal/eval/queries"
)

func TestCompute_ZeroHits(t *testing.T) {
	row := metrics.Compute(harness.QueryResult{
		Query: queries.LabeledQuery{ID: "q1", GoldTurnUUIDs: []string{"u1"}},
		Hits:  nil,
	}, func(string) string { return "" })
	if row.RecallAt1 != 0 || row.RecallAt3 != 0 || row.RecallAt5 != 0 || row.RecallAt8 != 0 {
		t.Errorf("recall: %+v want all 0", row)
	}
	if row.MRR != 0 {
		t.Errorf("MRR=%v, want 0", row.MRR)
	}
	if row.NDCG != 0 {
		t.Errorf("NDCG=%v, want 0", row.NDCG)
	}
	if row.ReinforcementSum != 0 {
		t.Errorf("ReinforcementSum=%v, want 0", row.ReinforcementSum)
	}
}

func TestCompute_GoldAtRank8(t *testing.T) {
	hits := make([]harness.Hit, 8)
	for i := range hits {
		hits[i] = harness.Hit{Rank: i + 1, NodeID: "n" + itoa(i)}
	}
	resolver := func(id string) string {
		if id == "n7" {
			return "u1"
		}
		return ""
	}
	row := metrics.Compute(harness.QueryResult{
		Query: queries.LabeledQuery{ID: "q1", GoldTurnUUIDs: []string{"u1"}},
		Hits:  hits,
	}, resolver)
	if row.RecallAt8 != 1 {
		t.Errorf("RecallAt8=%v, want 1", row.RecallAt8)
	}
	if row.RecallAt5 != 0 || row.RecallAt3 != 0 || row.RecallAt1 != 0 {
		t.Errorf("expected zero recall below rank 8: %+v", row)
	}
	want := 1.0 / 8.0
	if math.Abs(row.MRR-want) > 1e-9 {
		t.Errorf("MRR=%v, want %v", row.MRR, want)
	}
}

func TestCompute_MultipleGoldHitsRanks1And3(t *testing.T) {
	hits := []harness.Hit{
		{Rank: 1, NodeID: "n1"},
		{Rank: 2, NodeID: "n2"},
		{Rank: 3, NodeID: "n3"},
	}
	resolver := func(id string) string {
		if id == "n1" || id == "n3" {
			return "u-gold"
		}
		return ""
	}
	row := metrics.Compute(harness.QueryResult{
		Query: queries.LabeledQuery{ID: "q1", GoldTurnUUIDs: []string{"u-gold"}},
		Hits:  hits,
	}, resolver)
	if row.RecallAt1 != 1 {
		t.Errorf("RecallAt1=%v, want 1", row.RecallAt1)
	}
	if row.MRR != 1 {
		t.Errorf("MRR=%v, want 1", row.MRR)
	}
	if row.NDCG <= 0 || row.NDCG > 1 {
		t.Errorf("NDCG=%v out of (0, 1]", row.NDCG)
	}
}

func TestWriteRun_EmptyRowsStillProducesValidSummary(t *testing.T) {
	dir := t.TempDir()
	summary := metrics.Aggregate("run-empty", "alpha", nil)
	if err := metrics.WriteRun(dir, nil, summary); err != nil {
		t.Fatalf("WriteRun: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	var got metrics.Summary
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.QueriesExecuted != 0 {
		t.Errorf("QueriesExecuted=%d, want 0", got.QueriesExecuted)
	}
	if got.RunID != "run-empty" {
		t.Errorf("RunID=%q, want run-empty", got.RunID)
	}
	// queries.jsonl should exist as an empty file (no rows encoded).
	st, err := os.Stat(filepath.Join(dir, "queries.jsonl"))
	if err != nil {
		t.Fatalf("queries.jsonl: %v", err)
	}
	if st.Size() != 0 {
		t.Errorf("queries.jsonl size=%d, want 0 for empty rows", st.Size())
	}
}

func TestAggregate_HandlesEmptyCategoryWithoutPanic(t *testing.T) {
	rows := []metrics.QueryRow{
		{QueryID: "q1", Category: "", RecallAt1: 1, MRR: 1},
		{QueryID: "q2", Category: "paraphrase", RecallAt1: 0, MRR: 0},
	}
	s := metrics.Aggregate("run-1", "alpha", rows)
	if s.QueriesExecuted != 2 {
		t.Errorf("QueriesExecuted=%d, want 2", s.QueriesExecuted)
	}
	if _, ok := s.PerCategory[""]; !ok {
		t.Errorf("PerCategory missing empty-string bucket: %v", s.PerCategory)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
