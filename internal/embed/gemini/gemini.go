// Package gemini implements the Embedder interface against Google's
// Gemini embeddings API via go-genai (google.golang.org/genai).
//
// The dimensionality is fixed by the chosen model (e.g.,
// "text-embedding-004" returns 768-dim vectors). Memmy does not normalize
// here — the caller (service layer) normalizes at index/query boundaries.
//
// Live tests are gated behind GEMINI_API_KEY so they only run when the
// developer has supplied a key.
package gemini

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/genai"
)

// Embedder is a Gemini-backed embedder.
type Embedder struct {
	client *genai.Client
	model  string
	dim    int
}

// Options configures the Gemini embedder.
type Options struct {
	// APIKey for the Gemini Developer API. Required.
	APIKey string
	// Model is the embedding model name (e.g., "text-embedding-004").
	Model string
	// Dim is the dimensionality the chosen Model returns. Required so
	// the rest of memmy (HNSW + storage) can be configured at startup.
	Dim int
}

// New constructs a Gemini Embedder. The client is created up front so
// repeated Embed calls reuse one HTTP client.
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
	return &Embedder{client: c, model: opts.Model, dim: opts.Dim}, nil
}

// Dim returns the configured dimensionality.
func (e *Embedder) Dim() int { return e.dim }

// Embed batches all texts in a single API call. Returned vectors are
// NOT normalized; the caller normalizes at index/query boundaries.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	contents := make([]*genai.Content, len(texts))
	for i, t := range texts {
		contents[i] = genai.NewContentFromText(t, genai.RoleUser)
	}
	resp, err := e.client.Models.EmbedContent(ctx, e.model, contents, nil)
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
