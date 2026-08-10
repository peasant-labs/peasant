package settings

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

func selectionAccessor() Accessor[TreeSelection] {
	return Accessor[TreeSelection]{
		Get: func(c *config.Config) TreeSelection {
			return TreeSelection{Mode: c.Selection.Mode, Harnesses: c.Selection.Harnesses}
		},
		Set: func(c *config.Config, ts TreeSelection) {
			c.Selection.Mode = ts.Mode
			c.Selection.Harnesses = ts.Harnesses
		},
	}
}

func treeRegistry(src kit.TreeSource) Registry {
	return Registry{Sections: []Section{
		{
			Key:    "transcripts",
			Title:  "select transcripts",
			Fields: []Field{Tree("selection", "transcripts", selectionAccessor(), src, WithSelectionRestoration())},
		},
	}}
}

// runAll recursively runs a command (expanding tea.BatchMsg) and returns every
// leaf message it produced. It runs each leaf command exactly once, so a
// self-perpetuating spinner tick does not loop.
func runAll(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, runAll(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// drainInit runs the flow's startup commands and feeds their messages back in,
// completing the tree's fixture load synchronously.
func drainInit(f Flow) Flow {
	for _, m := range runAll(f.Init()) {
		f, _ = f.Update(m)
	}
	return f
}

func TestFlow_TreeFieldCommitsSelection(t *testing.T) {
	path, loaded := writeConfigFile(t)
	loaded.Selection = config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			string(defaults.LegacyHarnessClaude): {Sessions: []string{"sess-p1"}},
			string(defaults.LegacyHarnessGemini): {Sessions: []string{"sess-v1", "sess-v2"}},
		},
	}
	if err := config.SaveAtomic(path, loaded); err != nil {
		t.Fatalf("save selected baseline: %v", err)
	}
	d, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	src := scannerfix.NewFixtureTreeSource("standard")
	f := NewFlow(theme.New(theme.ModeDark), treeRegistry(src), d)
	f.SetSize(80, 20)
	f = drainInit(f)

	// The saved selection is restored into the standard fixture and derived as a
	// selected per-harness allowlist through the accessor.
	if d.Working().Selection.Mode != config.SelectionModeSelected {
		t.Fatalf("selection mode after load = %q", d.Working().Selection.Mode)
	}

	f = send(f, "tab") // to receipt
	if !f.OnReceipt() {
		t.Fatalf("did not reach receipt")
	}
	f = send(f, "enter") // commit
	if !f.Committed() {
		t.Fatalf("tree flow did not commit: err=%v", f.Err())
	}

	reloaded, err := config.Parse(mustRead(t, path))
	if err != nil {
		t.Fatalf("parse committed: %v", err)
	}
	if reloaded.Selection.Mode != config.SelectionModeSelected {
		t.Fatalf("committed selection mode = %q", reloaded.Selection.Mode)
	}
	if len(reloaded.Selection.Harnesses) == 0 {
		t.Fatalf("committed selection has no harness allowlist")
	}
}

func TestFlow_TreeFieldRestoresSavedSelectionOnFirstSuccessfulLoad(t *testing.T) {
	path, loaded := writeConfigFile(t)
	loaded.Selection = config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			string(defaults.LegacyHarnessClaude): {Sessions: []string{"sess-p2"}},
		},
	}
	if err := config.SaveAtomic(path, loaded); err != nil {
		t.Fatalf("save selected baseline: %v", err)
	}
	draft, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	flow := NewFlow(theme.New(theme.ModeDark), treeRegistry(scannerfix.NewFixtureTreeSource("standard")), draft)
	flow.SetSize(80, 20)
	flow = drainInit(flow)

	selection := draft.Working().Selection
	claude := selection.Harnesses[string(defaults.LegacyHarnessClaude)]
	if selection.Mode != config.SelectionModeSelected || len(claude.Sessions) != 1 || claude.Sessions[0] != "sess-p2" {
		t.Fatalf("first successful load replaced saved selection: %#v", selection)
	}
	if len(claude.Projects) != 0 || len(selection.Harnesses) != 1 {
		t.Fatalf("first successful load retained fixture checks instead of the saved baseline: %#v", selection)
	}
}

func TestFlow_GenericTreeKeepsGenericSelectAllHelp(t *testing.T) {
	path, loaded := writeConfigFile(t)
	loaded.Selection.Mode = config.SelectionModeSelected
	draft, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	flow := NewFlow(theme.New(theme.ModeDark), treeRegistry(scannerfix.NewFixtureTreeSource("standard")), draft)
	flow.SetSize(100, 20)
	flow = drainInit(flow)
	flow = send(flow, "?")

	view := flow.View()
	if !strings.Contains(view, "select all") {
		t.Fatalf("generic tree footer lost select-all help:\n%s", view)
	}
	if strings.Contains(view, "select all projects") {
		t.Fatalf("generic tree inherited kickstart-only project wording:\n%s", view)
	}
}

func TestFlow_TreeFieldRefreshPreservesCurrentEdits(t *testing.T) {
	path, loaded := writeConfigFile(t)
	loaded.Selection = config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			string(defaults.LegacyHarnessClaude): {Sessions: []string{"sess-p2"}},
		},
	}
	if err := config.SaveAtomic(path, loaded); err != nil {
		t.Fatalf("save selected baseline: %v", err)
	}
	draft, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	registry := treeRegistry(scannerfix.NewFixtureTreeSource("standard"))
	field, ok := registry.Sections[0].Fields[0].(*treeField)
	if !ok {
		t.Fatalf("tree registry field has type %T", registry.Sections[0].Fields[0])
	}
	flow := NewFlow(theme.New(theme.ModeDark), registry, draft)
	flow.SetSize(80, 20)
	flow = drainInit(flow)

	// The cursor starts on the Claude root. Toggling it changes the on-screen
	// edit from one explicit session to the whole available Claude project.
	flow = send(flow, "space")
	beforeRefresh := selectionAccessor().Get(draft.Working())
	claude := beforeRefresh.Harnesses[string(defaults.LegacyHarnessClaude)]
	if len(claude.Projects) != 1 || len(claude.Sessions) != 0 {
		t.Fatalf("user edit did not select the current Claude project: %#v", beforeRefresh)
	}

	var refresh tea.Cmd
	field.tree, refresh = field.tree.Load()
	for _, message := range runAll(refresh) {
		flow, _ = flow.Update(message)
	}
	afterRefresh := selectionAccessor().Get(draft.Working())
	if !selectionsEqual(afterRefresh, beforeRefresh) {
		t.Fatalf("refresh reapplied the saved baseline over current edits\n before: %#v\n  after: %#v", beforeRefresh, afterRefresh)
	}
}

type failFirstTreeSource struct {
	attempts int
	inner    kit.TreeSource
}

func (s *failFirstTreeSource) Load(ctx context.Context) ([]*kit.TreeNode, error) {
	s.attempts++
	if s.attempts == 1 {
		return nil, fmt.Errorf("fixture scan failed before a successful load")
	}
	return s.inner.Load(ctx)
}

func TestFlow_TreeFieldWaitsForFirstSuccessfulLoad(t *testing.T) {
	path, loaded := writeConfigFile(t)
	loaded.Selection = config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			string(defaults.LegacyHarnessClaude): {Sessions: []string{"sess-p2"}},
		},
	}
	if err := config.SaveAtomic(path, loaded); err != nil {
		t.Fatalf("save selected baseline: %v", err)
	}
	draft, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	source := &failFirstTreeSource{inner: scannerfix.NewFixtureTreeSource("standard")}
	registry := treeRegistry(source)
	field := registry.Sections[0].Fields[0].(*treeField)
	flow := NewFlow(theme.New(theme.ModeDark), registry, draft)
	flow.SetSize(80, 20)
	flow = drainInit(flow)
	if field.baselineApplied {
		t.Fatal("failed load consumed the one baseline-application opportunity")
	}

	var retry tea.Cmd
	field.tree, retry = field.tree.Load()
	for _, message := range runAll(retry) {
		flow, _ = flow.Update(message)
	}
	if !field.baselineApplied {
		t.Fatal("first successful load did not apply the saved baseline")
	}
	selection := draft.Working().Selection.Harnesses[string(defaults.LegacyHarnessClaude)]
	if len(selection.Sessions) != 1 || selection.Sessions[0] != "sess-p2" {
		t.Fatalf("retry did not restore saved session: %#v", draft.Working().Selection)
	}
}

var _ kit.TreeSource = (*failFirstTreeSource)(nil)

func TestFlow_ConflictBlocksReceiptWithActionableError(t *testing.T) {
	path, loaded := writeConfigFile(t)
	before := mustRead(t, path)
	d, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	src := scannerfix.NewFixtureTreeSource("conflict")
	f := NewFlow(theme.New(theme.ModeDark), treeRegistry(src), d)
	f.SetSize(80, 20)
	f = drainInit(f)

	f = send(f, "tab")   // to receipt
	f = send(f, "enter") // attempt commit — blocked by the conflict node

	if f.Committed() {
		t.Fatalf("conflict selection was committed")
	}
	if f.Err() == nil {
		t.Fatalf("no error surfaced for the conflict")
	}
	for _, part := range []string{"what:", "why:", "fix:", "conflict"} {
		if !strings.Contains(f.Err().Error(), part) {
			t.Fatalf("error not actionable (missing %q): %v", part, f.Err())
		}
	}
	// Fail closed: nothing written.
	if string(before) != string(mustRead(t, path)) {
		t.Fatalf("blocked commit still wrote to disk")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
