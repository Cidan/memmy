package queries_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cidan/memmy/internal/eval/corpus"
	"github.com/Cidan/memmy/internal/eval/queries"
)

func TestFakeGenerator_ProducesParaphraseAndDistractor(t *testing.T) {
	turns := []corpus.StoredTurn{
		{UUID: "u1", Text: "Alpha is the first letter. Beta follows.", Timestamp: time.Unix(1, 0)},
		{UUID: "u2", Text: "Gamma is the third. Delta follows.", Timestamp: time.Unix(2, 0)},
	}
	g := queries.NewFakeGenerator()
	out, err := g.Generate(context.Background(), turns, queries.GenerateRequest{
		Categories: []queries.Category{queries.CategoryParaphrase, queries.CategoryDistractor},
		TargetN:    2,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("len=%d, want 4 (2 paraphrase + 2 distractor)", len(out))
	}
	var (
		para, dist int
	)
	for _, q := range out {
		switch q.Category {
		case queries.CategoryParaphrase:
			para++
			if len(q.GoldTurnUUIDs) != 1 {
				t.Errorf("paraphrase q has %d gold UUIDs, want 1", len(q.GoldTurnUUIDs))
			}
		case queries.CategoryDistractor:
			dist++
			if len(q.GoldTurnUUIDs) != 0 {
				t.Errorf("distractor q has %d gold UUIDs, want 0", len(q.GoldTurnUUIDs))
			}
		}
	}
	if para != 2 || dist != 2 {
		t.Errorf("category counts: para=%d dist=%d", para, dist)
	}
}

func TestFakeJudge_TokenOverlapScoring(t *testing.T) {
	q := queries.LabeledQuery{Text: "alpha sentence comparison"}
	cands := []queries.Candidate{
		{NodeID: "n1", Text: "alpha sentence inside the corpus"},
		{NodeID: "n2", Text: "completely unrelated content here"},
	}
	verdicts, err := queries.NewFakeJudge().Judge(context.Background(), q, cands)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if len(verdicts) != 2 {
		t.Fatalf("len=%d, want 2", len(verdicts))
	}
	if verdicts[0].Score != 1.0 {
		t.Errorf("verdict[0].Score=%v, want 1.0", verdicts[0].Score)
	}
	if verdicts[1].Score != 0.0 {
		t.Errorf("verdict[1].Score=%v, want 0.0", verdicts[1].Score)
	}
}

func TestStore_PutDedupAndList(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	gen := queries.NewFakeGenerator().Version()
	corpusHash := "abc"
	q := queries.LabeledQuery{
		ID:            queries.QueryID("hello", queries.CategoryParaphrase),
		Category:      queries.CategoryParaphrase,
		Text:          "hello",
		GoldTurnUUIDs: []string{"u1"},
		GeneratedAt:   time.Unix(1700000000, 0).UTC(),
	}
	for i := range 3 {
		if err := s.Put(ctx, q, gen, corpusHash); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	count, err := s.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Errorf("count=%d, want 1 (dedup)", count)
	}
	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 1 || all[0].ID != q.ID {
		t.Errorf("All=%v, want 1 of %s", all, q.ID)
	}
	if all[0].GoldTurnUUIDs[0] != "u1" {
		t.Errorf("GoldTurnUUIDs=%v", all[0].GoldTurnUUIDs)
	}
}

func TestStore_CountForGeneration(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	gen := "g/v1"
	corpusHash := "abc"
	for i, text := range []string{"hello", "world"} {
		q := queries.LabeledQuery{
			ID:            queries.QueryID(text, queries.CategoryParaphrase),
			Category:      queries.CategoryParaphrase,
			Text:          text,
			GoldTurnUUIDs: []string{"u1"},
			GeneratedAt:   time.Unix(1700000000+int64(i), 0).UTC(),
		}
		if err := s.Put(ctx, q, gen, corpusHash); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	n, err := s.CountForGeneration(ctx, gen, corpusHash, queries.CategoryParaphrase)
	if err != nil {
		t.Fatalf("CountForGeneration: %v", err)
	}
	if n != 2 {
		t.Errorf("n=%d, want 2", n)
	}
	other, err := s.CountForGeneration(ctx, gen, "different-corpus", queries.CategoryParaphrase)
	if err != nil {
		t.Fatalf("CountForGeneration other: %v", err)
	}
	if other != 0 {
		t.Errorf("other corpus n=%d, want 0", other)
	}
}

func TestStore_Embedding_RoundTrip(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	q := queries.LabeledQuery{
		ID:          queries.QueryID("hello", queries.CategoryParaphrase),
		Category:    queries.CategoryParaphrase,
		Text:        "hello",
		GeneratedAt: time.Unix(1700000000, 0).UTC(),
	}
	if err := s.Put(ctx, q, "g/v1", "abc"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	want := []float32{0.1, 0.2, 0.3, 0.4}
	if err := s.PutEmbedding(ctx, q.ID, want); err != nil {
		t.Fatalf("PutEmbedding: %v", err)
	}
	got, ok, err := s.Embedding(ctx, q.ID, 4)
	if err != nil {
		t.Fatalf("Embedding: %v", err)
	}
	if !ok {
		t.Fatal("Embedding ok=false")
	}
	if len(got) != 4 {
		t.Fatalf("len=%d, want 4", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("vec[%d]=%v, want %v", i, got[i], want[i])
		}
	}
}

func openStore(t *testing.T) *queries.Store {
	t.Helper()
	s, err := queries.OpenStore(filepath.Join(t.TempDir(), "queries.sqlite"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
