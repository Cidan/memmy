package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/spf13/cobra"

	"github.com/Cidan/memmy"
	"github.com/Cidan/memmy/internal/embed"
	"github.com/Cidan/memmy/internal/embed/fake"
	"github.com/Cidan/memmy/internal/eval/dataset"
	"github.com/Cidan/memmy/internal/eval/harness"
	"github.com/Cidan/memmy/internal/eval/manifest"
)

const defaultGeminiModel = "gemini-embedding-2"
const defaultGeminiDim = 768

func newIngestCmd() *cobra.Command {
	var (
		sessionsPath string
		datasetName  string
		embedderKind string
		geminiModel  string
		geminiDim    int
		fakeDim      int
		limit        int
	)
	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Extract Claude Code JSONL into a dataset, embedding new chunks",
		Example: `  memmy-eval ingest --sessions ~/.claude/projects/-home-foo --dataset alpha --embedder fake
  memmy-eval ingest --sessions session.jsonl --dataset alpha --embedder gemini`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if sessionsPath == "" || datasetName == "" {
				return errors.New("--sessions and --dataset are required")
			}
			ctx := cmd.Context()

			ds, err := dataset.Open("", datasetName)
			if err != nil {
				return err
			}

			emb, modelID, kind, err := buildEmbedder(ctx, embedderKind, geminiModel, geminiDim, fakeDim)
			if err != nil {
				return err
			}

			fileCount, err := preflightFileCount(sessionsPath, limit)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "ingest: %d source file(s) under %q -> dataset %q\n", fileCount, sessionsPath, datasetName)

			pb := newIngestProgress(fileCount)
			res, err := harness.Ingest(ctx, datasetName, harness.IngestOptions{
				SessionsPath:    sessionsPath,
				CorpusStorePath: ds.CorpusDBPath(),
				EmbedCachePath:  ds.CorpusDBPath() + ".embcache",
				Embedder:        emb,
				EmbedderModelID: modelID,
				EmbedderKind:    kind,
				Limit:           limit,
				Progress:        pb,
			})
			if err != nil {
				return fmt.Errorf("ingest: %w", err)
			}

			// Update dataset manifest with the post-ingest snapshot.
			now := time.Now().UTC()
			existing, _ := manifest.ReadDataset(ds.ManifestPath())
			if existing.CreatedAt.IsZero() {
				existing.CreatedAt = now
			}
			existing.Name = datasetName
			existing.SchemaVersion = manifest.SchemaVersion
			existing.SessionsSourcePath = sessionsPath
			existing.EmbedderModel = modelID
			existing.EmbedderDim = emb.Dim()
			existing.EmbedderKind = kind
			existing.ChunkCount = res.ChunksEmbedded + existing.ChunkCount
			existing.CorpusSnapshotHash = res.CorpusSnapshot
			existing.UpdatedAt = now
			if err := manifest.WriteDataset(ds.ManifestPath(), existing); err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr,
				"ingest done: files scanned=%d ingested=%d skipped(dup)=%d turns_added=%d chunks_embedded=%d snapshot=%s\n",
				res.FilesScanned, res.FilesIngested, res.FilesSkippedDup, res.TurnsAdded, res.ChunksEmbedded, res.CorpusSnapshot,
			)
			return nil
		},
	}
	cmd.Flags().StringVar(&sessionsPath, "sessions", "", "path to a session .jsonl file OR a directory of them")
	cmd.Flags().StringVar(&datasetName, "dataset", "", "dataset name (becomes a subdirectory under MEMMY_EVAL_HOME)")
	cmd.Flags().StringVar(&embedderKind, "embedder", "fake", "embedder backend: fake | gemini")
	cmd.Flags().StringVar(&geminiModel, "gemini-model", defaultGeminiModel, "Gemini model name (when --embedder=gemini)")
	cmd.Flags().IntVar(&geminiDim, "gemini-dim", defaultGeminiDim, "Gemini output dimensionality")
	cmd.Flags().IntVar(&fakeDim, "fake-dim", 64, "fake-embedder dimensionality (when --embedder=fake)")
	cmd.Flags().IntVar(&limit, "limit", 0, "cap files processed (0 = no limit); useful for smoke runs")
	return cmd
}

// buildEmbedder picks an embedder + identity based on the --embedder flag.
func buildEmbedder(ctx context.Context, kind, geminiModel string, geminiDim, fakeDim int) (embed.Embedder, string, string, error) {
	switch kind {
	case "fake":
		if fakeDim < 1 {
			return nil, "", "", errors.New("--fake-dim must be >= 1")
		}
		return fake.New(fakeDim), harness.FakeEmbedderModelID(fakeDim), "fake", nil
	case "gemini":
		key := os.Getenv("GEMINI_API_KEY")
		if key == "" {
			return nil, "", "", errors.New("--embedder=gemini requires GEMINI_API_KEY")
		}
		emb, err := memmy.NewGeminiEmbedder(ctx, memmy.GeminiEmbedderOptions{
			APIKey: key,
			Model:  geminiModel,
			Dim:    geminiDim,
		})
		if err != nil {
			return nil, "", "", fmt.Errorf("gemini embedder: %w", err)
		}
		return emb, fmt.Sprintf("%s/%d", geminiModel, geminiDim), "gemini", nil
	default:
		return nil, "", "", fmt.Errorf("unknown --embedder=%q (want: fake|gemini)", kind)
	}
}

// preflightFileCount counts the source files ingest will process so
// the progress bar can size itself before harness.Ingest streams.
func preflightFileCount(sessions string, limit int) (int, error) {
	st, err := os.Stat(sessions)
	if err != nil {
		return 0, fmt.Errorf("stat: %w", err)
	}
	count := 1
	if st.IsDir() {
		entries, err := os.ReadDir(sessions)
		if err != nil {
			return 0, err
		}
		count = 0
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				count++
			}
		}
	}
	if limit > 0 && count > limit {
		count = limit
	}
	return count, nil
}

// ingestProgress wires schollz/progressbar around the Progress callbacks
// the harness emits. We render two bars: per-file, and per-chunk
// (initialized lazily once the harness reports its first chunk batch).
type ingestProgress struct {
	files  *progressbar.ProgressBar
	chunks *progressbar.ProgressBar
}

func newIngestProgress(totalFiles int) *ingestProgress {
	return &ingestProgress{
		files: progressbar.NewOptions(totalFiles,
			progressbar.OptionSetDescription("files"),
			progressbar.OptionShowCount(),
			progressbar.OptionSetWidth(30),
			progressbar.OptionSetWriter(os.Stderr),
		),
	}
}

func (p *ingestProgress) StartFiles(total int) {
	if p.files != nil {
		p.files.ChangeMax(total)
	}
}

func (p *ingestProgress) FileDone(_ string, _ int) {
	if p.files != nil {
		_ = p.files.Add(1)
	}
}

func (p *ingestProgress) StartChunks(total int) {
	if p.chunks == nil {
		p.chunks = progressbar.NewOptions(total,
			progressbar.OptionSetDescription("chunks"),
			progressbar.OptionShowCount(),
			progressbar.OptionSetWidth(30),
			progressbar.OptionSetWriter(os.Stderr),
		)
	} else {
		p.chunks.ChangeMax(total)
	}
}

func (p *ingestProgress) ChunkDone(n int) {
	if p.chunks == nil {
		p.chunks = progressbar.NewOptions(-1,
			progressbar.OptionSetDescription("chunks"),
			progressbar.OptionShowCount(),
			progressbar.OptionSpinnerType(14),
			progressbar.OptionSetWriter(os.Stderr),
		)
	}
	_ = p.chunks.Add(n)
}

func (p *ingestProgress) Finish() {
	if p.files != nil {
		_ = p.files.Finish()
	}
	if p.chunks != nil {
		_ = p.chunks.Finish()
	}
	fmt.Fprintln(os.Stderr)
}
