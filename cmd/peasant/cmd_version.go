package main

import (
	"fmt"
	"io"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/spf13/cobra"
)

// printVersion is the ONE print path for the version line. Both the version
// subcommand and the root -v/--version alias call it, so the three outputs
// cannot drift from each other for the same built binary.
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "peasant %s\n", defaults.Version)
}

// BuildVersionCommand returns the version subcommand. It stays registered even
// though the root aliases -v and --version, so `peasant version` keeps working
// and stays the canonical spelling for scripts and install guides.
func BuildVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, args []string) {
			printVersion(cmd.OutOrStdout())
		},
	}
}

// versionRequestedError is the sentinel the root pre-run returns when -v or
// --version was passed on the command line.
//
// It exists so the alias stays testable: a programmatic Execute of the built
// root command can assert on the printed line AND on the exit mapping that
// main applies, instead of the alias ending in os.Exit(0) inside the pre-run
// where no test can observe it. The same shape is used by
// pushNothingAttemptedError, which also needs a distinct process status.
type versionRequestedError struct{}

// Error returns an empty message: the version line was already printed, and a
// manual -v must not leak an extra error line such as "unknown shorthand".
func (e *versionRequestedError) Error() string { return "" }
