package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/whiskydumb/yawn/internal/queue"
)

// stateLabels lists the states in pipeline order.
var stateLabels = []struct {
	state queue.State
	label string
}{
	{queue.StatePending, "pending"},
	{queue.StateDownloading, "downloading"},
	{queue.StateDownloaded, "downloaded"},
	{queue.StateUploading, "uploading"},
	{queue.StateDone, "done"},
	{queue.StateFailed, "failed"},
	{queue.StateSkipped, "skipped"},
}

func newStatusCmd() *cobra.Command {
	var showFailed bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "show the state of the queue",
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

			sources, err := q.Sources(ctx)
			if err != nil {
				return err
			}
			if len(sources) > 0 {
				fmt.Println("sources:")
				for _, s := range sources {
					name := s.Title
					if name == "" {
						name = s.URL
					}
					fmt.Printf("  [%s] %s\n", s.Kind, name)
				}
				fmt.Println()
			}

			st, err := q.Stats(ctx)
			if err != nil {
				return err
			}
			fmt.Println("queue:")
			for _, sl := range stateLabels {
				if n := st.ByState[sl.state]; n > 0 {
					fmt.Printf("  %-13s %5d\n", sl.label, n)
				}
			}
			fmt.Printf("  %-13s %5d\n", "total", st.Total)
			if st.Total > 0 {
				done := st.ByState[queue.StateDone]
				fmt.Printf("  %-13s %5.1f%%\n", "progress", float64(done)/float64(st.Total)*100)
			}
			fmt.Printf("\non disk: %s\n", humanBytes(st.LocalBytes))
			if st.TargetsTotal > 0 {
				fmt.Printf("uploads: %d of %d\n", st.TargetsDone, st.TargetsTotal)
			}

			if showFailed {
				failed, err := q.FailedItems(ctx, 50)
				if err != nil {
					return err
				}
				if len(failed) == 0 {
					fmt.Println("\nnothing has failed")
					return nil
				}
				fmt.Printf("\nfailed (%d):\n", len(failed))
				for _, it := range failed {
					fmt.Printf("  %s  %s\n      %s\n",
						it.VideoID, trimTitle(it.Title), strings.TrimSpace(it.LastError))
				}
				fmt.Println("\nput them back with: yawn retry")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&showFailed, "failed", false, "list failed videos with their reasons")
	return cmd
}

func newRetryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retry [video id]",
		Short: "put failed tasks back in the queue",
		Long: "Clears the attempt counter and requeues failed videos.\n" +
			"Without an argument every failed video, with one only that video.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			q, err := openQueue(cfg)
			if err != nil {
				return err
			}
			defer q.Close()

			videoID := ""
			if len(args) > 0 {
				videoID = args[0]
			}
			n, err := q.Retry(cmd.Context(), videoID)
			if err != nil {
				return err
			}
			fmt.Printf("requeued: %d\n", n)
			return nil
		},
	}
}

func newStoresCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stores",
		Short: "check the configured stores",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			stores, err := buildStores(cfg)
			if err != nil {
				return err
			}
			defer closeStores(stores)

			if len(stores) == 0 {
				fmt.Println("no stores configured, yawn will behave as a plain downloader")
				fmt.Println(storeKindsHint())
				return nil
			}
			failed := 0
			for _, s := range stores {
				if err := s.Available(ctx); err != nil {
					fmt.Printf("  %-16s %-8s UNAVAILABLE: %v\n", s.Name(), s.Kind(), err)
					failed++
					continue
				}
				fmt.Printf("  %-16s %-8s ready\n", s.Name(), s.Kind())
			}
			if failed > 0 {
				return fmt.Errorf("unavailable stores: %d", failed)
			}
			return nil
		},
	}
}
