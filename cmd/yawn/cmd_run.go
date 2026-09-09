package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskydumb/yawn/internal/config"
	"github.com/whiskydumb/yawn/internal/events"
	"github.com/whiskydumb/yawn/internal/lockfile"
	"github.com/whiskydumb/yawn/internal/media"
	"github.com/whiskydumb/yawn/internal/pipeline"
	"github.com/whiskydumb/yawn/internal/queue"
	"golang.org/x/term"
)

func newRunCmd() *cobra.Command {
	var (
		workers      int
		uploaders    int
		quality      string
		rateLimit    string
		maxDisk      string
		uploadRate   string
		downloadOnly bool
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "run the pipeline: download, upload, free the space",
		Long: "Downloads queued videos and uploads them to the configured stores.\n" +
			"Ctrl+C stops cleanly: progress is saved and the next run continues\n" +
			"from the same place.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := applyRunFlags(&cfg, workers, uploaders, quality, rateLimit, maxDisk, uploadRate, downloadOnly); err != nil {
				return err
			}

			// the lock does two jobs: it stops two runs from fighting over one
			// queue, and it proves every claimed task in the database is
			// leftovers from a crashed process
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
				Extractor: newExtractor(cfg),
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
	cmd.Flags().StringVar(&rateLimit, "rate-limit", "", "speed ceiling for the whole tool, e.g. 3M")
	cmd.Flags().StringVar(&maxDisk, "max-disk", "", "how much to keep on disk in flight, e.g. 40G")
	cmd.Flags().StringVar(&uploadRate, "upload-rate-limit", "", "upload ceiling for the whole tool, e.g. 3M")
	cmd.Flags().BoolVar(&downloadOnly, "download-only", false, "download without uploading")
	return cmd
}

func applyRunFlags(cfg *config.Config, workers, uploaders int, quality, rateLimit, maxDisk, uploadRate string, downloadOnly bool) error {
	if workers > 0 {
		cfg.Download.Workers = workers
	}
	if uploaders > 0 {
		cfg.Upload.Workers = uploaders
	}
	if quality != "" {
		cfg.Download.Quality = quality
		cfg.Download.Format = ""
		if _, err := cfg.Download.FormatSelector(); err != nil {
			return err
		}
	}
	if rateLimit != "" {
		v, err := config.ParseSize(rateLimit)
		if err != nil {
			return err
		}
		cfg.Download.RateLimit = v
	}
	if maxDisk != "" {
		v, err := config.ParseSize(maxDisk)
		if err != nil {
			return err
		}
		cfg.Download.MaxDisk = v
	}
	if uploadRate != "" {
		v, err := config.ParseSize(uploadRate)
		if err != nil {
			return err
		}
		cfg.Upload.RateLimit = v
	}
	if downloadOnly {
		// the stores are dropped rather than skipped, or the pipeline would
		// delete files after a successful upload to nowhere
		cfg.Stores = nil
		cfg.Upload.DeleteAfter = false
	}
	return nil
}

func printRunHeader(cfg config.Config, p *pipeline.Pipeline) {
	selector, _ := cfg.Download.FormatSelector()
	fmt.Println(strings.Repeat("=", 64))
	fmt.Printf("  quality    : %s\n", cfg.Download.Quality)
	fmt.Printf("  format     : %s\n", selector)
	fmt.Printf("  workers    : %d downloading, %d uploading\n",
		cfg.Download.Workers, cfg.Upload.Workers)
	fmt.Printf("  disk cap   : %s\n", cfg.Download.MaxDisk)
	if cfg.Download.RateLimit > 0 {
		fmt.Printf("  download   : up to %s/s in total\n", cfg.Download.RateLimit)
	}
	if cfg.Upload.RateLimit > 0 {
		fmt.Printf("  upload     : up to %s/s in total\n", cfg.Upload.RateLimit)
	}
	fmt.Printf("  identity   : %s\n", p.Identities().Describe())
	names := p.StoreNames()
	if len(names) == 0 {
		fmt.Println("  stores     : none, files stay in the work directory")
	} else {
		fmt.Printf("  stores     : %s\n", strings.Join(names, ", "))
	}
	fmt.Println(strings.Repeat("=", 64))
}

// reporter prints what is happening to a plain console. There is one status
// line for all workers: if each worker returned the carriage itself, two
// parallel downloads would overwrite each other into noise.
type reporter struct {
	active    map[int64]activeItem
	statusLen int
	done      int
	failed    int

	// live means stdout is a terminal. Redirected to a file or a pipe a
	// carriage return does not erase anything, it just lands in the text, so
	// the status line is skipped and only events are printed.
	live bool
}

type activeItem struct {
	title   string
	videoID string
	store   string
	done    int64
	total   int64
	speed   float64
}

func (a activeItem) label() string {
	if a.title != "" {
		return trimTitle(a.title)
	}
	if a.videoID != "" {
		return a.videoID
	}
	return "(untitled)"
}

func startReporter(bus *events.Bus, wg *sync.WaitGroup) func() {
	ch, unsubscribe := bus.Subscribe()
	stop := make(chan struct{})

	go func() {
		defer wg.Done()
		r := &reporter{
			active: make(map[int64]activeItem),
			live:   term.IsTerminal(int(os.Stdout.Fd())),
		}
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()

		for {
			select {
			case <-stop:
				r.clearStatus()
				return
			case <-tick.C:
				r.drawStatus()
			case e, ok := <-ch:
				if !ok {
					r.clearStatus()
					return
				}
				r.handle(e)
			}
		}
	}()

	return func() {
		close(stop)
		unsubscribe()
	}
}

func (r *reporter) handle(e events.Event) {
	switch e.Kind {
	case events.KindProgress:
		r.active[e.ItemID] = activeItem{
			title: e.Title, videoID: e.VideoID, store: e.Store,
			done: e.Done, total: e.Total, speed: e.Speed,
		}

	case events.KindDownloadStart:
		r.active[e.ItemID] = activeItem{title: e.Title, videoID: e.VideoID}

	case events.KindDownloadDone:
		r.line("downloaded %s  %s", label(e), humanBytes(e.Done))

	case events.KindUploadDone:
		r.line("uploaded   %s to %s", label(e), e.Store)

	case events.KindItemDone:
		delete(r.active, e.ItemID)
		r.done++
		r.line("done       %s", label(e))

	case events.KindFailed:
		delete(r.active, e.ItemID)
		r.failed++
		r.line("FAILED     %s: %s", label(e), e.Message)

	case events.KindStalled, events.KindResumed, events.KindInfo, events.KindWarn:
		r.line("%s", e.Message)
	}
}

func (r *reporter) line(format string, args ...any) {
	r.clearStatus()
	fmt.Printf("  "+format+"\n", args...)
	r.drawStatus()
}

func (r *reporter) clearStatus() {
	if !r.live || r.statusLen == 0 {
		return
	}
	fmt.Print("\r", strings.Repeat(" ", r.statusLen), "\r")
	r.statusLen = 0
}

func (r *reporter) drawStatus() {
	if !r.live {
		return
	}
	if len(r.active) == 0 {
		r.clearStatus()
		return
	}
	// one line, so show whichever item is furthest along
	var shown activeItem
	best := -2.0
	for _, a := range r.active {
		pct := -1.0
		if a.total > 0 {
			pct = float64(a.done) / float64(a.total)
		}
		if pct > best {
			best, shown = pct, a
		}
	}

	verb := "downloading"
	if shown.store != "" {
		verb = "uploading to " + shown.store
	}
	status := fmt.Sprintf("  [%d done, %d active] %s %s",
		r.done, len(r.active), verb, shown.label())
	if best >= 0 {
		status += fmt.Sprintf(" %.1f%%", best*100)
	}
	if shown.speed > 0 {
		status += " " + humanSpeed(shown.speed)
	}

	r.clearStatus()
	fmt.Print(status)
	r.statusLen = len([]rune(status))
}

func label(e events.Event) string {
	if e.Title != "" {
		return trimTitle(e.Title)
	}
	if e.VideoID != "" {
		return e.VideoID
	}
	return "(untitled)"
}

func trimTitle(s string) string {
	const limit = 44
	if s == "" {
		return "(untitled)"
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-3]) + "..."
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func humanSpeed(bps float64) string {
	if bps <= 0 {
		return ""
	}
	return humanBytes(int64(bps)) + "/s"
}

func printSummary(ctx context.Context, q *queue.Store) {
	st, err := q.Stats(context.WithoutCancel(ctx))
	if err != nil {
		return
	}
	fmt.Printf("\ntotal %d, done %d, pending %d, failed %d, on disk %s\n",
		st.Total, st.ByState[queue.StateDone], st.Remaining(),
		st.ByState[queue.StateFailed], humanBytes(st.LocalBytes))
}
