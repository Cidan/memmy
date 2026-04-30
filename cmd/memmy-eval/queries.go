package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Cidan/memmy/internal/eval/corpus"
	"github.com/Cidan/memmy/internal/eval/dataset"
	"github.com/Cidan/memmy/internal/eval/manifest"
	"github.com/Cidan/memmy/internal/eval/queries"
)

func newQueriesCmd() *cobra.Command {
	var (
		datasetName string
		n           int
		categories  string
	)
	cmd := &cobra.Command{
		Use:   "queries",
		Short: "Generate (or top-up) labeled queries for a dataset",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if datasetName == "" {
				return errors.New("--dataset is required")
			}
			ctx := cmd.Context()
			ds, err := dataset.Open("", datasetName)
			if err != nil {
				return err
			}
			cs, err := corpus.OpenStore(ds.CorpusDBPath())
			if err != nil {
				return err
			}
			defer cs.Close()
			snapshot, err := cs.SnapshotHash(ctx)
			if err != nil {
				return err
			}
			var turns []corpus.StoredTurn
			if err := cs.IterateTurns(ctx, func(st corpus.StoredTurn) error {
				turns = append(turns, st)
				return nil
			}); err != nil {
				return err
			}
			if len(turns) == 0 {
				return errors.New("queries: corpus has no turns; run `memmy-eval ingest` first")
			}

			cats := parseCategories(categories)
			gen := queries.NewFakeGenerator()
			fmt.Fprintf(os.Stderr, "queries: generator=%s corpus_snapshot=%s n=%d categories=%v\n", gen.Version(), snapshot, n, cats)

			qstore, err := queries.OpenStore(ds.QueriesDBPath())
			if err != nil {
				return err
			}
			defer qstore.Close()

			out, err := gen.Generate(ctx, turns, queries.GenerateRequest{
				Categories: cats,
				TargetN:    n,
			})
			if err != nil {
				return err
			}
			added := 0
			for _, q := range out {
				existing, err := qstore.CountForGeneration(ctx, gen.Version(), snapshot, q.Category)
				if err != nil {
					return err
				}
				if err := qstore.Put(ctx, q, gen.Version(), snapshot); err != nil {
					return err
				}
				after, err := qstore.CountForGeneration(ctx, gen.Version(), snapshot, q.Category)
				if err != nil {
					return err
				}
				if after > existing {
					added++
				}
			}
			total, err := qstore.Count(ctx)
			if err != nil {
				return err
			}
			if dsm, err := manifest.ReadDataset(ds.ManifestPath()); err == nil {
				dsm.QueryCount = total
				dsm.UpdatedAt = time.Now().UTC()
				_ = manifest.WriteDataset(ds.ManifestPath(), dsm)
			}
			fmt.Fprintf(os.Stderr, "queries done: added=%d generated=%d total=%d\n", added, len(out), total)
			return nil
		},
	}
	cmd.Flags().StringVar(&datasetName, "dataset", "", "dataset name")
	cmd.Flags().IntVar(&n, "n", 10, "target queries per category")
	cmd.Flags().StringVar(&categories, "categories", "paraphrase,distractor", "comma-separated categories: paraphrase,negation,topic-jump,distractor,stale-relevant,temporal")
	return cmd
}

func parseCategories(csv string) []queries.Category {
	parts := strings.Split(csv, ",")
	out := make([]queries.Category, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, queries.Category(p))
	}
	if len(out) == 0 {
		out = []queries.Category{queries.CategoryParaphrase, queries.CategoryDistractor}
	}
	return out
}
