package embedcache_test

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Cidan/memmy/internal/embed"
	"github.com/Cidan/memmy/internal/embed/fake"
	"github.com/Cidan/memmy/internal/eval/embedcache"
)

func TestPutGet_RoundTrip(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	want := []float32{0.1, 0.2, 0.3, 0.4}
	if err := c.Put(ctx, "fake-4", 4, "hello", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, hit, err := c.Get(ctx, "fake-4", 4, "hello")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !hit {
		t.Fatal("Get reported miss for stored content")
	}
	if len(got) != len(want) {
		t.Fatalf("dim mismatch: got %d want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("vec[%d]=%v, want %v", i, got[i], want[i])
		}
	}
}

func TestGet_MissReportsFalse(t *testing.T) {
	c := openCache(t)
	_, hit, err := c.Get(context.Background(), "fake-4", 4, "absent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if hit {
		t.Fatal("Get reported hit for absent content")
	}
}

func TestEmbedBatch_DedupsRepeatedCalls(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	emb := &countingEmbedder{Embedder: fake.New(8)}

	texts := []string{"alpha", "beta", "gamma"}
	first, err := c.EmbedBatch(ctx, emb, "fake-8", embed.EmbedTaskRetrievalDocument, texts)
	if err != nil {
		t.Fatalf("EmbedBatch first: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("first len=%d, want 3", len(first))
	}
	firstCount := emb.embedCalls.Load()
	if firstCount == 0 {
		t.Fatal("expected at least one embedder call on cold cache")
	}

	second, err := c.EmbedBatch(ctx, emb, "fake-8", embed.EmbedTaskRetrievalDocument, texts)
	if err != nil {
		t.Fatalf("EmbedBatch second: %v", err)
	}
	if emb.embedCalls.Load() != firstCount {
		t.Fatalf("warm cache called embedder again: before=%d after=%d", firstCount, emb.embedCalls.Load())
	}
	for i := range first {
		if len(first[i]) != len(second[i]) {
			t.Fatalf("dim drift at %d", i)
		}
		for j := range first[i] {
			if first[i][j] != second[i][j] {
				t.Errorf("vec[%d][%d]=%v vs %v", i, j, first[i][j], second[i][j])
			}
		}
	}
}

func TestEmbedBatch_PreservesInputOrderWithMixedHits(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	emb := &countingEmbedder{Embedder: fake.New(8)}

	if _, err := c.EmbedBatch(ctx, emb, "fake-8", embed.EmbedTaskRetrievalDocument, []string{"alpha", "gamma"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mixed := []string{"alpha", "beta", "gamma", "delta"}
	got, err := c.EmbedBatch(ctx, emb, "fake-8", embed.EmbedTaskRetrievalDocument, mixed)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}

	want, err := emb.Embed(ctx, embed.EmbedTaskRetrievalDocument, mixed)
	if err != nil {
		t.Fatalf("oracle Embed: %v", err)
	}
	for i := range mixed {
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Errorf("position %d component %d: got %v want %v", i, j, got[i][j], want[i][j])
				break
			}
		}
	}
}

func TestCount(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	emb := fake.New(8)

	for _, s := range []string{"a", "b", "c"} {
		v, err := emb.Embed(ctx, embed.EmbedTaskUnspecified, []string{s})
		if err != nil {
			t.Fatalf("Embed: %v", err)
		}
		if err := c.Put(ctx, "fake-8", 8, s, v[0]); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	n, err := c.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("Count=%d, want 3", n)
	}
}

func openCache(t *testing.T) *embedcache.Cache {
	t.Helper()
	c, err := embedcache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

type countingEmbedder struct {
	embed.Embedder
	embedCalls atomic.Int64
}

func (c *countingEmbedder) Embed(ctx context.Context, task embed.EmbedTask, texts []string) ([][]float32, error) {
	c.embedCalls.Add(int64(len(texts)))
	return c.Embedder.Embed(ctx, task, texts)
}
