package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskydumb/yawn/internal/importer"
	"github.com/whiskydumb/yawn/internal/media"
	"github.com/whiskydumb/yawn/internal/queue"
)

func newImportCmd() *cobra.Command {
	var (
		metaDir  string
		dryRun   bool
		verify   bool
		showAll  bool
		maxItems int
	)
	cmd := &cobra.Command{
		Use:   "import <directory>",
		Short: "register videos that are already on disk as downloaded",
		Long: "Walks a directory, finds videos by the [id] in their names and puts\n" +
			"them in the queue as already downloaded. A later run uploads them and\n" +
			"frees the space instead of fetching everything again.\n\n" +
			"Files stay where they are. Only `yawn run` removes them, and only\n" +
			"once every configured store has a copy.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			if st, err := os.Stat(root); err != nil || !st.IsDir() {
				return fmt.Errorf("%s is not a directory", root)
			}

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			fmt.Printf("scanning %s ...\n", root)
			res, err := importer.Scan(ctx, root, metaDir)
			if err != nil {
				return err
			}
			if len(res.Found) == 0 {
				fmt.Println("found no videos with a recognisable id")
				printProblems(res.Problems, showAll)
				return nil
			}

			found := res.Found
			if maxItems > 0 && len(found) > maxItems {
				found = found[:maxItems]
			}

			fmt.Printf("found %d videos, %s\n", len(res.Found), humanBytes(res.Bytes))
			withMeta := 0
			for _, f := range res.Found {
				if f.InfoJSON != "" {
					withMeta++
				}
			}
			fmt.Printf("metadata for %d of them\n", withMeta)
			printProblems(res.Problems, showAll)

			if verify {
				prober := media.NewProber(cfg.Extractor.FFprobe)
				if err := prober.Available(ctx); err != nil {
					return err
				}
				var broken int
				found, broken = verifyAll(ctx, prober, found)
				if broken > 0 {
					fmt.Printf("%d files did not pass the check and were left out\n", broken)
				}
			}

			if dryRun {
				fmt.Println("\ndry run, nothing was written")
				preview(found)
				return nil
			}

			q, err := openQueue(cfg)
			if err != nil {
				return err
			}
			defer q.Close()

			imported, skipped := 0, 0
			for _, f := range found {
				ok, err := q.ImportDownloaded(ctx, queue.NewItem{
					VideoID:    f.VideoID,
					Title:      f.Title,
					Uploader:   f.Uploader,
					UploadDate: f.UploadDate,
					Duration:   f.Duration,
				}, f.Path, f.Bytes)
				if err != nil {
					return err
				}
				if ok {
					imported++
				} else {
					skipped++
				}
			}

			fmt.Printf("\nimported %d", imported)
			if skipped > 0 {
				fmt.Printf(", skipped %d already finished", skipped)
			}
			fmt.Println()
			fmt.Println("next: yawn run, which uploads them and frees the space")
			return nil
		},
	}
	cmd.Flags().StringVar(&metaDir, "meta-dir", "",
		"extra directory to search for .info.json when a video has no sidecar")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be imported and stop")
	cmd.Flags().BoolVar(&verify, "verify", true, "check every file with ffprobe before importing")
	cmd.Flags().BoolVar(&showAll, "show-skipped", false, "list every file that was skipped")
	cmd.Flags().IntVar(&maxItems, "limit", 0, "import at most this many videos (0 means all)")
	return cmd
}

// verifyAll probes each file and drops the ones that fail.
//
// This is the whole point of importing rather than trusting the directory: a
// truncated file left by an interrupted download looks exactly like a good
// one until something reads it.
func verifyAll(ctx context.Context, prober *media.Prober, found []importer.Found) ([]importer.Found, int) {
	fmt.Printf("checking %d files with ffprobe...\n", len(found))

	good := make([]importer.Found, 0, len(found))
	broken := 0
	started := time.Now()

	for i, f := range found {
		if ctx.Err() != nil {
			break
		}
		if i%25 == 0 && i > 0 {
			fmt.Printf("  %d/%d, %s elapsed\n", i, len(found), time.Since(started).Truncate(time.Second))
		}
		if _, err := prober.Verify(ctx, f.Path, f.Duration); err != nil {
			fmt.Printf("  BROKEN %s: %v\n", filepath.Base(f.Path), err)
			broken++
			continue
		}
		good = append(good, f)
	}
	return good, broken
}

func printProblems(problems []importer.Problem, showAll bool) {
	if len(problems) == 0 {
		return
	}
	fmt.Printf("skipped %d files\n", len(problems))
	limit := len(problems)
	if !showAll && limit > 5 {
		limit = 5
	}
	for _, p := range problems[:limit] {
		fmt.Printf("  %s: %s\n", filepath.Base(p.Path), p.Reason)
	}
	if limit < len(problems) {
		fmt.Printf("  ... and %d more, use --show-skipped to see them\n", len(problems)-limit)
	}
}

func preview(found []importer.Found) {
	limit := len(found)
	if limit > 10 {
		limit = 10
	}
	for _, f := range found[:limit] {
		title := f.Title
		if title == "" {
			title = "(no metadata)"
		}
		fmt.Printf("  %s  %-10s  %s\n", f.VideoID, humanBytes(f.Bytes), trimTitle(title))
	}
	if limit < len(found) {
		fmt.Printf("  ... and %d more\n", len(found)-limit)
	}
}
