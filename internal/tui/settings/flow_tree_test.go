package settings

import (
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/config"
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
	return treeRegistryWithLabel(src, "transcripts")
}

func treeRegistryWithLabel(src kit.TreeSource, label string) Registry {
	return Registry{Sections: []Section{
		{
			Key:    "transcripts",
			Title:  "select transcripts",
			Fields: []Field{Tree("selection", label, selectionAccessor(), src)},
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
	d, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	src := scannerfix.NewFixtureTreeSource("standard")
	f := NewFlow(theme.New(theme.ModeDark), treeRegistry(src), d)
	f.SetSize(80, 20)
	f = drainInit(f)

	// The standard fixture has a partial selection, so the derived selection is
	// "selected" with a per-harness allowlist — written through the accessor.
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

func TestFlow_ConflictBlocksReceiptWithActionableError(t *testing.T) {
	path, loaded := writeConfigFile(t)
	before := mustRead(t, path)
	d, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	src := scannerfix.NewFixtureTreeSource("conflict")
	f := NewFlow(theme.New(theme.ModeDark), treeRegistryWithLabel(src, ""), d)
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
	errorText := f.Err().Error()
	if !strings.Contains(errorText, "the transcript selection still contains") || strings.Contains(errorText, `selection ""`) {
		t.Fatalf("blank presentation label leaked into conflict wording: %v", f.Err())
	}
	assertActionableScreenError(t, f.Err())
	if !strings.Contains(errorText, "conflict") {
		t.Fatalf("conflict error omitted its cause: %v", f.Err())
	}
	if !strings.Contains(errorText, "when: validating the final review before the atomic config commit.") {
		t.Fatalf("conflict error omitted the commit-stage timing: %v", f.Err())
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
