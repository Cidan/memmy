// Package fake provides a deterministic Embedder for tests.
//
// Every call with the same text returns the same vector. The mapping is
// SHA-256 → []float32 by interpreting consecutive 4-byte chunks as
// uint32 → float in [-1, 1]. With the hash-derived seed it covers the
// vector cleanly enough for similarity-based tests.
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"

	"github.com/Cidan/memmy/internal/embed"
)

// Embedder is a deterministic test embedder.
type Embedder struct {
	dim int
}

// New returns a fake embedder of the requested dimensionality (>= 1).
func New(dim int) *Embedder {
	if dim < 1 {
		dim = 64
	}
	return &Embedder{dim: dim}
}

func (e *Embedder) Dim() int { return e.dim }

// Embed satisfies embed.Embedder. The fake intentionally ignores the
// task parameter — the deterministic hash mapping is task-invariant,
// which keeps service-level tests stable across the task-typed
// rollout. (Production-quality task differentiation lives in the
// gemini embedder.)
func (e *Embedder) Embed(_ context.Context, _ embed.EmbedTask, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.vector(t)
	}
	return out, nil
}

// vector produces a deterministic dim-length vector from text.
// Uses SHA-256 to seed and a counter-based hash chain to fill the vector.
// Components fall in [-1, 1].
func (e *Embedder) vector(text string) []float32 {
	v := make([]float32, e.dim)
	seed := sha256.Sum256([]byte(text))

	// Use a counter to keep producing more bytes deterministically:
	// h_i = sha256(seed || i). Each h_i gives 8 uint32 → 8 components.
	var counterBuf [4]byte
	idx := 0
	for chunk := uint32(0); idx < e.dim; chunk++ {
		binary.LittleEndian.PutUint32(counterBuf[:], chunk)
		h := sha256.New()
		h.Write(seed[:])
		h.Write(counterBuf[:])
		sum := h.Sum(nil)
		for i := 0; i < len(sum) && idx < e.dim; i += 4 {
			u := binary.LittleEndian.Uint32(sum[i : i+4])
			// Map [0, 2^32) → [-1, 1].
			v[idx] = float32(float64(u)/math.MaxUint32*2 - 1)
			idx++
		}
	}
	return v
}
