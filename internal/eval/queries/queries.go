// Package queries owns the labeled-query side of the eval framework.
//
// A LabeledQuery couples a query string with the gold node IDs (or
// turn UUIDs) that should rank highly when the query runs against the
// replayed memmy store. Labels come either from a Generator (cheap
// rule-based for tests, LLM for production) or from a Judge that
// scores returned candidates after the fact.
//
// The package ships pluggable Generator / Judge interfaces and Fake
// implementations that work without network access. Real Gemini-backed
// implementations are wired in cmd/memmy-eval to keep this package
// import-graph clean for unit tests.
package queries

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Cidan/memmy/internal/eval/corpus"
)

// Category labels the kind of query for per-bucket metric reporting.
// Generators produce queries grouped by category and downstream metric
// aggregation slices results by category.
type Category string

const (
	// CategoryParaphrase: lightly reworded version of a known turn —
	// the gold turn should rank top-1.
	CategoryParaphrase Category = "paraphrase"
	// CategoryNegation: query that should NOT match the named turn.
	CategoryNegation Category = "negation"
	// CategoryTopicJump: query targets a topic only one specific turn covered.
	CategoryTopicJump Category = "topic-jump"
	// CategoryDistractor: query similar in surface form but topically distinct.
	CategoryDistractor Category = "distractor"
	// CategoryStaleRelevant: query targets an old turn — tests decay vs relevance.
	CategoryStaleRelevant Category = "stale-relevant"
	// CategoryTemporal: query references a time window ("yesterday").
	CategoryTemporal Category = "temporal"
)

// AllCategories returns every supported category in declaration order.
func AllCategories() []Category {
	return []Category{
		CategoryParaphrase,
		CategoryNegation,
		CategoryTopicJump,
		CategoryDistractor,
		CategoryStaleRelevant,
		CategoryTemporal,
	}
}

// LabeledQuery is the unit a Generator produces and a query battery
// consumes. GoldTurnUUIDs are the corpus turn UUIDs whose chunks
// should be considered "correct hits"; metrics map them to memmy node
// IDs at scoring time via the source turn UUID stored in node text.
type LabeledQuery struct {
	ID            string
	Category      Category
	Text          string
	GoldTurnUUIDs []string
	Notes         string
	GeneratedAt   time.Time
}

// QueryID returns the stable hash used as primary key.
func QueryID(text string, cat Category) string {
	h := sha256.New()
	_, _ = h.Write([]byte(string(cat)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// Generator turns a corpus into a labeled query set. Implementations
// must be deterministic given the same (corpus, request) pair — the
// dedup key in the queries store assumes identical re-runs produce
// identical outputs.
type Generator interface {
	// Version is the generator-version field used as part of the
	// dedup key. Bump when the prompting strategy changes.
	Version() string
	// Generate returns up to req.TargetN queries per category in
	// req.Categories, drawn from `turns`.
	Generate(ctx context.Context, turns []corpus.StoredTurn, req GenerateRequest) ([]LabeledQuery, error)
}

// GenerateRequest configures one Generator.Generate call.
type GenerateRequest struct {
	Categories []Category
	TargetN    int // per category
}

// Judge scores a (query, candidate) pair as relevant or not. Used to
// expand gold labels after a run by asking an LLM "did the candidate
// actually answer this query?" Returned scores are in [0, 1].
type Judge interface {
	Version() string
	Judge(ctx context.Context, q LabeledQuery, candidates []Candidate) ([]Verdict, error)
}

// Candidate is a single returned chunk handed to the Judge.
type Candidate struct {
	NodeID  string
	TurnUUID string
	Text    string
}

// Verdict is the Judge's assessment of one candidate.
type Verdict struct {
	NodeID  string
	Score   float64 // 0..1, higher = more relevant
	Reason  string
}

// FakeGenerator emits deterministic per-turn queries for tests.
// It does NOT use any external service. Output:
//   - paraphrase: the first sentence of the turn, prefixed with "about:"
//   - distractor: a phrase guaranteed not to appear in the corpus
type FakeGenerator struct{}

// NewFakeGenerator returns the deterministic test Generator.
func NewFakeGenerator() *FakeGenerator { return &FakeGenerator{} }

// Version satisfies Generator.
func (FakeGenerator) Version() string { return "fake-generator/v1" }

// Generate satisfies Generator.
func (FakeGenerator) Generate(_ context.Context, turns []corpus.StoredTurn, req GenerateRequest) ([]LabeledQuery, error) {
	if len(turns) == 0 {
		return nil, errors.New("queries: no turns to generate from")
	}
	if req.TargetN <= 0 {
		req.TargetN = 5
	}
	if len(req.Categories) == 0 {
		req.Categories = []Category{CategoryParaphrase, CategoryDistractor}
	}
	out := make([]LabeledQuery, 0, len(req.Categories)*req.TargetN)
	for _, cat := range req.Categories {
		switch cat {
		case CategoryParaphrase:
			for i, tn := range turns {
				if i >= req.TargetN {
					break
				}
				text := "about: " + firstSentence(tn.Text)
				out = append(out, LabeledQuery{
					ID:            QueryID(text, cat),
					Category:      cat,
					Text:          text,
					GoldTurnUUIDs: []string{tn.UUID},
					Notes:         "fake paraphrase from turn " + tn.UUID,
					GeneratedAt:   tn.Timestamp,
				})
			}
		case CategoryDistractor:
			for i := range req.TargetN {
				text := fmt.Sprintf("zzzzz unrelated query token-%d unicornsRus", i)
				out = append(out, LabeledQuery{
					ID:          QueryID(text, cat),
					Category:    cat,
					Text:        text,
					Notes:       "fake distractor (no gold turns)",
					GeneratedAt: turns[0].Timestamp,
				})
			}
		default:
			// Unsupported category in the fake generator: emit a tagged
			// stub query without gold so metrics still slice cleanly.
			for i := range req.TargetN {
				text := fmt.Sprintf("[fake-%s-%d]", cat, i)
				out = append(out, LabeledQuery{
					ID:          QueryID(text, cat),
					Category:    cat,
					Text:        text,
					GeneratedAt: turns[0].Timestamp,
				})
			}
		}
	}
	return out, nil
}

// FakeJudge declares any candidate whose text shares a non-trivial
// token with the query "relevant" (score 1.0); everything else 0.0.
type FakeJudge struct{}

// NewFakeJudge returns the deterministic test Judge.
func NewFakeJudge() *FakeJudge { return &FakeJudge{} }

// Version satisfies Judge.
func (FakeJudge) Version() string { return "fake-judge/v1" }

// Judge satisfies Judge.
func (FakeJudge) Judge(_ context.Context, q LabeledQuery, cands []Candidate) ([]Verdict, error) {
	qTokens := tokenize(q.Text)
	out := make([]Verdict, len(cands))
	for i, c := range cands {
		cTokens := tokenize(c.Text)
		v := Verdict{NodeID: c.NodeID}
		if hasOverlap(qTokens, cTokens) {
			v.Score = 1.0
			v.Reason = "token overlap"
		} else {
			v.Reason = "no overlap"
		}
		out[i] = v
	}
	return out, nil
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	for i, r := range s {
		if r == '.' || r == '!' || r == '?' {
			return strings.TrimSpace(s[:i+1])
		}
	}
	return s
}

func tokenize(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for w := range strings.FieldsSeq(strings.ToLower(s)) {
		w = strings.Trim(w, ".,!?:;\"'()[]{}")
		if len(w) >= 4 {
			out[w] = struct{}{}
		}
	}
	return out
}

func hasOverlap(a, b map[string]struct{}) bool {
	for w := range a {
		if _, ok := b[w]; ok {
			return true
		}
	}
	return false
}
