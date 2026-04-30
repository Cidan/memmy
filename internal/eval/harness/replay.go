package harness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Cidan/memmy"
	"github.com/Cidan/memmy/internal/embed"
	"github.com/Cidan/memmy/internal/eval/corpus"
	"github.com/Cidan/memmy/internal/eval/embedcache"
)

// ReplayOptions configures one Replay call.
type ReplayOptions struct {
	// CorpusStorePath: source of turns (chronological).
	CorpusStorePath string
	// EmbedCachePath: cache primed by Ingest. Replay's embedder consults
	// this cache so re-running with the same corpus does not re-embed.
	EmbedCachePath string
	// MemmyDBPath: where to materialize the per-run memmy SQLite db.
	MemmyDBPath string
	// Embedder produces vectors via the cache wrapper.
	Embedder embed.Embedder
	// EmbedderModelID is the cache key namespace.
	EmbedderModelID string
	// ServiceConfig is the memmy service tunable bundle. Optional;
	// nil means defaults.
	ServiceConfig *memmy.ServiceConfig
	// HNSW configures the per-tenant index. Optional; nil means defaults.
	HNSW *memmy.HNSWConfig
	// FlatScanThreshold below which Recall uses linear scan. 0 = default.
	FlatScanThreshold int
	// HNSWRandSeed seeds the HNSW layer-assignment RNG. 0 = time-derived.
	HNSWRandSeed uint64
	// TenantTuple identifies the synthetic tenant memmy will write under.
	// Defaults to {agent: memmy-eval, dataset: <DatasetName>}.
	TenantTuple map[string]string
	// DatasetName is used as the default dataset key in TenantTuple.
	DatasetName string
}

// ReplayResult exposes the live service so a caller can run queries
// without reopening. Close releases the underlying SQLite handle.
type ReplayResult struct {
	Service       memmy.Service
	Closer        interface{ Close() error }
	Tenant        map[string]string
	FakeClock     *memmy.FakeClock
	TurnsReplayed int
	NodesWritten  int
	StartedAt     time.Time
	FinishedAt    time.Time
}

// Close releases the underlying memmy storage handle.
func (r *ReplayResult) Close() error {
	if r == nil || r.Closer == nil {
		return nil
	}
	return r.Closer.Close()
}

// OpenService opens a memmy service backed by opts.MemmyDBPath plus
// an embedcache wrapper around opts.Embedder. Used both by Replay (to
// build a fresh db from corpus) and by query-only paths that copied a
// cached baseline db into place. clockSeed is what the FakeClock is
// initialized to; pass the corpus's first-turn timestamp for fresh
// replays or the corpus's last-turn timestamp for query-only opens
// (so decay math sees a clock that's at-or-after every Node's
// LastTouched).
func OpenService(opts ReplayOptions, clockSeed time.Time) (*ReplayResult, error) {
	if opts.MemmyDBPath == "" {
		return nil, errors.New("harness: MemmyDBPath required")
	}
	if opts.Embedder == nil {
		return nil, errors.New("harness: Embedder required")
	}
	if opts.EmbedderModelID == "" {
		return nil, errors.New("harness: EmbedderModelID required")
	}
	if opts.DatasetName == "" {
		opts.DatasetName = "default"
	}
	tenant := opts.TenantTuple
	if len(tenant) == 0 {
		tenant = map[string]string{"agent": "memmy-eval", "dataset": opts.DatasetName}
	}

	cache, err := embedcache.Open(opts.EmbedCachePath)
	if err != nil {
		return nil, err
	}
	wrappedEmbedder := newCachingEmbedder(opts.Embedder, cache, opts.EmbedderModelID)

	if clockSeed.IsZero() {
		clockSeed = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	cl := memmy.NewFakeClock(clockSeed)

	svc, closer, err := memmy.Open(memmy.Options{
		DBPath:            opts.MemmyDBPath,
		Embedder:          wrappedEmbedder,
		Clock:             cl,
		ServiceConfig:     opts.ServiceConfig,
		HNSW:              opts.HNSW,
		FlatScanThreshold: opts.FlatScanThreshold,
		HNSWRandSeed:      opts.HNSWRandSeed,
	})
	if err != nil {
		_ = cache.Close()
		return nil, fmt.Errorf("harness: memmy.Open: %w", err)
	}
	return &ReplayResult{
		Service:   svc,
		Closer:    multiCloser{closer, cache},
		Tenant:    tenant,
		FakeClock: cl,
		StartedAt: time.Now().UTC(),
	}, nil
}

// Replay reads every turn from the corpus store in chronological
// order, advances a FakeClock to each turn's timestamp, and calls
// memmy.Service.Write under the configured tenant. The FakeClock
// drives the Service's decay math so that decay/reinforcement
// dynamics observed in the resulting db reflect the real time gaps
// between turns rather than wall-clock latency of the harness.
func Replay(ctx context.Context, opts ReplayOptions) (*ReplayResult, error) {
	if opts.CorpusStorePath == "" {
		return nil, errors.New("harness: CorpusStorePath required")
	}
	store, err := corpus.OpenStore(opts.CorpusStorePath)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	firstTurnTime, err := earliestTurnTime(ctx, store)
	if err != nil {
		return nil, err
	}
	clockSeed := firstTurnTime
	if !clockSeed.IsZero() {
		clockSeed = clockSeed.Add(-time.Second)
	}
	res, err := OpenService(opts, clockSeed)
	if err != nil {
		return nil, err
	}

	if err := store.IterateTurns(ctx, func(t corpus.StoredTurn) error {
		if !t.Timestamp.IsZero() {
			res.FakeClock.Set(t.Timestamp)
		} else {
			res.FakeClock.Advance(time.Second)
		}
		w, werr := res.Service.Write(ctx, memmy.WriteRequest{
			Tenant:  res.Tenant,
			Message: t.Text,
			Metadata: map[string]string{
				"turn_uuid":  t.UUID,
				"session_id": t.SessionID,
				"role":       t.Role,
			},
		})
		if werr != nil {
			return fmt.Errorf("harness: replay turn %s: %w", t.UUID, werr)
		}
		res.TurnsReplayed++
		res.NodesWritten += len(w.NodeIDs)
		return nil
	}); err != nil {
		_ = res.Close()
		return nil, err
	}
	res.FinishedAt = time.Now().UTC()
	return res, nil
}

// LastTurnTime returns the latest turn timestamp in the corpus store
// at path. Used by callers that opened a primed memmy db without
// running Replay; they need to seed the FakeClock so subsequent
// Recall sees a clock at-or-after every persisted LastTouched.
func LastTurnTime(ctx context.Context, corpusStorePath string) (time.Time, error) {
	store, err := corpus.OpenStore(corpusStorePath)
	if err != nil {
		return time.Time{}, err
	}
	defer store.Close()
	var last time.Time
	if err := store.IterateTurns(ctx, func(t corpus.StoredTurn) error {
		if t.Timestamp.After(last) {
			last = t.Timestamp
		}
		return nil
	}); err != nil {
		return time.Time{}, err
	}
	return last, nil
}

// earliestTurnTime peeks the corpus to find the chronologically first
// turn so the FakeClock starts there. Empty corpus yields zero time.
func earliestTurnTime(ctx context.Context, store *corpus.Store) (time.Time, error) {
	var first time.Time
	err := store.IterateTurns(ctx, func(t corpus.StoredTurn) error {
		first = t.Timestamp
		return errStopIter
	})
	if err != nil && !errors.Is(err, errStopIter) {
		return time.Time{}, err
	}
	return first, nil
}

var errStopIter = errors.New("stop iter")

// cachingEmbedder consults the embedcache before delegating to the
// wrapped embedder. Memmy chunks during Write, so the embedder it
// calls receives chunked text that matches what Ingest already cached.
type cachingEmbedder struct {
	inner   embed.Embedder
	cache   *embedcache.Cache
	modelID string
}

func newCachingEmbedder(inner embed.Embedder, cache *embedcache.Cache, modelID string) *cachingEmbedder {
	return &cachingEmbedder{inner: inner, cache: cache, modelID: modelID}
}

func (c *cachingEmbedder) Dim() int { return c.inner.Dim() }

func (c *cachingEmbedder) Embed(ctx context.Context, task embed.EmbedTask, texts []string) ([][]float32, error) {
	return c.cache.EmbedBatch(ctx, c.inner, c.modelID, task, texts)
}

// multiCloser closes a slice of io.Closers in order, returning the
// first error.
type multiCloser []interface{ Close() error }

func (m multiCloser) Close() error {
	var first error
	for _, c := range m {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

