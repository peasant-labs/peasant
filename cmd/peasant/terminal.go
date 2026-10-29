package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// noTerminalError reports that an interactive command could not reach a
// terminal. It replaces Bubble Tea's raw "could not open TTY" error so a piped,
// scripted, or detached invocation gets one actionable line and no usage dump.
type noTerminalError struct{ command string }

func (e noTerminalError) Error() string {
	msg := fmt.Sprintf("`peasant %s` needs an interactive terminal; run it from a terminal session", e.command)
	if alt := noTerminalAlternative[e.command]; alt != "" {
		msg += " (" + alt + ")"
	}
	return msg
}

// noTerminalAlternative names what to run instead of each interactive command
// when there is no terminal.
var noTerminalAlternative = map[string]string{
	"kickstart": "to ingest without the wizard, use `peasant harvest`",
	"config":    "to change settings without it, edit config.yaml; `peasant --help` shows its path under --config",
	"tui":       "for the dashboard, sessions and trends without a terminal UI, use `peasant web start`",
}

// terminalAvailable reports whether a Bubble Tea program can both read keys
// and draw: stdout must be a terminal (so escape codes never land in a pipe or
// file), and input must be reachable (see inputTerminalAvailable, which is
// platform-specific).
func terminalAvailable() bool {
	return term.IsTerminal(int(os.Stdout.Fd())) && inputTerminalAvailable()
}

// requireTerminal is the check every production Bubble Tea boundary runs
// before it starts a program.
func requireTerminal(command string) error {
	if !terminalAvailable() {
		return noTerminalError{command: command}
	}
	return nil
}

// cleanTerminalError unwraps a noTerminalError from a command's error chain,
// returns it on its own, and silences cobra's usage block, which only helps
// with argument mistakes.
func cleanTerminalError(cmd *cobra.Command, err error) error {
	var noTTY noTerminalError
	if errors.As(err, &noTTY) {
		cmd.SilenceUsage = true
		return noTTY
	}
	return err
}

// withCleanTerminalError wraps cmd's RunE with cleanTerminalError.
func withCleanTerminalError(cmd *cobra.Command) *cobra.Command {
	inner := cmd.RunE
	cmd.RunE = func(c *cobra.Command, args []string) error {
		return cleanTerminalError(c, inner(c, args))
	}
	return cmd
}
