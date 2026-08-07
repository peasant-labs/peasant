package push

import (
	"github.com/peasant-labs/peasant/internal/testutil"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

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

func TestWizard_InitialConfirm_Yes(t *testing.T) {
	m := NewPushWizard(testSessions())
	if m.page != pageInitialConfirm {
		t.Fatalf("expected pageInitialConfirm, got %d", m.page)
	}

	// confirmSel defaults to 0 (Yes), press enter to confirm.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(PushWizardModel)
	if m.page != pageSessionReview {
		t.Fatalf("expected pageSessionReview after enter (Yes selected), got %d", m.page)
	}
}

func TestWizard_InitialConfirm_No(t *testing.T) {
	m := NewPushWizard(testSessions())

	// Move down to "No" option, then press enter.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(PushWizardModel)
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(PushWizardModel)
	if !m.quitting {
		t.Fatal("expected quitting after selecting No")
	}
	if cmd == nil {
		t.Fatal("expected tea.Quit cmd")
	}
}

func TestWizard_SessionReview_SpaceCycles(t *testing.T) {
	sessions := testSessions()
	m := NewPushWizard(sessions)
	// Advance to session review.
	m.page = pageSessionReview
	m.cursor = 0

	if m.sessions[0].Action != PushWithRedaction {
		t.Fatal("expected default PushWithRedaction")
	}

	// Space toggles to exclude.
	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(PushWizardModel)
	if m.sessions[0].Action != PushExclude {
		t.Fatalf("expected PushExclude after space, got %v", m.sessions[0].Action)
	}

	// Space toggles back.
	updated, _ = m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(PushWizardModel)
	if m.sessions[0].Action != PushWithRedaction {
		t.Fatalf("expected PushWithRedaction after second space, got %v", m.sessions[0].Action)
	}
}

func TestWizard_SessionReview_QSetsExclude(t *testing.T) {
	m := NewPushWizard(testSessions())
	m.page = pageSessionReview
	m.cursor = 1

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	m = updated.(PushWizardModel)
	if m.sessions[1].Action != PushExclude {
		t.Fatalf("expected PushExclude after 'q', got %v", m.sessions[1].Action)
	}
}

func TestWizard_SessionReview_WSetsWithRedaction(t *testing.T) {
	m := NewPushWizard(testSessions())
	m.page = pageSessionReview
	m.sessions[0].Action = PushExclude
	m.cursor = 0

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'w', Text: "w"})
	m = updated.(PushWizardModel)
	if m.sessions[0].Action != PushWithRedaction {
		t.Fatalf("expected PushWithRedaction after 'w', got %v", m.sessions[0].Action)
	}
}

func TestWizard_SessionReview_ApproveAll(t *testing.T) {
	m := NewPushWizard(testSessions())
	m.page = pageSessionReview
	m.sessions[0].Action = PushExclude
	m.sessions[1].Action = PushExclude

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	m = updated.(PushWizardModel)
	for i, s := range m.sessions {
		if s.Action != PushWithRedaction {
			t.Fatalf("session %d: expected PushWithRedaction, got %v", i, s.Action)
		}
	}
}

func TestWizard_SessionReview_ExcludeAll(t *testing.T) {
	m := NewPushWizard(testSessions())
	m.page = pageSessionReview

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = updated.(PushWizardModel)
	for i, s := range m.sessions {
		if s.Action != PushExclude {
			t.Fatalf("session %d: expected PushExclude, got %v", i, s.Action)
		}
	}
}

func TestWizard_PageNavigation(t *testing.T) {
	m := NewPushWizard(testSessions())

	// Page 1 → Page 2 (confirmSel=0 is Yes, press enter)
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(PushWizardModel)
	if m.page != pageSessionReview {
		t.Fatalf("expected pageSessionReview, got %d", m.page)
	}

	// Page 2 → Page 3
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(PushWizardModel)
	if m.page != pageRedactionPreview {
		t.Fatalf("expected pageRedactionPreview, got %d", m.page)
	}

	// Page 3 → Page 4
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(PushWizardModel)
	if m.page != pageFinalConfirm {
		t.Fatalf("expected pageFinalConfirm, got %d", m.page)
	}

	// Page 4 confirm (confirmSel=0 is Yes, press enter)
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(PushWizardModel)
	if !m.confirmed {
		t.Fatal("expected confirmed after enter on final page (Yes selected)")
	}
	if cmd == nil {
		t.Fatal("expected tea.Quit cmd on confirmation")
	}
}

func TestWizard_SelectedSessionIDs(t *testing.T) {
	m := NewPushWizard(testSessions())
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

func TestWizard_RedactionStatus(t *testing.T) {
	sessions := testSessions()

	// Session 0: current (applied, hash matches)
	if got := sessions[0].redactionStatus(); got != "current" {
		t.Errorf("session 0: expected 'current', got %q", got)
	}
	// Session 1: stale (applied, hash differs)
	if got := sessions[1].redactionStatus(); got != "stale" {
		t.Errorf("session 1: expected 'stale', got %q", got)
	}
	// Session 2: raw (not applied)
	if got := sessions[2].redactionStatus(); got != "raw" {
		t.Errorf("session 2: expected 'raw', got %q", got)
	}
}

func TestWizard_RedactionStatus_NilMeta(t *testing.T) {
	s := PushWizardSession{
		Row:  ingest.PushSessionRow{SessionID: "test"},
		Meta: nil,
	}
	if got := s.redactionStatus(); got != "unknown" {
		t.Errorf("expected 'unknown' for nil meta, got %q", got)
	}
}

func TestWizard_BackNavigation(t *testing.T) {
	m := NewPushWizard(testSessions())
	m.page = pageSessionReview

	// Esc from session review goes back to initial confirm.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(PushWizardModel)
	if m.page != pageInitialConfirm {
		t.Fatalf("expected pageInitialConfirm after esc, got %d", m.page)
	}
}

func TestWizard_FinalConfirm_BackNavigation(t *testing.T) {
	m := NewPushWizard(testSessions())
	m.page = pageFinalConfirm

	// Move down to "No, go back" option, then press enter.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(PushWizardModel)
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(PushWizardModel)
	if m.page != pageSessionReview {
		t.Fatalf("expected pageSessionReview after selecting 'No, go back', got %d", m.page)
	}
}

func TestWizard_ConfirmSel_BoundaryClamping(t *testing.T) {
	t.Run("InitialConfirm", func(t *testing.T) {
		m := NewPushWizard(testSessions())
		if m.page != pageInitialConfirm {
			t.Fatalf("expected pageInitialConfirm, got %d", m.page)
		}
		if m.confirmSel != 0 {
			t.Fatalf("expected confirmSel=0 initially, got %d", m.confirmSel)
		}

		// Press Up at position 0 — should stay at 0 (no underflow).
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		m = updated.(PushWizardModel)
		if m.confirmSel != 0 {
			t.Fatalf("expected confirmSel=0 after up at top, got %d", m.confirmSel)
		}

		// Press Down — should move to 1.
		updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = updated.(PushWizardModel)
		if m.confirmSel != 1 {
			t.Fatalf("expected confirmSel=1 after down, got %d", m.confirmSel)
		}

		// Press Down again at position 1 — should stay at 1 (no overflow).
		updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = updated.(PushWizardModel)
		if m.confirmSel != 1 {
			t.Fatalf("expected confirmSel=1 after down at bottom, got %d", m.confirmSel)
		}
	})

	t.Run("FinalConfirm", func(t *testing.T) {
		m := NewPushWizard(testSessions())
		m.page = pageFinalConfirm
		// confirmSel resets to 0 when entering final confirm page.
		m.confirmSel = 0

		// Press Up at position 0 — should stay at 0 (no underflow).
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		m = updated.(PushWizardModel)
		if m.confirmSel != 0 {
			t.Fatalf("expected confirmSel=0 after up at top, got %d", m.confirmSel)
		}

		// Press Down — should move to 1.
		updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = updated.(PushWizardModel)
		if m.confirmSel != 1 {
			t.Fatalf("expected confirmSel=1 after down, got %d", m.confirmSel)
		}

		// Press Down again at position 1 — should stay at 1 (no overflow).
		updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = updated.(PushWizardModel)
		if m.confirmSel != 1 {
			t.Fatalf("expected confirmSel=1 after down at bottom, got %d", m.confirmSel)
		}
	})
}

func TestWizard_SessionReview_Navigation(t *testing.T) {
	m := NewPushWizard(testSessions())
	m.page = pageSessionReview
	m.cursor = 0
	m.height = 40

	// Move down.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(PushWizardModel)
	if m.cursor != 1 {
		t.Fatalf("expected cursor=1 after down, got %d", m.cursor)
	}

	// Move down again.
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(PushWizardModel)
	if m.cursor != 2 {
		t.Fatalf("expected cursor=2 after second down, got %d", m.cursor)
	}

	// Move down at bottom (should clamp).
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(PushWizardModel)
	if m.cursor != 2 {
		t.Fatalf("expected cursor=2 at bottom, got %d", m.cursor)
	}

	// Move up.
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(PushWizardModel)
	if m.cursor != 1 {
		t.Fatalf("expected cursor=1 after up, got %d", m.cursor)
	}
}

func TestWizard_CtrlC_Cancels(t *testing.T) {
	m := NewPushWizard(testSessions())
	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = updated.(PushWizardModel)
	if !m.quitting {
		t.Fatal("expected quitting after ctrl+c")
	}
	if cmd == nil {
		t.Fatal("expected tea.Quit cmd")
	}
}

func TestWizard_RedactionPreview_EscGoesBack(t *testing.T) {
	m := NewPushWizard(testSessions())
	m.page = pageRedactionPreview

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(PushWizardModel)
	if m.page != pageSessionReview {
		t.Fatalf("expected pageSessionReview after esc from redaction preview, got %d", m.page)
	}
}

// --- WizardCandidates / Locked (branch-conflict withholding) ---

// strPtr returns a pointer to s, for the *string GitBranch field.
func strPtr(s string) *string { return &s }

// conflictSelectionFixture builds a selection matcher plus three rows that
// exercise the three BranchMatch outcomes against a REAL matcher:
//   - kept:     repo-one, branch main → single rule admits   → Yes
//   - excluded: repo-one, branch dev  → single rule rejects  → No  (dropped)
//   - conflict: repo-two, branch main → two rules disagree   → WithheldConflict
//
// Two rules share repo-two with different branch sets so the conflict is real
// (not synthesized by setting Locked by hand).
func conflictSelectionFixture() (sel ingest.SelectionMatcher, kept, excluded, conflict ingest.PushSessionRow) {
	const (
		r1 = "git@github.com:user/repo-one.git"
		r2 = "git@github.com:user/repo-two.git"
	)
	b := ingest.NewSelectionMatcherBuilder()
	b.AddHarness(string(defaults.HarnessClaudeCode))
	b.AddProject(string(defaults.HarnessClaudeCode), r1, "", "main")    // kept admit
	b.AddProject(string(defaults.HarnessClaudeCode), r2, "", "main")    // conflict admit
	b.AddProject(string(defaults.HarnessClaudeCode), r2, "", "feature") // conflict reject
	sel = b.Build()

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
	return sel, kept, excluded, conflict
}

func TestWizardCandidates_Partition(t *testing.T) {
	t.Parallel()
	sel, kept, excluded, conflict := conflictSelectionFixture()

	out := WizardCandidates([]ingest.PushSessionRow{kept, excluded, conflict}, &sel)

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

func TestWizardCandidates_NilMatcherKeepsAll(t *testing.T) {
	t.Parallel()
	_, kept, excluded, conflict := conflictSelectionFixture()

	out := WizardCandidates([]ingest.PushSessionRow{kept, excluded, conflict}, nil)
	if len(out) != 3 {
		t.Fatalf("nil matcher should keep all 3 sessions, got %d", len(out))
	}
	for i, c := range out {
		if c.Locked {
			t.Errorf("candidate %d (%s): nil matcher should lock nothing", i, c.Row.SessionID)
		}
	}
}

func TestWizardCandidates_SelectedExcludesLocked(t *testing.T) {
	t.Parallel()
	sel, kept, excluded, conflict := conflictSelectionFixture()

	m := NewPushWizard(WizardCandidates([]ingest.PushSessionRow{kept, excluded, conflict}, &sel))
	ids := m.SelectedSessionIDs()

	if len(ids) != 1 || ids[0] != "kept-001" {
		t.Fatalf("SelectedSessionIDs should be exactly [kept-001] (Locked excluded), got %v", ids)
	}
}

func TestWizard_SessionReview_SkipsLocked(t *testing.T) {
	t.Parallel()
	sessions := []PushWizardSession{
		{Row: ingest.PushSessionRow{SessionID: "keep"}, Action: PushWithRedaction},
		{Row: ingest.PushSessionRow{SessionID: "locked"}, Action: PushExclude, Locked: true},
	}
	m := NewPushWizard(sessions)
	m.page = pageSessionReview
	m.cursor = 1 // cursor on the Locked row

	// space must NOT toggle a Locked row.
	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(PushWizardModel)
	if m.sessions[1].Action != PushExclude {
		t.Errorf("space on Locked row should not change Action; got %v", m.sessions[1].Action)
	}

	// 'w' (push approve) must NOT select a Locked row.
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'w', Text: "w"})
	m = updated.(PushWizardModel)
	if m.sessions[1].Action != PushExclude {
		t.Errorf("'w' on Locked row should not select it; got %v", m.sessions[1].Action)
	}

	// 'a' (approve all) selects unlocked rows but skips Locked.
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	m = updated.(PushWizardModel)
	if m.sessions[0].Action != PushWithRedaction {
		t.Errorf("'a' should select the unlocked row; got %v", m.sessions[0].Action)
	}
	if m.sessions[1].Action != PushExclude {
		t.Errorf("'a' should skip the Locked row; got %v", m.sessions[1].Action)
	}

	// 'x' (exclude all) leaves the Locked row excluded (never selected).
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = updated.(PushWizardModel)
	if m.sessions[1].Action != PushExclude {
		t.Errorf("'x' should leave Locked row excluded; got %v", m.sessions[1].Action)
	}

	// The Locked row is never in the approved set.
	for _, id := range m.SelectedSessionIDs() {
		if id == "locked" {
			t.Errorf("Locked session must never appear in SelectedSessionIDs")
		}
	}
}

func TestWizard_SessionReview_LockedRendered(t *testing.T) {
	t.Parallel()
	sessions := []PushWizardSession{
		{Row: ingest.PushSessionRow{SessionID: "locked", ModelHarness: string(defaults.HarnessClaudeCode), ProjectName: "p"}, Action: PushExclude, Locked: true},
	}
	m := NewPushWizard(sessions)
	m.page = pageSessionReview
	m.height = 40
	if got := m.viewSessionReview(); !strings.Contains(got, "withheld: branch conflict") {
		t.Errorf("session review should render Locked row as 'withheld: branch conflict'; got:\n%s", got)
	}
}

func TestWizard_RedactionPreview_NoSelectedBlocksAdvance(t *testing.T) {
	m := NewPushWizard(testSessions())
	m.page = pageRedactionPreview
	// Exclude all sessions.
	for i := range m.sessions {
		m.sessions[i].Action = PushExclude
	}

	// Enter should NOT advance to final confirm when nothing selected.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(PushWizardModel)
	if m.page != pageRedactionPreview {
		t.Fatalf("expected to stay on pageRedactionPreview when no sessions selected, got %d", m.page)
	}
}

// TestWizard_RedactionPreview_MakesNoPromiseItCannotKeep is the guard that did not
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
func TestWizard_RedactionPreview_MakesNoPromiseItCannotKeep(t *testing.T) {
	m := NewPushWizard(testSessions())
	m.page = pageRedactionPreview
	// Select everything, so the screen renders its full summary rather than the
	// empty-selection short form.
	for i := range m.sessions {
		m.sessions[i].Action = PushWithRedaction
	}
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
	screen := strings.Join(strings.Fields(ansi.Strip(m.redactionPreviewContent())), " ")

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
	for _, forbidden := range []string{
		"No raw data will leave your machine",
		"All sessions are redacted",
		"will be redacted before push",
		"will be re-redacted before push",
		"Standard redaction will be applied",
		"ready to push as-is",
	} {
		if strings.Contains(screen, forbidden) {
			t.Errorf("the consent screen must not promise %q: redaction finds KNOWN PATTERNS in metadata and content, so an "+
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
		"METADATA",                             // what is redacted, still
		"file paths and diagnostic locations",  // fields the configured level actually rewrites
		"CONVERSATION CONTENT",                 // and what now is too
		"KNOWN PATTERNS",                       // the hedge, which must survive every rewording
		"not a guarantee",                      // said outright rather than implied
		"can differ from what you see locally", // the consequence of redacting content
		"deselect it",                          // the remedy that is one keystroke away here
	} {
		if !strings.Contains(screen, required) {
			t.Errorf("the consent screen must state %q so the user can see what leaves and what to do about it; got:\n%s",
				required, screen)
		}
	}
	for _, identityField := range []string{"the host slug", "the project name", "the git remote", "the git branch"} {
		if strings.Contains(screen, "METADATA — "+identityField) || strings.Contains(screen, ", "+identityField) ||
			strings.Contains(screen, "and "+identityField+" —") {
			t.Errorf("the consent screen claims %q is redacted at the configured level, but it is published as recorded; got:\n%s",
				identityField, screen)
		}
	}
}
