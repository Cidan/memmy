// Package chunker splits a message into sentence-sliding-window chunks.
// See DESIGN.md §4.1.
//
// The window is size=3, stride=2 (one-sentence overlap). For 10 sentences
// the windows are [1,2,3] [3,4,5] [5,6,7] [7,8,9] [9,10] (the trailing
// window is shorter when sentences run out).
package chunker

import (
	"strings"
	"unicode"
)

// Chunk is one sliding-window result.
type Chunk struct {
	// SentenceSpan is [start, end) over the input sentence array.
	SentenceSpan [2]int
	// Text is the concatenated sentence text for this window.
	Text string
}

// Default window parameters.
const (
	DefaultWindowSize = 3
	DefaultStride     = 2
)

// Chunk splits text into sliding-window chunks using default parameters.
func Default(text string) []Chunk { return Chunkify(text, DefaultWindowSize, DefaultStride) }

// Chunkify splits text into windows of windowSize sentences advanced by
// stride. windowSize must be >= 1; stride must be >= 1 and <= windowSize.
func Chunkify(text string, windowSize, stride int) []Chunk {
	if windowSize < 1 {
		windowSize = 1
	}
	if stride < 1 {
		stride = 1
	}
	if stride > windowSize {
		stride = windowSize
	}

	sentences := SplitSentences(text)
	if len(sentences) == 0 {
		return nil
	}

	var out []Chunk
	for start := 0; start < len(sentences); start += stride {
		end := start + windowSize
		if end > len(sentences) {
			end = len(sentences)
		}
		out = append(out, Chunk{
			SentenceSpan: [2]int{start, end},
			Text:         strings.Join(sentences[start:end], " "),
		})
		if end == len(sentences) {
			break
		}
	}
	return out
}

// commonAbbreviations are recognized to avoid splitting after them.
// Lowercased; matched case-insensitively against the trailing token.
var commonAbbreviations = map[string]struct{}{
	"mr": {}, "mrs": {}, "ms": {}, "dr": {}, "prof": {},
	"sr": {}, "jr": {}, "st": {}, "mt": {},
	"e.g": {}, "i.e": {}, "cf": {}, "etc": {}, "vs": {},
	"u.s": {}, "u.k": {}, "n.y": {}, "a.m": {}, "p.m": {},
	"no": {}, "fig": {}, "ed": {}, "eds": {}, "vol": {},
	"inc": {}, "ltd": {}, "co": {}, "corp": {},
}

// SplitSentences breaks text into sentences using a rule-based splitter:
// terminators (.?!) end a sentence unless preceded by a known abbreviation
// or by a single capital letter (an initial like "J. R. R. Tolkien").
func SplitSentences(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var (
		sentences []string
		buf       strings.Builder
		runes     = []rune(text)
	)

	flush := func() {
		s := strings.TrimSpace(buf.String())
		if s != "" {
			sentences = append(sentences, s)
		}
		buf.Reset()
	}

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		buf.WriteRune(r)

		if r != '.' && r != '?' && r != '!' {
			continue
		}

		// Coalesce trailing terminators (e.g., "!!", "?!").
		for i+1 < len(runes) && isTerminator(runes[i+1]) {
			i++
			buf.WriteRune(runes[i])
		}

		// Look at the next non-space rune to decide whether this is a sentence break.
		j := i + 1
		for j < len(runes) && unicode.IsSpace(runes[j]) {
			j++
		}
		// End-of-text: terminate sentence.
		if j >= len(runes) {
			flush()
			i = j - 1
			continue
		}
		// For '.' specifically, check abbreviations/initials. Lower-case
		// continuation is NOT a reliable signal — users frequently fail
		// to capitalize the next sentence — so we only suppress the
		// split when the trailing token is a known abbreviation or a
		// single-letter initial.
		if r == '.' {
			if isAbbreviationOrInitial(buf.String()) {
				continue
			}
		}
		flush()
		i = j - 1
	}
	// Flush any trailing fragment without a terminator.
	flush()

	return sentences
}

func isTerminator(r rune) bool { return r == '.' || r == '?' || r == '!' }

// isAbbreviationOrInitial inspects the just-flushed sentence buffer ending
// in '.' and reports whether the trailing token is a common abbreviation
// or a single-capital-letter initial.
func isAbbreviationOrInitial(s string) bool {
	// Trim trailing whitespace and the terminator(s).
	t := strings.TrimRight(s, " \t\n\r")
	t = strings.TrimRight(t, ".?!")
	if t == "" {
		return false
	}

	// Single capital letter initial like "J" in "J. R. R. Tolkien".
	if last := lastWord(t); len(last) == 1 {
		r := rune(last[0])
		if unicode.IsUpper(r) && unicode.IsLetter(r) {
			return true
		}
	}

	// Match abbreviation against the trailing token (lowercased).
	last := strings.ToLower(lastWord(t))
	if _, ok := commonAbbreviations[last]; ok {
		return true
	}
	return false
}

func lastWord(s string) string {
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			return s[i+1:]
		}
	}
	return s
}
