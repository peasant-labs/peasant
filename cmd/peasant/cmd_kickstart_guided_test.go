package main

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

// TestKickstartCommandMountsGuidedProgram proves the real Cobra command reaches
// the production Program rather than a test-only handler. Discovery and the Tea
// runner are external boundaries; Program, Flow, BuildRegistry, fields, and
// Draft remain the real mounted path.
func TestKickstartCommandMountsGuidedProgram(t *testing.T) {
	t.Parallel()
	var runnerCalls int
	deps := defaultKickstartCommandDeps()
	deps.discover = func(context.Context, string, string, *discoverySpinner) (ftue.ProviderInventory, []ftue.SessionListing) {
		return ftue.ProviderInventory{}, nil
	}
	deps.existingUser = func(string) string { return "" }
	deps.runFlow = func(model tea.Model) error {
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
		if !strings.Contains(flowView, "choose which recorded sessions to import.") {
			t.Fatalf("production command did not mount guided registry framing:\n%s", flowView)
		}
		return nil
	}

	if _, err := executeWithDataDir(t, buildKickstartCommand(deps), t.TempDir(), nil); err != nil {
		t.Fatalf("run production kickstart command: %v", err)
	}
	if runnerCalls != 1 {
		t.Fatalf("production kickstart command called the Tea runner %d times, want 1", runnerCalls)
	}
}
