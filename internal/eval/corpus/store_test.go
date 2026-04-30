package corpus_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cidan/memmy/internal/eval/corpus"
)

func TestStore_PutAndIterateInOrder(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)

	// Insert out-of-order so we can confirm IterateTurns sorts by ts.
	turns := []corpus.Turn{
		{UUID: "t3", SessionID: "s", Role: "user", Text: "third", Timestamp: now.Add(2 * time.Minute), SourceFile: "/x"},
		{UUID: "t1", SessionID: "s", Role: "user", Text: "first", Timestamp: now, SourceFile: "/x"},
		{UUID: "t2", SessionID: "s", Role: "assistant", Text: "second", Timestamp: now.Add(time.Minute), SourceFile: "/x"},
	}
	for _, tn := range turns {
		if err := s.PutTurn(ctx, tn); err != nil {
			t.Fatalf("PutTurn: %v", err)
		}
	}
	count, err := s.CountTurns(ctx)
	if err != nil {
		t.Fatalf("CountTurns: %v", err)
	}
	if count != 3 {
		t.Fatalf("count=%d, want 3", count)
	}

	var seen []string
	if err := s.IterateTurns(ctx, func(st corpus.StoredTurn) error {
		seen = append(seen, st.UUID)
		return nil
	}); err != nil {
		t.Fatalf("IterateTurns: %v", err)
	}
	want := []string{"t1", "t2", "t3"}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("order[%d]=%q, want %q", i, seen[i], want[i])
		}
	}
}

func TestStore_PutTurnIdempotent(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	tn := corpus.Turn{UUID: "u1", SessionID: "s", Role: "user", Text: "x", Timestamp: now, SourceFile: "/x"}
	for i := range 3 {
		if err := s.PutTurn(ctx, tn); err != nil {
			t.Fatalf("PutTurn %d: %v", i, err)
		}
	}
	count, err := s.CountTurns(ctx)
	if err != nil {
		t.Fatalf("CountTurns: %v", err)
	}
	if count != 1 {
		t.Errorf("count=%d, want 1 (deduped)", count)
	}
}

func TestStore_HasSourceFile(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	sf := corpus.SourceFile{
		Path:        "/x.jsonl",
		ModTime:     time.Unix(1700000000, 0).UTC(),
		SizeBytes:   100,
		ContentHash: "abc",
		IngestedAt:  time.Now().UTC(),
	}
	got, err := s.HasSourceFile(ctx, sf)
	if err != nil {
		t.Fatalf("HasSourceFile: %v", err)
	}
	if got {
		t.Fatal("HasSourceFile=true before Put")
	}
	if err := s.PutSourceFile(ctx, sf); err != nil {
		t.Fatalf("PutSourceFile: %v", err)
	}
	got, err = s.HasSourceFile(ctx, sf)
	if err != nil {
		t.Fatalf("HasSourceFile after put: %v", err)
	}
	if !got {
		t.Fatal("HasSourceFile=false after Put")
	}

	// Different mtime → miss.
	sf.ModTime = sf.ModTime.Add(time.Hour)
	got, err = s.HasSourceFile(ctx, sf)
	if err != nil {
		t.Fatalf("HasSourceFile new mtime: %v", err)
	}
	if got {
		t.Fatal("HasSourceFile reported hit on different mtime")
	}
}

func TestStore_SnapshotHashStable(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	for i, u := range []string{"a", "b", "c"} {
		if err := s.PutTurn(ctx, corpus.Turn{
			UUID: u, SessionID: "s", Role: "user", Text: "x",
			Timestamp: now.Add(time.Duration(i) * time.Second), SourceFile: "/x",
		}); err != nil {
			t.Fatalf("PutTurn: %v", err)
		}
	}
	h1, err := s.SnapshotHash(ctx)
	if err != nil {
		t.Fatalf("SnapshotHash: %v", err)
	}
	h2, err := s.SnapshotHash(ctx)
	if err != nil {
		t.Fatalf("SnapshotHash: %v", err)
	}
	if h1 != h2 || h1 == "" {
		t.Errorf("hash unstable or empty: %q vs %q", h1, h2)
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	h, size, _, err := corpus.HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	if size != 5 {
		t.Errorf("size=%d, want 5", size)
	}
	if len(h) != 64 {
		t.Errorf("hash hex len=%d, want 64", len(h))
	}
}

func openStore(t *testing.T) *corpus.Store {
	t.Helper()
	s, err := corpus.OpenStore(filepath.Join(t.TempDir(), "corpus.sqlite"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
