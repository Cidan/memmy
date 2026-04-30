package corpus_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cidan/memmy/internal/eval/corpus"
)

const sampleJSONL = `{"type":"file-history-snapshot","messageId":"x","snapshot":{}}
{"type":"user","isSidechain":false,"uuid":"u1","sessionId":"s1","timestamp":"2026-03-30T18:45:44.250Z","gitBranch":"HEAD","message":{"role":"user","content":"hello world"}}
{"type":"assistant","isSidechain":false,"uuid":"a1","parentUuid":"u1","sessionId":"s1","timestamp":"2026-03-30T18:45:50.250Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"don't extract me"},{"type":"text","text":"hi back"}]}}
{"type":"user","isSidechain":true,"uuid":"u2","sessionId":"s1","timestamp":"2026-03-30T18:46:00.250Z","message":{"role":"user","content":"sidechain skipped"}}
{"type":"system","uuid":"sys","sessionId":"s1","timestamp":"2026-03-30T18:46:10.250Z","message":{"role":"system","content":"system skipped"}}
{"type":"user","uuid":"u3","sessionId":"s1","timestamp":"2026-03-30T18:46:20.250Z","message":{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"ignored tool"}]},{"type":"text","text":"  block text  "}]}}
{"type":"assistant","uuid":"a2","sessionId":"s1","timestamp":"2026-03-30T18:46:30.250Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"only thinking"}]}}
`

func TestExtract_FilterAndDecode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(sampleJSONL), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var got []corpus.Turn
	if err := corpus.Extract(path, func(t corpus.Turn) error {
		got = append(got, t)
		return nil
	}); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d turns, want 3 (u1, a1, u3)", len(got))
	}
	if got[0].UUID != "u1" || got[0].Text != "hello world" {
		t.Errorf("turn[0]=%+v", got[0])
	}
	if got[1].UUID != "a1" || got[1].Text != "hi back" {
		t.Errorf("turn[1]=%+v", got[1])
	}
	if got[2].UUID != "u3" || got[2].Text != "block text" {
		t.Errorf("turn[2]=%+v (text should trim and skip tool_result blocks)", got[2])
	}
	if got[0].SessionID != "s1" {
		t.Errorf("SessionID=%q", got[0].SessionID)
	}
	if got[0].SourceFile != path {
		t.Errorf("SourceFile=%q want %q", got[0].SourceFile, path)
	}
	if got[0].Timestamp.IsZero() {
		t.Errorf("Timestamp zero for turn[0]")
	}
}

func TestExtract_DirectoryWalkOrdered(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b.jsonl", "a.jsonl", "z.txt"} {
		body := `{"type":"user","uuid":"u-` + name + `","sessionId":"s1","timestamp":"2026-03-30T18:45:44.250Z","message":{"role":"user","content":"x"}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	files, err := corpus.ListJSONLFiles(dir)
	if err != nil {
		t.Fatalf("ListJSONLFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2 (skip .txt)", len(files))
	}
	if !strings.HasSuffix(files[0], "a.jsonl") || !strings.HasSuffix(files[1], "b.jsonl") {
		t.Errorf("not lex-sorted: %v", files)
	}

	var seen []string
	if err := corpus.Extract(dir, func(t corpus.Turn) error {
		seen = append(seen, t.UUID)
		return nil
	}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("got %d turns, want 2", len(seen))
	}
	if seen[0] != "u-a.jsonl" || seen[1] != "u-b.jsonl" {
		t.Errorf("not in file order: %v", seen)
	}
}

func TestExtract_SingleFileMustHaveJSONLExtension(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "session.txt")
	if err := os.WriteFile(bad, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := corpus.ListJSONLFiles(bad); err == nil {
		t.Error("expected error for non-jsonl single file")
	}
}

func TestExtract_CallbackPropagatesError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	body := `{"type":"user","uuid":"u","sessionId":"s","timestamp":"2026-03-30T18:45:44.250Z","message":{"role":"user","content":"x"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	want := os.ErrInvalid
	err := corpus.Extract(path, func(_ corpus.Turn) error { return want })
	if err == nil {
		t.Fatal("expected callback error to propagate")
	}
}
