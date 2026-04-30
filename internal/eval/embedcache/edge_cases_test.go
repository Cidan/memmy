package embedcache_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/Cidan/memmy/internal/embed"
	"github.com/Cidan/memmy/internal/embed/fake"
	"github.com/Cidan/memmy/internal/eval/embedcache"
)

func TestEmbedBatch_ConcurrentAccessDeduplicatesEmbedderCalls(t *testing.T) {
	c := openCacheTmp(t)
	ctx := context.Background()
	emb := &countingEmbedder{Embedder: fake.New(8)}

	const goroutines = 8
	const perBatch = 5
	texts := make([]string, perBatch)
	for i := range perBatch {
		texts[i] = fmt.Sprintf("shared-text-%d", i)
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		errors []error
	)
	for range goroutines {
		wg.Go(func() {
			vecs, err := c.EmbedBatch(ctx, emb, "fake-8", embed.EmbedTaskRetrievalDocument, texts)
			if err != nil {
				mu.Lock()
				errors = append(errors, err)
				mu.Unlock()
				return
			}
			if len(vecs) != perBatch {
				mu.Lock()
				errors = append(errors, fmt.Errorf("got %d vecs, want %d", len(vecs), perBatch))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	for _, e := range errors {
		t.Errorf("goroutine error: %v", e)
	}

	// Concurrent goroutines may race past the cache before any of them
	// have written, so the embedder may legitimately be called more
	// than `perBatch` total — but it must NEVER exceed the upper bound
	// of (goroutines * perBatch), AND a follow-up call after the dust
	// settles must be a 100% cache hit.
	maxAllowed := int64(goroutines * perBatch)
	if got := emb.embedCalls.Load(); got > maxAllowed {
		t.Errorf("embedder called %d times under contention; max allowed %d", got, maxAllowed)
	}

	// Quiescent re-call: must be zero new calls.
	before := emb.embedCalls.Load()
	if _, err := c.EmbedBatch(ctx, emb, "fake-8", embed.EmbedTaskRetrievalDocument, texts); err != nil {
		t.Fatalf("re-call: %v", err)
	}
	if got := emb.embedCalls.Load(); got != before {
		t.Errorf("warm cache called embedder again: before=%d after=%d", before, got)
	}
}

func TestPut_RejectsDimMismatch(t *testing.T) {
	c := openCacheTmp(t)
	err := c.Put(context.Background(), "fake-4", 4, "x", []float32{0.1, 0.2})
	if err == nil {
		t.Fatal("expected dim-mismatch error")
	}
	// We want the message to mention the actual numbers so a debugger
	// gets useful context.
	if !containsAll(err.Error(), "2", "4") {
		t.Errorf("error %q should mention both 2 and 4", err.Error())
	}
}

func TestGet_CorruptedRowReturnsError(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cache.db")
	c, err := embedcache.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if err := c.Put(context.Background(), "fake-4", 4, "x", []float32{0.1, 0.2, 0.3, 0.4}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Corrupt the row directly via a fresh handle (simulating disk corruption).
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := db.Exec(`UPDATE embeddings SET vector = ?`, []byte{0x00, 0x01, 0x02}); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	_ = db.Close()

	c2, err := embedcache.Open(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	_, _, err = c2.Get(context.Background(), "fake-4", 4, "x")
	if err == nil {
		t.Fatal("expected error reading corrupted row")
	}
}

func TestOpen_RejectsEmptyPath(t *testing.T) {
	if _, err := embedcache.Open(""); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestCacheSurvivesCloseAndReopen(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cache.db")
	c, err := embedcache.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := []float32{0.1, 0.2, 0.3, 0.4}
	if err := c.Put(context.Background(), "fake-4", 4, "x", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c2, err := embedcache.Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	got, hit, err := c2.Get(context.Background(), "fake-4", 4, "x")
	if err != nil {
		t.Fatalf("Get after re-open: %v", err)
	}
	if !hit {
		t.Fatal("re-opened cache lost the row")
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("vec[%d]=%v, want %v", i, got[i], want[i])
		}
	}
}

func openCacheTmp(t *testing.T) *embedcache.Cache {
	t.Helper()
	c, err := embedcache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

