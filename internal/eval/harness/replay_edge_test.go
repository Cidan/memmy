package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Cidan/memmy"
	"github.com/Cidan/memmy/internal/embed"
	"github.com/Cidan/memmy/internal/embed/fake"
	"github.com/Cidan/memmy/internal/eval/harness"
)

// Empty corpus = no JSONL turns. Replay must still build a usable
// memmy.Service handle so callers can introspect the empty state.
func TestReplay_EmptyCorpus(t *testing.T) {
	root := t.TempDir()
	sessionsPath := filepath.Join(root, "empty.jsonl")
	if err := os.WriteFile(sessionsPath, []byte(""), 0o600); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	ctx := context.Background()
	const dim = 32
	emb := fake.New(dim)
	modelID := harness.FakeEmbedderModelID(dim)
	if _, err := harness.Ingest(ctx, "alpha", harness.IngestOptions{
		SessionsPath:    sessionsPath,
		CorpusStorePath: filepath.Join(root, "corpus.sqlite"),
		EmbedCachePath:  filepath.Join(root, "embedcache.sqlite"),
		Embedder:        emb,
		EmbedderModelID: modelID,
		EmbedderKind:    "fake",
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	res, err := harness.Replay(ctx, harness.ReplayOptions{
		CorpusStorePath: filepath.Join(root, "corpus.sqlite"),
		EmbedCachePath:  filepath.Join(root, "embedcache.sqlite"),
		MemmyDBPath:     filepath.Join(root, "memmy.db"),
		Embedder:        emb,
		EmbedderModelID: modelID,
		HNSWRandSeed:    42,
		DatasetName:     "alpha",
	})
	if err != nil {
		t.Fatalf("Replay (empty corpus): %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	if res.TurnsReplayed != 0 {
		t.Errorf("TurnsReplayed=%d, want 0", res.TurnsReplayed)
	}
	if res.NodesWritten != 0 {
		t.Errorf("NodesWritten=%d, want 0", res.NodesWritten)
	}
	if res.Service == nil {
		t.Error("Service is nil for empty-corpus replay")
	}
}

// After Replay primes the cache, a second Replay against the same
// corpus must NOT call the underlying embedder again — every chunk
// vector should come from the cache.
func TestReplay_CacheReuseAcrossInvocations(t *testing.T) {
	root := t.TempDir()
	sessionsPath := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(sessionsPath, []byte(synthJSONL), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx := context.Background()
	const dim = 32
	base := fake.New(dim)
	modelID := harness.FakeEmbedderModelID(dim)

	// Ingest primes the cache. Subsequent Replay must hit only.
	if _, err := harness.Ingest(ctx, "alpha", harness.IngestOptions{
		SessionsPath:    sessionsPath,
		CorpusStorePath: filepath.Join(root, "corpus.sqlite"),
		EmbedCachePath:  filepath.Join(root, "embedcache.sqlite"),
		Embedder:        base,
		EmbedderModelID: modelID,
		EmbedderKind:    "fake",
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	wrapped := &replayCounter{Embedder: base}
	res, err := harness.Replay(ctx, harness.ReplayOptions{
		CorpusStorePath: filepath.Join(root, "corpus.sqlite"),
		EmbedCachePath:  filepath.Join(root, "embedcache.sqlite"),
		MemmyDBPath:     filepath.Join(root, "memmy.db"),
		Embedder:        wrapped,
		EmbedderModelID: modelID,
		HNSWRandSeed:    42,
		DatasetName:     "alpha",
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	if got := wrapped.calls.Load(); got != 0 {
		t.Errorf("Replay called underlying embedder %d times; want 0 (all cached by Ingest)", got)
	}
}

func TestReplay_CustomTenantTuple(t *testing.T) {
	root := t.TempDir()
	sessionsPath := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(sessionsPath, []byte(synthJSONL), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx := context.Background()
	const dim = 32
	emb := fake.New(dim)
	modelID := harness.FakeEmbedderModelID(dim)
	if _, err := harness.Ingest(ctx, "alpha", harness.IngestOptions{
		SessionsPath:    sessionsPath,
		CorpusStorePath: filepath.Join(root, "corpus.sqlite"),
		EmbedCachePath:  filepath.Join(root, "embedcache.sqlite"),
		Embedder:        emb,
		EmbedderModelID: modelID,
		EmbedderKind:    "fake",
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	custom := map[string]string{"user": "ada", "scope": "test"}
	res, err := harness.Replay(ctx, harness.ReplayOptions{
		CorpusStorePath: filepath.Join(root, "corpus.sqlite"),
		EmbedCachePath:  filepath.Join(root, "embedcache.sqlite"),
		MemmyDBPath:     filepath.Join(root, "memmy.db"),
		Embedder:        emb,
		EmbedderModelID: modelID,
		HNSWRandSeed:    42,
		DatasetName:     "alpha",
		TenantTuple:     custom,
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	for k, v := range custom {
		if res.Tenant[k] != v {
			t.Errorf("tenant[%s]=%q, want %q", k, res.Tenant[k], v)
		}
	}
	if res.Tenant["agent"] == "memmy-eval" {
		t.Errorf("custom tenant was overridden by default; got %v", res.Tenant)
	}

	// Sanity: writes landed under the custom tenant — Recall returns hits.
	rec, err := res.Service.Recall(ctx, memmy.RecallRequest{
		Tenant: custom,
		Query:  "Apples sweet",
		K:      3,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(rec.Results) == 0 {
		t.Error("Recall returned no hits under custom tenant")
	}
}

// replayCounter wraps an embedder and counts how many texts pass
// through its Embed. Local to this file (not shared with other tests).
type replayCounter struct {
	embed.Embedder
	calls atomic.Int64
}

func (c *replayCounter) Embed(ctx context.Context, task embed.EmbedTask, texts []string) ([][]float32, error) {
	c.calls.Add(int64(len(texts)))
	return c.Embedder.Embed(ctx, task, texts)
}
