package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

// TestKickstartCommandMountsGuidedProgram proves the real Cobra command reaches
// the production Program rather than a test-only handler. Discovery and the Tea
// runner are external boundaries; Program, Flow, BuildRegistry, fields, and
// Draft remain the real mounted path.
func TestKickstartCommandMountsGuidedProgram(t *testing.T) {
	t.Parallel()
	wantBuilder := reflect.ValueOf(BuildKickstartCommand).Pointer()
	var catalogBuilder func() *cobra.Command
	for _, build := range commands {
		if reflect.ValueOf(build).Pointer() == wantBuilder {
			catalogBuilder = build
			break
		}
	}
	if catalogBuilder == nil {
		t.Fatal("production command catalog does not mount BuildKickstartCommand")
	}
	if command := catalogBuilder(); command.Name() != "kickstart" {
		t.Fatalf("cataloged BuildKickstartCommand produced %q, want kickstart", command.Name())
	}

	var runnerCalls int
	var legacyCalls int
	deps := defaultKickstartCommandDeps()
	if deps.runFlow == nil || deps.runModel == nil || deps.readRetention == nil {
		t.Fatal("production kickstart defaults do not select the complete guided path")
	}
	if reflect.ValueOf(deps.runFlow).Pointer() != reflect.ValueOf(runKickstartFlow).Pointer() {
		t.Fatal("production kickstart defaults do not select runKickstartFlow")
	}
	deps.discover = func(context.Context, string, string, *discoverySpinner) (ftue.ProviderInventory, []ftue.SessionListing, kickstart.SubagentRelation) {
		return ftue.ProviderInventory{}, nil, nil
	}
	deps.existingUser = func(string) string { return "" }
	deps.readRetention = func() (int, bool) { return 90, true }
	deps.run = func(ftue.WizardModel) error {
		legacyCalls++
		return nil
	}
	deps.runModel = func(model tea.Model) error {
		runnerCalls++
		mounted, ok := model.(kickstart.Model)
		if !ok {
			t.Fatalf("kickstart Tea runner received %T, want the production kickstart.Model", model)
		}
		program := mounted.Program()
		program.SetSize(120, 40)
		connectView := ansiPattern.ReplaceAllString(program.View(), "")
		if !strings.Contains(connectView, "connect to a village") {
			t.Fatalf("production command did not mount connect framing:\n%s", connectView)
		}
		program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		flowView := ansiPattern.ReplaceAllString(program.View(), "")
		if !strings.Contains(flowView, "choose sessions to import") {
			t.Fatalf("production command did not mount selection section:\n%s", flowView)
		}
		assertMountedSelectionSearch(t, flowView)
		return nil
	}

	if _, err := executeWithDataDir(t, buildKickstartCommand(deps), t.TempDir(), nil); err != nil {
		t.Fatalf("run production kickstart command: %v", err)
	}
	if runnerCalls != 1 {
		t.Fatalf("production kickstart command called the Tea runner %d times, want 1", runnerCalls)
	}
	if legacyCalls != 0 {
		t.Fatalf("production kickstart command selected the retained legacy runner %d times", legacyCalls)
	}
}
