// Command memmy-eval is the validation harness CLI for memmy.
//
// It owns four phases of the framework:
//
//	ingest   — extract Claude Code session JSONL into a versioned dataset
//	queries  — generate (or top-up) labeled queries against an ingested corpus
//	run      — replay the corpus into a fresh memmy db, run the query battery,
//	           write per-run metrics
//	sweep    — run a parameter matrix over the same corpus + query set
//	ls       — list datasets in the configured root
//
// Datasets live OUTSIDE this repo at $MEMMY_EVAL_HOME (default
// ~/.local/share/memmy-eval/<name>/).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

func main() {
	root := newRootCmd()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "memmy-eval:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var verbose bool
	root := &cobra.Command{
		Use:   "memmy-eval",
		Short: "Validation harness for memmy (datasets live in $MEMMY_EVAL_HOME)",
		Long: `memmy-eval extracts Claude Code session JSONL into versioned datasets
under ~/.local/share/memmy-eval/<name>/ (overridable via $MEMMY_EVAL_HOME),
runs labeled query batteries against fresh memmy databases, and reports
per-run + per-config metrics.

All datasets live OUTSIDE the repo. The framework code is in the repo;
the data is local-only.`,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			level := slog.LevelInfo
			if verbose {
				level = slog.LevelDebug
			}
			h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
			slog.SetDefault(slog.New(h))
		},
		SilenceUsage: true,
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose logging to stderr")

	root.AddCommand(newIngestCmd())
	root.AddCommand(newQueriesCmd())
	root.AddCommand(newRunCmd())
	root.AddCommand(newSweepCmd())
	root.AddCommand(newLsCmd())

	return root
}
