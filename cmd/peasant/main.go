package main

import (
	"errors"
	"os"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/spf13/cobra"
)

// commands is the static registry of all top-level command builders.
// Each entry returns a fully configured *cobra.Command subtree.
var commands = [...]func() *cobra.Command{
	BuildTUICommand,
	BuildWebCommand,
	BuildIngestCommand,
	BuildMetricsCommand,
	BuildVillageCommand,
	BuildModelsCommand,
	BuildSessionsCommand,
	BuildVersionCommand,
	BuildKickstartCommand,
	BuildAnnotateCommand,
	BuildMemoryCommand,
	BuildExportCommand,
	BuildPruneCommand,
	BuildRedactCommand,
	BuildDocgenCommand,
	BuildPlayIngestCommand,
	BuildPlayPushCommand,
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "peasant",
		Short: "Developer observability for AI coding agent sessions",
	}
	rootCmd.PersistentFlags().String("config", string(defaults.Config.FilePath), "Path to config file (default: ~/.config/peasant/config.yaml)")
	rootCmd.PersistentFlags().String("data-dir", "", "Override the data directory (default: $XDG_DATA_HOME or ~/.local/share); the DB + peasant-sync live under <data-dir>/peasant")
	rootCmd.PersistentFlags().String("config-dir", "", "Override the config directory (default: $XDG_CONFIG_HOME or ~/.config); config lives under <config-dir>/peasant")
	rootCmd.PersistentFlags().String("state-dir", "", "Override the state directory (default: $XDG_STATE_HOME or ~/.local/state); logs/PID live under <state-dir>/peasant")

	for _, build := range commands {
		rootCmd.AddCommand(build())
	}

	if err := rootCmd.Execute(); err != nil {
		os.Exit(exitCodeFor(err).Int())
	}
}

// exitCodeFor maps a command failure to the status peasant exits with.
//
// Only one distinction is drawn, and it exists for a reader that cannot read
// the message: a generated Git hook sees the status and nothing else. A push
// that failed before sending anything must not be reported by that hook as one
// that published part of its work.
func exitCodeFor(err error) defaults.ExitCode {
	var nothingAttempted *pushNothingAttemptedError
	if errors.As(err, &nothingAttempted) {
		return defaults.ExitNothingAttempted
	}
	return defaults.ExitFailure
}
