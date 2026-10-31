package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

// seedResetFixture writes a config, a database, a peasant-sync transcript and a
// state file under isolated directories and returns their paths.
func seedResetFixture(t *testing.T) (dataDir, configDir, stateDir string, paths []string) {
	t.Helper()
	// Every location is a per-test directory passed by flag, so the tests stay
	// parallel and never resolve the developer's real state directory.
	dataDir, configDir, stateDir = t.TempDir(), t.TempDir(), t.TempDir()
	paths = []string{
		defaults.ResolveConfigFilePathWith(configDir).String(),
		string(defaults.ResolveDBFilePathWith(dataDir)),
		// SQLite sidecars a killed process leaves behind (for example
		// `peasant sessions list | head` dying of SIGPIPE). A stale WAL left
		// beside the next, fresh database could be replayed into it.
		string(defaults.ResolveDBFilePathWith(dataDir)) + "-wal",
		string(defaults.ResolveDBFilePathWith(dataDir)) + "-shm",
		string(defaults.ResolveDBFilePathWith(dataDir)) + "-journal",
		filepath.Join(string(defaults.ResolveDataDirPathWith(dataDir)), "peasant-sync", "proj", "s1", "s1--transcript.jsonl"),
		filepath.Join(string(defaults.ResolveStateDirPathWith(stateDir)), "web-18690.pid"),
	}
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dataDir, configDir, stateDir, paths
}

func runKickstartReset(t *testing.T, dataDir, configDir, stateDir string, stdin *bytes.Buffer, args ...string) (error, bool) {
	t.Helper()
	deps := defaultKickstartCommandDeps()
	wizardReached := false
	deps.discover = func(context.Context, string, string, *discoverySpinner) (ftue.ProviderInventory, []ftue.SessionListing, kickstart.SubagentRelation) {
		wizardReached = true
		return ftue.ProviderInventory{}, nil, nil
	}
	deps.runFlow = func(*cobra.Command, kickstartCommandDeps, string, ftue.ProviderInventory, []ftue.SessionListing, kickstart.SubagentRelation) error {
		return nil
	}
	root := newTestRoot()
	root.AddCommand(buildKickstartCommand(deps))
	root.SetIn(stdin)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"--data-dir", dataDir, "--config-dir", configDir, "--state-dir", stateDir, "kickstart", "--reset"}, args...))
	return root.Execute(), wizardReached
}

// TestKickstartResetRefusesWithoutConsent proves --reset deletes nothing when
// its input is not a terminal and --confirm is absent.
func TestKickstartResetRefusesWithoutConsent(t *testing.T) {
	t.Parallel()
	dataDir, configDir, stateDir, paths := seedResetFixture(t)
	err, reached := runKickstartReset(t, dataDir, configDir, stateDir, bytes.NewBufferString("y\n"))
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("kickstart --reset without a terminal: err = %v, want a refusal naming --confirm", err)
	}
	if reached {
		t.Fatal("kickstart continued into discovery after refusing the reset")
	}
	for _, p := range paths {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Fatalf("refused reset still removed %s: %v", p, statErr)
		}
	}
}

// TestKickstartResetWithConfirmRemovesEverything keeps the scripted path.
func TestKickstartResetWithConfirmRemovesEverything(t *testing.T) {
	t.Parallel()
	dataDir, configDir, stateDir, paths := seedResetFixture(t)
	if err, _ := runKickstartReset(t, dataDir, configDir, stateDir, &bytes.Buffer{}, "--confirm"); err != nil {
		t.Fatalf("kickstart --reset --confirm: %v", err)
	}
	for _, p := range paths {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Fatalf("confirmed reset left %s (stat err %v)", p, statErr)
		}
	}
}

// TestKickstartResetPromptAnswers covers the interactive answers: only y or Y
// consents; N, an empty line and EOF keep everything. It also checks the
// prompt names the --state-dir location it would remove.
func TestKickstartResetPromptAnswers(t *testing.T) {
	t.Parallel()
	for _, c := range loadConsentAnswers(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			stateDir := t.TempDir()
			var prompt bytes.Buffer
			got := promptKickstartReset(&prompt, strings.NewReader(c.Input), kickstartResetTargets("cfg.yaml", "", stateDir))
			if got != c.Want {
				t.Fatalf("answer %q: consent = %v, want %v", c.Input, got, c.Want)
			}
			if !strings.Contains(prompt.String(), string(defaults.ResolveStateDirPathWith(stateDir))) || !strings.Contains(prompt.String(), "[y/N]") {
				t.Fatalf("prompt does not name the --state-dir location:\n%s", prompt.String())
			}
		})
	}
}

// TestKickstartConfirmRequiresReset proves --confirm alone is rejected before
// anything runs, instead of being silently ignored.
func TestKickstartConfirmRequiresReset(t *testing.T) {
	t.Parallel()
	deps := defaultKickstartCommandDeps()
	reached := false
	deps.discover = func(context.Context, string, string, *discoverySpinner) (ftue.ProviderInventory, []ftue.SessionListing, kickstart.SubagentRelation) {
		reached = true
		return ftue.ProviderInventory{}, nil, nil
	}
	root := newTestRoot()
	root.AddCommand(buildKickstartCommand(deps))
	root.SetIn(&bytes.Buffer{})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--data-dir", t.TempDir(), "--config-dir", t.TempDir(), "--state-dir", t.TempDir(), "kickstart", "--confirm"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--confirm only applies with --reset") {
		t.Fatalf("kickstart --confirm without --reset: err = %v", err)
	}
	if reached {
		t.Fatal("kickstart --confirm without --reset started discovery")
	}
}
