package metrics_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cidan/memmy/internal/eval/harness"
	"github.com/Cidan/memmy/internal/eval/inspect"
	"github.com/Cidan/memmy/internal/eval/metrics"
	"github.com/Cidan/memmy/internal/eval/queries"
)

func TestCompute_RecallAndMRR(t *testing.T) {
	qr := harness.QueryResult{
		Query: queries.LabeledQuery{
			ID:            "q1",
			Category:      queries.CategoryParaphrase,
			Text:          "hello",
			GoldTurnUUIDs: []string{"t1"},
		},
		Hits: []harness.Hit{
			{Rank: 1, NodeID: "n1", Score: 0.9},
			{Rank: 2, NodeID: "n2", Score: 0.8},
			{Rank: 3, NodeID: "n3", Score: 0.7},
		},
	}
	resolver := func(nodeID string) string {
		switch nodeID {
		case "n2":
			return "t1"
		case "n1":
			return "t-other"
		}
		return ""
	}
	row := metrics.Compute(qr, resolver)
	if row.RecallAt1 != 0 {
		t.Errorf("recall@1=%v, want 0 (gold at rank 2)", row.RecallAt1)
	}
	if row.RecallAt3 != 1 {
		t.Errorf("recall@3=%v, want 1", row.RecallAt3)
	}
	if math.Abs(row.MRR-0.5) > 1e-9 {
		t.Errorf("MRR=%v, want 0.5", row.MRR)
	}
}

func TestCompute_RecallAt1WhenGoldFirst(t *testing.T) {
	qr := harness.QueryResult{
		Query: queries.LabeledQuery{
			ID:            "q1",
			Category:      queries.CategoryParaphrase,
			GoldTurnUUIDs: []string{"t1"},
		},
		Hits: []harness.Hit{{Rank: 1, NodeID: "n1"}, {Rank: 2, NodeID: "n2"}},
	}
	resolver := func(id string) string {
		if id == "n1" {
			return "t1"
		}
		return ""
	}
	row := metrics.Compute(qr, resolver)
	if row.RecallAt1 != 1 {
		t.Errorf("recall@1=%v, want 1", row.RecallAt1)
	}
	if row.MRR != 1 {
		t.Errorf("MRR=%v, want 1", row.MRR)
	}
	if row.NDCG != 1 {
		t.Errorf("NDCG=%v, want 1", row.NDCG)
	}
}

func TestCompute_NoGoldNoCredit(t *testing.T) {
	qr := harness.QueryResult{
		Query: queries.LabeledQuery{ID: "q1", Category: queries.CategoryDistractor},
		Hits:  []harness.Hit{{Rank: 1, NodeID: "n1"}},
	}
	resolver := func(string) string { return "" }
	row := metrics.Compute(qr, resolver)
	if row.RecallAt1 != 0 || row.RecallAt3 != 0 || row.MRR != 0 || row.NDCG != 0 {
		t.Errorf("expected all-zero metrics, got %+v", row)
	}
}

func TestCompute_ReinforcementDeltaFromPrePost(t *testing.T) {
	pre := []inspect.NodeState{
		{NodeID: "n1", Weight: 1.0},
		{NodeID: "n2", Weight: 2.0},
	}
	post := []inspect.NodeState{
		{NodeID: "n1", Weight: 1.5},
		{NodeID: "n2", Weight: 2.0},
	}
	qr := harness.QueryResult{
		Query:     queries.LabeledQuery{ID: "q1"},
		Hits:      []harness.Hit{{Rank: 1, NodeID: "n1"}, {Rank: 2, NodeID: "n2"}},
		PreState:  pre,
		PostState: post,
	}
	row := metrics.Compute(qr, func(string) string { return "" })
	if math.Abs(row.ReinforcementSum-0.5) > 1e-9 {
		t.Errorf("ReinforcementSum=%v, want 0.5", row.ReinforcementSum)
	}
	if math.Abs(row.ReinforcementMax-0.5) > 1e-9 {
		t.Errorf("ReinforcementMax=%v, want 0.5", row.ReinforcementMax)
	}
}

func TestAggregate_AveragesAndPerCategory(t *testing.T) {
	rows := []metrics.QueryRow{
		{QueryID: "q1", Category: "paraphrase", RecallAt1: 1, RecallAt3: 1, MRR: 1, NDCG: 1, ReinforcementSum: 0.5},
		{QueryID: "q2", Category: "paraphrase", RecallAt1: 0, RecallAt3: 1, MRR: 0.5, NDCG: 0.6, ReinforcementSum: 0.1},
		{QueryID: "q3", Category: "distractor", RecallAt1: 0, RecallAt3: 0, MRR: 0, NDCG: 0, ReinforcementSum: 0.0},
	}
	s := metrics.Aggregate("run-1", "alpha", rows)
	if s.QueriesExecuted != 3 {
		t.Errorf("QueriesExecuted=%d, want 3", s.QueriesExecuted)
	}
	if math.Abs(s.OverallRecallAt1-(1.0/3.0)) > 1e-9 {
		t.Errorf("OverallRecallAt1=%v", s.OverallRecallAt1)
	}
	para, ok := s.PerCategory["paraphrase"]
	if !ok || para.N != 2 {
		t.Errorf("paraphrase missing or wrong N: %+v", para)
	}
	if para.RecallAt1 != 0.5 {
		t.Errorf("paraphrase RecallAt1=%v, want 0.5", para.RecallAt1)
	}
}

func TestWriteRun_ProducesFiles(t *testing.T) {
	dir := t.TempDir()
	rows := []metrics.QueryRow{
		{QueryID: "q1", Category: "paraphrase", Text: "hello", RecallAt1: 1, MRR: 1, NDCG: 1, FinishedAtUnixMS: time.Now().UnixMilli()},
	}
	s := metrics.Aggregate("run-1", "alpha", rows)
	if err := metrics.WriteRun(dir, rows, s); err != nil {
		t.Fatalf("WriteRun: %v", err)
	}
	for _, name := range []string{"queries.jsonl", "summary.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	var got metrics.Summary
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if got.OverallRecallAt1 != 1 {
		t.Errorf("OverallRecallAt1=%v, want 1", got.OverallRecallAt1)
	}
}
