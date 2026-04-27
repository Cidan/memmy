// Package embed defines the Embedder interface and concrete implementations
// in subpackages. See DESIGN.md §9.2.
package embed

import "context"

// Embedder produces vectors for input text. Returned vectors are NOT
// normalized — callers normalize at index/query boundaries.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
}
