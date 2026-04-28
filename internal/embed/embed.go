// Package embed defines the Embedder interface and concrete implementations
// in subpackages. See DESIGN.md §9.2.
package embed

import "context"

// EmbedTask classifies how the embedded text will be used. Different
// embedding models tune outputs differently for retrieval vs.
// classification vs. similarity, so the caller passes intent and the
// embedder maps it to whatever wire format the underlying model wants
// (a `task_type` API parameter on gemini-embedding-001; an in-band
// prompt prefix on gemini-embedding-2; ignored entirely on the fake
// embedder used in tests).
//
// memmy itself only emits Unspecified, RetrievalDocument, and
// RetrievalQuery today — the other values are reserved for future use
// and listed here so the embedder layer doesn't need to grow when a
// future feature wants them.
type EmbedTask uint8

const (
	EmbedTaskUnspecified EmbedTask = iota
	// EmbedTaskRetrievalDocument is used when persisting a chunk for
	// later retrieval (Service.Write). Tunes the embedding for the
	// "document being searched against" side of an asymmetric pair.
	EmbedTaskRetrievalDocument
	// EmbedTaskRetrievalQuery is used when embedding a recall query
	// (Service.Recall). Tunes the embedding for the "search input" side.
	EmbedTaskRetrievalQuery
	EmbedTaskSemanticSimilarity
	EmbedTaskClassification
	EmbedTaskClustering
	EmbedTaskCodeRetrievalQuery
	EmbedTaskQuestionAnswering
	EmbedTaskFactVerification
)

// Embedder produces vectors for input text. Returned vectors are NOT
// normalized — callers normalize at index/query boundaries.
type Embedder interface {
	Embed(ctx context.Context, task EmbedTask, texts []string) ([][]float32, error)
	Dim() int
}
