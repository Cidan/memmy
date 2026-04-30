// Command memmy runs the memmy LLM-memory MCP server, plus the
// `migrate` subcommand that applies the bundled Neo4j schema.
//
// `memmy serve` (default if no subcommand) wires the configured
// embedder, Neo4j storage, MemoryService, and transport adapters
// under a suture supervisor (DESIGN.md §11). It refuses to start
// against a Neo4j whose schema version doesn't match this build —
// run `memmy migrate` first.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/thejerf/suture/v4"

	"github.com/Cidan/memmy"
	"github.com/Cidan/memmy/internal/clock"
	"github.com/Cidan/memmy/internal/config"
	"github.com/Cidan/memmy/internal/embed"
	"github.com/Cidan/memmy/internal/embed/fake"
	"github.com/Cidan/memmy/internal/embed/gemini"
	"github.com/Cidan/memmy/internal/service"
	neo4jstore "github.com/Cidan/memmy/internal/storage/neo4j"
	mcpadapter "github.com/Cidan/memmy/internal/transport/mcp"
)

func main() {
	root := newRootCmd()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "memmy:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var configPath string
	root := &cobra.Command{
		Use:   "memmy",
		Short: "memmy — LLM-memory MCP server backed by Neo4j",
		// Default action when invoked with no subcommand is `serve`.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), configPath)
		},
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", "memmy.yaml", "path to YAML configuration file")

	root.AddCommand(newServeCmd(&configPath))
	root.AddCommand(newMigrateCmd(&configPath))
	return root
}

func newServeCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the memmy MCP server (default action)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), *configPath)
		},
	}
}

func newMigrateCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending Neo4j schema migrations and exit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			if err := memmy.Migrate(cmd.Context(), memmy.MigrationOptions{
				Neo4j: memmy.Neo4jOptions{
					URI:            cfg.Storage.Neo4j.URI,
					User:           cfg.Storage.Neo4j.User,
					Password:       cfg.Storage.Neo4j.Password,
					Database:       cfg.Storage.Neo4j.Database,
					ConnectTimeout: cfg.Storage.Neo4j.ConnectTimeout,
				},
				Dim: cfg.EmbedderDim(),
			}); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
			fmt.Fprintln(os.Stderr, "memmy: schema up-to-date")
			return nil
		},
	}
}

func runServe(ctx context.Context, configPath string) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	logger.Info("config loaded", "path", configPath, "storage_backend", cfg.Storage.Backend, "embedder_backend", cfg.Embedder.Backend)

	storage, err := openStorage(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer storage.Close()

	if err := guardSchema(ctx, storage); err != nil {
		return err
	}

	embedder, err := buildEmbedder(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build embedder: %w", err)
	}

	if cfg.EmbedderDim() != storage.Dim() {
		return fmt.Errorf("embedder dim (%d) does not match storage dim (%d)", cfg.EmbedderDim(), storage.Dim())
	}

	tenantSchema, err := service.NewTenantSchemaFromConfig(cfg.Tenant)
	if err != nil {
		return fmt.Errorf("build tenant schema: %w", err)
	}

	svc, err := service.New(
		storage.Graph(),
		storage.VectorIndex(),
		embedder,
		clock.Real{},
		serviceConfigFromYAML(cfg),
		tenantSchema,
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
		adapter := mcpadapter.New(svc, tenantSchema)
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
				adapter: mcpadapter.New(svc, tenantSchema),
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

func openStorage(ctx context.Context, cfg config.Config) (*neo4jstore.Storage, error) {
	if cfg.Storage.Backend != "neo4j" {
		return nil, fmt.Errorf("unsupported storage backend %q (only neo4j is supported)", cfg.Storage.Backend)
	}
	return neo4jstore.Open(ctx, neo4jstore.Options{
		URI:               cfg.Storage.Neo4j.URI,
		Username:          cfg.Storage.Neo4j.User,
		Password:          cfg.Storage.Neo4j.Password,
		Database:          cfg.Storage.Neo4j.Database,
		ConnectTimeout:    cfg.Storage.Neo4j.ConnectTimeout,
		Dim:               cfg.EmbedderDim(),
		FlatScanThreshold: cfg.VectorIndex.FlatScanThreshold,
	})
}

// guardSchema rejects start-up against a database whose schema version
// is older than what this build of memmy expects. The remediation is
// `memmy migrate --config <path>`.
func guardSchema(ctx context.Context, storage *neo4jstore.Storage) error {
	want, err := neo4jstore.RequiredSchemaVersion()
	if err != nil {
		return fmt.Errorf("read required schema version: %w", err)
	}
	got, err := storage.CurrentSchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("read current schema version: %w", err)
	}
	if got != want {
		return fmt.Errorf("memmy: database schema v%d required, current v%d. Run `memmy migrate --config <path>` first", want, got)
	}
	return nil
}

// ----- embedder -----

func buildEmbedder(ctx context.Context, cfg config.Config) (embed.Embedder, error) {
	switch cfg.Embedder.Backend {
	case "fake":
		return fake.New(cfg.Embedder.Fake.Dim), nil
	case "gemini":
		if cfg.Embedder.Gemini.APIKey == "" {
			return nil, errors.New("config: embedder.gemini.api_key required")
		}
		return gemini.New(ctx, gemini.Options{
			APIKey: cfg.Embedder.Gemini.APIKey,
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

		RefractoryPeriod: cfg.Memory.RefractoryPeriod,
		LogDampening:     cfg.Memory.LogDampening,
		MarkMaxNodes:     cfg.Memory.MarkMaxNodes,
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
