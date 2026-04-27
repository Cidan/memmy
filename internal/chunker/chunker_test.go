package chunker_test

import (
	"strings"
	"testing"

	"github.com/Cidan/memmy/internal/chunker"
)

func TestSlidingWindow_TenSentences(t *testing.T) {
	text := "S1. S2. S3. S4. S5. S6. S7. S8. S9. S10."
	chunks := chunker.Default(text)
	want := [][2]int{
		{0, 3}, {2, 5}, {4, 7}, {6, 9}, {8, 10},
	}
	if len(chunks) != len(want) {
		t.Fatalf("len=%d, want %d; chunks=%v", len(chunks), len(want), chunks)
	}
	for i, c := range chunks {
		if c.SentenceSpan != want[i] {
			t.Errorf("chunk %d: span=%v, want %v", i, c.SentenceSpan, want[i])
		}
	}
}

func TestSlidingWindow_TrailingShortWindow(t *testing.T) {
	// Spans should still include the last sentence even if the trailing
	// window is shorter than windowSize.
	text := "S1. S2. S3. S4."
	chunks := chunker.Default(text)
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	last := chunks[len(chunks)-1]
	if last.SentenceSpan[1] != 4 {
		t.Errorf("last span end=%d, want 4", last.SentenceSpan[1])
	}
}

func TestSlidingWindow_OneSentence(t *testing.T) {
	chunks := chunker.Default("Just one sentence.")
	if len(chunks) != 1 {
		t.Fatalf("len=%d, want 1", len(chunks))
	}
	if chunks[0].SentenceSpan != [2]int{0, 1} {
		t.Errorf("span=%v, want [0,1)", chunks[0].SentenceSpan)
	}
}

func TestSlidingWindow_TwoSentences(t *testing.T) {
	chunks := chunker.Default("First. Second.")
	if len(chunks) != 1 {
		t.Fatalf("len=%d, want 1", len(chunks))
	}
	if chunks[0].SentenceSpan != [2]int{0, 2} {
		t.Errorf("span=%v, want [0,2)", chunks[0].SentenceSpan)
	}
}

func TestSlidingWindow_Empty(t *testing.T) {
	if got := chunker.Default(""); got != nil {
		t.Fatalf("empty input produced %v", got)
	}
	if got := chunker.Default("   \n\t  "); got != nil {
		t.Fatalf("whitespace input produced %v", got)
	}
}

func TestSplitSentences_SimpleTerminators(t *testing.T) {
	got := chunker.SplitSentences("Hello world. How are you? Fine!")
	want := []string{"Hello world.", "How are you?", "Fine!"}
	if len(got) != len(want) {
		t.Fatalf("len=%d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitSentences_Abbreviations(t *testing.T) {
	got := chunker.SplitSentences("Mr. Smith went home. Dr. Jones followed. The end.")
	want := []string{"Mr. Smith went home.", "Dr. Jones followed.", "The end."}
	if len(got) != len(want) {
		t.Fatalf("len=%d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitSentences_Initials(t *testing.T) {
	got := chunker.SplitSentences("J. R. R. Tolkien wrote books. He was English.")
	want := []string{"J. R. R. Tolkien wrote books.", "He was English."}
	if len(got) != len(want) {
		t.Fatalf("len=%d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitSentences_TrailingNoTerminator(t *testing.T) {
	got := chunker.SplitSentences("First. Second")
	want := []string{"First.", "Second"}
	if len(got) != len(want) {
		t.Fatalf("len=%d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSlidingWindow_SpanMatchesText(t *testing.T) {
	text := "Alpha. Beta. Gamma. Delta. Epsilon."
	sentences := chunker.SplitSentences(text)
	chunks := chunker.Default(text)
	for _, c := range chunks {
		want := strings.Join(sentences[c.SentenceSpan[0]:c.SentenceSpan[1]], " ")
		if c.Text != want {
			t.Errorf("span=%v: text=%q, want %q", c.SentenceSpan, c.Text, want)
		}
	}
}
