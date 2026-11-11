package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/claimprompt"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
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
		fmt.Fprintf(out, "Log in to peasant online at:\n  %s\n", loginURL)
	}
}

func BuildLoginCommand() *cobra.Command {
	var local bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to peasant online via GitHub",
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
			// The ownership question belongs to the standalone sign-in path. A
			// claim failure never fails a completed login: the decision is not
			// recorded, so `peasant user claim` can retry.
			claimDeps := signInClaimDeps{
				in:          cmd.InOrStdin(),
				out:         cmd.ErrOrStderr(),
				interactive: commandInputIsTerminal(cmd.InOrStdin()),
				statePath:   claimprompt.DefaultPath(defaults.ResolveStateDirPathWith(stateDirOverride(cmd)).String()),
				now:         func() int64 { return time.Now().UnixMilli() },
				capture: func(ctx context.Context) (auth.CredentialSnapshot, error) {
					return auth.CaptureCredentials(ctx, configDir)
				},
				claim: func(ctx context.Context, snapshot auth.CredentialSnapshot, all bool) (store.ClaimResult, error) {
					db, cleanup, err := openDB(cmd)
					if err != nil {
						return store.ClaimResult{}, err
					}
					defer cleanup()
					return db.ClaimArtifacts(ctx, snapshot, all, store.ClaimSourceSignInPrompt, time.Now().UnixMilli())
				},
			}
			if err := promptSignInClaim(cmd.Context(), creds, claimDeps); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), claimPromptWarning(err))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&local, "local", false, "Log in to a local development server of peasant online (https://localhost:8443)")
	_ = cmd.Flags().MarkHidden("local")
	return cmd
}
