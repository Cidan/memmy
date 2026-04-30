package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Cidan/memmy/internal/eval/dataset"
	"github.com/Cidan/memmy/internal/eval/sweep"
)

func newSweepCmd() *cobra.Command {
	var (
		datasetName  string
		matrixPath   string
		baseConfig   string
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
		Use:   "sweep",
		Short: "Run a parameter matrix over the same dataset + query set",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if datasetName == "" {
				return errors.New("--dataset is required")
			}
			if matrixPath == "" {
				return errors.New("--matrix is required")
			}
			ctx := cmd.Context()
			matrix, err := sweep.Load(matrixPath)
			if err != nil {
				return err
			}
			emb, modelID, _, err := buildEmbedder(ctx, embedderKind, geminiModel, geminiDim, fakeDim)
			if err != nil {
				return err
			}
			baseCfg, baseHNSW, err := loadServiceConfig(firstNonEmpty(baseConfig, matrix.Base))
			if err != nil {
				return err
			}

			ds, err := dataset.Open("", datasetName)
			if err != nil {
				return err
			}
			sweepID := newRunID()
			sweepDir := filepath.Join(ds.RunsDir(), sweepID)
			if err := os.MkdirAll(sweepDir, 0o700); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "sweep: id=%s entries=%d outDir=%s\n", sweepID, len(matrix.Entries), sweepDir)

			for i, entry := range matrix.Entries {
				cfg, err := sweep.ApplyServiceOverrides(baseCfg, entry.Overrides)
				if err != nil {
					return fmt.Errorf("entry %s: %w", entry.Name, err)
				}
				hnsw, err := sweep.ApplyHNSWOverrides(baseHNSW, entry.HNSW)
				if err != nil {
					return fmt.Errorf("entry %s: %w", entry.Name, err)
				}
				runID := sweepID + "/" + sanitize(entry.Name)
				out, err := executeRun(ctx, runID, datasetName, matrixPath, emb, modelID, cfg, hnsw, runOptions{
					K: k, Hops: hops, Oversample: oversample, Seed: seed,
				})
				if err != nil {
					return fmt.Errorf("entry %s: %w", entry.Name, err)
				}
				fmt.Fprintf(os.Stderr, "  [%d/%d] %s recall@5=%.3f reinforce_mean=%.4f outDir=%s\n",
					i+1, len(matrix.Entries), entry.Name, out.Summary.OverallRecallAt5,
					out.Summary.OverallReinforcementMean, out.OutDir)
			}
			fmt.Fprintf(os.Stderr, "sweep done: %d entries written under %s\n", len(matrix.Entries), sweepDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&datasetName, "dataset", "", "dataset name")
	cmd.Flags().StringVar(&matrixPath, "matrix", "", "path to sweep YAML")
	cmd.Flags().StringVar(&baseConfig, "base", "", "base service config YAML; overrides matrix.base")
	cmd.Flags().StringVar(&embedderKind, "embedder", "fake", "embedder backend: fake | gemini")
	cmd.Flags().StringVar(&geminiModel, "gemini-model", defaultGeminiModel, "")
	cmd.Flags().IntVar(&geminiDim, "gemini-dim", defaultGeminiDim, "")
	cmd.Flags().IntVar(&fakeDim, "fake-dim", 64, "")
	cmd.Flags().IntVar(&k, "k", 8, "queries returned per Recall")
	cmd.Flags().IntVar(&hops, "hops", 1, "graph expansion hops")
	cmd.Flags().IntVar(&oversample, "oversample", 0, "vector-search oversample")
	cmd.Flags().Uint64Var(&seed, "hnsw-seed", 42, "HNSW RNG seed")
	return cmd
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		out = []byte("entry")
	}
	return string(out)
}

