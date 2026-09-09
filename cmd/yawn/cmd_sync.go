package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/whiskydumb/yawn/internal/events"
	"github.com/whiskydumb/yawn/internal/lockfile"
	"github.com/whiskydumb/yawn/internal/media"
	"github.com/whiskydumb/yawn/internal/pipeline"
	"github.com/whiskydumb/yawn/internal/queue"
	"github.com/whiskydumb/yawn/internal/refresh"
	"sync"
)

func newSyncCmd() *cobra.Command {
	var (
		workers     int
		uploaders   int
		quality     string
		rateLimit   string
		maxDisk     string
		uploadRate  string
		refreshOnly bool
	)
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "catch up on every source, then work the queue",
		Long: "Re-expands every channel, playlist and link list already in the\n" +
			"queue, adds whatever is new, and runs the pipeline. Then it exits.\n\n" +
			"This is the piece a schedule needs: put it in cron, a systemd timer\n" +
			"or Task Scheduler and yawn keeps an archive current without a\n" +
			"process sitting around between runs.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := applyRunFlags(&cfg, workers, uploaders, quality, rateLimit, maxDisk, uploadRate, false); err != nil {
				return err
			}

			// held for the whole command: refreshing and running are one unit
			// of work, and a second yawn joining halfway would fight over the
			// same queue
			lock, err := lockfile.Acquire(cfg.Database + ".lock")
			if errors.Is(err, lockfile.ErrBusy) {
				return fmt.Errorf("another yawn is already working on the queue %s", cfg.Database)
			}
			if err != nil {
				return err
			}
			defer lock.Release()

			q, err := openQueue(cfg)
			if err != nil {
				return err
			}
			defer q.Close()

			ex := newExtractor(cfg)
			if err := ex.Available(ctx); err != nil {
				return err
			}

			added, err := refreshAll(ctx, q, ex)
			if err != nil {
				return err
			}
			if refreshOnly {
				fmt.Printf("\nadded %d, stopping here as asked\n", added)
				return nil
			}

			busy, err := q.HasWork(ctx, cfg.Download.Retries)
			if err != nil {
				return err
			}
			if !busy {
				fmt.Println("\nnothing to do")
				return nil
			}

			stores, err := buildStores(cfg)
			if err != nil {
				return err
			}
			defer closeStores(stores)

			bus := events.NewBus(512)
			defer bus.Close()

			p, err := pipeline.New(pipeline.Options{
				Config:    cfg,
				Queue:     q,
				Extractor: ex,
				Prober:    media.NewProber(cfg.Extractor.FFprobe),
				Converter: media.NewConverter(cfg.Extractor.FFmpeg),
				Stores:    stores,
				Bus:       bus,
				Logger:    newLogger(),
				Exclusive: true,
			})
			if err != nil {
				return err
			}

			fmt.Println()
			printRunHeader(cfg, p)

			var wg sync.WaitGroup
			wg.Add(1)
			stopReporter := startReporter(bus, &wg)

			runErr := p.Run(ctx)
			stopReporter()
			wg.Wait()

			printSummary(ctx, q)
			return runErr
		},
	}
	cmd.Flags().IntVar(&workers, "workers", 0, "download workers (0 means use the config)")
	cmd.Flags().IntVar(&uploaders, "upload-workers", 0, "upload workers (0 means use the config)")
	cmd.Flags().StringVar(&quality, "quality", "", "quality: 360, 720, 1080, best, audio")
	cmd.Flags().StringVar(&rateLimit, "rate-limit", "", "download ceiling for the whole tool, e.g. 3M")
	cmd.Flags().StringVar(&uploadRate, "upload-rate-limit", "", "upload ceiling for the whole tool, e.g. 3M")
	cmd.Flags().StringVar(&maxDisk, "max-disk", "", "how much to keep on disk in flight, e.g. 40G")
	cmd.Flags().BoolVar(&refreshOnly, "refresh-only", false, "update the queue without downloading")
	return cmd
}

// refreshAll re-expands every source and reports what each one contributed.
func refreshAll(ctx context.Context, q *queue.Store, ex extractor) (int, error) {
	sources, err := q.Sources(ctx)
	if err != nil {
		return 0, err
	}
	if len(sources) == 0 {
		fmt.Println("no sources yet, add one with: yawn add <url>")
		return 0, nil
	}

	fmt.Printf("checking %d sources for new videos\n", len(sources))
	r := &refresh.Refresher{Queue: q, Extractor: ex}

	results, err := r.All(ctx, func(res refresh.Result) {
		label := refresh.Label(res.Source)
		switch {
		case res.Err != nil:
			fmt.Printf("  %-40s FAILED: %v\n", trimTitle(label), res.Err)
		case res.Added > 0:
			fmt.Printf("  %-40s %d new of %d\n", trimTitle(label), res.Added, res.Seen)
		default:
			fmt.Printf("  %-40s up to date\n", trimTitle(label))
		}
	})
	if err != nil {
		return 0, err
	}

	added, _, failed := refresh.Totals(results)
	if failed > 0 {
		fmt.Printf("%d sources could not be checked\n", failed)
	}
	return added, nil
}
