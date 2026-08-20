package main

import (
	"fmt"
	"io"
	"os"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/spf13/cobra"
)

// loginURLPrinter builds the onURL callback BuildLoginCommand hands to
// auth.LoginFrom: it prints the exact login URL to out BEFORE the login blocks
// on the OAuth callback, on every run — not only when browser.Open fails — so a
// user whose browser opened to the wrong profile (or didn't open at all) can
// still find the link immediately. Kept as a standalone function so it is
// testable without driving the real network/browser login.
func loginURLPrinter(out io.Writer) func(string) {
	return func(loginURL string) {
		fmt.Fprintf(out, "Log in to the village at:\n  %s\n", loginURL)
	}
}

func BuildLoginCommand() *cobra.Command {
	var local bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to the Peasant village via GitHub",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := resolveConfigPath(cmd)
			configDir := configDirOverride(cmd)

			var villageURL string
			if local {
				villageURL = defaults.LocalVillageURL.String()
				// Auto-switch: clear any existing session so the user doesn't
				// have to manually logout before switching to local.
				_ = auth.LogoutFrom(cmd.Context(), configDir)
			} else {
				// Env var takes precedence over config file so that
				// PEASANT_VILLAGE_URL=https://localhost:8443 peasant login works as expected.
				villageURL = os.Getenv("PEASANT_VILLAGE_URL")
				if villageURL == "" {
					cfg, err := loadConfig(cfgPath)
					if err == nil && cfg.Village.URL != "" {
						villageURL = cfg.Village.URL
					}
				}
				if villageURL == "" {
					villageURL = defaults.DefaultVillageURL.String()
				}
			}

			creds, err := auth.LoginFrom(cmd.Context(), villageURL, false, configDir, loginURLPrinter(cmd.OutOrStdout()))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged in as %s\n", creds.Username)
			return nil
		},
	}
	cmd.Flags().BoolVar(&local, "local", false, "Log in to the local development village (https://localhost:8443)")
	_ = cmd.Flags().MarkHidden("local")
	return cmd
}
