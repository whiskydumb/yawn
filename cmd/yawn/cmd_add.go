package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/whiskydumb/yawn/internal/extract"
	"github.com/whiskydumb/yawn/internal/refresh"
	"github.com/whiskydumb/yawn/internal/source"
)

// extractor is what add and sync need, named so the commands read the same.
type extractor = extract.Extractor

func newAddCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "add <url|file>...",
		Short: "add a channel, playlist, video or link list to the queue",
		Long: "Expands a channel or playlist into videos and puts them in the queue.\n" +
			"Videos already known are skipped, so the command can be repeated to\n" +
			"pick up whatever a channel published since. `yawn sync` does that for\n" +
			"every source at once.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			q, err := openQueue(cfg)
			if err != nil {
				return err
			}
			defer q.Close()

			ex := newExtractor(cfg)
			if err := ex.Available(ctx); err != nil {
				return err
			}
			r := &refresh.Refresher{Queue: q, Extractor: ex, Limit: limit}

			totalAdded, totalSeen := 0, 0
			for _, arg := range args {
				inputs, err := source.Parse(arg)
				if err != nil {
					return err
				}
				fmt.Printf("expanding %s ...\n", arg)

				res, err := r.AddSource(ctx, inputs[0])
				if err != nil {
					return err
				}
				fmt.Printf("  %s: found %d, added %d new\n",
					refresh.Label(res.Source), res.Seen, res.Added)
				totalAdded += res.Added
				totalSeen += res.Seen
			}

			fmt.Printf("\nfound %d in total, added %d new\n", totalSeen, totalAdded)
			if totalAdded > 0 {
				fmt.Println("next: yawn run")
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "take at most this many videos from a source (0 means all)")
	return cmd
}
