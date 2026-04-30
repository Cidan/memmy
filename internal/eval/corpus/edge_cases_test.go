package corpus_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cidan/memmy/internal/eval/corpus"
)

func TestExtract_MalformedJSONReportsFileAndLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.jsonl")
	body := strings.Join([]string{
		`{"type":"user","uuid":"u1","sessionId":"s","timestamp":"2026-04-27T12:00:00Z","message":{"role":"user","content":"ok"}}`,
		`{this is not json at all`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := corpus.Extract(path, func(_ corpus.Turn) error { return nil })
	if err == nil {
		t.Fatal("expected malformed JSON to produce an error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not mention file path %q", err.Error(), path)
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error %q does not mention 'line 2'", err.Error())
	}
}

func TestExtract_EmbeddedNewlinesAndTabsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "newlines.jsonl")
	// embedded \n and \t in the JSON-encoded string
	body := `{"type":"user","uuid":"u1","sessionId":"s","timestamp":"2026-04-27T12:00:00Z","message":{"role":"user","content":"line1\nline2\tindented"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got corpus.Turn
	if err := corpus.Extract(path, func(tn corpus.Turn) error {
		got = tn
		return nil
	}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want := "line1\nline2\tindented"
	if got.Text != want {
		t.Errorf("text=%q, want %q", got.Text, want)
	}

	store, err := corpus.OpenStore(filepath.Join(dir, "corpus.sqlite"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.PutTurn(context.Background(), got); err != nil {
		t.Fatalf("PutTurn: %v", err)
	}
	var seen string
	if err := store.IterateTurns(context.Background(), func(st corpus.StoredTurn) error {
		seen = st.Text
		return nil
	}); err != nil {
		t.Fatalf("IterateTurns: %v", err)
	}
	if seen != want {
		t.Errorf("stored text=%q, want %q", seen, want)
	}
}

func TestExtract_ThinkingOnlyContentSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thinking.jsonl")
	body := `{"type":"assistant","uuid":"a1","sessionId":"s","timestamp":"2026-04-27T12:00:00Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hidden"},{"type":"tool_use","id":"t","name":"x"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	count := 0
	if err := corpus.Extract(path, func(_ corpus.Turn) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if count != 0 {
		t.Errorf("got %d turns, want 0 (thinking-only assistant message must be skipped)", count)
	}
}

func TestExtract_EmptyContentSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	body := `{"type":"user","uuid":"u","sessionId":"s","timestamp":"2026-04-27T12:00:00Z","message":{"role":"user","content":""}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	count := 0
	if err := corpus.Extract(path, func(_ corpus.Turn) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if count != 0 {
		t.Errorf("got %d turns, want 0 (empty content)", count)
	}
}

func TestExtract_MissingTimestampParsesAsZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-ts.jsonl")
	body := `{"type":"user","uuid":"u","sessionId":"s","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var seen corpus.Turn
	got := 0
	if err := corpus.Extract(path, func(tn corpus.Turn) error {
		seen = tn
		got++
		return nil
	}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != 1 {
		t.Fatalf("got %d turns, want 1", got)
	}
	if !seen.Timestamp.IsZero() {
		t.Errorf("Timestamp=%s, want zero (missing field)", seen.Timestamp)
	}
}

func TestHashFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty")
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gotHash, gotSize, gotMtime, err := corpus.HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	if gotSize != 0 {
		t.Errorf("size=%d, want 0", gotSize)
	}
	wantSum := sha256.Sum256(nil)
	if gotHash != hex.EncodeToString(wantSum[:]) {
		t.Errorf("hash=%q, want %q", gotHash, hex.EncodeToString(wantSum[:]))
	}
	if gotMtime.IsZero() {
		t.Error("mtime is zero — should be the file's stat mtime")
	}
}

func TestHashFile_MissingFile(t *testing.T) {
	_, _, _, err := corpus.HashFile(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected wrapped os.ErrNotExist, got %v", err)
	}
}

// ListJSONLFiles on a non-jsonl single file should error; we already
// have a test for that. Here we add the missing-path case.
func TestListJSONLFiles_MissingPathErrors(t *testing.T) {
	_, err := corpus.ListJSONLFiles(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "stat") {
		t.Errorf("error %q does not mention stat", err.Error())
	}
}

// guarded against future regressions: the sample synthesized JSONL
// fmt format must remain decodable by the extractor.
func TestExtract_SampleFmt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fmt.jsonl")
	body := fmt.Sprintf(`{"type":"user","uuid":"u","sessionId":"s","timestamp":"2026-04-27T12:00:00Z","message":{"role":"user","content":%q}}`+"\n", "hello world")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got corpus.Turn
	if err := corpus.Extract(path, func(tn corpus.Turn) error { got = tn; return nil }); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got.Text != "hello world" {
		t.Errorf("Text=%q", got.Text)
	}
}
