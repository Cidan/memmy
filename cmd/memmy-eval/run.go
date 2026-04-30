package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/Cidan/memmy"
	"github.com/Cidan/memmy/internal/embed"
	"github.com/Cidan/memmy/internal/eval/corpus"
	"github.com/Cidan/memmy/internal/eval/dataset"
	"github.com/Cidan/memmy/internal/eval/embedcache"
	"github.com/Cidan/memmy/internal/eval/harness"
	"github.com/Cidan/memmy/internal/eval/manifest"
	"github.com/Cidan/memmy/internal/eval/metrics"
	"github.com/Cidan/memmy/internal/eval/queries"
	"github.com/Cidan/memmy/internal/eval/sweep"
)

func newRunCmd() *cobra.Command {
	var (
		datasetName  string
		configPath   string
		embedderKind string
		geminiModel  string
		geminiDim    int
		fakeDim      int
		k            int
		hops         int
		oversample   int
		seed         uint64
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Replay a dataset into a fresh memmy db, run the query battery, write metrics",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if datasetName == "" {
				return errors.New("--dataset is required")
			}
			ctx := cmd.Context()
			emb, modelID, _, err := buildEmbedder(ctx, embedderKind, geminiModel, geminiDim, fakeDim)
			if err != nil {
				return err
			}
			cfg, hnsw, err := loadServiceConfig(configPath)
			if err != nil {
				return err
			}
			runID := newRunID()
			out, err := executeRun(ctx, runID, datasetName, configPath, emb, modelID, cfg, hnsw, runOptions{
				K: k, Hops: hops, Oversample: oversample, Seed: seed,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "run done: id=%s queries=%d recall@5=%.3f reinforce_mean=%.4f outDir=%s\n",
				runID, out.Summary.QueriesExecuted, out.Summary.OverallRecallAt5,
				out.Summary.OverallReinforcementMean, out.OutDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&datasetName, "dataset", "", "dataset name")
	cmd.Flags().StringVar(&configPath, "config", "", "path to a YAML service config (optional; defaults applied if absent)")
	cmd.Flags().StringVar(&embedderKind, "embedder", "fake", "embedder backend: fake | gemini")
	cmd.Flags().StringVar(&geminiModel, "gemini-model", defaultGeminiModel, "")
	cmd.Flags().IntVar(&geminiDim, "gemini-dim", defaultGeminiDim, "")
	cmd.Flags().IntVar(&fakeDim, "fake-dim", 64, "")
	cmd.Flags().IntVar(&k, "k", 8, "queries returned per Recall")
	cmd.Flags().IntVar(&hops, "hops", 1, "graph expansion hops")
	cmd.Flags().IntVar(&oversample, "oversample", 0, "vector-search oversample (0 = service default)")
	cmd.Flags().Uint64Var(&seed, "hnsw-seed", 42, "HNSW layer-assignment RNG seed (test determinism)")
	return cmd
}

// runOptions bundles the CLI knobs that don't belong in ServiceConfig.
type runOptions struct {
	K          int
	Hops       int
	Oversample int
	Seed       uint64
}

// runOutput is what executeRun returns to its caller (run + sweep).
type runOutput struct {
	OutDir   string
	Summary  metrics.Summary
	RunID    string
	Manifest manifest.RunManifest
}

// executeRun is the shared body used by both `run` and `sweep`.
func executeRun(
	ctx context.Context,
	runID string,
	datasetName string,
	configPath string,
	emb embedderHandle,
	modelID string,
	cfg memmy.ServiceConfig,
	hnsw memmy.HNSWConfig,
	opts runOptions,
) (runOutput, error) {
	ds, err := dataset.Open("", datasetName)
	if err != nil {
		return runOutput{}, err
	}
	outDir, err := ds.RunDir(runID)
	if err != nil {
		return runOutput{}, err
	}
	memmyDB := filepath.Join(outDir, "memmy.db")
	startedAt := time.Now().UTC()

	cs, err := corpus.OpenStore(ds.CorpusDBPath())
	if err != nil {
		return runOutput{}, err
	}
	defer cs.Close()
	corpusHash, err := cs.SnapshotHash(ctx)
	if err != nil {
		return runOutput{}, err
	}
	turnByText := map[string]string{}
	var lastTurnTime time.Time
	if err := cs.IterateTurns(ctx, func(st corpus.StoredTurn) error {
		turnByText[st.Text] = st.UUID
		if st.Timestamp.After(lastTurnTime) {
			lastTurnTime = st.Timestamp
		}
		return nil
	}); err != nil {
		return runOutput{}, err
	}
	resolveByHitText := func(hitText string) string {
		for original, uuid := range turnByText {
			if strings.Contains(original, hitText) {
				return uuid
			}
		}
		return ""
	}

	cfgCopy := cfg
	hnswCopy := hnsw
	replayOpts := harness.ReplayOptions{
		CorpusStorePath: ds.CorpusDBPath(),
		EmbedCachePath:  ds.CorpusDBPath() + ".embcache",
		MemmyDBPath:     memmyDB,
		Embedder:        emb,
		EmbedderModelID: modelID,
		ServiceConfig:   &cfgCopy,
		HNSW:            &hnswCopy,
		HNSWRandSeed:    opts.Seed,
		DatasetName:     datasetName,
	}

	bkey := baselineKey(corpusHash, modelID, emb.Dim(), cfg, hnsw, opts.Seed)
	baselinePath := filepath.Join(ds.Root, "baselines", bkey+".sqlite")
	cacheHit := false

	if _, err := os.Stat(baselinePath); err == nil {
		if err := copyFile(baselinePath, memmyDB); err != nil {
			return runOutput{}, fmt.Errorf("copy baseline: %w", err)
		}
		cacheHit = true
		fmt.Fprintf(os.Stderr, "run: reusing primed memmy db from baselines/%s.sqlite\n", bkey)
	}

	var replay *harness.ReplayResult
	if cacheHit {
		replay, err = harness.OpenService(replayOpts, lastTurnTime)
		if err != nil {
			return runOutput{}, fmt.Errorf("open primed: %w", err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "run: replaying corpus into fresh memmy db (no baseline for key %s)\n", bkey)
		replay, err = harness.Replay(ctx, replayOpts)
		if err != nil {
			return runOutput{}, fmt.Errorf("replay: %w", err)
		}
		// Snapshot the post-replay db as the baseline for future runs
		// with the same (corpus, embedder, write-time config) tuple.
		// Must close first so SQLite checkpoints WAL into the main file.
		if err := replay.Close(); err != nil {
			return runOutput{}, fmt.Errorf("close after replay: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(baselinePath), 0o700); err != nil {
			return runOutput{}, fmt.Errorf("mkdir baselines: %w", err)
		}
		if err := copyFile(memmyDB, baselinePath); err != nil {
			return runOutput{}, fmt.Errorf("save baseline: %w", err)
		}
		// Re-open the same db (now-quiescent) for queries.
		replay, err = harness.OpenService(replayOpts, lastTurnTime)
		if err != nil {
			return runOutput{}, fmt.Errorf("reopen after baseline save: %w", err)
		}
	}
	defer replay.Close()

	qstore, err := queries.OpenStore(ds.QueriesDBPath())
	if err != nil {
		return runOutput{}, err
	}
	defer qstore.Close()
	all, err := qstore.All(ctx)
	if err != nil {
		return runOutput{}, err
	}
	if len(all) == 0 {
		return runOutput{}, errors.New("no queries in dataset; run `memmy-eval queries` first")
	}

	if err := prewarmQueryEmbeddings(ctx, qstore, ds, emb, modelID, all); err != nil {
		return runOutput{}, err
	}

	results, err := harness.RunQueries(ctx, all, harness.RunQueriesOptions{
		Service:      replay.Service,
		Tenant:       replay.Tenant,
		InspectPath:  memmyDB,
		K:            opts.K,
		Hops:         opts.Hops,
		OversampleN:  opts.Oversample,
		FakeClock:    replay.FakeClock,
		AdvanceClock: 2 * time.Minute,
	})
	if err != nil {
		return runOutput{}, err
	}

	rows := make([]metrics.QueryRow, 0, len(results))
	for _, qr := range results {
		row := metrics.Compute(qr, func(nodeID string) string {
			for _, h := range qr.Hits {
				if h.NodeID == nodeID {
					return resolveByHitText(h.Text)
				}
			}
			return ""
		})
		rows = append(rows, row)
	}
	summary := metrics.Aggregate(runID, datasetName, rows)
	if err := metrics.WriteRun(outDir, rows, summary); err != nil {
		return runOutput{}, err
	}

	cfgRaw, _ := json.Marshal(cfg)
	hnswRaw, _ := json.Marshal(hnsw)
	rm := manifest.RunManifest{
		SchemaVersion:      manifest.SchemaVersion,
		RunID:              runID,
		DatasetName:        datasetName,
		StartedAt:          startedAt,
		FinishedAt:         time.Now().UTC(),
		MemmyGitSHA:        manifest.MemmyGitSHA(),
		ConfigPath:         configPath,
		ServiceConfigJSON:  cfgRaw,
		HNSWConfigJSON:     hnswRaw,
		HNSWRandSeed:       opts.Seed,
		QueriesExecuted:    len(results),
		CorpusSnapshotHash: corpusHash,
	}
	if err := manifest.WriteRun(filepath.Join(outDir, "manifest.json"), rm); err != nil {
		return runOutput{}, err
	}
	return runOutput{OutDir: outDir, Summary: summary, RunID: runID, Manifest: rm}, nil
}

// embedderHandle is the embedder type used by executeRun. Aliased so
// we can swap the underlying interface without rewriting call sites.
type embedderHandle = interface {
	Dim() int
	Embed(ctx context.Context, task memmy.EmbedTask, texts []string) ([][]float32, error)
}

// baselineKey hashes everything that affects memmy.db contents after
// Replay so two configs that produce identical primed databases share
// a single cached baseline. Decay/reinforcement params (NodeLambda,
// NodeDelta, etc.) ARE included even though they don't affect Write
// today, because our threshold for "is this safe to cache" is
// strictly correctness, not maximum hit rate.
func baselineKey(corpusHash, modelID string, dim int, cfg memmy.ServiceConfig, hnsw memmy.HNSWConfig, seed uint64) string {
	cfgRaw, _ := json.Marshal(cfg)
	hnswRaw, _ := json.Marshal(hnsw)
	h := sha256.New()
	fmt.Fprintf(h, "v1\x00%s\x00%s\x00%d\x00%d\x00", corpusHash, modelID, dim, seed)
	_, _ = h.Write(cfgRaw)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(hnswRaw)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// copyFile is a simple stream copy from src to dst (overwriting dst).
// We use it to seed runs/<id>/memmy.db from a cached baseline and to
// snapshot a freshly-replayed memmy.db into the baseline cache.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// prewarmQueryEmbeddings batches every query text through the embed
// cache before the query battery runs. Without this, memmy's Recall
// would call the embedder once per query inline (947 serial Gemini
// RPCs ~= 16 minutes); with it, we make max(ceil(N/100), 1) batched
// calls instead. Each query's vector is also persisted into
// queries.sqlite so future re-runs are entirely free of API calls.
func prewarmQueryEmbeddings(
	ctx context.Context,
	qstore *queries.Store,
	ds *dataset.Dataset,
	emb embedderHandle,
	modelID string,
	all []queries.LabeledQuery,
) error {
	cache, err := embedcache.Open(ds.CorpusDBPath() + ".embcache")
	if err != nil {
		return fmt.Errorf("prewarm: open cache: %w", err)
	}
	defer cache.Close()

	texts := make([]string, len(all))
	for i, q := range all {
		texts[i] = q.Text
	}
	vecs, err := cache.EmbedBatch(ctx, emb, modelID, embed.EmbedTaskRetrievalQuery, texts)
	if err != nil {
		return fmt.Errorf("prewarm: embed: %w", err)
	}
	for i, q := range all {
		if err := qstore.PutEmbedding(ctx, q.ID, vecs[i]); err != nil {
			return fmt.Errorf("prewarm: persist embedding for %s: %w", q.ID, err)
		}
	}
	return nil
}

// loadServiceConfig reads a YAML config file (optional) and returns
// the resolved ServiceConfig + HNSWConfig. When path is empty the
// memmy defaults are returned.
func loadServiceConfig(path string) (memmy.ServiceConfig, memmy.HNSWConfig, error) {
	cfg := memmy.DefaultServiceConfig()
	hnsw := memmy.DefaultHNSWConfig()
	if path == "" {
		return cfg, hnsw, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, hnsw, fmt.Errorf("read config: %w", err)
	}
	var doc struct {
		Service map[string]any `yaml:"service"`
		HNSW    map[string]any `yaml:"hnsw"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return cfg, hnsw, fmt.Errorf("parse config: %w", err)
	}
	cfg, err = sweep.ApplyServiceOverrides(cfg, doc.Service)
	if err != nil {
		return cfg, hnsw, err
	}
	hnsw, err = sweep.ApplyHNSWOverrides(hnsw, doc.HNSW)
	if err != nil {
		return cfg, hnsw, err
	}
	return cfg, hnsw, nil
}

func newRunID() string {
	id, err := ulid.New(uint64(time.Now().UTC().UnixMilli()), ulid.Monotonic(rand.Reader, 0))
	if err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + id.String()
}
