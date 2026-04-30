// Package metrics computes the IR + dynamics scores the eval harness
// reports per run. Two slices: standard ranking quality (recall@k,
// MRR, nDCG) and memmy-specific dynamics (per-query weight delta on
// the top-K hits — proxy for "did Recall reinforce the right nodes").
package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/Cidan/memmy/internal/eval/harness"
)

// QueryRow is one row in queries.jsonl — flat for easy plotting.
type QueryRow struct {
	QueryID            string   `json:"query_id"`
	Category           string   `json:"category"`
	Text               string   `json:"text"`
	GoldTurnUUIDs      []string `json:"gold_turn_uuids"`
	HitNodeIDs         []string `json:"hit_node_ids"`
	HitGoldFlags       []bool   `json:"hit_gold_flags"`
	HitScores          []float64 `json:"hit_scores"`
	RecallAt1          float64  `json:"recall_at_1"`
	RecallAt3          float64  `json:"recall_at_3"`
	RecallAt5          float64  `json:"recall_at_5"`
	RecallAt8          float64  `json:"recall_at_8"`
	MRR                float64  `json:"mrr"`
	NDCG               float64  `json:"ndcg"`
	ReinforcementSum   float64  `json:"reinforcement_sum"`
	ReinforcementMax   float64  `json:"reinforcement_max"`
	StartedAtUnixMS    int64    `json:"started_at_unix_ms"`
	FinishedAtUnixMS   int64    `json:"finished_at_unix_ms"`
	Error              string   `json:"error,omitempty"`
}

// Summary is the per-run aggregate written to summary.json.
type Summary struct {
	RunID                  string             `json:"run_id"`
	DatasetName            string             `json:"dataset_name"`
	QueriesExecuted        int                `json:"queries_executed"`
	OverallRecallAt1       float64            `json:"overall_recall_at_1"`
	OverallRecallAt3       float64            `json:"overall_recall_at_3"`
	OverallRecallAt5       float64            `json:"overall_recall_at_5"`
	OverallRecallAt8       float64            `json:"overall_recall_at_8"`
	OverallMRR             float64            `json:"overall_mrr"`
	OverallNDCG            float64            `json:"overall_ndcg"`
	OverallReinforcementMean float64          `json:"overall_reinforcement_mean"`
	PerCategory            map[string]Category `json:"per_category"`
	GeneratedAt            time.Time          `json:"generated_at"`
}

// Category bundles per-category aggregates. (Distinct from
// queries.Category which is a string label; this is the row.)
type Category struct {
	N                  int     `json:"n"`
	RecallAt1          float64 `json:"recall_at_1"`
	RecallAt3          float64 `json:"recall_at_3"`
	RecallAt5          float64 `json:"recall_at_5"`
	RecallAt8          float64 `json:"recall_at_8"`
	MRR                float64 `json:"mrr"`
	NDCG               float64 `json:"ndcg"`
	ReinforcementMean  float64 `json:"reinforcement_mean"`
}

// Compute scores one query's hits against its gold labels and the
// pre/post node-state pair captured by the harness. Returns the
// flat row for queries.jsonl.
func Compute(qr harness.QueryResult, turnUUIDForNode func(string) string) QueryRow {
	row := QueryRow{
		QueryID:          qr.Query.ID,
		Category:         string(qr.Query.Category),
		Text:             qr.Query.Text,
		GoldTurnUUIDs:    qr.Query.GoldTurnUUIDs,
		StartedAtUnixMS:  qr.StartedAt.UnixMilli(),
		FinishedAtUnixMS: qr.FinishedAt.UnixMilli(),
		Error:            qr.Error,
	}
	row.HitNodeIDs = make([]string, len(qr.Hits))
	row.HitGoldFlags = make([]bool, len(qr.Hits))
	row.HitScores = make([]float64, len(qr.Hits))

	gold := stringSet(qr.Query.GoldTurnUUIDs)
	for i, h := range qr.Hits {
		row.HitNodeIDs[i] = h.NodeID
		row.HitScores[i] = h.Score
		if turnUUIDForNode != nil {
			turn := turnUUIDForNode(h.NodeID)
			row.HitGoldFlags[i] = turn != "" && gold[turn]
		}
	}
	row.RecallAt1 = recallAt(row.HitGoldFlags, 1, len(gold) > 0)
	row.RecallAt3 = recallAt(row.HitGoldFlags, 3, len(gold) > 0)
	row.RecallAt5 = recallAt(row.HitGoldFlags, 5, len(gold) > 0)
	row.RecallAt8 = recallAt(row.HitGoldFlags, 8, len(gold) > 0)
	row.MRR = mrr(row.HitGoldFlags)
	row.NDCG = ndcg(row.HitGoldFlags)

	// Reinforcement deltas: pair pre-state by NodeID, take post - pre.
	preByID := map[string]float64{}
	for _, st := range qr.PreState {
		preByID[st.NodeID] = st.Weight
	}
	for _, st := range qr.PostState {
		pre, ok := preByID[st.NodeID]
		if !ok {
			continue
		}
		d := st.Weight - pre
		row.ReinforcementSum += d
		if d > row.ReinforcementMax {
			row.ReinforcementMax = d
		}
	}
	return row
}

// Aggregate summarises a slice of QueryRows into a Summary.
func Aggregate(runID, datasetName string, rows []QueryRow) Summary {
	s := Summary{
		RunID:           runID,
		DatasetName:     datasetName,
		QueriesExecuted: len(rows),
		PerCategory:     map[string]Category{},
		GeneratedAt:     time.Now().UTC(),
	}
	if len(rows) == 0 {
		return s
	}
	byCat := map[string][]QueryRow{}
	for _, r := range rows {
		byCat[r.Category] = append(byCat[r.Category], r)
		s.OverallRecallAt1 += r.RecallAt1
		s.OverallRecallAt3 += r.RecallAt3
		s.OverallRecallAt5 += r.RecallAt5
		s.OverallRecallAt8 += r.RecallAt8
		s.OverallMRR += r.MRR
		s.OverallNDCG += r.NDCG
		s.OverallReinforcementMean += r.ReinforcementSum
	}
	n := float64(len(rows))
	s.OverallRecallAt1 /= n
	s.OverallRecallAt3 /= n
	s.OverallRecallAt5 /= n
	s.OverallRecallAt8 /= n
	s.OverallMRR /= n
	s.OverallNDCG /= n
	s.OverallReinforcementMean /= n
	for cat, list := range byCat {
		c := Category{N: len(list)}
		for _, r := range list {
			c.RecallAt1 += r.RecallAt1
			c.RecallAt3 += r.RecallAt3
			c.RecallAt5 += r.RecallAt5
			c.RecallAt8 += r.RecallAt8
			c.MRR += r.MRR
			c.NDCG += r.NDCG
			c.ReinforcementMean += r.ReinforcementSum
		}
		nn := float64(len(list))
		c.RecallAt1 /= nn
		c.RecallAt3 /= nn
		c.RecallAt5 /= nn
		c.RecallAt8 /= nn
		c.MRR /= nn
		c.NDCG /= nn
		c.ReinforcementMean /= nn
		s.PerCategory[cat] = c
	}
	return s
}

// WriteRun writes queries.jsonl + summary.json to outDir.
func WriteRun(outDir string, rows []QueryRow, summary Summary) error {
	if outDir == "" {
		return errors.New("metrics: outDir required")
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("metrics: mkdir: %w", err)
	}
	// queries.jsonl
	jp := filepath.Join(outDir, "queries.jsonl")
	jf, err := os.Create(jp)
	if err != nil {
		return fmt.Errorf("metrics: create %q: %w", jp, err)
	}
	enc := json.NewEncoder(jf)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			_ = jf.Close()
			return fmt.Errorf("metrics: encode row: %w", err)
		}
	}
	if err := jf.Close(); err != nil {
		return fmt.Errorf("metrics: close jsonl: %w", err)
	}
	// summary.json
	sp := filepath.Join(outDir, "summary.json")
	raw, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("metrics: marshal summary: %w", err)
	}
	if err := os.WriteFile(sp, raw, 0o600); err != nil {
		return fmt.Errorf("metrics: write summary: %w", err)
	}
	return nil
}

func recallAt(flags []bool, k int, hasGold bool) float64 {
	if !hasGold {
		return 0
	}
	if len(flags) < k {
		k = len(flags)
	}
	for i := 0; i < k; i++ {
		if flags[i] {
			return 1.0
		}
	}
	return 0
}

func mrr(flags []bool) float64 {
	for i, f := range flags {
		if f {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

func ndcg(flags []bool) float64 {
	dcg := 0.0
	relCount := 0
	for i, f := range flags {
		if f {
			dcg += 1.0 / math.Log2(float64(i+2))
			relCount++
		}
	}
	if relCount == 0 {
		return 0
	}
	idcg := 0.0
	for i := 0; i < relCount; i++ {
		idcg += 1.0 / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

func stringSet(in []string) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, s := range in {
		out[s] = true
	}
	return out
}

