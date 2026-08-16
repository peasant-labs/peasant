package kickstart_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// newProgramHarness builds a Program over a temp config with a fixture scanner
// source, plus a recorder of the effect order (config-save vs ingest vs
// retention) so the ordering invariants can be asserted. The returned draft is
// the same one the flow commits.
type effectLog struct{ events []string }

func newTestProgram(t *testing.T, deps kickstart.ProgramDeps) (kickstart.Program, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, config.BaseConfig()); err != nil {
		t.Fatalf("seed base config: %v", err)
	}
	loaded, err := config.Parse(mustReadFile(t, path))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}
	deps.Theme = theme.New(theme.ModeDark)
	deps.Draft = draft
	if deps.Source == nil {
		deps.Source = scannerfix.NewFixtureTreeSource("standard")
	}
	p := kickstart.NewProgram(deps)
	p.SetSize(80, 24)
	return p, path
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return data
}

func press(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

// declineOAuth answers the connect-now confirm with "no" (default focus), which
// skips login and enters the flow. It returns the program in PhaseFlow.
func declineOAuth(t *testing.T, p kickstart.Program) kickstart.Program {
	t.Helper()
	var command tea.Cmd
	p, command = p.Update(press(tea.KeyEnter)) // default focus is "no" -> skip
	if p.Phase() != kickstart.PhaseFlow {
		t.Fatalf("after declining OAuth, phase = %s, want flow", p.Phase())
	}
	return drainProgram(p, command)
}

// advanceToCommit tabs through the flow steps to the receipt and confirms,
// committing the config. Extra tabs on the receipt are no-ops. It returns the
// command the commit transition produced (the batched ingest+spinner start).
func advanceToCommit(p kickstart.Program) (kickstart.Program, tea.Cmd) {
	for i := 0; i < 8; i++ {
		p, _ = p.Update(press(tea.KeyTab))
	}
	var cmd tea.Cmd
	p, cmd = p.Update(press(tea.KeyEnter)) // receipt confirm = commit
	return p, cmd
}

// drainCmds runs a command to completion, unwrapping one level of tea.BatchMsg,
// and feeds every produced message back through the program until it settles.
// This mirrors how the bubbletea runtime would execute the batched ingest and
// spinner-tick commands the program emits after commit.
func drainCmds(t *testing.T, p kickstart.Program, cmd tea.Cmd) kickstart.Program {
	t.Helper()
	queue := []tea.Cmd{cmd}
	// Bounded: spinner ticks re-queue themselves, so the loop is capped rather
	// than run to a (never-reached) empty queue.
	for iter := 0; len(queue) > 0 && iter < 200; iter++ {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		switch m := msg.(type) {
		case tea.BatchMsg:
			queue = append(queue, m...)
		case nil:
			// nothing
		default:
			var next tea.Cmd
			p, next = p.Update(m)
			if next != nil {
				queue = append(queue, next)
			}
		}
		if p.Phase() == kickstart.PhaseDone {
			break
		}
	}
	return p
}

// TestProgram_IngestRunsOnlyAfterCommit proves the legacy ordering: the ingest
// runner is not invoked until the flow's atomic commit has persisted the config.
func TestProgram_IngestRunsOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	log := &effectLog{}
	var committedAtIngest bool
	var configPath string

	// Seed a config whose base selection is mode:all, then have the flow commit a
	// DISTINCT non-default selection (mode:selected with an explicit session
	// allowlist). The on-disk file must show mode:all until the commit lands, so
	// the ordering assertion is non-vacuous: reading base would fail it.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, config.BaseConfig()); err != nil {
		t.Fatalf("seed base config: %v", err)
	}
	configPath = path
	baseOnDisk, _ := config.Parse(mustReadFile(t, path))
	if baseOnDisk.Selection.Mode != config.SelectionModeAll {
		t.Fatalf("precondition: base on-disk mode = %q, want all", baseOnDisk.Selection.Mode)
	}
	loaded, err := config.Parse(mustReadFile(t, path))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}
	// The non-default selection the flow will persist.
	wantSel := config.SelectionConfig{
		Mode:      config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{string(defaults.LegacyHarnessClaude): {Sessions: []string{"sess-p1"}}},
	}
	draft.Working().Selection = wantSel

	var modeAtIngest config.SelectionMode
	var sessionsAtIngest []string
	deps := kickstart.ProgramDeps{
		Draft:   draft,
		Theme:   theme.New(theme.ModeDark),
		Source:  scannerfix.NewFixtureTreeSource("standard"),
		Context: context.Background(),
		Ingest: func(_ context.Context) (*ftue.IngestResult, error) {
			log.events = append(log.events, "ingest")
			// The config file must already carry the committed non-default
			// selection by now - not the seeded base mode:all.
			data, _ := os.ReadFile(configPath)
			cfg, _ := config.Parse(data)
			modeAtIngest = cfg.Selection.Mode
			if hc, ok := cfg.Selection.Harnesses[string(defaults.LegacyHarnessClaude)]; ok {
				sessionsAtIngest = hc.Sessions
			}
			committedAtIngest = cfg.Selection.Mode == config.SelectionModeSelected
			return &ftue.IngestResult{New: 1}, nil
		},
	}
	p := kickstart.NewProgram(deps)
	p.SetSize(80, 24)

	p = declineOAuth(t, p)
	if len(log.events) != 0 {
		t.Fatalf("ingest ran during OAuth/flow before commit: %v", log.events)
	}
	var commitCmd tea.Cmd
	p, commitCmd = advanceToCommit(p)
	if !p.Committed() {
		t.Fatalf("flow did not commit; phase=%s", p.Phase())
	}

	// Drive the ingest completion message the runner's command produced.
	p = drainCmds(t, p, commitCmd)

	if len(log.events) != 1 || log.events[0] != "ingest" {
		t.Fatalf("expected exactly one ingest after commit, got %v", log.events)
	}
	if !committedAtIngest {
		t.Fatalf("ingest saw on-disk mode %q, want the committed non-default %q (ordering violated)", modeAtIngest, config.SelectionModeSelected)
	}
	if len(sessionsAtIngest) != 1 || sessionsAtIngest[0] != "sess-p1" {
		t.Fatalf("ingest saw on-disk sessions %v, want the committed [sess-p1]", sessionsAtIngest)
	}
	if p.IngestResult() == nil || p.IngestResult().New != 1 {
		t.Fatalf("ingest result not recorded: %+v", p.IngestResult())
	}
}

// TestProgram_EscConfirmWritesNothing proves that a confirmed esc-exit during the
// flow leaves the config bytes unchanged and never runs ingest.
func TestProgram_EscConfirmWritesNothing(t *testing.T) {
	t.Parallel()
	var ingestRan bool
	deps := kickstart.ProgramDeps{
		Ingest: func(_ context.Context) (*ftue.IngestResult, error) {
			ingestRan = true
			return &ftue.IngestResult{}, nil
		},
	}
	p, path := newTestProgram(t, deps)
	before := mustReadFile(t, path)

	p = declineOAuth(t, p)
	// Dirty something, then esc -> confirm exit. The esc MUST open the confirm
	// modal first (not exit outright): a mutation that lets esc skip the confirm
	// fails here rather than silently passing on the outcome-only checks below.
	p, _ = p.Update(press(tea.KeyEscape))
	if !p.Confirming() {
		t.Fatal("esc did not open the exit-confirm modal (it must always confirm before exiting)")
	}
	if p.Exited() {
		t.Fatal("esc exited outright without a confirm step")
	}
	p, _ = p.Update(press(tea.KeyLeft))  // focus "yes"
	p, _ = p.Update(press(tea.KeyEnter)) // confirm exit

	if !p.Exited() {
		t.Fatal("confirmed esc did not mark the program exited")
	}
	if p.Committed() {
		t.Fatal("a confirmed exit must not commit")
	}
	if ingestRan {
		t.Fatal("ingest ran after a confirmed no-save exit")
	}
	after := mustReadFile(t, path)
	if string(before) != string(after) {
		t.Fatalf("confirmed exit changed config bytes\n before=%q\n after=%q", before, after)
	}
}

// TestProgram_RetentionWritesAfterConfigSave proves the retention write happens
// after the config save (legacy ordering) and only when a preference is set.
func TestProgram_RetentionWritesAfterConfigSave(t *testing.T) {
	t.Parallel()
	retentionPath := filepath.Join(t.TempDir(), ".claude", "settings.json")
	var order []string
	var configModeAtRetention string
	var configPath string

	deps := kickstart.ProgramDeps{
		RetentionDays: 45,
		Retention: kickstart.RetentionWriterFunc(func(days int) error {
			order = append(order, "retention")
			data, _ := os.ReadFile(configPath)
			cfg, _ := config.Parse(data)
			configModeAtRetention = string(cfg.Selection.Mode)
			return ftue.WriteClaudeCleanupDaysAt(retentionPath, days)
		}),
		Ingest: func(_ context.Context) (*ftue.IngestResult, error) {
			order = append(order, "ingest")
			return &ftue.IngestResult{}, nil
		},
	}
	p, path := newTestProgram(t, deps)
	configPath = path

	p = declineOAuth(t, p)
	var commitCmd tea.Cmd
	p, commitCmd = advanceToCommit(p)
	if !p.Committed() {
		t.Fatal("flow did not commit")
	}
	p = drainCmds(t, p, commitCmd)

	if p.RetentionErr() != nil {
		t.Fatalf("retention write errored: %v", p.RetentionErr())
	}
	if configModeAtRetention == "" {
		t.Fatal("retention ran before the config was saved (ordering violated)")
	}
	if len(order) < 2 || order[0] != "retention" || order[1] != "ingest" {
		t.Fatalf("effect order = %v, want retention before ingest (both after commit)", order)
	}
	// The retention file carries the chosen cleanup days.
	days, ok := readCleanupDays(t, retentionPath)
	if !ok || days != 45 {
		t.Fatalf("retention file cleanupPeriodDays = %d present=%t, want 45", days, ok)
	}
}

// TestProgram_OAuthLoginFeedsConnected proves that accepting the connect-now
// prompt runs the injected login runner and marks the program connected before
// the flow is built.
func TestProgram_OAuthLoginFeedsConnected(t *testing.T) {
	t.Parallel()
	var loginCalls int
	deps := kickstart.ProgramDeps{
		Login: func(_ context.Context) (string, error) {
			loginCalls++
			return "octocat", nil
		},
	}
	p, _ := newTestProgram(t, deps)
	// Focus "yes" then confirm -> runs login.
	p, _ = p.Update(press(tea.KeyLeft))
	p, cmd := p.Update(press(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("accepting OAuth produced no login command")
	}
	// The accept transition returns a batch (login runner + spinner tick); run
	// the batch's child commands and feed each result back so the login result
	// lands without spinning on ticks.
	var flowInit tea.Cmd
	for _, child := range unwrapBatch(cmd) {
		if child == nil {
			continue
		}
		if m := child(); m != nil {
			var next tea.Cmd
			p, next = p.Update(m)
			if p.Phase() == kickstart.PhaseFlow && next != nil {
				flowInit = next
			}
		}
	}
	p = runFlowInitOnce(t, p, flowInit)
	if loginCalls != 1 {
		t.Fatalf("login runner called %d times, want 1", loginCalls)
	}
	if !p.Connected() {
		t.Fatal("program not marked connected after successful login")
	}
	if p.Phase() != kickstart.PhaseFlow {
		t.Fatalf("phase after login = %s, want flow", p.Phase())
	}
	// Widen before inspecting the section strip: at a narrow width the strip
	// truncates its right-most tab labels, which is orthogonal to whether login
	// revealed the sharing section.
	p.SetSize(120, 40)
	if !strings.Contains(stripRender(p.View()), "sharing") {
		t.Fatal("connected flow did not reveal sharing before its first navigation input")
	}
}

// TestProgram_AlreadyConnectedSkipsOAuth proves that when the machine already
// holds valid village credentials the connect-now step is skipped entirely: the
// program opens straight into the flow, already marked connected, with NO key
// press.
func TestProgram_AlreadyConnectedSkipsOAuth(t *testing.T) {
	t.Parallel()
	deps := kickstart.ProgramDeps{AlreadyConnected: true}
	p, _ := newTestProgram(t, deps)

	if p.Phase() != kickstart.PhaseFlow {
		t.Fatalf("AlreadyConnected phase = %s, want flow (OAuth step must be skipped)", p.Phase())
	}
	if !p.Connected() {
		t.Fatal("AlreadyConnected did not mark the program connected")
	}
}

// TestProgram_DestinationVisibilityCommits proves the village-gated destination
// step is offered when connected (VillageConnected) and that a choice made on
// its visibility radio is what the single atomic commit persists to
// config.Push.Visibility. It drives the kickstart registry through a
// settings.Flow so the destination step can be located by its section key rather
// than a brittle tab count.
func TestProgram_DestinationVisibilityCommits(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, config.BaseConfig()); err != nil {
		t.Fatalf("seed base config: %v", err)
	}
	// The seeded base visibility is private; the choice we drive must differ so
	// the assertion is non-vacuous.
	base, _ := config.Parse(mustReadFile(t, path))
	if base.Push.Visibility != config.VisibilityPrivate {
		t.Fatalf("precondition: base visibility = %q, want private", base.Push.Visibility)
	}
	draft, err := settings.NewDraft(path, base)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}

	reg := kickstart.BuildRegistry(kickstart.Options{
		Source:           scannerfix.NewFixtureTreeSource("standard"),
		VillageConnected: true,
	})
	f := settings.NewFlow(theme.New(theme.ModeDark), reg, draft)
	f.SetSize(80, 24)
	f = drainSettingsFlowInit(f, f.Init())

	tab := tea.KeyPressMsg{Code: tea.KeyTab}
	down := tea.KeyPressMsg{Code: tea.KeyDown}
	space := tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}

	// Advance to the destination step (bounded so a missing/hidden step fails
	// loudly rather than looping).
	reached := false
	for i := 0; i < 12 && !f.OnReceipt(); i++ {
		if f.CurrentSectionKey() == kickstart.SectionDestination {
			reached = true
			break
		}
		f, _ = f.Update(tab)
	}
	if !reached {
		t.Fatal("destination step was never reached; it must be visible when VillageConnected")
	}

	// Move the radio off private (order: private, group, public) to public and
	// commit the choice into the working draft.
	f, _ = f.Update(down)
	f, _ = f.Update(down)
	f, _ = f.Update(space)

	// Advance to the receipt and confirm the single atomic commit.
	for i := 0; i < 12 && !f.OnReceipt(); i++ {
		f, _ = f.Update(tab)
	}
	if !f.OnReceipt() {
		t.Fatal("never reached the receipt step")
	}
	f, _ = f.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !f.Committed() {
		t.Fatalf("flow did not commit; err=%v", f.Err())
	}

	got, err := config.Parse(mustReadFile(t, path))
	if err != nil {
		t.Fatalf("parse committed config: %v", err)
	}
	if got.Push.Visibility != config.VisibilityPublic {
		t.Fatalf("committed visibility = %q, want public (the driven radio choice)", got.Push.Visibility)
	}
}

func drainSettingsFlowInit(flow settings.Flow, command tea.Cmd) settings.Flow {
	for _, message := range collectMsgs(command) {
		flow, _ = flow.Update(message)
	}
	return flow
}

// unwrapBatch runs cmd and, if it produced a tea.BatchMsg, returns its child
// commands; otherwise it returns a single command re-yielding the message so the
// caller can process it uniformly.
func unwrapBatch(cmd tea.Cmd) []tea.Cmd {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		return batch
	}
	m := msg
	return []tea.Cmd{func() tea.Msg { return m }}
}

func readCleanupDays(t *testing.T, path string) (int, bool) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		return 0, false
	}
	return ftue.ReadClaudeCleanupDaysAt(path)
}
