package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/spf13/cobra"
)

// TestBuildMigrateCommandFlags proves the skeleton carries the reclaim-mirror
// flag set from the design (§7.2): --dry-run, --limit, --confirm, --json.
func TestBuildMigrateCommandFlags(t *testing.T) {
	cmd := BuildMigrateCommand()
	for _, name := range []string{"dry-run", "limit", "confirm", defaults.JSONFlagName} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("migrate is missing --%s", name)
		}
	}
	if got := cmd.Flags().Lookup("limit").DefValue; got != "0" {
		t.Errorf("--limit default = %q, want 0 (all)", got)
	}
}

// TestBuildMigrateCommandRegistered proves `peasant migrate` resolves from the
// production root command tree.
func TestBuildMigrateCommandRegistered(t *testing.T) {
	root := buildRootCommand()
	found := false
	for _, sub := range root.Commands() {
		if sub.Name() == "migrate" {
			found = true
		}
	}
	if !found {
		t.Fatal("`peasant migrate` is not registered on the root command")
	}
}

// TestBuildMigrateCommandNotImplemented proves the skeleton refuses with the
// not-implemented error instead of running: nothing is converted and nothing
// is deleted.
func TestBuildMigrateCommandNotImplemented(t *testing.T) {
	for _, args := range [][]string{{}, {"--dry-run"}, {"--limit", "5"}, {"--confirm"}, {"--json"}} {
		cmd := BuildMigrateCommand()
		cmd.SetArgs(args)
		err := cmd.Execute()
		if !errors.Is(err, errMigrateNotImplemented) {
			t.Errorf("migrate %v: Execute() = %v, want errMigrateNotImplemented", args, err)
		}
	}
}

// TestRunVerifyContentNotImplemented proves `harvest verify --content`
// refuses before opening the database: the content check has no engine yet.
func TestRunVerifyContentNotImplemented(t *testing.T) {
	err := runVerify(&cobra.Command{}, false, true, false)
	if err == nil || !strings.Contains(err.Error(), "--content is not implemented yet") {
		t.Fatalf("runVerify(content) = %v, want the --content not-implemented refusal", err)
	}
}

// TestRunVerifyRepairRequiresContent proves --repair without --content names
// the fix instead of running a schema check the caller did not ask for.
func TestRunVerifyRepairRequiresContent(t *testing.T) {
	err := runVerify(&cobra.Command{}, false, false, true)
	if err == nil || !strings.Contains(err.Error(), "--repair requires --content") {
		t.Fatalf("runVerify(repair without content) = %v, want the --repair refusal", err)
	}
}

// TestVerifyCommandFlags proves the additive flags exist on the production
// verify subcommand without changing the existing --verbose flag.
func TestVerifyCommandFlags(t *testing.T) {
	root := buildRootCommand()
	harvest, _, err := root.Find([]string{"harvest", "verify"})
	if err != nil || harvest == nil || harvest.Name() != "verify" {
		t.Fatalf("cannot resolve `peasant harvest verify`: %v", err)
	}
	for _, name := range []string{"verbose", "content", "repair"} {
		if harvest.Flags().Lookup(name) == nil {
			t.Errorf("harvest verify is missing --%s", name)
		}
	}
}
