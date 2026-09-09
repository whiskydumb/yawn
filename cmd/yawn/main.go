package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/whiskydumb/yawn/internal/config"
	"github.com/whiskydumb/yawn/internal/extract/ytdlp"
	"github.com/whiskydumb/yawn/internal/queue"
	"github.com/whiskydumb/yawn/internal/store"

	// store implementations register themselves
	_ "github.com/whiskydumb/yawn/internal/store/drive"
	_ "github.com/whiskydumb/yawn/internal/store/local"
	_ "github.com/whiskydumb/yawn/internal/store/s3"
)

// version is set by the linker in release builds.
var version = "dev"

var (
	flagConfig  string
	flagVerbose bool
)

func main() {
	root := &cobra.Command{
		Use:           "yawn",
		Short:         "youtube channel archiver with pluggable storage",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&flagConfig, "config", "c", "",
		"path to the config (defaults to "+config.DefaultFileName+" nearby)")
	root.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "verbose logging")

	root.AddCommand(
		newInitCmd(),
		newAddCmd(),
		newImportCmd(),
		newRunCmd(),
		newSyncCmd(),
		newStatusCmd(),
		newRetryCmd(),
		newStoresCmd(),
		newAuthCmd(),
		newTUICmd(),
	)

	ctx := withSignals(context.Background())
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// withSignals turns Ctrl+C into a context cancellation. A second Ctrl+C kills
// the process: the first is a request to stop cleanly, which on a long
// download takes time the user may not want to wait for.
func withSignals(parent context.Context) context.Context {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-ch
		fmt.Fprintln(os.Stderr, "\nstopping cleanly, progress is being saved (Ctrl+C again to quit now)")
		cancel()
		<-ch
		fmt.Fprintln(os.Stderr, "quitting now")
		os.Exit(130)
	}()
	return ctx
}

func newLogger() *slog.Logger {
	level := slog.LevelWarn
	if flagVerbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func loadConfig() (config.Config, error) {
	return config.Load(flagConfig)
}

func openQueue(cfg config.Config) (*queue.Store, error) {
	return queue.Open(cfg.Database)
}

func newExtractor(cfg config.Config) *ytdlp.Extractor {
	return ytdlp.New(cfg.Extractor.Binary, cfg.Extractor.ExtraArgs)
}

func buildStores(cfg config.Config) ([]store.Uploader, error) {
	var out []store.Uploader
	for _, sc := range cfg.Stores {
		if !sc.IsEnabled() {
			continue
		}
		u, err := store.Build(sc.Type, sc.Name, sc.Layout, sc.Options)
		if err != nil {
			closeStores(out)
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func closeStores(stores []store.Uploader) {
	for _, s := range stores {
		_ = s.Close()
	}
}
