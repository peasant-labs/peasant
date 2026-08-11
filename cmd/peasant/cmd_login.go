package main

import (
	"fmt"
	"os"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/spf13/cobra"
)

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

			creds, err := auth.LoginFrom(cmd.Context(), villageURL, false, configDir)
			if err != nil {
				return err
			}
			fmt.Printf("Logged in as %s\n", creds.Username)
			return nil
		},
	}
	cmd.Flags().BoolVar(&local, "local", false, "Log in to the local development village (https://localhost:8443)")
	_ = cmd.Flags().MarkHidden("local")
	return cmd
}
