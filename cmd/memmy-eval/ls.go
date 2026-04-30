package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Cidan/memmy/internal/eval/dataset"
)

func newLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List datasets in the configured root",
		RunE: func(_ *cobra.Command, _ []string) error {
			stats, err := dataset.ListDatasets("")
			if err != nil {
				return err
			}
			root, _ := dataset.DefaultRoot()
			fmt.Fprintf(os.Stderr, "root: %s\n", root)
			if len(stats) == 0 {
				fmt.Fprintln(os.Stderr, "(no datasets)")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tCHUNKS\tQUERIES\tRUNS\tCORPUS\tQUERIES_DB\tUPDATED")
			for _, s := range stats {
				updated := s.UpdatedAt.Format("2006-01-02 15:04 MST")
				if s.UpdatedAt.IsZero() {
					updated = "-"
				}
				fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%v\t%v\t%s\n",
					s.Name, s.ChunkCount, s.QueryCount, s.RunCount, s.HasCorpus, s.HasQueries, updated)
			}
			return tw.Flush()
		},
	}
}
