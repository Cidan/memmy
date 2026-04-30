package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/Cidan/memmy/internal/eval/inspect"
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
			cfg, err := loadServiceConfig(configPath)
			if err != nil {
				return err
			}
			conn, err := neo4jConnFromEnv()
			if err != nil {
				return err
			}
			runID := newRunID()
			out, err := executeRun(ctx, runID, datasetName, configPath, emb, modelID, cfg, conn, runOptions{
				K: k, Hops: hops, Oversample: oversample,
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
	return cmd
}

// runOptions bundles the CLI knobs that don't belong in ServiceConfig.
type runOptions struct {
	K          int
	Hops       int
	Oversample int
}

// runOutput is what executeRun returns to its caller (run + sweep).
type runOutput struct {
	OutDir   string
	Summary  metrics.Summary
	RunID    string
	Manifest manifest.RunManifest
}

// executeRun is the shared body used by both `run` and `sweep`. It
// always replays from the corpus into a fresh per-tenant Neo4j state
// (the runID is folded into the tenant tuple so reruns don't collide
// with each other in the shared Neo4j db).
func executeRun(
	ctx context.Context,
	runID string,
	datasetName string,
	configPath string,
	emb embedderHandle,
	modelID string,
	cfg memmy.ServiceConfig,
	conn inspect.Connection,
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
	if err := cs.IterateTurns(ctx, func(st corpus.StoredTurn) error {
		turnByText[st.Text] = st.UUID
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
	tenant := map[string]string{
		"agent":   "memmy-eval",
		"dataset": datasetName,
		"run":     runID,
	}
	replayOpts := harness.ReplayOptions{
		CorpusStorePath: ds.CorpusDBPath(),
		EmbedCachePath:  ds.CorpusDBPath() + ".embcache",
		Embedder:        emb,
		EmbedderModelID: modelID,
		ServiceConfig:   &cfgCopy,
		DatasetName:     datasetName,
		Neo4j:           conn,
		TenantTuple:     tenant,
	}

	fmt.Fprintf(os.Stderr, "run: replaying corpus into Neo4j tenant %v\n", tenant)
	replay, err := harness.Replay(ctx, replayOpts)
	if err != nil {
		return runOutput{}, fmt.Errorf("replay: %w", err)
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
		InspectConn:  conn,
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
	rm := manifest.RunManifest{
		SchemaVersion:      manifest.SchemaVersion,
		RunID:              runID,
		DatasetName:        datasetName,
		StartedAt:          startedAt,
		FinishedAt:         time.Now().UTC(),
		MemmyGitSHA:        manifest.MemmyGitSHA(),
		ConfigPath:         configPath,
		ServiceConfigJSON:  cfgRaw,
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
// the resolved ServiceConfig. When path is empty the memmy defaults
// are returned.
func loadServiceConfig(path string) (memmy.ServiceConfig, error) {
	cfg := memmy.DefaultServiceConfig()
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	var doc struct {
		Service map[string]any `yaml:"service"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	cfg, err = sweep.ApplyServiceOverrides(cfg, doc.Service)
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}

// neo4jConnFromEnv resolves the Neo4j connection from the same env
// vars the storage test helper uses (NEO4J_URI / NEO4J_USER /
// NEO4J_PASSWORD / NEO4J_DATABASE). Password is required; everything
// else falls back to localhost defaults.
func neo4jConnFromEnv() (inspect.Connection, error) {
	pw := os.Getenv("NEO4J_PASSWORD")
	if pw == "" {
		return inspect.Connection{}, errors.New("memmy-eval: NEO4J_PASSWORD env required")
	}
	return inspect.Connection{
		URI:      envOr("NEO4J_URI", "bolt://localhost:7687"),
		User:     envOr("NEO4J_USER", "neo4j"),
		Password: pw,
		Database: envOr("NEO4J_DATABASE", "neo4j"),
	}, nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func newRunID() string {
	id, err := ulid.New(uint64(time.Now().UTC().UnixMilli()), ulid.Monotonic(rand.Reader, 0))
	if err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + id.String()
}
