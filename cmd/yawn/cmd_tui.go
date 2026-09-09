package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"github.com/whiskydumb/yawn/internal/events"
	"github.com/whiskydumb/yawn/internal/lockfile"
	"github.com/whiskydumb/yawn/internal/media"
	"github.com/whiskydumb/yawn/internal/pipeline"
	"github.com/whiskydumb/yawn/internal/queue"
	"github.com/whiskydumb/yawn/internal/tui"
	"golang.org/x/term"
)

func newTUICmd() *cobra.Command {
	var (
		workers    int
		uploaders  int
		quality    string
		rateLimit  string
		maxDisk    string
		uploadRate string
		logFile    string
	)
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "run the pipeline with a live dashboard",
		Long: "Same work as `yawn run`, shown as a dashboard.\n" +
			"q stops the pipeline the same way Ctrl+C does in run: workers\n" +
			"finish their current step and hand their tasks back.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// a dashboard needs a real terminal: redirected to a file or a
			// pipe it would spray escape codes instead of output
			if !term.IsTerminal(int(os.Stdout.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) {
				return errors.New("tui needs an interactive terminal, use `yawn run` when input or output is redirected")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := applyRunFlags(&cfg, workers, uploaders, quality, rateLimit, maxDisk, uploadRate, false); err != nil {
				return err
			}

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

			// the dashboard owns the terminal, so the log has to go to a file
			// or it would tear the screen apart
			logger, closeLog, err := fileLogger(logFile)
			if err != nil {
				return err
			}
			defer closeLog()

			bus := events.NewBus(1024)
			defer bus.Close()

			p, err := pipeline.New(pipeline.Options{
				Config:    cfg,
				Queue:     q,
				Extractor: newExtractor(cfg),
				Prober:    media.NewProber(cfg.Extractor.FFprobe),
				Converter: media.NewConverter(cfg.Extractor.FFmpeg),
				Stores:    stores,
				Bus:       bus,
				Logger:    logger,
				Exclusive: true,
			})
			if err != nil {
				return err
			}

			return runDashboard(cmd.Context(), p, q, bus, cfg.Download.Quality, logFile)
		},
	}
	cmd.Flags().IntVar(&workers, "workers", 0, "download workers (0 means use the config)")
	cmd.Flags().IntVar(&uploaders, "upload-workers", 0, "upload workers (0 means use the config)")
	cmd.Flags().StringVar(&quality, "quality", "", "quality: 360, 720, 1080, best, audio")
	cmd.Flags().StringVar(&rateLimit, "rate-limit", "", "speed ceiling for the whole tool, e.g. 3M")
	cmd.Flags().StringVar(&maxDisk, "max-disk", "", "how much to keep on disk in flight, e.g. 40G")
	cmd.Flags().StringVar(&uploadRate, "upload-rate-limit", "", "upload ceiling for the whole tool, e.g. 3M")
	cmd.Flags().StringVar(&logFile, "log-file", "yawn.log", "where to write the log while the dashboard is up")
	return cmd
}

func runDashboard(ctx context.Context, p *pipeline.Pipeline, q *queue.Store,
	bus *events.Bus, quality, logFile string) error {

	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	model := tui.New(tui.Options{
		Stats:   q.Stats,
		Disk:    p.DiskUsage,
		Stop:    stop,
		Stores:  p.StoreNames(),
		Quality: quality,
	})
	prog := tea.NewProgram(model, tea.WithContext(ctx))

	ch, unsubscribe := bus.Subscribe()
	defer unsubscribe()
	go func() {
		for e := range ch {
			prog.Send(tui.Event(e))
		}
	}()

	runErr := make(chan error, 1)
	go func() {
		err := p.Run(runCtx)
		runErr <- err
		prog.Send(tui.Finished(err))
	}()

	if _, err := prog.Run(); err != nil {
		stop()
		<-runErr
		return err
	}

	// the dashboard is gone; give the pipeline the chance to wind down
	stop()
	err := <-runErr

	printSummary(ctx, q)
	if logFile != "" {
		fmt.Printf("log: %s\n", logFile)
	}
	return err
}

// fileLogger sends the log to a file so it survives the dashboard.
func fileLogger(path string) (*slog.Logger, func(), error) {
	if path == "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot open the log file %s: %w", path, err)
	}
	level := slog.LevelInfo
	if flagVerbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: level}))
	return logger, func() { f.Close() }, nil
}
