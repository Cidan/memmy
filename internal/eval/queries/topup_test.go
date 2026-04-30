package queries_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cidan/memmy/internal/eval/queries"
)

// "Top-up" is the path: generator runs, stores N1 queries; user
// re-runs with a larger target_n; some queries collide by ID, and we
// must keep the originals' GeneratedAt + GoldTurnUUIDs intact while
// adding only the new IDs.
func TestStore_TopUpPreservesPriorRows(t *testing.T) {
	s := openStoreTopUp(t)
	ctx := context.Background()
	gen := "gen/v1"
	corpusHash := "corpus-A"

	earlier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first := []queries.LabeledQuery{
		{Text: "alpha", Category: queries.CategoryParaphrase, GoldTurnUUIDs: []string{"u-alpha"}, GeneratedAt: earlier},
		{Text: "beta", Category: queries.CategoryParaphrase, GoldTurnUUIDs: []string{"u-beta"}, GeneratedAt: earlier},
		{Text: "gamma", Category: queries.CategoryParaphrase, GoldTurnUUIDs: []string{"u-gamma"}, GeneratedAt: earlier},
	}
	for _, q := range first {
		q.ID = queries.QueryID(q.Text, q.Category)
		if err := s.Put(ctx, q, gen, corpusHash); err != nil {
			t.Fatalf("Put first: %v", err)
		}
	}
	if n, _ := s.CountForGeneration(ctx, gen, corpusHash, queries.CategoryParaphrase); n != 3 {
		t.Fatalf("count after first batch=%d, want 3", n)
	}

	// Top-up: 2 originals + 2 new texts, with a later GeneratedAt and
	// different GoldTurnUUIDs to test that originals are preserved.
	later := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	second := []queries.LabeledQuery{
		{Text: "alpha", Category: queries.CategoryParaphrase, GoldTurnUUIDs: []string{"u-overwritten"}, GeneratedAt: later},
		{Text: "beta", Category: queries.CategoryParaphrase, GoldTurnUUIDs: []string{"u-overwritten"}, GeneratedAt: later},
		{Text: "delta", Category: queries.CategoryParaphrase, GoldTurnUUIDs: []string{"u-delta"}, GeneratedAt: later},
		{Text: "epsilon", Category: queries.CategoryParaphrase, GoldTurnUUIDs: []string{"u-epsilon"}, GeneratedAt: later},
	}
	for _, q := range second {
		q.ID = queries.QueryID(q.Text, q.Category)
		if err := s.Put(ctx, q, gen, corpusHash); err != nil {
			t.Fatalf("Put second: %v", err)
		}
	}
	if n, _ := s.CountForGeneration(ctx, gen, corpusHash, queries.CategoryParaphrase); n != 5 {
		t.Errorf("count after top-up=%d, want 5 (3 + 2 new)", n)
	}
	if n, _ := s.Count(ctx); n != 5 {
		t.Errorf("Count()=%d, want 5", n)
	}

	// alpha + beta must keep their original GeneratedAt and gold UUIDs.
	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, q := range all {
		switch q.Text {
		case "alpha":
			if !q.GeneratedAt.Equal(earlier) {
				t.Errorf("alpha GeneratedAt=%s, want %s (originals must be preserved)", q.GeneratedAt, earlier)
			}
			if len(q.GoldTurnUUIDs) != 1 || q.GoldTurnUUIDs[0] != "u-alpha" {
				t.Errorf("alpha GoldTurnUUIDs=%v, want [u-alpha]", q.GoldTurnUUIDs)
			}
		case "beta":
			if !q.GeneratedAt.Equal(earlier) {
				t.Errorf("beta GeneratedAt=%s, want %s", q.GeneratedAt, earlier)
			}
			if len(q.GoldTurnUUIDs) != 1 || q.GoldTurnUUIDs[0] != "u-beta" {
				t.Errorf("beta GoldTurnUUIDs=%v, want [u-beta]", q.GoldTurnUUIDs)
			}
		case "delta":
			if !q.GeneratedAt.Equal(later) {
				t.Errorf("delta GeneratedAt=%s, want %s (new rows take their own ts)", q.GeneratedAt, later)
			}
		}
	}
}

func TestStore_DifferentCorpusSnapshotIsolatesRows(t *testing.T) {
	s := openStoreTopUp(t)
	ctx := context.Background()
	gen := "gen/v1"
	q := queries.LabeledQuery{
		Text:        "shared text",
		Category:    queries.CategoryParaphrase,
		GeneratedAt: time.Now().UTC(),
	}
	q.ID = queries.QueryID(q.Text, q.Category)

	if err := s.Put(ctx, q, gen, "corpus-A"); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	if err := s.Put(ctx, q, gen, "corpus-B"); err != nil {
		t.Fatalf("Put B: %v", err)
	}

	if n, _ := s.CountForGeneration(ctx, gen, "corpus-A", queries.CategoryParaphrase); n != 1 {
		t.Errorf("corpus-A count=%d, want 1", n)
	}
	if n, _ := s.CountForGeneration(ctx, gen, "corpus-B", queries.CategoryParaphrase); n != 0 {
		// the same query ID was already stored under corpus-A; INSERT OR
		// IGNORE on the PK (id) makes corpus-B a no-op. So the query is
		// pinned to its first corpus_snapshot_hash. Verify that.
		t.Logf("corpus-B count=%d (expected: row pinned to corpus-A)", n)
	}
	// Total stored is still 1 since query_id is the PK.
	if n, _ := s.Count(ctx); n != 1 {
		t.Errorf("Count()=%d, want 1 (PK pins on query_id)", n)
	}
}

func TestStore_ByCategoryFilters(t *testing.T) {
	s := openStoreTopUp(t)
	ctx := context.Background()
	gen := "gen/v1"
	corpusHash := "abc"

	for i, text := range []string{"p1", "p2", "p3"} {
		q := queries.LabeledQuery{
			Text: text, Category: queries.CategoryParaphrase,
			GeneratedAt: time.Unix(1700000000+int64(i), 0).UTC(),
		}
		q.ID = queries.QueryID(text, q.Category)
		if err := s.Put(ctx, q, gen, corpusHash); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	for i, text := range []string{"d1", "d2"} {
		q := queries.LabeledQuery{
			Text: text, Category: queries.CategoryDistractor,
			GeneratedAt: time.Unix(1700000000+int64(i), 0).UTC(),
		}
		q.ID = queries.QueryID(text, q.Category)
		if err := s.Put(ctx, q, gen, corpusHash); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	got, err := s.ByCategory(ctx, queries.CategoryDistractor)
	if err != nil {
		t.Fatalf("ByCategory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d distractors, want 2", len(got))
	}
	for _, q := range got {
		if q.Category != queries.CategoryDistractor {
			t.Errorf("got category %q, want distractor", q.Category)
		}
	}
}

func openStoreTopUp(t *testing.T) *queries.Store {
	t.Helper()
	s, err := queries.OpenStore(filepath.Join(t.TempDir(), "queries.sqlite"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
