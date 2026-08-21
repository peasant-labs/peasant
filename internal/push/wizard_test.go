package push

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/schema"
)

// wizardTestWidth and wizardTestHeight are the region every interaction test
// mounts the wizard into. They are large enough that the tree, the preview, and
// the receipt all render their full form.
const (
	wizardTestWidth  = 120
	wizardTestHeight = 40
)

// testTheme is the palette every wizard test renders with.
func testTheme() theme.Theme { return theme.New(theme.ModeDark) }

// testSessions returns a set of sessions for wizard tests.
func testSessions() []PushWizardSession {
	return []PushWizardSession{
		{
			Row: ingest.PushSessionRow{
				SessionID:    "sess-aaa-111",
				ModelHarness: string(defaults.HarnessClaudeCode),
				ProjectName:  "my-project",
				StartMs:      1700000000000,
			},
			Meta: &schema.UnifiedMetadata{
				ContentHash: "hash-a",
				Redaction:   schema.RedactionInfo{Applied: true, ContentHashAtRedact: "hash-a"},
			},
			Action: PushWithRedaction,
		},
		{
			Row: ingest.PushSessionRow{
				SessionID:    "sess-bbb-222",
				ModelHarness: string(defaults.HarnessOpenCode),
				ProjectName:  "other-project",
				StartMs:      1700001000000,
			},
			Meta: &schema.UnifiedMetadata{
				ContentHash: "hash-b-new",
				Redaction:   schema.RedactionInfo{Applied: true, ContentHashAtRedact: "hash-b-old"},
			},
			Action: PushWithRedaction,
		},
		{
			Row: ingest.PushSessionRow{
				SessionID:    "sess-ccc-333",
				ModelHarness: string(defaults.HarnessClaudeCode),
				ProjectName:  "raw-project",
				StartMs:      1700002000000,
			},
			Meta: &schema.UnifiedMetadata{
				ContentHash: "hash-c",
				Redaction:   schema.RedactionInfo{Applied: false},
			},
			Action: PushWithRedaction,
		},
	}
}

// mountWizard builds the wizard over the fixture sessions and sizes it, which
// is what the mounted program does before the first frame.
func mountWizard(sessions []PushWizardSession) PushWizardModel {
	return mountWizardSize(sessions, wizardTestWidth, wizardTestHeight)
}

// mountWizardSize mounts the wizard into an exact terminal region.
func mountWizardSize(sessions []PushWizardSession, width, height int) PushWizardModel {
	m := NewPushWizard(testTheme(), sessions, testPublishedTurns())
	updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return updated.(PushWizardModel)
}

// collectMsgs runs a command and returns every message it produced, flattening
// a batch into its parts.
func collectMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	var out []tea.Msg
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			out = append(out, collectMsgs(c)...)
		}
		return out
	}
	if msg != nil {
		out = append(out, msg)
	}
	return out
}

// drainWizard feeds back every message a command produced, and their immediate
// follow-ups, as the runtime would. It stops one level deep so a repeating
// animation tick cannot loop.
func drainWizard(m PushWizardModel, cmd tea.Cmd) PushWizardModel {
	for _, msg := range collectMsgs(cmd) {
		updated, follow := m.Update(msg)
		m = updated.(PushWizardModel)
		for _, next := range collectMsgs(follow) {
			updated, _ = m.Update(next)
			m = updated.(PushWizardModel)
		}
	}
	return m
}

// pressKey sends one key press and settles the work it started.
func pressKey(m PushWizardModel, msg tea.KeyPressMsg) PushWizardModel {
	updated, cmd := m.Update(msg)
	return drainWizard(updated.(PushWizardModel), cmd)
}

// pressKeyWithCmd sends one key press and returns the command it produced,
// unsettled, for a test that asserts on the command itself.
func pressKeyWithCmd(m PushWizardModel, msg tea.KeyPressMsg) (PushWizardModel, tea.Cmd) {
	updated, cmd := m.Update(msg)
	return updated.(PushWizardModel), cmd
}

// Key events the interaction tests send.
func keyEnter() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEnter} }
func keyEsc() tea.KeyPressMsg   { return tea.KeyPressMsg{Code: tea.KeyEsc} }
func keyLeft() tea.KeyPressMsg  { return tea.KeyPressMsg{Code: tea.KeyLeft} }
func keyDown() tea.KeyPressMsg  { return tea.KeyPressMsg{Code: tea.KeyDown} }
func keyUp() tea.KeyPressMsg    { return tea.KeyPressMsg{Code: tea.KeyUp} }
func keySpace() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeySpace} }
func keyRune(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// acceptStart answers the first page with yes, which opens the selector.
func acceptStart(m PushWizardModel) PushWizardModel {
	return pressKey(pressKey(m, keyLeft()), keyEnter())
}

func TestWizard_Start_Accept(t *testing.T) {
	m := mountWizard(testSessions())
	if m.page != pageInitialConfirm {
		t.Fatalf("expected pageInitialConfirm, got %s", m.page)
	}
	m = acceptStart(m)
	if m.page != pageSessionReview {
		t.Fatalf("expected pageSessionReview after accepting the start prompt, got %s", m.page)
	}
}

func TestWizard_Start_Decline(t *testing.T) {
	m := mountWizard(testSessions())
	// The prompt opens on "no", so enter alone declines.
	m = pressKey(m, keyEnter())
	if !m.quitting {
		t.Fatal("expected quitting after declining the start prompt")
	}
}

func TestWizard_Selection_SpaceTogglesTheHighlightedProject(t *testing.T) {
	m := acceptStart(mountWizard(testSessions()))
	if m.sessions[0].Action != PushWithRedaction {
		t.Fatal("expected the first session to open selected")
	}
	// The cursor opens on the first project row, whose only child is the first
	// session.
	m = pressKey(m, keySpace())
	if m.sessions[0].Action != PushExclude {
		t.Fatalf("expected PushExclude after space, got %v", m.sessions[0].Action)
	}
	m = pressKey(m, keySpace())
	if m.sessions[0].Action != PushWithRedaction {
		t.Fatalf("expected PushWithRedaction after a second space, got %v", m.sessions[0].Action)
	}
}

func TestWizard_Selection_SpaceTogglesTheHighlightedSession(t *testing.T) {
	m := acceptStart(mountWizard(testSessions()))
	// Down moves from the project row onto its session row.
	m = pressKey(m, keyDown())
	m = pressKey(m, keySpace())
	if m.sessions[0].Action != PushExclude {
		t.Fatalf("expected PushExclude after space on the session row, got %v", m.sessions[0].Action)
	}
	if m.sessions[1].Action != PushWithRedaction {
		t.Fatalf("a toggle must change one session only; session 1 is %v", m.sessions[1].Action)
	}
}

func TestWizard_Selection_SelectAllCycles(t *testing.T) {
	m := acceptStart(mountWizard(testSessions()))
	// Every session opens selected, so the ring skips the step that would
	// change nothing: the first press clears the forest, the second restores
	// the selection the cycle started from.
	m = pressKey(m, keyRune('a'))
	for i, s := range m.sessions {
		if s.Action != PushExclude {
			t.Fatalf("session %d: expected PushExclude after the clear step, got %v", i, s.Action)
		}
	}
	m = pressKey(m, keyRune('a'))
	for i, s := range m.sessions {
		if s.Action != PushWithRedaction {
			t.Fatalf("session %d: expected PushWithRedaction after the restore step, got %v", i, s.Action)
		}
	}
}

func TestWizard_Selection_Navigation(t *testing.T) {
	m := acceptStart(mountWizard(testSessions()))
	if got := m.tree.Cursor(); got != 0 {
		t.Fatalf("expected the cursor to open on the first row, got %d", got)
	}
	m = pressKey(m, keyDown())
	if got := m.tree.Cursor(); got != 1 {
		t.Fatalf("expected cursor=1 after down, got %d", got)
	}
	m = pressKey(m, keyUp())
	if got := m.tree.Cursor(); got != 0 {
		t.Fatalf("expected cursor=0 after up, got %d", got)
	}
}

func TestWizard_PageWalk(t *testing.T) {
	m := acceptStart(mountWizard(testSessions()))
	m = pressKey(m, keyEnter())
	if m.page != pageRedactionPreview {
		t.Fatalf("expected pageRedactionPreview, got %s", m.page)
	}
	m = pressKey(m, keyEnter())
	if m.page != pageFinalConfirm {
		t.Fatalf("expected pageFinalConfirm, got %s", m.page)
	}
	m = pressKey(m, keyLeft())
	m, cmd := pressKeyWithCmd(m, keyEnter())
	m = drainWizard(m, cmd)
	if !m.confirmed {
		t.Fatal("expected confirmed after accepting the final prompt")
	}
}

func TestWizard_SelectedSessionIDs(t *testing.T) {
	m := mountWizard(testSessions())
	m.sessions[1].Action = PushExclude

	ids := m.SelectedSessionIDs()
	if len(ids) != 2 {
		t.Fatalf("expected 2 selected sessions, got %d", len(ids))
	}
	if ids[0] != "sess-aaa-111" {
		t.Errorf("expected sess-aaa-111, got %s", ids[0])
	}
	if ids[1] != "sess-ccc-333" {
		t.Errorf("expected sess-ccc-333, got %s", ids[1])
	}
}

func TestWizard_RedactionState(t *testing.T) {
	sessions := testSessions()
	cases := []struct {
		index int
		want  RedactionState
	}{
		{index: 0, want: RedactionStateCurrent},
		{index: 1, want: RedactionStateStale},
		{index: 2, want: RedactionStateRaw},
	}
	for _, c := range cases {
		if got := sessions[c.index].RedactionState(); got != c.want {
			t.Errorf("session %d: RedactionState() = %s, want %s", c.index, got, c.want)
		}
	}
}

func TestWizard_RedactionState_NilMeta(t *testing.T) {
	s := PushWizardSession{Row: ingest.PushSessionRow{SessionID: "test"}}
	if got := s.RedactionState(); got != RedactionStateUnknown {
		t.Errorf("RedactionState() = %s, want %s for nil metadata", got, RedactionStateUnknown)
	}
}

func TestWizard_BackNavigation(t *testing.T) {
	m := acceptStart(mountWizard(testSessions()))
	m = pressKey(m, keyEsc())
	if m.page != pageInitialConfirm {
		t.Fatalf("expected pageInitialConfirm after esc, got %s", m.page)
	}
}

func TestWizard_FinalConfirm_Decline_ReturnsToSelection(t *testing.T) {
	m := acceptStart(mountWizard(testSessions()))
	m = pressKey(m, keyEnter())
	m = pressKey(m, keyEnter())
	if m.page != pageFinalConfirm {
		t.Fatalf("expected pageFinalConfirm, got %s", m.page)
	}
	// The prompt opens on "no", so enter alone declines and returns.
	m = pressKey(m, keyEnter())
	if m.page != pageSessionReview {
		t.Fatalf("expected pageSessionReview after declining the final prompt, got %s", m.page)
	}
	if m.confirmed {
		t.Fatal("declining the final prompt must not confirm the push")
	}
}

func TestWizard_CtrlC_Cancels(t *testing.T) {
	m := mountWizard(testSessions())
	next, cmd := pressKeyWithCmd(m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !next.quitting {
		t.Fatal("expected quitting after ctrl+c")
	}
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
}

func TestWizard_Help_OpensAndCloses(t *testing.T) {
	m := acceptStart(mountWizard(testSessions()))
	m = pressKey(m, keyRune('?'))
	if !m.helping {
		t.Fatal("expected the help overlay to open on ?")
	}
	view := ansi.Strip(m.viewString())
	if !strings.Contains(view, "keyboard shortcuts") {
		t.Errorf("the help overlay must name itself; got:\n%s", view)
	}
	for _, want := range []string{"tab", "shift+tab", "?"} {
		if !strings.Contains(view, want) {
			t.Errorf("the help overlay must list %q; got:\n%s", want, view)
		}
	}
	m = pressKey(m, keyRune('?'))
	if m.helping {
		t.Fatal("expected the help overlay to close on a second ?")
	}
}

func TestWizard_Selection_FooterNamesTheKitKeys(t *testing.T) {
	m := acceptStart(mountWizard(testSessions()))
	view := ansi.Strip(m.viewString())
	for _, want := range []string{"tab", "prev field"} {
		if !strings.Contains(view, want) {
			t.Errorf("the selection footer must name %q; got:\n%s", want, view)
		}
	}
	// The footer is truncated from the right, so the help key is asserted on the
	// dispatchable set the footer and the overlay are both derived from.
	found := false
	for _, action := range m.availableActions() {
		if action == keymap.ActionHelp {
			found = true
		}
	}
	if !found {
		t.Error("the selection page must dispatch the help key")
	}
}

// --- WizardCandidates / Locked (branch-conflict withholding) ---

// strPtr returns a pointer to s, for the *string GitBranch field.
func strPtr(s string) *string { return &s }

// conflictSelectionFixture builds command-prepared decisions plus three rows
// that exercise the three BranchMatch outcomes:
//   - kept:     repo-one, branch main → single rule admits   → Yes
//   - excluded: repo-one, branch dev  → single rule rejects  → No  (dropped)
//   - conflict: repo-two, branch main → two rules disagree   → WithheldConflict
//
// Candidate matching is covered at the command boundary. This fixture isolates
// the wizard's partition of those already-proven decisions.
func conflictSelectionFixture() (sel *SessionSelection, kept, excluded, conflict ingest.PushSessionRow) {
	const (
		r1 = "git@github.com:user/repo-one.git"
		r2 = "git@github.com:user/repo-two.git"
	)
	kept = ingest.PushSessionRow{
		SessionID: "kept-001", ModelHarness: string(defaults.HarnessClaudeCode),
		ProjectName: "alpha", GitRemote: r1, GitBranch: strPtr("main"),
	}
	excluded = ingest.PushSessionRow{
		SessionID: "excl-002", ModelHarness: string(defaults.HarnessClaudeCode),
		ProjectName: "alpha", GitRemote: r1, GitBranch: strPtr("dev"),
	}
	conflict = ingest.PushSessionRow{
		SessionID: "conf-003", ModelHarness: string(defaults.HarnessClaudeCode),
		ProjectName: "beta", GitRemote: r2, GitBranch: strPtr("main"),
	}
	sel = NewSessionSelection(map[ingest.SessionID]ingest.BranchMatch{
		ingest.SessionID(kept.SessionID):     ingest.BranchMatchYes,
		ingest.SessionID(excluded.SessionID): ingest.BranchMatchNo,
		ingest.SessionID(conflict.SessionID): ingest.BranchMatchWithheldConflict,
	})
	return sel, kept, excluded, conflict
}

func TestWizardCandidates_Partition(t *testing.T) {
	t.Parallel()
	sel, kept, excluded, conflict := conflictSelectionFixture()

	out := WizardCandidates([]ingest.PushSessionRow{kept, excluded, conflict}, sel)

	// excluded is dropped; kept (unlocked) first, then conflict (Locked).
	if len(out) != 2 {
		t.Fatalf("expected 2 candidates (kept + withheld), got %d: %+v", len(out), out)
	}
	if out[0].Locked {
		t.Errorf("expected first candidate unlocked (kept), got Locked=true")
	}
	if out[0].Row.SessionID != "kept-001" {
		t.Errorf("expected kept-001 first, got %s", out[0].Row.SessionID)
	}
	if out[0].Action != PushWithRedaction {
		t.Errorf("kept candidate should default to PushWithRedaction, got %v", out[0].Action)
	}
	if !out[1].Locked {
		t.Errorf("expected second candidate Locked (withheld conflict)")
	}
	if out[1].Row.SessionID != "conf-003" {
		t.Errorf("expected conf-003 second (withheld), got %s", out[1].Row.SessionID)
	}
	if out[1].Action != PushExclude {
		t.Errorf("withheld candidate should be PushExclude, got %v", out[1].Action)
	}
}

func TestWizardCandidates_NilSelectionKeepsAll(t *testing.T) {
	t.Parallel()
	_, kept, excluded, conflict := conflictSelectionFixture()

	out := WizardCandidates([]ingest.PushSessionRow{kept, excluded, conflict}, nil)
	if len(out) != 3 {
		t.Fatalf("nil selection should keep all 3 sessions, got %d", len(out))
	}
	for i, c := range out {
		if c.Locked {
			t.Errorf("candidate %d (%s): nil selection should lock nothing", i, c.Row.SessionID)
		}
	}
}

func TestWizardCandidates_SelectedExcludesLocked(t *testing.T) {
	t.Parallel()
	sel, kept, excluded, conflict := conflictSelectionFixture()

	m := mountWizard(WizardCandidates([]ingest.PushSessionRow{kept, excluded, conflict}, sel))
	ids := m.SelectedSessionIDs()

	if len(ids) != 1 || ids[0] != "kept-001" {
		t.Fatalf("SelectedSessionIDs should be exactly [kept-001] (Locked excluded), got %v", ids)
	}
}

// lockedSessions is the two-row fixture the withheld-session tests drive: one
// selectable session and one the branch-aware selection withheld.
func lockedSessions() []PushWizardSession {
	return []PushWizardSession{
		{
			Row: ingest.PushSessionRow{
				SessionID: "keep", ModelHarness: string(defaults.HarnessClaudeCode), ProjectName: "alpha",
			},
			Action: PushWithRedaction,
		},
		{
			Row: ingest.PushSessionRow{
				SessionID: "locked", ModelHarness: string(defaults.HarnessClaudeCode), ProjectName: "beta",
			},
			Action: PushExclude,
			Locked: true,
		},
	}
}

func TestWizard_Selection_SkipsWithheldSessions(t *testing.T) {
	t.Parallel()
	m := acceptStart(mountWizard(lockedSessions()))
	// Move onto the withheld session row: project alpha, session keep, project
	// beta, session locked.
	for i := 0; i < 3; i++ {
		m = pressKey(m, keyDown())
	}
	m = pressKey(m, keySpace())
	if m.sessions[1].Action != PushExclude {
		t.Errorf("space on a withheld row must not change its action; got %v", m.sessions[1].Action)
	}
	// The selectable row opens selected, so the ring clears it first and
	// restores it next. The withheld row must not move on either step.
	m = pressKey(m, keyRune('a'))
	if m.sessions[1].Action != PushExclude {
		t.Errorf("the clear step must leave the withheld row excluded; got %v", m.sessions[1].Action)
	}
	m = pressKey(m, keyRune('a'))
	if m.sessions[0].Action != PushWithRedaction {
		t.Errorf("the restore step must select the selectable row; got %v", m.sessions[0].Action)
	}
	if m.sessions[1].Action != PushExclude {
		t.Errorf("select all must skip the withheld row; got %v", m.sessions[1].Action)
	}
	for _, id := range m.SelectedSessionIDs() {
		if id == "locked" {
			t.Error("a withheld session must never appear in SelectedSessionIDs")
		}
	}
}

func TestWizard_Selection_ReportsWhyASessionIsWithheld(t *testing.T) {
	t.Parallel()
	m := acceptStart(mountWizard(lockedSessions()))
	for i := 0; i < 3; i++ {
		m = pressKey(m, keyDown())
	}
	view := strings.Join(strings.Fields(ansi.Strip(m.viewString())), " ")
	if !strings.Contains(view, "withheld") {
		t.Errorf("the selector must say the highlighted session is withheld; got:\n%s", view)
	}
	if !strings.Contains(view, "branch") {
		t.Errorf("the selector must name the branch conflict as the reason; got:\n%s", view)
	}
}

func TestWizard_Notice_NoSelectionBlocksAdvance(t *testing.T) {
	m := acceptStart(mountWizard(testSessions()))
	// Every session opens selected, so one press of the select-all key clears
	// the whole forest.
	m = pressKey(m, keyRune('a'))
	m = pressKey(m, keyEnter())
	if m.page != pageRedactionPreview {
		t.Fatalf("expected pageRedactionPreview, got %s", m.page)
	}
	m = pressKey(m, keyEnter())
	if m.page != pageRedactionPreview {
		t.Fatalf("expected to stay on the consent page with nothing selected, got %s", m.page)
	}
	view := ansi.Strip(m.viewString())
	if !strings.Contains(view, "no sessions are selected") {
		t.Errorf("the consent page must say why it cannot advance; got:\n%s", view)
	}
}

func TestWizard_Notice_Scrolls(t *testing.T) {
	// A short region is what makes the consent copy longer than the page, which
	// is the state scrolling exists for. The region shrank when the copy did:
	// the page dropped the stored-copy classification, so at 80x24 the whole
	// notice now fits and there is nothing to scroll.
	m := acceptStart(mountWizardSize(testSessions(), 80, 14))
	m = pressKey(m, keyEnter())
	first := m.viewString()
	m = pressKey(m, keyDown())
	if m.noticeScroll != 1 {
		t.Fatalf("expected the consent page to scroll by one row, got %d", m.noticeScroll)
	}
	if m.viewString() == first {
		t.Error("the consent page rendered the same screen after scrolling")
	}
	m = pressKey(m, keyUp())
	if m.noticeScroll != 0 {
		t.Fatalf("expected the consent page to scroll back, got %d", m.noticeScroll)
	}
}

// TestWizard_Notice_MakesNoPromiseItCannotKeep is the guard that did not
// exist when the screen promised something untrue.
//
// This is the screen a user confirms a publish on, so it is the highest-stakes
// place in the product for a privacy claim. It used to end with "All sessions are
// redacted using the safety-net redactor (Standard level) before upload. No raw
// data will leave your machine." The second sentence is an absolute guarantee that
// the push path could not deliver then and still cannot: matching finds KNOWN
// patterns and cannot promise it found every one. Push now redacts metadata AND
// content, through the same function the redact command uses, so the screen says
// both - what it must never say again is an absolute.
//
// It survived because nothing asserted it. Every sentence on this screen had zero
// test coverage, so the claim could be written once and never re-examined. That is
// the gap this test closes, and it is why the forbidden list is phrased as claims
// rather than as exact strings: a reworded promise is still a promise.
func TestWizard_Notice_MakesNoPromiseItCannotKeep(t *testing.T) {
	m := mountWizard(testSessions())
	// Assertions run on WHITESPACE-NORMALISED text, because the screen wraps.
	//
	// This is not convenience. A wrapped line puts a newline inside a sentence, so
	// a substring guard stops seeing any claim that happens to straddle the wrap -
	// "removes all secrets" becomes "removes all\nsecrets" and the over-claim
	// corpus goes quiet on a screen that makes the claim. Where the line breaks
	// falls out of the terminal width, so which claims are visible to the guard
	// would depend on the user's window. The forbid corpus one directory over
	// normalises for the same reason: gofmt decides where a comment wraps, not the
	// author.
	// Style-color escape sequences are stripped before the text is inspected:
	// styled text that wraps now carries a per-line color reset at each wrap
	// point, so a raw substring guard would see escape bytes between two words a
	// sentence keeps together. Stripping leaves the visible text this screen
	// prints, which is what these claims are about.
	screen := strings.Join(strings.Fields(ansi.Strip(m.noticePanel(wizardTestWidth).View())), " ")

	// The over-claims come from the SHARED corpus, not from a list written here.
	//
	// This test carried its own list of six exact strings while its header
	// described them as claims rather than exact strings - a THIRD copy of a list
	// that already exists once for internal/config and once for cmd/peasant, on
	// the surface this file's own header calls the highest-stakes place in the
	// product for a privacy claim. Two of the shared needles match wording this
	// screen could acquire verbatim, and adding it here was measured green.
	//
	// The corpus header records that duplicating this list once before is what let
	// two copies drift on whether the text was lower-cased before comparing, which
	// is the single detail deciding whether a claim at the start of a sentence is
	// seen at all.
	overclaims, err := testutil.Overclaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(overclaims) == 0 {
		t.Fatal("the shared over-claim corpus is empty, so this screen is being checked against nothing")
	}
	for _, claim := range overclaims {
		if claim.Asserts(screen) {
			t.Errorf("the consent screen makes the over-claim %q.\n%s\nThis is the screen a user reads at the moment "+
				"they decide what to publish, so a completeness promise here is the most expensive one in the "+
				"product.\ngot:\n%s", claim.Needle, strings.TrimSpace(claim.Why), screen)
		}
	}
	// The six phrasings this screen printed or nearly printed, kept beside the
	// corpus because they are specific to this surface's wording rather than to
	// redaction copy in general.
	// The classification of the STORED copy is forbidden here, not merely absent.
	//
	// The screen used to count the selected sessions by the redaction record of
	// the copy on this machine: never redacted, older rule set, current rule set.
	// Every count was true and none was about the push, so a maintainer read
	// "session(s) whose stored copy has never been redacted" at the moment of
	// consent and had nothing to do with it. Whatever the stored copy holds, what
	// leaves is redacted on its way out. Naming the phrasings keeps a reworded
	// return of the same idea out of this screen.
	for _, forbidden := range []string{
		"No raw data will leave your machine",
		"All sessions are redacted",
		"will be redacted before push",
		"will be re-redacted before push",
		"Standard redaction will be applied",
		"ready to push as-is",
		"stored copy",
		"has never been redacted",
		"an older rule set",
		"at the current rule set",
	} {
		if strings.Contains(screen, forbidden) {
			t.Errorf("the consent screen must not promise %q: redaction finds known patterns in metadata and content, so an "+
				"absolute claim of completeness is one it cannot keep whatever it covers; got:\n%s", forbidden, screen)
		}
	}

	// And it has to say what actually happens, or a reader is left to assume.
	//
	// The required list CHANGED when push began redacting content through the same
	// path as the redact command. It used to require "published AS RECORDED" and
	// "That is the whole of it." - both true then and false now, and both would
	// have pinned this screen to understating what the product does. A required
	// phrase is a claim like any other: it has to be re-examined when the
	// behaviour it describes moves, or it holds the copy to the old behaviour
	// while every test stays green.
	for _, required := range []string{
		"metadata",                                // what is redacted, still
		"file paths and diagnostic locations",     // fields the configured level actually rewrites
		"conversation content",                    // and what now is too
		"known patterns",                          // the hedge, which must survive every rewording
		"not a guarantee",                         // said outright rather than implied
		"can differ from what you see locally",    // the consequence of redacting content
		"deselect it",                             // the remedy that is one keystroke away here
		"before it publishes them to the village", // what replaced the stored-copy counts
		"go back with esc to review one",          // where the user sees the published text
	} {
		if !strings.Contains(screen, required) {
			t.Errorf("the consent screen must state %q so the user can see what leaves and what to do about it; got:\n%s",
				required, screen)
		}
	}
	for _, identityField := range []string{"the host slug", "the project name", "the git remote", "the git branch"} {
		if strings.Contains(screen, "metadata - "+identityField) || strings.Contains(screen, ", "+identityField) ||
			strings.Contains(screen, "and "+identityField+" -") {
			t.Errorf("the consent screen claims %q is redacted at the configured level, but it is published as recorded; got:\n%s",
				identityField, screen)
		}
	}
}

// TestWizard_SelectionTree_WithheldRowIsAConflict proves the withheld session
// reaches the tree as the non-selectable state, which is what stops every
// selection key from moving it into the push set.
func TestWizard_SelectionTree_WithheldRowIsAConflict(t *testing.T) {
	t.Parallel()
	m := mountWizard(lockedSessions())
	leaf, ok := m.leaves["locked"]
	if !ok {
		t.Fatal("the withheld session has no row in the selection tree")
	}
	if leaf.State != kit.Conflict {
		t.Errorf("withheld row state = %s, want %s", leaf.State, kit.Conflict)
	}
}

// windowSize is the resize message a mounted program receives from the
// terminal.
func windowSize(width, height int) tea.WindowSizeMsg {
	return tea.WindowSizeMsg{Width: width, Height: height}
}

// TestWizard_Receipt_CountsThePushAndNotADeletion is the guard on the last
// screen before an upload.
//
// It used to read "N session(s) leave this machine." over "M of N candidate
// sessions stay on it." A push copies: nothing leaves and nothing is removed.
// Read at the moment of confirmation, that pair says the selected sessions go
// away and the rest are what is left - which is how a maintainer read it on a
// real store.
func TestWizard_Receipt_CountsThePushAndNotADeletion(t *testing.T) {
	m := acceptStart(mountWizard(testSessions()))
	// Space on the first project row deselects its only session, so the receipt
	// has both a pushed set and a skipped one.
	m = pressKey(m, keySpace())
	m = pressKey(m, keyEnter())
	m = pressKey(m, keyEnter())
	if m.page != pageFinalConfirm {
		t.Fatalf("expected pageFinalConfirm, got %s", m.page)
	}
	screen := strings.Join(strings.Fields(ansi.Strip(m.viewString())), " ")
	for _, want := range []string{
		"push 2 session(s) to the village.",
		"1 session(s) are not selected and are not pushed.",
		"nothing is removed from this machine.",
		wizardReceiptDetails,
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("the receipt must state %q; got:\n%s", want, screen)
		}
	}
	for _, forbidden := range []string{"leave this machine", "stay on it", "stored copy"} {
		if strings.Contains(screen, forbidden) {
			t.Errorf("the receipt must not say %q: a push removes nothing; got:\n%s", forbidden, screen)
		}
	}
}

// TestWizard_Receipt_OmitsTheSkippedLineWhenNothingIsSkipped proves the second
// line is conditional: with every candidate selected there is no skipped set,
// and a line reporting zero of them would be noise on the confirmation screen.
func TestWizard_Receipt_OmitsTheSkippedLineWhenNothingIsSkipped(t *testing.T) {
	m := acceptStart(mountWizard(testSessions()))
	m = pressKey(m, keyEnter())
	m = pressKey(m, keyEnter())
	screen := strings.Join(strings.Fields(ansi.Strip(m.viewString())), " ")
	if !strings.Contains(screen, "push 3 session(s) to the village.") {
		t.Errorf("the receipt must count the whole push; got:\n%s", screen)
	}
	if strings.Contains(screen, "are not selected and are not pushed") {
		t.Errorf("the receipt must not report a skipped set that is empty; got:\n%s", screen)
	}
}
