package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
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
	assertKickstartProductionFactoryDelegation(t)

	var runnerCalls int
	var legacyCalls int
	deps := defaultKickstartCommandDeps()
	if deps.runFlow == nil || deps.runModel == nil || deps.readRetention == nil {
		t.Fatal("production kickstart defaults do not select the complete guided path")
	}
	if reflect.ValueOf(deps.runFlow).Pointer() != reflect.ValueOf(runKickstartFlow).Pointer() {
		t.Fatal("production kickstart defaults do not select runKickstartFlow")
	}
	deps.discover = func(context.Context, string, string, *discoverySpinner) (ftue.ProviderInventory, []ftue.SessionListing) {
		return ftue.ProviderInventory{}, nil
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

// assertKickstartProductionFactoryDelegation pins the actual exported factory
// to the mounted default-dependency builder. The behavioral half of the test
// above replaces only external boundaries on those defaults. Keeping this
// direct-delegation assertion alongside it means the catalog cannot quietly
// clear the guided runner and fall through to the retained legacy wizard while
// a parallel test command remains green.
func assertKickstartProductionFactoryDelegation(t *testing.T) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "cmd_kickstart.go", nil, 0)
	if err != nil {
		t.Fatalf("parse production kickstart factory: %v", err)
	}
	var factory *ast.FuncDecl
	for _, declaration := range file.Decls {
		candidate, ok := declaration.(*ast.FuncDecl)
		if ok && candidate.Name.Name == "BuildKickstartCommand" {
			factory = candidate
			break
		}
	}
	if factory == nil || factory.Body == nil || len(factory.Body.List) != 1 {
		t.Fatal("BuildKickstartCommand must directly delegate once to the production default builder")
	}
	result, ok := factory.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(result.Results) != 1 {
		t.Fatal("BuildKickstartCommand must directly return the production default builder")
	}
	buildCall, ok := result.Results[0].(*ast.CallExpr)
	if !ok || len(buildCall.Args) != 1 || !identifierNamed(buildCall.Fun, "buildKickstartCommand") {
		t.Fatal("BuildKickstartCommand must directly call buildKickstartCommand with one defaults argument")
	}
	defaultsCall, ok := buildCall.Args[0].(*ast.CallExpr)
	if !ok || len(defaultsCall.Args) != 0 || !identifierNamed(defaultsCall.Fun, "defaultKickstartCommandDeps") {
		t.Fatal("BuildKickstartCommand must pass defaultKickstartCommandDeps directly without overriding the guided runner")
	}
}

func identifierNamed(expression ast.Expr, name string) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == name
}
