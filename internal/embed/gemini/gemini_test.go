package gemini_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/Cidan/memmy/internal/embed"
	"github.com/Cidan/memmy/internal/embed/gemini"
)

// TestGemini_Live exercises the real Gemini API. It is skipped unless
// GEMINI_API_KEY is set, so unattended CI runs do not require credentials.
func TestGemini_Live(t *testing.T) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live Gemini test")
	}
	model := os.Getenv("GEMINI_EMBED_MODEL")
	if model == "" {
		model = "text-embedding-004"
	}
	dim := 768
	if d := os.Getenv("GEMINI_EMBED_DIM"); d != "" {
		// Allow callers to supply an alternate dim if they configure a
		// different model via env.
		_, err := fmt.Sscanf(d, "%d", &dim)
		if err != nil {
			t.Fatalf("invalid GEMINI_EMBED_DIM: %v", err)
		}
	}

	ctx := context.Background()
	e, err := gemini.New(ctx, gemini.Options{
		APIKey: key,
		Model:  model,
		Dim:    dim,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := e.Embed(ctx, embed.EmbedTaskRetrievalDocument, []string{"hello world", "another test sentence"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2", len(out))
	}
	for i, v := range out {
		if len(v) != dim {
			t.Errorf("vec %d: dim=%d, want %d", i, len(v), dim)
		}
	}
}
