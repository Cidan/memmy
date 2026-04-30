// Package harness is the composition layer that turns a Claude Code
// session directory into a memmy store, runs query batteries against
// it, and captures the per-node state changes that result.
//
// Three top-level entry points: Ingest (corpus extraction + embedding
// cache priming), Replay (build a fresh memmy db from the corpus with
// a controllable Clock), and RunQueries (execute a query battery and
// snapshot before/after node state).
package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Cidan/memmy/internal/chunker"
	"github.com/Cidan/memmy/internal/embed"
	"github.com/Cidan/memmy/internal/eval/corpus"
	"github.com/Cidan/memmy/internal/eval/embedcache"
	"github.com/Cidan/memmy/internal/eval/manifest"
)

// Progress is the optional callback Ingest invokes after each file
// and after each chunk batch. Wire to a progressbar in the binary;
// pass nil in tests.
type Progress interface {
	StartFiles(total int)
	FileDone(path string, turns int)
	StartChunks(total int)
	ChunkDone(n int)
	Finish()
}

// IngestOptions configures one Ingest call.
type IngestOptions struct {
	// SessionsPath: file or directory passed to corpus.Extract.
	SessionsPath string
	// CorpusStorePath: where to materialize corpus.sqlite.
	CorpusStorePath string
	// EmbedCachePath: where to materialize the content-addressed cache.
	EmbedCachePath string
	// Embedder produces chunk vectors. Wrap with the Gemini embedder in
	// production; pass fake.New(...) in tests.
	Embedder embed.Embedder
	// EmbedderModelID is the identity stored in the cache key. Bump
	// when the embedder model changes.
	EmbedderModelID string
	// EmbedderKind is "fake" or "gemini" — recorded in the manifest.
	EmbedderKind string
	// Limit caps how many files get walked when SessionsPath is a
	// directory (0 = no cap). Useful for smoke runs.
	Limit int
	// Progress receives per-file and per-chunk updates. Optional.
	Progress Progress
	// Logger is the destination for structured ingest events. Optional.
	Logger *slog.Logger
}

// IngestResult summarizes one Ingest invocation.
type IngestResult struct {
	FilesScanned    int
	FilesIngested   int
	FilesSkippedDup int
	TurnsAdded      int
	ChunksEmbedded  int
	ChunksCacheHit  int
	CorpusSnapshot  string
	Manifest        manifest.DatasetManifest
}

// Ingest walks SessionsPath, extracts turns, persists them to the
// corpus store, chunks them, and primes the embedding cache. Idempotent
// per source file (skipped on re-run if path+mtime+sha unchanged).
func Ingest(ctx context.Context, datasetName string, opts IngestOptions) (IngestResult, error) {
	if opts.SessionsPath == "" {
		return IngestResult{}, errors.New("harness: SessionsPath required")
	}
	if opts.CorpusStorePath == "" {
		return IngestResult{}, errors.New("harness: CorpusStorePath required")
	}
	if opts.EmbedCachePath == "" {
		return IngestResult{}, errors.New("harness: EmbedCachePath required")
	}
	if opts.Embedder == nil {
		return IngestResult{}, errors.New("harness: Embedder required")
	}
	if opts.EmbedderModelID == "" {
		return IngestResult{}, errors.New("harness: EmbedderModelID required")
	}
	if opts.EmbedderKind == "" {
		opts.EmbedderKind = "unknown"
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	files, err := corpus.ListJSONLFiles(opts.SessionsPath)
	if err != nil {
		return IngestResult{}, err
	}
	if opts.Limit > 0 && len(files) > opts.Limit {
		files = files[:opts.Limit]
	}
	if opts.Progress != nil {
		opts.Progress.StartFiles(len(files))
	}

	store, err := corpus.OpenStore(opts.CorpusStorePath)
	if err != nil {
		return IngestResult{}, err
	}
	defer store.Close()
	cache, err := embedcache.Open(opts.EmbedCachePath)
	if err != nil {
		return IngestResult{}, err
	}
	defer cache.Close()

	res := IngestResult{FilesScanned: len(files)}

	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		hash, size, mtime, herr := corpus.HashFile(f)
		if herr != nil {
			return res, herr
		}
		sf := corpus.SourceFile{
			Path:        f,
			ModTime:     mtime,
			SizeBytes:   size,
			ContentHash: hash,
			IngestedAt:  time.Now().UTC(),
		}
		hit, err := store.HasSourceFile(ctx, sf)
		if err != nil {
			return res, err
		}
		if hit {
			res.FilesSkippedDup++
			if opts.Progress != nil {
				opts.Progress.FileDone(f, 0)
			}
			continue
		}

		fileTurns := 0
		var pendingChunks []string
		flush := func() error {
			if len(pendingChunks) == 0 {
				return nil
			}
			vecs, err := cache.EmbedBatch(ctx, opts.Embedder, opts.EmbedderModelID, embed.EmbedTaskRetrievalDocument, pendingChunks)
			if err != nil {
				return err
			}
			res.ChunksEmbedded += len(vecs)
			if opts.Progress != nil {
				opts.Progress.ChunkDone(len(vecs))
			}
			pendingChunks = pendingChunks[:0]
			return nil
		}
		err = corpus.Extract(f, func(t corpus.Turn) error {
			if err := store.PutTurn(ctx, t); err != nil {
				return err
			}
			fileTurns++
			res.TurnsAdded++
			for _, ch := range chunker.Default(t.Text) {
				pendingChunks = append(pendingChunks, ch.Text)
			}
			if len(pendingChunks) >= 64 {
				return flush()
			}
			return nil
		})
		if err != nil {
			return res, err
		}
		if err := flush(); err != nil {
			return res, err
		}
		if err := store.PutSourceFile(ctx, sf); err != nil {
			return res, err
		}
		res.FilesIngested++
		if opts.Progress != nil {
			opts.Progress.FileDone(f, fileTurns)
		}
		logger.Debug("ingest file", slog.String("path", f), slog.Int("turns", fileTurns))
	}
	if opts.Progress != nil {
		opts.Progress.Finish()
	}

	res.CorpusSnapshot, err = store.SnapshotHash(ctx)
	if err != nil {
		return res, err
	}
	res.Manifest = manifest.DatasetManifest{
		SchemaVersion:      manifest.SchemaVersion,
		Name:               datasetName,
		SessionsSourcePath: opts.SessionsPath,
		EmbedderModel:      opts.EmbedderModelID,
		EmbedderDim:        opts.Embedder.Dim(),
		EmbedderKind:       opts.EmbedderKind,
		ChunkCount:         res.ChunksEmbedded,
		CorpusSnapshotHash: res.CorpusSnapshot,
		UpdatedAt:          time.Now().UTC(),
	}
	return res, nil
}

// FakeEmbedderModelID is the conventional ModelID for cache entries
// produced by internal/embed/fake. Identity-keyed so different fake
// dims do not collide.
func FakeEmbedderModelID(dim int) string {
	h := sha256.Sum256(fmt.Appendf(nil, "fake-embedder-dim-%d", dim))
	return "fake-" + strings.ToLower(hex.EncodeToString(h[:4]))
}
