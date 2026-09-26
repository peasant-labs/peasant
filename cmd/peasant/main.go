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
	BuildUpgradeCommand,
	BuildKickstartCommand,
	BuildConfigCommand,
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
	rootCmd := buildRootCommand()
	if err := rootCmd.Execute(); err != nil {
		os.Exit(exitCodeFor(err).Int())
	}
}

// buildRootCommand assembles the production Cobra root from the one static
// command registry used by main. Tests may resolve commands from this exact
// tree without executing external command dependencies.
func buildRootCommand() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "peasant",
		Short: "Developer observability for AI coding agent sessions",
		// The root has no work of its own: with no subcommand it prints help,
		// which is what Cobra's non-runnable-root path (flag.ErrHelp) shows
		// today. Being runnable matters only because that same path returns
		// ErrHelp BEFORE PersistentPreRunE runs, which would make the
		// -v/--version alias below unreachable for `peasant -v` itself.
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	rootCmd.PersistentFlags().String("config", string(defaults.Config.FilePath), "Path to config file (default: ~/.config/peasant/config.yaml)")
	rootCmd.PersistentFlags().String("data-dir", "", "Override the data directory (default: $XDG_DATA_HOME or ~/.local/share); the DB + peasant-sync live under <data-dir>/peasant")
	rootCmd.PersistentFlags().String("config-dir", "", "Override the config directory (default: $XDG_CONFIG_HOME or ~/.config); config lives under <config-dir>/peasant")
	rootCmd.PersistentFlags().String("state-dir", "", "Override the state directory (default: $XDG_STATE_HOME or ~/.local/state); logs/PID live under <state-dir>/peasant")
	// -v / --version alias the version subcommand. They are persistent on
	// purpose: `peasant web --version` prints the version and stops before the
	// subcommand runs, which is normal CLI behavior for a persistent flag. The
	// -v shorthand is deliberately reserved: no command under cmd/peasant may
	// claim it while the alias exists.
	//
	// A child command's OWN --version flag shadows this one (verified for
	// `upgrade`, whose --version is a release-tag string): name collisions
	// resolve to the child flag, and the pre-run below ignores a --version
	// that is not the root boolean.
	rootCmd.PersistentFlags().BoolP("version", "v", false, "Print the version")
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		requested, err := cmd.Flags().GetBool("version")
		if err != nil || !requested {
			return nil
		}
		// Same print path as the version subcommand; the sentinel error is
		// then mapped to exit 0 by main. Cobra would otherwise echo the empty
		// sentinel message and the usage block after the pre-run, so silence
		// them for this invocation before returning.
		printVersion(cmd.OutOrStdout())
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		return &versionRequestedError{}
	}

	for _, build := range commands {
		rootCmd.AddCommand(build())
	}
	// Cobra only reaches PersistentPreRunE for a RUNNABLE command; a command
	// that merely groups subcommands short-circuits with flag.ErrHelp first
	// inside execute(). Those groups get a help-printing RunE below, so the
	// version alias reaches `peasant web`, `peasant metrics`, and every other
	// parent exactly like it reaches a leaf command.
	makeGroupCommandsRunnable(rootCmd)
	return rootCmd
}

// makeGroupCommandsRunnable gives every command that only groups subcommands a
// help-printing RunE. Without it, `peasant web --version` would show help
// instead of the version line: a non-runnable command returns flag.ErrHelp
// from execute() BEFORE the persistent pre-run chain runs.
//
// The visible behavior of the group commands is unchanged: `peasant web` still
// prints web's help and exits 0, exactly where the non-runnable ErrHelp path
// ended. Commands that already run something (a builder-owned Run/RunE, such
// as `sessions`) are left untouched.
func makeGroupCommandsRunnable(cmd *cobra.Command) {
	for _, sub := range cmd.Commands() {
		if !sub.Runnable() && len(sub.Commands()) > 0 {
			sub.RunE = func(c *cobra.Command, args []string) error {
				return c.Help()
			}
		}
		makeGroupCommandsRunnable(sub)
	}
}

// exitCodeFor maps a command failure to the status peasant exits with.
//
// Only one distinction is drawn, and it exists for a reader that cannot read
// the message: a generated Git hook sees the status and nothing else. A push
// that failed before sending anything must not be reported by that hook as one
// that published part of its work. The version alias is the mirror image: its
// sentinel error is a success, and main maps it to exit 0.
func exitCodeFor(err error) defaults.ExitCode {
	var versionRequested *versionRequestedError
	if errors.As(err, &versionRequested) {
		return defaults.ExitOK
	}
	var nothingAttempted *pushNothingAttemptedError
	if errors.As(err, &nothingAttempted) {
		return defaults.ExitNothingAttempted
	}
	return defaults.ExitFailure
}
