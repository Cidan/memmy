// Command memmy runs the memmy LLM-memory MCP server.
//
// Wires the configured embedder, storage backend, MemoryService, and
// transport adapters under a suture supervisor (DESIGN.md §11).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thejerf/suture/v4"

	"github.com/Cidan/memmy/internal/clock"
	"github.com/Cidan/memmy/internal/config"
	"github.com/Cidan/memmy/internal/embed"
	"github.com/Cidan/memmy/internal/embed/fake"
	"github.com/Cidan/memmy/internal/embed/gemini"
	"github.com/Cidan/memmy/internal/service"
	bboltstore "github.com/Cidan/memmy/internal/storage/bbolt"
	mcpadapter "github.com/Cidan/memmy/internal/transport/mcp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "memmy:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "memmy.yaml", "path to YAML configuration file")
	flag.Parse()

	// Logs always go to stderr — in stdio mode stdout is reserved for
	// the MCP JSON-RPC frame stream.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger.Info("config loaded", "path", *configPath, "storage_backend", cfg.Storage.Backend, "embedder_backend", cfg.Embedder.Backend)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	storage, err := openStorage(cfg)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer storage.Close()

	embedder, err := buildEmbedder(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build embedder: %w", err)
	}

	if cfg.EmbedderDim() != storage.Dim() {
		return fmt.Errorf("embedder dim (%d) does not match storage dim (%d)", cfg.EmbedderDim(), storage.Dim())
	}

	svc, err := service.New(
		storage.Graph(),
		storage.VectorIndex(),
		embedder,
		clock.Real{},
		serviceConfigFromYAML(cfg),
	)
	if err != nil {
		return fmt.Errorf("build service: %w", err)
	}

	// Stdio mode is mutually exclusive with every other transport
	// (config.Validate enforces this). Run the MCP server directly on
	// stdin/stdout without a suture supervisor — the foreground call
	// blocks until ctx is cancelled (signal) or stdin closes.
	if t, ok := cfg.Server.Transports[config.TransportStdio]; ok && t.Enabled {
		logger.Info("memmy starting (stdio transport)")
		adapter := mcpadapter.New(svc)
		err := adapter.RunStdio(ctx)
		logger.Info("memmy stopped")
		// EOF on stdin is the normal "host is done" signal in stdio
		// mode; treat it (and ctx.Canceled) as graceful exit.
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
			return fmt.Errorf("stdio transport: %w", err)
		}
		return nil
	}

	supervisor := suture.New("memmy", suture.Spec{
		EventHook: func(ev suture.Event) {
			logger.Warn("supervisor event", "event", ev.String())
		},
	})

	for name, t := range cfg.Server.Transports {
		if !t.Enabled {
			continue
		}
		switch name {
		case config.TransportMCP:
			supervisor.Add(&mcpService{
				adapter: mcpadapter.New(svc),
				addr:    t.Addr,
				log:     logger,
			})
		default:
			logger.Warn("unsupported transport (skipping)", "name", name)
		}
	}

	logger.Info("memmy starting")
	if err := supervisor.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("supervisor: %w", err)
	}
	logger.Info("memmy stopped")
	return nil
}

// ----- storage -----

func openStorage(cfg config.Config) (*bboltstore.Storage, error) {
	switch cfg.Storage.Backend {
	case "bbolt":
		return bboltstore.Open(bboltstore.Options{
			Path: cfg.Storage.BBolt.Path,
			Dim:  cfg.EmbedderDim(),
			HNSW: bboltstore.HNSWConfig{
				M:              cfg.VectorIndex.HNSW.M,
				M0:             cfg.VectorIndex.HNSW.M0,
				EfConstruction: cfg.VectorIndex.HNSW.EfConstruction,
				EfSearch:       cfg.VectorIndex.HNSW.EfSearch,
				ML:             cfg.VectorIndex.HNSW.ML,
			},
			FlatScanThreshold: cfg.VectorIndex.FlatScanThreshold,
		})
	default:
		return nil, fmt.Errorf("unsupported storage backend %q", cfg.Storage.Backend)
	}
}

// ----- embedder -----

func buildEmbedder(ctx context.Context, cfg config.Config) (embed.Embedder, error) {
	switch cfg.Embedder.Backend {
	case "fake":
		return fake.New(cfg.Embedder.Fake.Dim), nil
	case "gemini":
		key := os.Getenv(cfg.Embedder.Gemini.APIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("env var %s is empty", cfg.Embedder.Gemini.APIKeyEnv)
		}
		return gemini.New(ctx, gemini.Options{
			APIKey: key,
			Model:  cfg.Embedder.Gemini.Model,
			Dim:    cfg.Embedder.Gemini.Dim,
		})
	}
	return nil, fmt.Errorf("unsupported embedder backend %q", cfg.Embedder.Backend)
}

// ----- service config mapping -----

func serviceConfigFromYAML(cfg config.Config) service.Config {
	return service.Config{
		ChunkWindowSize:    cfg.Memory.ChunkWindowSize,
		ChunkStride:        cfg.Memory.ChunkStride,
		DefaultK:           cfg.Memory.RetrievalK,
		DefaultHops:        cfg.Memory.RetrievalHops,
		DefaultOversample:  cfg.Memory.RetrievalOversample,
		SimAlpha:           cfg.Memory.Scoring.SimAlpha,
		WeightBeta:         cfg.Memory.Scoring.WeightBeta,
		DepthPenaltyFactor: 2.0,

		NodeLambda:            cfg.Memory.Decay.NodeLambda,
		EdgeStructuralLambda:  cfg.Memory.Decay.EdgeStructuralLambda,
		EdgeCoRetrievalLambda: cfg.Memory.Decay.EdgeCoRetrievalLambda,
		EdgeCoTraversalLambda: cfg.Memory.Decay.EdgeCoTraversalLambda,

		NodeDelta:                    cfg.Memory.Reinforce.NodeDelta,
		EdgeCoRetrievalBase:          cfg.Memory.Reinforce.EdgeCoRetrievalBase,
		EdgeCoTraversalMultiplier:    cfg.Memory.Reinforce.EdgeCoTraversalMultiplier,
		EdgeStructuralWeight:         cfg.Memory.Reinforce.EdgeStructuralWeight,
		EdgeStructuralTemporalWeight: cfg.Memory.Reinforce.EdgeStructuralTemporalWeight,

		EdgeFloor: cfg.Memory.Prune.EdgeFloor,
		NodeFloor: cfg.Memory.Prune.NodeFloor,
		WeightCap: cfg.Memory.WeightCap,

		StructuralRecentN:     cfg.Memory.StructuralRecentN,
		StructuralRecentDelta: cfg.Memory.StructuralRecentDelta,
	}
}

// ----- MCP supervised service -----

type mcpService struct {
	adapter *mcpadapter.Adapter
	addr    string
	log     *slog.Logger
}

func (m *mcpService) Serve(ctx context.Context) error {
	srv := &http.Server{
		Addr:              m.addr,
		Handler:           m.adapter.HTTPHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		m.log.Info("mcp transport listening", "addr", m.addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		<-errCh
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}
