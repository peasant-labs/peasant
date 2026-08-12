package settings

import (
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "shift+g":
		return tea.KeyPressMsg{Code: 'G', Text: "G"}
	default:
		return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
}

func urlAccessor() Accessor[string] {
	return Accessor[string]{
		Get: func(c *config.Config) string { return c.Village.URL },
		Set: func(c *config.Config, v string) { c.Village.URL = v },
	}
}

// testRegistry builds a two-section flow: an always-visible connection toggle,
// and an advanced section shown only when the toggle is on.
func testRegistry() Registry {
	return Registry{Sections: []Section{
		{
			Key:   "connection",
			Title: "connection",
			Fields: []Field{
				Toggle("connected", "connect to village", connectedAccessor()),
			},
		},
		{
			Key:   "advanced",
			Title: "advanced",
			When:  func(d *Draft) bool { return d.Working().Village.Connected },
			Fields: []Field{
				Text("url", "village url", urlAccessor()),
			},
		},
	}}
}

func newTestFlow(t *testing.T) (Flow, *Draft, string) {
	t.Helper()
	path, loaded := writeConfigFile(t)
	d, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	f := NewFlow(theme.New(theme.ModeDark), testRegistry(), d)
	f.SetSize(80, 12)
	return f, d, path
}

func send(f Flow, keys ...string) Flow {
	for _, k := range keys {
		f, _ = f.Update(key(k))
	}
	return f
}

func TestFlow_HelpOverlayOpensAndCloses(t *testing.T) {
	f, d, _ := newTestFlow(t)
	if f.Helping() {
		t.Fatalf("help overlay open before any key press")
	}

	// '?' opens the help overlay.
	f = send(f, "?")
	if !f.Helping() {
		t.Fatalf("'?' did not open the help overlay")
	}

	// While helping, every non-close key is swallowed and cannot leak into the
	// underlying step: tab (advance) and space (toggle the field) must be no-ops.
	stepBefore := f.Step()
	dirtyBefore := d.Dirty()
	f = send(f, "tab", "space")
	if !f.Helping() {
		t.Fatalf("a swallowed key closed the help overlay")
	}
	if f.Step() != stepBefore {
		t.Fatalf("tab leaked past the help overlay: step %d -> %d", stepBefore, f.Step())
	}
	if d.Dirty() != dirtyBefore {
		t.Fatalf("space leaked past the help overlay and mutated the draft")
	}

	// esc closes the overlay and returns to the step.
	f = send(f, "esc")
	if f.Helping() {
		t.Fatalf("esc did not close the help overlay")
	}
	if f.Confirming() {
		t.Fatalf("esc that closed help wrongly opened the exit-confirm modal")
	}
}

func TestFlow_EscAlwaysConfirmsEvenWhenClean(t *testing.T) {
	f, d, _ := newTestFlow(t)
	if d.Dirty() {
		t.Fatalf("draft unexpectedly dirty")
	}
	f = send(f, "esc")
	if !f.Confirming() {
		t.Fatalf("esc did not open the exit-confirm modal on a clean draft")
	}
	// Answering "no" (default) resumes the flow without exiting.
	f = send(f, "enter")
	if f.Confirming() || f.Exited() {
		t.Fatalf("answering no exited or stayed modal: confirming=%v exited=%v", f.Confirming(), f.Exited())
	}
}

func TestFlow_QuitOpensTheSameExitConfirmModal(t *testing.T) {
	f, _, _ := newTestFlow(t)
	if f.Confirming() {
		t.Fatalf("exit-confirm modal open before any key press")
	}
	// q leaves settings and, like esc, prompts the confirm-exit modal first.
	f = send(f, "q")
	if !f.Confirming() {
		t.Fatalf("q did not open the exit-confirm modal")
	}
	// Answering "no" (default) resumes the flow without exiting or committing.
	f = send(f, "enter")
	if f.Confirming() || f.Exited() || f.Committed() {
		t.Fatalf("answering no after q: confirming=%v exited=%v committed=%v",
			f.Confirming(), f.Exited(), f.Committed())
	}
	// esc still opens the same modal after q was cancelled.
	f = send(f, "esc")
	if !f.Confirming() {
		t.Fatalf("esc stopped opening the exit-confirm modal after a q/cancel cycle")
	}
}

func TestFlow_QuitConfirmedExitWritesNothing(t *testing.T) {
	f, _, path := newTestFlow(t)
	before, _ := os.ReadFile(path)

	f = send(f, "space") // toggle connection on (dirty)
	f = send(f, "q")     // q opens the modal
	f = send(f, "left")  // move highlight to "yes"
	f = send(f, "enter") // confirm exit

	if !f.Exited() {
		t.Fatalf("q-confirmed exit did not set Exited")
	}
	if f.Committed() {
		t.Fatalf("q-confirmed exit committed")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("q-confirmed exit wrote to disk")
	}
}

func TestFlow_ConfirmedExitWritesNothing(t *testing.T) {
	f, _, path := newTestFlow(t)
	before, _ := os.ReadFile(path)

	// Make an edit, then confirm exit.
	f = send(f, "space") // toggle connection on (dirty)
	f = send(f, "esc")   // open modal
	f = send(f, "left")  // move highlight to "yes"
	f = send(f, "enter") // confirm exit

	if !f.Exited() {
		t.Fatalf("confirmed exit did not set Exited")
	}
	if f.Committed() {
		t.Fatalf("confirmed exit committed")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("confirmed exit wrote to disk")
	}
}

func TestFlow_ReceiptConfirmIsTheCommitPoint(t *testing.T) {
	f, _, path := newTestFlow(t)
	before, _ := os.ReadFile(path)

	f = send(f, "space") // connection on
	// Advancing off the only visible step reaches the receipt.
	f = send(f, "tab") // into advanced (now visible)
	f = send(f, "tab") // to receipt
	if !f.OnReceipt() {
		t.Fatalf("did not reach receipt; step=%d", f.Step())
	}
	// Nothing is written before the receipt confirm.
	mid, _ := os.ReadFile(path)
	if string(before) != string(mid) {
		t.Fatalf("config written before the receipt confirm (mid-flow save)")
	}

	f = send(f, "enter") // commit
	if !f.Committed() {
		t.Fatalf("receipt confirm did not commit: err=%v", f.Err())
	}
	after, _ := os.ReadFile(path)
	reloaded, err := config.Parse(after)
	if err != nil {
		t.Fatalf("parse committed: %v", err)
	}
	if !reloaded.Village.Connected {
		t.Fatalf("committed config missing the toggle edit")
	}
}

func TestFlow_FreeBackNavRetainsState(t *testing.T) {
	f, d, _ := newTestFlow(t)
	baseURL := d.Baseline().Village.URL
	wantURL := baseURL + "x"
	f = send(f, "space") // connection on
	f = send(f, "tab")   // into advanced
	f = send(f, "x")     // type into the url field
	if got := d.Working().Village.URL; got != wantURL {
		t.Fatalf("url not captured: %q", got)
	}
	f = send(f, "shift+tab") // back to connection
	if f.CurrentSectionKey() != "connection" {
		t.Fatalf("back-nav did not return to connection: %q", f.CurrentSectionKey())
	}
	// State retained across navigation.
	if !d.Working().Village.Connected || d.Working().Village.URL != wantURL {
		t.Fatalf("state not retained: %#v", d.Working().Village)
	}
	f = send(f, "tab") // forward again — advanced still there, value intact
	if f.CurrentSectionKey() != "advanced" || d.Working().Village.URL != wantURL {
		t.Fatalf("forward-nav lost state: section=%q url=%q", f.CurrentSectionKey(), d.Working().Village.URL)
	}
}

func TestFlow_TextInputCapturesPrintableGlobalBindings(t *testing.T) {
	f, d, _ := newTestFlow(t)
	baseURL := d.Baseline().Village.URL
	f = send(f, "space")
	f = send(f, "tab")
	f = send(f, "q", "b", "?")

	if got, want := d.Working().Village.URL, baseURL+"qb?"; got != want {
		t.Fatalf("focused text field captured %q, want %q", got, want)
	}
	if f.Exited() || f.Confirming() || f.Helping() {
		t.Fatal("printable text leaked into quit, back, or help lifecycle handling")
	}
}

func TestFlow_HiddenStepDropsEditsAndReceiptReflects(t *testing.T) {
	f, d, _ := newTestFlow(t)
	baseURL := d.Baseline().Village.URL
	f = send(f, "space") // connection on -> advanced visible
	f = send(f, "tab")   // into advanced
	f = send(f, "x")     // edit the url
	if d.Working().Village.URL == baseURL {
		t.Fatalf("precondition: url not edited")
	}
	f = send(f, "shift+tab") // back to connection
	f = send(f, "space")     // connection OFF -> advanced now hidden
	f = send(f, "tab")       // advance -> reaches receipt (advanced dropped)

	if !f.OnReceipt() {
		t.Fatalf("did not reach receipt; step=%d", f.Step())
	}
	// The hidden step's edit was dropped.
	if d.Working().Village.URL != baseURL {
		t.Fatalf("hidden-step url edit not dropped: %q (baseline %q)", d.Working().Village.URL, baseURL)
	}
	// Connection returned to baseline too, so the receipt reports no changes.
	body := f.View()
	if !strings.Contains(body, "no changes") {
		t.Fatalf("receipt did not reflect the dropped edits:\n%s", body)
	}
}

func TestFlow_ConsentContextIsConvergedAndReadOnly(t *testing.T) {
	path, loaded := writeConfigFile(t)
	draft, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	baseURL := draft.Baseline().Village.URL
	providerCalls := 0
	flow := NewFlow(theme.New(theme.ModeDark), testRegistry(), draft,
		WithConsentSummary(func(ctx ConsentContext) (ConsentSummary, error) {
			providerCalls++
			if !ctx.HasVisibleField("connection", "connected") {
				t.Error("consent context omitted the visible connection field")
			}
			if ctx.HasVisibleField("advanced", "url") {
				t.Error("consent context retained a field hidden by converged draft state")
			}
			visible := ctx.VisibleFields()
			if len(visible) != 1 || visible[0].Kind != KindToggle {
				t.Errorf("visible consent fields = %#v, want one connection toggle", visible)
			}
			snapshot, snapshotErr := ctx.Config()
			if snapshotErr != nil {
				return ConsentSummary{}, snapshotErr
			}
			if snapshot.Village.Connected || snapshot.Village.URL != baseURL {
				t.Errorf("converged consent snapshot = %#v, want hidden edit reset", snapshot.Village)
			}
			snapshot.Village.URL = "https://mutated.example.test"
			return ConsentSummary{Values: []string{"converged context observed"}}, nil
		}))
	flow.SetSize(120, 24)

	flow = send(flow, "space", "tab", "x", "shift+tab", "space", "tab")
	if !flow.OnReceipt() {
		t.Fatalf("did not reach receipt; step=%d", flow.Step())
	}
	view := flow.View()
	if providerCalls != 1 || !strings.Contains(view, "converged context observed") {
		t.Fatalf("consent provider calls/view = %d/%q, want one mounted receipt call", providerCalls, view)
	}
	if got := draft.Working().Village.URL; got != baseURL {
		t.Fatalf("mutating detached consent snapshot changed Draft URL to %q, want %q", got, baseURL)
	}
}
