package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/whiskydumb/yawn/internal/store"
)

// loginCapable is implemented by stores that need an interactive login.
type loginCapable interface {
	Login(ctx context.Context, port int) error
	TokenPath() string
}

func newAuthCmd() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "auth [store name]",
		Short: "authorise a store that needs an interactive login",
		Long: "Opens the provider's consent page and saves the token next to the\n" +
			"config. Without an argument every store that needs a login is done\n" +
			"in turn.",
		Args: cobra.MaximumNArgs(1),
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

			wanted := ""
			if len(args) > 0 {
				wanted = args[0]
			}

			done := 0
			for _, s := range stores {
				if wanted != "" && s.Name() != wanted {
					continue
				}
				login, ok := s.(loginCapable)
				if !ok {
					if wanted != "" {
						return fmt.Errorf("store %q of type %s needs no login", s.Name(), s.Kind())
					}
					continue
				}
				fmt.Printf("authorising %s (%s), token goes to %s\n", s.Name(), s.Kind(), login.TokenPath())
				if err := login.Login(ctx, port); err != nil {
					return fmt.Errorf("store %q: %w", s.Name(), err)
				}
				done++
			}

			if done == 0 {
				if wanted != "" {
					return fmt.Errorf("no store named %q in the config, configured stores are: %s",
						wanted, strings.Join(storeNames(stores), ", "))
				}
				fmt.Println("no store needs an interactive login")
				return nil
			}
			fmt.Printf("\nauthorised %d store(s), check them with: yawn stores\n", done)
			return nil
		},
	}
	cmd.Flags().IntVar(&port, "port", 0,
		"fixed port for the redirect, needed when running in a container (0 picks a free one)")
	return cmd
}

func storeNames(stores []store.Uploader) []string {
	out := make([]string, 0, len(stores))
	for _, s := range stores {
		out = append(out, s.Name())
	}
	return out
}

// storeKindsHint lists the store types the binary was built with.
func storeKindsHint() string {
	return fmt.Sprintf("known store types: %v", store.Kinds())
}
