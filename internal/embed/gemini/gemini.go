// Package gemini implements the Embedder interface against Google's
// Gemini embeddings API via go-genai (google.golang.org/genai).
//
// Two task-hint strategies are supported:
//
//   - "param" — the model accepts a `task_type` API parameter
//     (gemini-embedding-001 and similar). Strategy: set
//     EmbedContentConfig.TaskType to the API enum string.
//
//   - "prefix" — the model expects task hints in-band as a
//     natural-language prompt prefix (gemini-embedding-2 and
//     similar). Strategy: prepend the documented prefix to each
//     input text and leave Config.TaskType unset.
//
// Strategy is selected from the model name. Defaults assume
// gemini-embedding-2.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/genai"

	"github.com/Cidan/memmy/internal/embed"
)

// Embedder is a Gemini-backed embedder.
type Embedder struct {
	client   *genai.Client
	model    string
	dim      int
	strategy taskStrategy
}

// Options configures the Gemini embedder.
type Options struct {
	APIKey string
	Model  string
	Dim    int
}

// New constructs a Gemini Embedder.
func New(ctx context.Context, opts Options) (*Embedder, error) {
	if opts.APIKey == "" {
		return nil, errors.New("gemini: APIKey is required")
	}
	if opts.Model == "" {
		return nil, errors.New("gemini: Model is required")
	}
	if opts.Dim < 1 {
		return nil, errors.New("gemini: Dim must be >= 1")
	}
	c, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  opts.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: NewClient: %w", err)
	}
	return &Embedder{
		client:   c,
		model:    opts.Model,
		dim:      opts.Dim,
		strategy: strategyFor(opts.Model),
	}, nil
}

func (e *Embedder) Dim() int { return e.dim }

// Embed satisfies embed.Embedder. The task hint is mapped to the
// strategy chosen at construction time: a TaskType API parameter for
// "param" models, or a prompt prefix on each input for "prefix"
// models. The fake embedder ignores task entirely; here we honor it.
func (e *Embedder) Embed(ctx context.Context, task embed.EmbedTask, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	contents := make([]*genai.Content, len(texts))
	// OutputDimensionality is documented as supported on
	// gemini-embedding-001 and gemini-embedding-2 (Matryoshka:
	// 3072 native, truncatable to 1536/768). Some models may ignore
	// it; the post-call dim mismatch check below is the safety net
	// that surfaces a clean error rather than silently writing
	// wrong-shape vectors into bbolt.
	cfg := &genai.EmbedContentConfig{
		OutputDimensionality: int32Ptr(int32(e.dim)),
	}
	switch e.strategy {
	case strategyParam:
		cfg.TaskType = taskTypeAPIString(task)
		for i, t := range texts {
			contents[i] = genai.NewContentFromText(t, genai.RoleUser)
		}
	case strategyPrefix:
		// gemini-embedding-2 expects task hints as a natural-language
		// prefix on the input text rather than a config parameter.
		prefixed := promptPrefix(task)
		for i, t := range texts {
			if prefixed != "" {
				t = prefixed + t
			}
			contents[i] = genai.NewContentFromText(t, genai.RoleUser)
		}
	}
	resp, err := e.client.Models.EmbedContent(ctx, e.model, contents, cfg)
	if err != nil {
		return nil, fmt.Errorf("gemini: EmbedContent: %w", err)
	}
	if len(resp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("gemini: response had %d embeddings, want %d",
			len(resp.Embeddings), len(texts))
	}
	out := make([][]float32, len(texts))
	for i, em := range resp.Embeddings {
		if em == nil {
			return nil, fmt.Errorf("gemini: nil embedding at index %d", i)
		}
		if len(em.Values) != e.dim {
			return nil, fmt.Errorf("gemini: embedding %d has dim %d, want %d",
				i, len(em.Values), e.dim)
		}
		out[i] = em.Values
	}
	return out, nil
}

// ----- task strategy -----

type taskStrategy uint8

const (
	strategyPrefix taskStrategy = iota // gemini-embedding-2 family
	strategyParam                      // gemini-embedding-001 family
)

// strategyFor selects the task-hint mechanism for a model name.
// Models documented to take a `task_type` parameter use strategyParam;
// every other model defaults to strategyPrefix (gemini-embedding-2's
// natural-language prefix scheme), which is the safer fallback because
// a prefix in the input is harmless if the model ignores it.
func strategyFor(model string) taskStrategy {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "gemini-embedding-001"):
		return strategyParam
	case strings.Contains(m, "text-embedding-004"):
		return strategyParam
	default:
		return strategyPrefix
	}
}

// taskTypeAPIString returns the API enum string for a task on
// strategyParam models. EmbedTaskUnspecified maps to "" (omit).
func taskTypeAPIString(task embed.EmbedTask) string {
	switch task {
	case embed.EmbedTaskRetrievalDocument:
		return "RETRIEVAL_DOCUMENT"
	case embed.EmbedTaskRetrievalQuery:
		return "RETRIEVAL_QUERY"
	case embed.EmbedTaskSemanticSimilarity:
		return "SEMANTIC_SIMILARITY"
	case embed.EmbedTaskClassification:
		return "CLASSIFICATION"
	case embed.EmbedTaskClustering:
		return "CLUSTERING"
	case embed.EmbedTaskCodeRetrievalQuery:
		return "CODE_RETRIEVAL_QUERY"
	case embed.EmbedTaskQuestionAnswering:
		return "QUESTION_ANSWERING"
	case embed.EmbedTaskFactVerification:
		return "FACT_VERIFICATION"
	default:
		return ""
	}
}

// promptPrefix returns the documented gemini-embedding-2 prefix for a
// task. EmbedTaskUnspecified maps to "" (no prefix).
//
// Prefix wording mirrors Google's published guidance:
//
//	RETRIEVAL_DOCUMENT     "title: none | text: "
//	RETRIEVAL_QUERY        "task: search result | query: "
//	SEMANTIC_SIMILARITY    "task: sentence similarity | query: "
//	CLASSIFICATION         "task: classification | query: "
//
// Other tasks fall through to "" (no prefix) — they're not used by
// memmy today and Google hasn't published a canonical prefix for
// them on gemini-embedding-2.
func promptPrefix(task embed.EmbedTask) string {
	switch task {
	case embed.EmbedTaskRetrievalDocument:
		// "title: none" because memmy doesn't carry a title alongside
		// each chunk; the doc explicitly recommends "title: none"
		// when no title exists.
		return "title: none | text: "
	case embed.EmbedTaskRetrievalQuery:
		return "task: search result | query: "
	case embed.EmbedTaskSemanticSimilarity:
		return "task: sentence similarity | query: "
	case embed.EmbedTaskClassification:
		return "task: classification | query: "
	default:
		return ""
	}
}

func int32Ptr(v int32) *int32 { return &v }
