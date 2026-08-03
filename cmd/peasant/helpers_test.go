package main

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
)

// newTestRoot builds a root command carrying the persistent --data-dir flag
// (mirrors main()), so a subcommand executed in a test honors --data-dir
// without mutating process-global env. This is what lets the command-driven
// tests run with t.Parallel() instead of t.Setenv("XDG_DATA_HOME", ...).
func newTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	root.PersistentFlags().String("state-dir", "", "")
	root.PersistentFlags().String("config", "", "")
	return root
}

// executeWithDataDir runs sub under a fresh root with --data-dir AND --config-dir
// both pointed at dir (a single t.TempDir() isolates data + config), capturing
// combined stdout+stderr. args are the sub-args (e.g. {"compute", "--dry-run"});
// the subcommand name is inserted automatically. Parallel-safe: it injects the
// dirs per-invocation via flags, touching no process env — so callers use
// t.Parallel() instead of t.Setenv. --config defaults to "" (no real config
// file is read; commands fall back to built-in defaults).
func executeWithDataDir(t *testing.T, sub *cobra.Command, dir string, args []string) (string, error) {
	t.Helper()
	root := newTestRoot()
	root.AddCommand(sub)
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, "--state-dir", dir, sub.Name()}, args...))
	err := root.Execute()
	return buf.String(), err
}

// buildTestRootCmd constructs the root command tree for testing using the real
// command builders (mirrors main()).
func buildTestRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{Use: "peasant"}
	rootCmd.AddCommand(BuildKickstartCommand())
	return rootCmd
}

func containsString(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}

// keysOf returns the keys of a map[string]interface{} for readable diagnostics.
func keysOf(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// subcommandNames returns the names of all subcommands under cmd.
func subcommandNames(cmd *cobra.Command) []string {
	names := make([]string, len(cmd.Commands()))
	for i, c := range cmd.Commands() {
		names[i] = c.Name()
	}
	return names
}
