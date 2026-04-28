package gemini

// White-box unit tests for the task-strategy / prefix helpers. These
// don't hit the network and run on every test invocation (the
// black-box live test in gemini_test.go remains gated behind
// GEMINI_API_KEY).

import (
	"strings"
	"testing"

	"github.com/Cidan/memmy/internal/embed"
)

func TestStrategyFor_DefaultIsPrefix(t *testing.T) {
	for _, m := range []string{
		"gemini-embedding-2",
		"GEMINI-EMBEDDING-2",
		"some-future-model",
		"",
	} {
		if got := strategyFor(m); got != strategyPrefix {
			t.Errorf("strategyFor(%q) = %d, want strategyPrefix (%d)", m, got, strategyPrefix)
		}
	}
}

func TestStrategyFor_KnownParamModels(t *testing.T) {
	for _, m := range []string{
		"gemini-embedding-001",
		"models/gemini-embedding-001",
		"text-embedding-004",
	} {
		if got := strategyFor(m); got != strategyParam {
			t.Errorf("strategyFor(%q) = %d, want strategyParam (%d)", m, got, strategyParam)
		}
	}
}

func TestTaskTypeAPIString_KnownTasks(t *testing.T) {
	cases := map[embed.EmbedTask]string{
		embed.EmbedTaskUnspecified:        "",
		embed.EmbedTaskRetrievalDocument:  "RETRIEVAL_DOCUMENT",
		embed.EmbedTaskRetrievalQuery:     "RETRIEVAL_QUERY",
		embed.EmbedTaskSemanticSimilarity: "SEMANTIC_SIMILARITY",
		embed.EmbedTaskClassification:     "CLASSIFICATION",
		embed.EmbedTaskClustering:         "CLUSTERING",
		embed.EmbedTaskCodeRetrievalQuery: "CODE_RETRIEVAL_QUERY",
		embed.EmbedTaskQuestionAnswering:  "QUESTION_ANSWERING",
		embed.EmbedTaskFactVerification:   "FACT_VERIFICATION",
	}
	for task, want := range cases {
		if got := taskTypeAPIString(task); got != want {
			t.Errorf("taskTypeAPIString(%d) = %q, want %q", task, got, want)
		}
	}
}

// TestPromptPrefix_DocumentedFormats locks the gemini-embedding-2
// prefix wording against Google's published guidance:
//
//	RETRIEVAL_DOCUMENT     "title: none | text: "
//	RETRIEVAL_QUERY        "task: search result | query: "
//	SEMANTIC_SIMILARITY    "task: sentence similarity | query: "
//	CLASSIFICATION         "task: classification | query: "
func TestPromptPrefix_DocumentedFormats(t *testing.T) {
	cases := []struct {
		task   embed.EmbedTask
		prefix string
	}{
		{embed.EmbedTaskRetrievalDocument, "title: none | text: "},
		{embed.EmbedTaskRetrievalQuery, "task: search result | query: "},
		{embed.EmbedTaskSemanticSimilarity, "task: sentence similarity | query: "},
		{embed.EmbedTaskClassification, "task: classification | query: "},
	}
	for _, c := range cases {
		if got := promptPrefix(c.task); got != c.prefix {
			t.Errorf("promptPrefix(%d) = %q, want %q", c.task, got, c.prefix)
		}
	}
	// Unspecified (and unmapped tasks) should produce no prefix so
	// memmy's writes don't sneak unintended task hints into the input.
	if got := promptPrefix(embed.EmbedTaskUnspecified); got != "" {
		t.Errorf("promptPrefix(Unspecified) = %q, want empty", got)
	}
	if got := promptPrefix(embed.EmbedTaskClustering); got != "" {
		t.Errorf("promptPrefix(Clustering) = %q, want empty (no documented gemini-embedding-2 prefix)", got)
	}
}

// TestPromptPrefix_DistinguishesQueryAndDocument is the load-bearing
// behavioral assertion: the two retrieval tasks memmy actually emits
// MUST produce different prefixes. If a future refactor accidentally
// collapses them, retrieval quality regresses silently.
func TestPromptPrefix_DistinguishesQueryAndDocument(t *testing.T) {
	doc := promptPrefix(embed.EmbedTaskRetrievalDocument)
	qry := promptPrefix(embed.EmbedTaskRetrievalQuery)
	if doc == qry {
		t.Fatalf("doc and query prefixes must differ; both = %q", doc)
	}
	if !strings.Contains(qry, "query") {
		t.Errorf("query prefix missing 'query': %q", qry)
	}
	if !strings.Contains(doc, "text") {
		t.Errorf("doc prefix missing 'text': %q", doc)
	}
}
