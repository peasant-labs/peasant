package ftue

import (
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"github.com/peasant-labs/schema/testcase"
	testassert "github.com/peasant-labs/schema/testcase/assert"
)

// mustParseTime parses an RFC3339 time string or panics. Test-only helper.
func mustParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic("mustParseTime: " + err.Error())
	}
	return t
}

// mockProgress is a test implementation of ProgressSnapshot.
type mockProgress struct {
	data map[string]StageProgress
}

func (m *mockProgress) Snapshot() map[string]StageProgress { return m.data }

// ---------------------------------------------------------------------------
// OAuthPage — with existing user (three-option flow)
// ---------------------------------------------------------------------------

func TestOAuthPage_ExistingUser_ContinueCompletesImmediately(t *testing.T) {
	p := NewOAuthPage("title", "desc", nil, nil, "http://example.com", "alice")

	opts, _ := p.displayOptions()
	if len(opts) != 3 {
		t.Fatalf("expected 3 options with existing user, got %d", len(opts))
	}

	// Press "1" → "Continue as alice" (index 0).
	updated, cmd := p.Update(tea.KeyPressMsg{Code: '1', Text: "1"})
	op := updated.(*OAuthPage)

	if !op.IsComplete() {
		t.Error("continue should complete the page immediately")
	}
	if !op.IsConnected() {
		t.Error("continue should mark as connected")
	}
	if cmd != nil {
		t.Error("continue should not fire any async command")
	}
}

func TestOAuthPage_ExistingUser_StayLocalCompletes(t *testing.T) {
	p := NewOAuthPage("title", "desc", nil, nil, "http://example.com", "alice")

	// Press "3" → "Stay local" (index 2).
	updated, _ := p.Update(tea.KeyPressMsg{Code: '3', Text: "3"})
	op := updated.(*OAuthPage)

	if !op.IsComplete() {
		t.Error("stay local should complete the page")
	}
	if op.IsConnected() {
		t.Error("stay local should not mark as connected")
	}
}

func TestOAuthPage_ExistingUser_NewAccountShowsLogoutStep(t *testing.T) {
	p := NewOAuthPage("title", "desc", nil, nil, "http://example.com", "alice")
	p.openBrowser = func(string) error { return nil }

	// Press "2" → "Log in with a different account" (index 1).
	updated, cmd := p.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
	op := updated.(*OAuthPage)

	if op.IsComplete() {
		t.Error("new account should not be complete before OAuth completes")
	}
	// No OAuth cmd yet — waiting for logout step.
	if cmd != nil {
		t.Error("new account should not fire OAuth command before user signs out")
	}
	if !op.reAuthLogoutPending {
		t.Error("reAuthLogoutPending should be true after selecting new account")
	}
	if op.authInProgress {
		t.Error("authInProgress should be false during logout step")
	}
}

func TestOAuthPage_ExistingUser_BrowserFailureSurfaced(t *testing.T) {
	p := NewOAuthPage("title", "desc", nil, nil, "http://example.com", "alice")
	p.openBrowser = func(string) error { return fmt.Errorf("no browser launcher found") }

	// Press "2" → "Log in with a different account" → triggers the logout step,
	// which opens the GitHub sign-out page in the browser.
	updated, _ := p.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
	op := updated.(*OAuthPage)

	if op.browserErr == nil {
		t.Fatal("browser-open failure should be captured, not swallowed")
	}
	view := op.View(80, 24)
	if !strings.Contains(view, "Could not open your browser") {
		t.Errorf("view should surface the browser failure, got:\n%s", view)
	}
	if !strings.Contains(view, "https://github.com/logout") {
		t.Errorf("view should tell the user to open the sign-out URL manually, got:\n%s", view)
	}
}

func TestOAuthPage_ExistingUser_NewAccountFiresOAuthAfterLogout(t *testing.T) {
	p := NewOAuthPage("title", "desc", nil, nil, "http://example.com", "alice")
	p.openBrowser = func(string) error { return nil }

	// Select "Log in with a different account".
	updated, _ := p.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
	// User pressed Enter after signing out of GitHub.
	afterEnter, cmd := updated.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	op := afterEnter.(*OAuthPage)

	if !op.authInProgress {
		t.Error("authInProgress should be true after user confirms logout")
	}
	if op.reAuthLogoutPending {
		t.Error("reAuthLogoutPending should be cleared after Enter")
	}
	if cmd == nil {
		t.Error("OAuth command should fire after user confirms logout")
	}
}

func TestOAuthPage_ExistingUser_NewAccountCompletesAfterAuth(t *testing.T) {
	p := NewOAuthPage("title", "desc", nil, nil, "http://example.com", "alice")
	p.openBrowser = func(string) error { return nil }

	// Select "Log in with a different account", confirm logout, simulate OAuth success.
	s1, _ := p.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
	s2, _ := s1.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	final, _ := s2.Update(loginResultMsg{username: "bob", err: nil})
	fp := final.(*OAuthPage)

	if !fp.IsComplete() {
		t.Error("should be complete after successful re-auth")
	}
	if !fp.IsConnected() {
		t.Error("should be connected after successful re-auth")
	}
}

// ---------------------------------------------------------------------------
// OAuthPage — Reset preserves existingUser
// ---------------------------------------------------------------------------

func TestOAuthPage_Reset_PreservesExistingUser(t *testing.T) {
	p := NewOAuthPage("title", "desc", nil, nil, "http://example.com", "alice")
	// Make a selection, then reset.
	p.Update(tea.KeyPressMsg{Code: '1', Text: "1"}) //nolint
	p.Reset()

	if p.existingUser != "alice" {
		t.Errorf("Reset should preserve existingUser, got %q", p.existingUser)
	}
	if p.selected != -1 {
		t.Errorf("Reset should clear selected, got %d", p.selected)
	}
}

// ---------------------------------------------------------------------------
// Wizard — WithExistingUser flows through to OAuthPage
// ---------------------------------------------------------------------------

func TestWizard_ExistingUser_PassedToOAuthPage(t *testing.T) {
	m := NewWizard(WithExistingUser("alice"))

	oauthPage, ok := m.pages[0].(*OAuthPage)
	if !ok {
		t.Fatal("page 0 should be OAuthPage")
	}
	if oauthPage.existingUser != "alice" {
		t.Errorf("existingUser = %q, want %q", oauthPage.existingUser, "alice")
	}
}

// ---------------------------------------------------------------------------
// IngestPage — progress rendering
// ---------------------------------------------------------------------------

func TestIngestPage_EmptySnapshot_ShowsNotStartedIcons(t *testing.T) {
	p := NewIngestPage("Ingesting")
	p.running = true
	p.progress = &mockProgress{data: map[string]StageProgress{
		"DISCOVER":      {},
		"DIFF":          {},
		"FILTER":        {},
		"EXTRACT+WRITE": {},
		"DB INSERT":     {},
		"INDEX":         {},
		"COMPUTE":       {},
		"ANNOTATE":      {},
		"CLEANUP":       {},
		"REPORT":        {},
	}}

	view := p.View(80, 24)
	// All stages should show "not started" icon (○)
	count := strings.Count(view, "○")
	if count != len(stageDisplayOrder) {
		t.Errorf("expected %d not-started icons (○), got %d", len(stageDisplayOrder), count)
	}
	// No completed or in-progress icons
	if strings.Contains(view, "✔") {
		t.Error("should not contain completed icon (✔) in empty snapshot")
	}
	if strings.Contains(view, "●") {
		t.Error("should not contain in-progress icon (●) in empty snapshot")
	}
}

func TestIngestPage_PartialProgress_ShowsProgressBars(t *testing.T) {
	p := NewIngestPage("Ingesting")
	p.running = true
	p.progress = &mockProgress{data: map[string]StageProgress{
		"DISCOVER":      {Done: 10, Total: 10, Ended: true},
		"DIFF":          {Done: 5, Total: 10},
		"FILTER":        {},
		"EXTRACT+WRITE": {},
		"DB INSERT":     {},
		"INDEX":         {},
		"COMPUTE":       {},
		"ANNOTATE":      {},
		"CLEANUP":       {},
		"REPORT":        {},
	}}

	view := p.View(80, 24)

	// Discover: completed
	if !strings.Contains(view, "✔") {
		t.Error("completed stage should show ✔")
	}
	// Diff: in progress
	if !strings.Contains(view, "●") {
		t.Error("in-progress stage should show ●")
	}
	// Should show counts for stages with Total > 0
	if !strings.Contains(view, "10/10") {
		t.Error("completed stage should show count 10/10")
	}
	if !strings.Contains(view, "5/10") {
		t.Error("in-progress stage should show count 5/10")
	}
}

func TestIngestPage_AllCompleted_ShowsCheckmarks(t *testing.T) {
	p := NewIngestPage("Ingesting")
	p.running = true
	p.progress = &mockProgress{data: map[string]StageProgress{
		"DISCOVER":      {Done: 5, Total: 5, Ended: true},
		"DIFF":          {Done: 5, Total: 5, Ended: true},
		"FILTER":        {Done: 5, Total: 5, Ended: true},
		"EXTRACT+WRITE": {Done: 5, Total: 5, Ended: true},
		"DB INSERT":     {Done: 5, Total: 5, Ended: true},
		"INDEX":         {Done: 5, Total: 5, Ended: true},
		"COMPUTE":       {Done: 5, Total: 5, Ended: true},
		"ANNOTATE":      {Done: 5, Total: 5, Ended: true},
		"CLEANUP":       {Done: 1, Total: 1, Ended: true},
		"REPORT":        {Done: 1, Total: 1, Ended: true},
	}}

	view := p.View(80, 24)
	checkCount := strings.Count(view, "✔")
	if checkCount != len(stageDisplayOrder) {
		t.Errorf("expected %d checkmarks (✔), got %d", len(stageDisplayOrder), checkCount)
	}
	if strings.Contains(view, "○") {
		t.Error("no stages should be not-started when all are complete")
	}
}

func TestIngestPage_ErrorStage_ShowsErrorIcon(t *testing.T) {
	p := NewIngestPage("Ingesting")
	p.running = true
	p.progress = &mockProgress{data: map[string]StageProgress{
		"DISCOVER":      {Done: 10, Total: 10, Ended: true},
		"DIFF":          {Done: 5, Total: 10, Ended: true, HasErr: true},
		"FILTER":        {},
		"EXTRACT+WRITE": {},
		"DB INSERT":     {},
		"INDEX":         {},
		"COMPUTE":       {},
		"ANNOTATE":      {},
		"CLEANUP":       {},
		"REPORT":        {},
	}}

	view := p.View(80, 24)
	if !strings.Contains(view, "✗") {
		t.Error("errored stage should show ✗ icon")
	}
}

func TestIngestPage_RestoresLegacyIngestionErrorCopy(t *testing.T) {
	p := NewIngestPage("Ingesting")
	p.err = errors.New("source unavailable")
	view := p.View(80, 24)
	if !strings.Contains(view, "Ingestion failed: source unavailable") || !strings.Contains(view, "You can run 'peasant ingest' manually at any time to retry.") {
		t.Fatalf("legacy ingestion recovery copy missing: %s", view)
	}
}

func TestIngestPage_NilProgress_FallsBackToMessage(t *testing.T) {
	p := NewIngestPage("Ingesting")
	p.running = true
	// progress is nil

	view := p.View(80, 24)
	if !strings.Contains(view, "please wait") {
		t.Error("nil progress should show fallback 'please wait' message")
	}
}

func TestIngestPage_TickKeepsTicking_WhileRunning(t *testing.T) {
	p := NewIngestPage("Ingesting")
	p.running = true

	updated, cmd := p.Update(progressTickMsg{})
	ip := updated.(*IngestPage)
	if !ip.running {
		t.Error("should still be running after tick")
	}
	if cmd == nil {
		t.Error("should return another tick command while running")
	}
}

func TestIngestPage_TickStops_WhenNotRunning(t *testing.T) {
	p := NewIngestPage("Ingesting")
	p.running = false

	_, cmd := p.Update(progressTickMsg{})
	if cmd != nil {
		t.Error("should not return tick command when not running")
	}
}

// ---------------------------------------------------------------------------
// PrivacyPreferencePage
// ---------------------------------------------------------------------------

// TestPrivacyPreferencePage_DefaultsToTheRecommendedLevel pins the default the
// wizard opens on.
//
// The index is derived rather than written as a literal. It used to be the literal
// 1, which was the recommended option only for as long as the list had the two
// entries it had when that was written; removing the first entry left the cursor
// one past the end, where the first read of SelectedLevel panics.
func TestPrivacyPreferencePage_DefaultsToTheRecommendedLevel(t *testing.T) {
	p := NewPrivacyPreferencePage("Privacy")
	want := recommendedPrivacyOption()
	if p.cursor != want {
		t.Errorf("default cursor = %d, want %d (the recommended option)", p.cursor, want)
	}
	if got := p.SelectedLevel(); got != config.RecommendedRedactionLevel.String() {
		t.Errorf("default SelectedLevel() = %q, want %q", got, config.RecommendedRedactionLevel)
	}
	// The default has to be a level the product will actually run, or the wizard
	// writes a configuration that breaks the next command.
	if !config.RedactionLevelOffered(redact.RedactionLevel(p.SelectedLevel())) {
		t.Errorf("the wizard opens on %q, which this version does not offer", p.SelectedLevel())
	}
}

// TestPrivacyPreferencePage_OffersOnlyLevelsTheProductOffers is the guard the
// requirement rests on: onboarding must not present a level other than the offered
// one, and it must not omit one either.
//
// It is the SOLE enforcement, and deliberately so. Two package init() panics used
// to duplicate it; they aborted the shipped binary on a static developer-facing
// mismatch - a safe additive widening of the offered menu made `peasant version`
// panic - and the count-comparing one caught strictly less than this does. This
// names the levels rather than only the count, and checks containment in both
// directions.
func TestPrivacyPreferencePage_OffersOnlyLevelsTheProductOffers(t *testing.T) {
	offered := make([]redact.RedactionLevel, 0, len(privacyOptions))
	for _, option := range privacyOptions {
		if !config.RedactionLevelOffered(option.level) {
			t.Errorf("the privacy page offers %q, which this version does not offer; a user selecting it would write a "+
				"configuration the next command refuses", option.level)
		}
		offered = append(offered, option.level)
	}
	for _, level := range config.OfferedRedactionLevels {
		if !slices.Contains(offered, level) {
			t.Errorf("the product offers %q but the privacy page does not; onboarding would silently withhold a real "+
				"choice", level)
		}
	}
	// Every level the ENGINE knows that is NOT offered must be absent. This is the
	// direction the requirement is about: minimal and maximum must not appear on
	// these screens at all.
	for _, level := range redact.AllRedactionLevels() {
		if config.RedactionLevelOffered(level) {
			continue
		}
		if slices.Contains(offered, level) {
			t.Errorf("the privacy page still presents %q", level)
		}
		// Not just the option list: the rendered screen must not name it either,
		// because the requirement is that onboarding does not MENTION it.
		if rendered := NewPrivacyPreferencePage("Privacy").View(100, 40); strings.Contains(strings.ToLower(rendered), level.String()) {
			t.Errorf("the rendered privacy screen mentions %q; onboarding must not offer or describe it. Screen:\n%s",
				level, rendered)
		}
	}
}

func TestPrivacyPreferencePage_CursorNavigation(t *testing.T) {
	p := NewPrivacyPreferencePage("Privacy")

	// Can't go above the first option.
	p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if p.cursor < 0 {
		t.Errorf("after up: cursor = %d, want a valid index", p.cursor)
	}
	for range privacyOptions {
		p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	}
	if p.cursor != 0 {
		t.Errorf("after walking up the whole list: cursor = %d, want 0", p.cursor)
	}

	// Move down to the last offered level. The bound is derived from the option
	// list rather than hardcoded, because the list holds only the levels the
	// product will actually run - maximum is not offered at all.
	last := len(privacyOptions) - 1
	for range privacyOptions {
		p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if p.cursor != last {
		t.Errorf("after walking down the whole list: cursor = %d, want %d", p.cursor, last)
	}
	if p.SelectedLevel() != string(privacyOptions[last].level) {
		t.Errorf("at cursor %d: SelectedLevel() = %q, want %q", last, p.SelectedLevel(), privacyOptions[last].level)
	}

	// Can't go past the end.
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.cursor != last {
		t.Errorf("after one more down: cursor = %d, want %d", p.cursor, last)
	}
}

func TestPrivacyPreferencePage_EnterConfirms(t *testing.T) {
	p := NewPrivacyPreferencePage("Privacy")
	if p.IsComplete() {
		t.Error("should not be complete before enter")
	}

	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !p.IsComplete() {
		t.Error("should be complete after enter")
	}
	if p.SelectedLevel() != "standard" {
		t.Errorf("SelectedLevel() = %q, want %q", p.SelectedLevel(), "standard")
	}
}

// TestPrivacyPreferencePage_NumberKeySelectsAndConfirms derives the shortcuts from
// the option list, so a level added to or removed from the wizard cannot leave a
// digit pointing at the wrong one.
func TestPrivacyPreferencePage_NumberKeySelectsAndConfirms(t *testing.T) {
	for index, option := range privacyOptions {
		key := rune('1' + index)
		t.Run(string(key), func(t *testing.T) {
			p := NewPrivacyPreferencePage("Privacy")
			p.Update(tea.KeyPressMsg{Code: key, Text: string(key)})
			if !p.IsComplete() {
				t.Errorf("pressing %q should complete the page", string(key))
			}
			if p.SelectedLevel() != string(option.level) {
				t.Errorf("pressing %q: SelectedLevel() = %q, want %q", string(key), p.SelectedLevel(), option.level)
			}
		})
	}
}

// TestPrivacyPreferencePage_DoesNotOfferAnUnsupportedLevel is the wizard-side half
// of the hard-fail ruling. Every command refuses to run under a level the product
// cannot apply, so onboarding must not be able to write one: a user would finish
// the wizard and find the next import and the next upload both refuse.
func TestPrivacyPreferencePage_DoesNotOfferAnUnsupportedLevel(t *testing.T) {
	p := NewPrivacyPreferencePage("Privacy")
	for index := range privacyOptions {
		key := rune('1' + index)
		p.Update(tea.KeyPressMsg{Code: key, Text: string(key)})
		if !config.RedactionLevelSupported(redact.RedactionLevel(p.SelectedLevel())) {
			t.Errorf("pressing %q selects %q, which every command refuses to run", string(key), p.SelectedLevel())
		}
	}
	// A digit past the end must not select anything, rather than wrapping onto a
	// level that is not offered.
	p = NewPrivacyPreferencePage("Privacy")
	past := rune('1' + len(privacyOptions))
	p.Update(tea.KeyPressMsg{Code: past, Text: string(past)})
	if p.IsComplete() {
		t.Errorf("pressing %q, past the end of the list, must not confirm a selection", string(past))
	}
	if strings.Contains(p.View(80, 24), "Maximum") {
		t.Error("the wizard must not display Maximum as an option")
	}
}

func TestPrivacyPreferencePage_Reset(t *testing.T) {
	p := NewPrivacyPreferencePage("Privacy")
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !p.IsComplete() {
		t.Fatal("should be complete after enter")
	}

	p.Reset()
	if p.IsComplete() {
		t.Error("should not be complete after reset")
	}
}

// TestPrivacyPreferencePage_StatesRatherThanOffersWhenOneLevelIsOffered pins the
// single-option contract: state the applied level rather than offer a choice.
//
// With one level offered, a radio glyph, a "(recommended)" tag and an up/down hint
// assert alternatives that do not exist. On a privacy screen that is the worst
// available implication - a careful reader looks for the hidden options and
// concludes something was withheld, on the one screen whose job is to say what is
// withheld from everyone else.
func TestPrivacyPreferencePage_StatesRatherThanOffersWhenOneLevelIsOffered(t *testing.T) {
	if len(config.OfferedRedactionLevels) != 1 {
		t.Skip("more than one level is offered, so the chooser is the correct rendering; see the sibling test")
	}
	view := NewPrivacyPreferencePage("Privacy").View(80, 24)

	// It states what will happen, naming the level.
	for _, want := range []string{"before they leave your machine", privacyLevelLabel(privacyOptions[0].level), "enter: continue"} {
		if !strings.Contains(view, want) {
			t.Errorf("the screen must state what will be applied; missing %q in:\n%s", want, view)
		}
	}

	// THE SCOPE NOUN, which is the word this screen's correctness turns on and
	// which was measured UNPINNED: changing it from metadata to transcripts was
	// green, so the sentence could be reverted to its false form with nothing
	// going red.
	//
	// On the automatic path - the only path a user has met by this point in
	// onboarding - nothing redacts transcript CONTENT. The pipeline's redactor
	// would, and no production caller sets it. What is redacted automatically is
	// metadata, on the way out. A screen promising more than that on the screen
	// whose job is to say what leaves the machine is wrong in the one direction
	// that costs the user something.
	// The needle is a LITERAL, deliberately, and that distinction has already
	// decided once whether this guard works. Written as
	// strings.Contains(view, privacyScopeNoun) it reads the same constant the
	// screen reads, so changing that constant moves both sides and the check
	// passes - measured GREEN in exactly that form. A needle derived from the code
	// under test asserts only that the code equals itself.
	//
	// What it pins is now TRUE rather than aspirational: push applies the same
	// content redaction `peasant redact` applies, through the same function, so
	// "your transcripts will be redacted" describes the automatic path. It was
	// corrected to metadata for one round, while that was not the case.
	if !strings.Contains(view, "transcripts will be redacted") {
		t.Errorf("the screen no longer says transcripts will be redacted. That claim is true - push redacts content "+
			"through the same path as the redact command - and this screen is where a user learns it:\n%s", view)
	}
	// The forbidden form: hedged wording that falsely claims content is not
	// redacted on the automatic path. Restoring it would understate what now
	// happens, on the screen whose job is saying what leaves the machine.
	if strings.Contains(view, "applied automatically to their METADATA") {
		t.Errorf("the screen still limits redaction to metadata. Content is redacted on the way out now, so this "+
			"understates the protection and reintroduces the split a user asked to be closed:\n%s", view)
	}

	// And the shared hedge, printed rather than restated. This screen was the only
	// redaction surface omitting it, which is how its wording drifted.
	if !strings.Contains(view, config.RedactionScopeSentence()) {
		t.Errorf("the screen does not print config.RedactionScopeSentence verbatim. Every other redaction surface does, "+
			"and a locally-worded hedge is how this one drifted into over-claiming:\nwant: %s\ngot:\n%s",
			config.RedactionScopeSentence(), view)
	}
	// And it asserts no alternatives. Each of these is an affordance implying a
	// menu the user cannot reach.
	for _, forbidden := range []string{"(recommended)", "●", "○", "↑/↓", "Choose your default"} {
		if strings.Contains(view, forbidden) {
			t.Errorf("the screen presents %q, which implies alternatives that do not exist for a one-level menu:\n%s",
				forbidden, view)
		}
	}
}

// TestPrivacyPreferencePage_ChoosesWhenMoreThanOneLevelIsOffered is the widening
// rehearsal: the chooser machinery is kept, not deleted, and this drives it.
//
// The multi-option rendering stays in the code and returns automatically when a
// second level is offered. Without this, the currently unreachable branch could
// rot unobserved.
//
// It drives View, not viewChooser. Calling the renderer directly proves only that
// the renderer renders; the required property is about the dispatch - that the
// screen changes shape on its own when the menu widens. The test therefore
// widens the policy menu and calls View rather than invoking viewChooser directly.
func TestPrivacyPreferencePage_ChoosesWhenMoreThanOneLevelIsOffered(t *testing.T) {
	// Not parallel: this temporarily widens package-level state that other tests
	// in this package read.
	restoreOffered := config.OfferedRedactionLevels
	restoreOptions := privacyOptions
	t.Cleanup(func() {
		config.OfferedRedactionLevels = restoreOffered
		privacyOptions = restoreOptions
	})

	// The widening the disposition table exists to absorb: a second level becomes
	// offered, and the wizard grows the option that goes with it.
	config.OfferedRedactionLevels = []redact.RedactionLevel{redact.Minimal, redact.Standard}
	privacyOptions = []privacyOption{
		{level: redact.Minimal, label: "Minimal", description: "A second level, offered.", keeps: "Keeps: less."},
		{level: redact.Standard, label: "Standard", recommended: true, description: restoreOptions[0].description, keeps: restoreOptions[0].keeps},
	}

	view := NewPrivacyPreferencePage("Privacy").View(80, 24)
	for _, want := range []string{"Choose your default", "●", "↑/↓", "(recommended)", "Minimal", "Standard"} {
		if !strings.Contains(view, want) {
			t.Errorf("with two levels offered the screen must present them as a CHOICE and come back to the chooser on "+
				"its own; missing %q in:\n%s", want, view)
		}
	}
	if strings.Contains(view, "will be redacted at the") {
		t.Errorf("the screen still states a single level while two are offered, so it is withholding one:\n%s", view)
	}
}

// TestPrivacyPreferencePage_DerivesItsExamplesFromTheRedactor ensures the
// onboarding examples remain coupled to the production redactor.
//
// The screen used to hand-write its examples, and both were wrong. It showed
// `[REDACTED_EMAIL]` - a bracket form the product never emits, with the string
// `REDACTED_` appearing nowhere in the redaction module - and, worse, it showed
// `/Users/alice/projects/ -> [REDACTED_PATH]`, promising that the whole path was
// withheld when the rules replace only the username segment. A user reading it
// believed their directory structure never left the machine.
//
// The assertions below are chosen so that DERIVING the examples is the only way to
// pass them, rather than re-typing today's correct strings:
//   - the invented token must not appear anywhere;
//   - the path example must still show the surviving directory, which is what makes
//     the partial redaction visible rather than over-claimed;
//   - a secret must be shown, the category the screen previously omitted entirely;
//   - and every rendered line must be what the redactor actually returns.
func TestPrivacyPreferencePage_DerivesItsExamplesFromTheRedactor(t *testing.T) {
	page := NewPrivacyPreferencePage("Privacy")
	if len(page.examples) == 0 {
		t.Fatal("the screen rendered no examples at all. That is the fail-safe path, which means privacyExamples " +
			"errored: a category the screen claims to redact is no longer being redacted, or the redactor could not be " +
			"built. Shipping is not blocked by an empty block, but the screen has stopped showing what redaction does.")
	}
	view := page.View(80, 24)

	if strings.Contains(view, "REDACTED_") {
		t.Errorf("the screen shows the invented token REDACTED_*, which the redaction module never emits; the examples are being "+
			"written rather than produced:\n%s", view)
	}
	// The username is replaced while the remaining directory stays visible.
	if !strings.Contains(view, "projects") {
		t.Errorf("the path example no longer shows the surviving directory. Path rules replace the USERNAME SEGMENT and "+
			"keep the rest, so hiding the whole path here would over-claim the protection:\n%s", view)
	}
	redactor, err := redact.NewRedactor(privacyOptions[0].level, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("build a redactor to check the screen against: %v", err)
	}
	var sawSecret bool
	for _, sample := range privacyExampleInputs {
		produced := redactor.RedactText(sample.text)
		if produced == sample.text {
			t.Errorf("the %s sample %q survives redaction unchanged, so the screen would show it being kept while "+
				"claiming the category is redacted", sample.why, sample.text)
			continue
		}
		if !strings.Contains(view, produced) {
			t.Errorf("the screen does not show what the redactor actually returns for the %s sample.\nsample:   %q\n"+
				"redactor: %q\nscreen:\n%s", sample.why, sample.text, produced, view)
		}
		if sample.why == redact.CategorySecrets {
			sawSecret = true
		}
	}
	if !sawSecret {
		t.Error("no secret example is shown. Standard claims to redact secrets, so the screen must demonstrate that category.")
	}
}

func TestPrivacyPreferencePage_Title(t *testing.T) {
	p := NewPrivacyPreferencePage("Privacy Preference")
	if p.Title() != "Privacy Preference" {
		t.Errorf("Title() = %q, want %q", p.Title(), "Privacy Preference")
	}
}

// ---------------------------------------------------------------------------
// LicensePage
// ---------------------------------------------------------------------------

func TestLicensePage_DefaultsToCC0(t *testing.T) {
	p := NewLicensePage("Content License")
	// Kickstart requires a real license (no "none" option); the default cursor sits
	// on the first/most-permissive entry, CC0.
	if got := p.SelectedLicense(); got != schema.LicenseCC0 {
		t.Errorf("default SelectedLicense = %q, want %q", got, schema.LicenseCC0)
	}
	// Invariant: every menu entry is a real license, so the page can never yield an
	// empty selection — a contributor who reaches it must pick one.
	for _, opt := range licenseOptions {
		if opt.license == "" {
			t.Error("licenseOptions contains an empty (none) entry; kickstart must offer only real licenses")
		}
	}
}

// TestWizard_LicensePage_AlwaysShown is a regression guard: the kickstart license
// page must be reachable regardless of whether the user connected to the village.
// It was briefly gated behind VillageConnected, which hid it from anyone who did
// not connect during kickstart.
func TestWizard_LicensePage_AlwaysShown(t *testing.T) {
	for _, connected := range []bool{true, false} {
		m := NewWizard()
		m.answers.VillageConnected = connected
		if m.shouldSkip(pageLicense) {
			t.Errorf("license page skipped with VillageConnected=%v; it must always be shown", connected)
		}
	}
}

func TestLicensePage_NumberKeySelectsAndConfirms(t *testing.T) {
	p := NewLicensePage("Content License")
	// "2" selects the second option (CC BY 4.0 — index 1) and confirms.
	p.Update(tea.KeyPressMsg{Text: "2"})
	if !p.IsComplete() {
		t.Error("number key should confirm the page")
	}
	if got := p.SelectedLicense(); got != schema.LicenseCCBY {
		t.Errorf("SelectedLicense after '2' = %q, want %q", got, schema.LicenseCCBY)
	}
}

// ---------------------------------------------------------------------------
// Wizard — page sequence includes PrivacyPreferencePage
// ---------------------------------------------------------------------------

func TestWizard_PageSequence_IncludesPrivacyPage(t *testing.T) {
	m := NewWizard()

	if len(m.pages) != 11 {
		t.Fatalf("expected 11 pages, got %d", len(m.pages))
	}

	// The privacy identity should mount PrivacyPreferencePage.
	pp, ok := m.pages[pagePrivacy].(*PrivacyPreferencePage)
	if !ok {
		t.Fatalf("page %d should be *PrivacyPreferencePage, got %T", pagePrivacy, m.pages[pagePrivacy])
	}
	if pp.Title() != "Privacy Preference" {
		t.Errorf("page %d title = %q, want %q", pagePrivacy, pp.Title(), "Privacy Preference")
	}

	// The license identity should mount LicensePage.
	if _, ok := m.pages[pageLicense].(*LicensePage); !ok {
		t.Errorf("page %d should be *LicensePage, got %T", pageLicense, m.pages[pageLicense])
	}

	// The consent identity should mount InfoPage.
	if _, ok := m.pages[pageSummary].(*InfoPage); !ok {
		t.Errorf("page %d should be *InfoPage, got %T", pageSummary, m.pages[pageSummary])
	}

	// The execution identity should mount IngestPage.
	if _, ok := m.pages[pageIngestion].(*IngestPage); !ok {
		t.Errorf("page %d should be *IngestPage, got %T", pageIngestion, m.pages[pageIngestion])
	}
}

// ---------------------------------------------------------------------------
// BuildSummaryContent — includes redaction level
// ---------------------------------------------------------------------------

func TestBuildSummaryContentShowsOnlyStandardRedaction(t *testing.T) {
	a := &WizardAnswers{
		VillageConnected: true,
		WantImport:       false,
		RedactionLevel:   "maximum",
	}

	content := BuildSummaryContent(a)
	if strings.Contains(content, "maximum") || !strings.Contains(content, "Standard (the only onboarding policy)") {
		t.Errorf("summary exposed a configured unsupported level instead of Standard; got:\n%s", content)
	}
	if !strings.Contains(content, "Redaction:") {
		t.Errorf("summary should contain 'Redaction:' label; got:\n%s", content)
	}
}

func TestBuildSummaryContent_DefaultRedactionLevel(t *testing.T) {
	a := &WizardAnswers{
		VillageConnected: false,
		WantImport:       false,
	}

	content := BuildSummaryContent(a)
	if !strings.Contains(content, "Standard") {
		t.Errorf("summary should show Standard when RedactionLevel is empty; got:\n%s", content)
	}
}

func TestBuildSummaryContent_ProviderSelections(t *testing.T) {
	a := &WizardAnswers{
		WantImport: true,
		ProviderSelections: []ProviderSelection{
			{Harness: string(defaults.HarnessClaudeCode), ImportAll: true},
			{Harness: string(defaults.HarnessOpenCode), ImportAll: false},
		},
		RedactionLevel: "standard",
	}

	content := BuildSummaryContent(a)
	if !strings.Contains(content, "Claude Code (all)") {
		t.Errorf("summary should show provider with (all) import; got:\n%s", content)
	}
	if !strings.Contains(content, "OpenCode (select sessions)") {
		t.Errorf("summary should show provider with (select sessions) import; got:\n%s", content)
	}
}

// ---------------------------------------------------------------------------
// ProviderSelectPage
// ---------------------------------------------------------------------------

func enabledProviderInventory(counts map[defaults.Harness]int) ProviderInventory {
	inventory := make(ProviderInventory, len(counts))
	for harness, count := range counts {
		inventory[harness] = ProviderDiscovery{SessionCount: count, Enabled: true}
	}
	return inventory
}

func TestProviderSelectPage_ShowsAllProvidersIncludingZeroCount(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{
		defaults.HarnessClaudeCode: 42,
		defaults.HarnessOpenCode:   0,
		defaults.HarnessGeminiCLI:  3,
	})
	p := NewProviderSelectPage("Select Providers", "desc", inventory)
	if len(p.providers) != len(defaults.AllHarnesses) {
		t.Fatalf("expected %d providers, got %d", len(defaults.AllHarnesses), len(p.providers))
	}
	// Providers with sessions should be checked; 0-count should not.
	for _, e := range p.providers {
		if e.sessionCount > 0 && !e.checked {
			t.Errorf("provider %q has %d sessions but is not checked", e.displayName, e.sessionCount)
		}
		if e.sessionCount == 0 && e.checked {
			t.Errorf("provider %q has 0 sessions but is checked", e.displayName)
		}
	}
}

func TestProviderSelectPage_DefaultState(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 10})
	p := NewProviderSelectPage("title", "desc", inventory)

	if len(p.providers) != len(defaults.AllHarnesses) {
		t.Fatalf("expected %d providers, got %d", len(defaults.AllHarnesses), len(p.providers))
	}
	// Claude (first in AllProviders) should be checked with sessions.
	if !p.providers[0].checked {
		t.Error("Claude should be checked by default (has sessions)")
	}
	if p.providers[0].importAll {
		t.Error("import mode should default to Select sessions (importAll=false)")
	}
	// Other providers with 0 sessions should not be checked.
	for _, e := range p.providers[1:] {
		if e.checked {
			t.Errorf("provider %q with 0 sessions should not be checked", e.displayName)
		}
	}
	if p.IsComplete() {
		t.Error("page should not be complete initially")
	}
}

func TestProviderSelectPage_DefaultsEveryOperationalHarnessSelected(t *testing.T) {
	for _, enabledHarness := range defaults.AllHarnesses {
		inventory := ProviderInventory{}
		for _, harness := range defaults.AllHarnesses {
			inventory[harness] = ProviderDiscovery{SessionCount: 1, Enabled: harness == enabledHarness}
		}
		p := NewProviderSelectPage("title", "desc", inventory)
		for _, provider := range p.providers {
			if !provider.checked {
				t.Errorf("configured harness %q: operational provider %q was not selected by default", enabledHarness, provider.provider)
			}
		}
	}
}

func TestProviderSelectPage_FlatRowsWithCheckedProvider(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 10})
	p := NewProviderSelectPage("title", "desc", inventory)

	// Only providers with sessions appear in flatRows (0-count are excluded).
	// Claude is checked: provider + 2 sub-items = 3 rows.
	rows := p.flatRows()
	if len(rows) != 3 {
		t.Fatalf("expected 3 flat rows (provider + 2 sub-items), got %d", len(rows))
	}
	if rows[0].kind != providerRowProvider {
		t.Errorf("row 0 kind = %d, want providerRowProvider", rows[0].kind)
	}
	if rows[1].kind != providerRowImportAll {
		t.Errorf("row 1 kind = %d, want providerRowImportAll", rows[1].kind)
	}
	if rows[2].kind != providerRowSelect {
		t.Errorf("row 2 kind = %d, want providerRowSelect", rows[2].kind)
	}
}

func TestProviderSelectPage_FlatRowsWithUncheckedProvider(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 10})
	p := NewProviderSelectPage("title", "desc", inventory)

	// Uncheck Claude — sub-items disappear.
	p.providers[0].checked = false
	rows := p.flatRows()
	// Only Claude (unchecked, 1 row) is in flatRows; 0-count providers are excluded.
	if len(rows) != 1 {
		t.Fatalf("expected 1 flat row (unchecked provider only), got %d", len(rows))
	}
	if rows[0].kind != providerRowProvider {
		t.Errorf("row 0 kind = %d, want providerRowProvider", rows[0].kind)
	}
}

func TestProviderSelectPage_SpaceTogglesCheckbox(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 10})
	p := NewProviderSelectPage("title", "desc", inventory)

	// Toggle off.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if p.providers[0].checked {
		t.Error("space should uncheck the provider")
	}

	// Toggle back on.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if !p.providers[0].checked {
		t.Error("space should re-check the provider")
	}
}

func TestProviderSelectPage_SpaceTogglesEnterConfirms(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 10})
	p := NewProviderSelectPage("title", "desc", inventory)

	// Space on a provider row should toggle the checkbox.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if p.providers[0].checked {
		t.Error("space should uncheck the provider (toggle)")
	}
	if p.IsComplete() {
		t.Error("space should not complete the page")
	}

	// Re-check and confirm with Enter.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !p.IsComplete() {
		t.Error("enter should complete the page when a provider is checked")
	}
}

func TestProviderSelectPage_SubItemSelectsImportMode(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 10})
	p := NewProviderSelectPage("title", "desc", inventory)

	// Default: importAll = false (Select sessions). Cursor at row 0 (provider).
	// Move to row 1 (Import all sub-item) and press Space to select.
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if !p.providers[0].importAll {
		t.Error("space on Import all sub-item should set importAll=true")
	}

	// Move to row 2 (Select sessions sub-item) and press Space.
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if p.providers[0].importAll {
		t.Error("space on Select sessions sub-item should set importAll=false")
	}
}

func TestProviderSelectPage_SubItemsDisappearOnUncheck(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 10})
	p := NewProviderSelectPage("title", "desc", inventory)

	// Initially 3 rows: Claude provider + 2 sub-items (0-count excluded from flatRows).
	rows := p.flatRows()
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows initially, got %d", len(rows))
	}

	// Uncheck Claude at row 0.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})

	// Now 1 row: Claude unchecked (no sub-items).
	rows = p.flatRows()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after uncheck, got %d", len(rows))
	}
}

func TestProviderSelectPage_CursorClampsOnUncheck(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 10})
	p := NewProviderSelectPage("title", "desc", inventory)

	// Move cursor to row 2 (Select sessions sub-item).
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.cursor != 2 {
		t.Fatalf("cursor = %d, want 2", p.cursor)
	}

	// Now move back to provider row (row 0) and uncheck it.
	p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})

	// After uncheck, flat rows = [provider]. Cursor should be at 0 (provider).
	rows := p.flatRows()
	if p.cursor >= len(rows) {
		t.Errorf("cursor %d should be < len(rows) %d after unchecking", p.cursor, len(rows))
	}
}

func TestProviderSelectPage_EnterConfirmsFromAnyPosition(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 10})
	p := NewProviderSelectPage("title", "desc", inventory)

	// Enter from row 0 (provider) should complete when a provider is checked.
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !p.IsComplete() {
		t.Error("enter should complete the page when a provider is checked")
	}
}

func TestProviderSelectPage_EnterWithNoneCheckedSkipsImport(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 10})
	p := NewProviderSelectPage("title", "desc", inventory)

	// Uncheck the provider.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	// Enter should complete (skip import) even with no providers checked.
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !p.IsComplete() {
		t.Error("enter should complete the page (skip import) when no providers are checked")
	}
	if len(p.Selections()) != 0 {
		t.Error("selections should be empty when no providers are checked")
	}
}

func TestProviderSelectPage_Selections(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{
		defaults.HarnessClaudeCode: 10,
		defaults.HarnessOpenCode:   5,
	})
	p := NewProviderSelectPage("title", "desc", inventory)

	// Flat rows (0-count providers excluded; only Claude + OpenCode):
	//   0: Claude (provider, checked)
	//   1: Import all (Claude)
	//   2: Select sessions (Claude)
	//   3: OpenCode (provider, checked)
	//   4: Import all (OpenCode)
	//   5: Select sessions (OpenCode)

	// Switch Claude to import-all: move to row 1 (Import all) and press Space.
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "}) // select Import all

	// Navigate to OpenCode provider row (row 3) and uncheck it.
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})    // row 2
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})    // row 3 (OpenCode)
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "}) // uncheck OpenCode

	sels := p.Selections()
	if len(sels) != 1 {
		t.Fatalf("expected 1 selection, got %d", len(sels))
	}
	if sels[0].Harness != string(defaults.HarnessClaudeCode) {
		t.Errorf("selected provider = %q, want %q", sels[0].Harness, string(defaults.HarnessClaudeCode))
	}
	if !sels[0].ImportAll {
		t.Error("Claude should have ImportAll=true after selecting sub-item")
	}
}

func TestProviderSelectPage_Reset(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 10})
	p := NewProviderSelectPage("title", "desc", inventory)

	// Complete the page: Enter confirms from any position.
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !p.IsComplete() {
		t.Fatal("page should be complete")
	}

	p.Reset()
	if p.IsComplete() {
		t.Error("page should not be complete after reset")
	}
}

func TestProviderSelectPage_CursorNavigation(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{
		defaults.HarnessClaudeCode: 10,
		defaults.HarnessOpenCode:   5,
	})
	p := NewProviderSelectPage("title", "desc", inventory)

	// Flat rows (0-count excluded; only Claude + OpenCode):
	//   0: Claude (provider, checked)
	//   1: Import all (Claude)
	//   2: Select sessions (Claude)
	//   3: OpenCode (provider, checked)
	//   4: Import all (OpenCode)
	//   5: Select sessions (OpenCode)
	rows := p.flatRows()
	if len(rows) != 6 {
		t.Fatalf("expected 6 flat rows, got %d", len(rows))
	}

	if p.cursor != 0 {
		t.Errorf("initial cursor = %d, want 0", p.cursor)
	}

	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.cursor != 1 {
		t.Errorf("after 1x down: cursor = %d, want 1", p.cursor)
	}

	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.cursor != 2 {
		t.Errorf("after 2x down: cursor = %d, want 2", p.cursor)
	}

	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.cursor != 3 {
		t.Errorf("after 3x down: cursor = %d, want 3 (OpenCode)", p.cursor)
	}

	// Navigate to end.
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.cursor != 5 {
		t.Errorf("after navigating to end: cursor = %d, want 5 (last row)", p.cursor)
	}

	// Can't go past last row.
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.cursor != 5 {
		t.Errorf("cursor should be clamped at 5, got %d", p.cursor)
	}

	p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if p.cursor != 4 {
		t.Errorf("after up: cursor = %d, want 4", p.cursor)
	}
}

func TestProviderSelectPage_ViewContent(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 42})
	p := NewProviderSelectPage("Select Providers", "Choose providers", inventory)

	view := p.View(80, 24)
	for _, want := range []string{
		"Select Providers",
		"Choose providers",
		"Claude Code",
		"42 sessions",
		"Import all",
		"Select sessions",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("View should contain %q; got:\n%s", want, view)
		}
	}
}

func TestProviderSelectPage_ViewNoTabInHelpBar(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 10})
	p := NewProviderSelectPage("title", "desc", inventory)
	view := p.View(80, 24)
	if strings.Contains(view, "tab:") {
		t.Error("help bar should not mention 'tab:' — Tab key is no longer used")
	}
}

// ---------------------------------------------------------------------------
// TreeSelectPage — expand/collapse and section jumping
// ---------------------------------------------------------------------------

func TestTreeSelectPage_LExpandsProviderAndRemote(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Provider starts collapsed.
	if p.providers[0].expanded {
		t.Fatal("provider should start collapsed")
	}

	// Press l to expand.
	p.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	if !p.providers[0].expanded {
		t.Error("l should expand provider")
	}

	// Move cursor to project row and press right arrow to expand project.
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	p.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if !p.providers[0].remotes[0].expanded {
		t.Error("right arrow should expand project")
	}
}

func TestTreeSelectPage_HCollapsesProviderAndRemote(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Expand everything first.
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true

	// Move to project row and press h to collapse.
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	p.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
	if p.providers[0].remotes[0].expanded {
		t.Error("h should collapse project")
	}

	// Move to provider row and press left to collapse.
	p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	p.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if p.providers[0].expanded {
		t.Error("left should collapse provider")
	}
}

func TestTreeSelectPage_HOnSessionCollapsesParentWorktree(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s2"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Expand provider, remote, and worktree.
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true
	p.providers[0].remotes[0].worktrees[0].expanded = true

	// Move cursor to a session row: provider(0) -> remote(1) -> worktree(2) -> session(3).
	p.cursor = 3
	p.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
	if p.providers[0].remotes[0].worktrees[0].expanded {
		t.Error("h on session row should collapse parent worktree")
	}
}

func TestTreeSelectPage_LOnSessionDoesNotToggleSelection(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Expand everything.
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true
	p.providers[0].remotes[0].worktrees[0].expanded = true

	// Move to session row: provider(0) -> remote(1) -> worktree(2) -> session(3).
	p.cursor = 3
	p.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	if p.sessionSel[0][0][0][0] {
		t.Error("l on session row should NOT toggle selection")
	}
}

func TestTreeSelectPage_ExpandOnlyDoesNotToggle(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Expand provider.
	p.providers[0].expanded = true
	// Press l on provider (already expanded) — should stay expanded, not toggle.
	p.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	if !p.providers[0].expanded {
		t.Error("l should only expand, never collapse (should stay expanded)")
	}
}

func TestTreeSelectPage_BracketLeftJumpsToPreviousSibling(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-b", SessionID: "s2"},
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "proj-c", SessionID: "s3"},
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	// Flat items: Claude(0), proj-a(1), proj-b(2), OpenCode(3).

	// From OpenCode (provider, cursor=3), [ should jump to Claude (cursor=0).
	p.cursor = 3
	p.Update(tea.KeyPressMsg{Code: '[', Text: "["})
	if p.cursor != 0 {
		t.Errorf("after [ from OpenCode provider: cursor = %d, want 0 (Claude)", p.cursor)
	}

	// From proj-b (cursor=2), [ should jump to proj-a (cursor=1).
	p.cursor = 2
	p.Update(tea.KeyPressMsg{Code: '[', Text: "["})
	if p.cursor != 1 {
		t.Errorf("after [ from proj-b: cursor = %d, want 1 (proj-a)", p.cursor)
	}

	// From Claude (cursor=0), [ should be no-op.
	p.cursor = 0
	p.Update(tea.KeyPressMsg{Code: '[', Text: "["})
	if p.cursor != 0 {
		t.Errorf("after [ from first provider: cursor = %d, want 0 (no-op)", p.cursor)
	}
}

func TestTreeSelectPage_BracketLeft_FirstProjectStaysOnProject(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-b", SessionID: "s2"},
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	// Flat items: Claude(0), proj-a(1), proj-b(2).

	// From proj-a (first project, cursor=1), [ should be no-op (stay on proj-a).
	// It must NOT jump to the provider header at cursor=0.
	p.cursor = 1
	p.Update(tea.KeyPressMsg{Code: '[', Text: "["})
	if p.cursor != 1 {
		t.Errorf("after [ from first project: cursor = %d, want 1 (no-op, should not jump to provider header)", p.cursor)
	}

	// Sanity: from proj-b (cursor=2), [ should jump to proj-a (cursor=1).
	p.cursor = 2
	p.Update(tea.KeyPressMsg{Code: '[', Text: "["})
	if p.cursor != 1 {
		t.Errorf("after [ from proj-b: cursor = %d, want 1 (proj-a)", p.cursor)
	}
}

func TestTreeSelectPage_BracketRightJumpsToNextSibling(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-b", SessionID: "s2"},
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "proj-c", SessionID: "s3"},
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	// Flat items: Claude(0), proj-a(1), proj-b(2), OpenCode(3).

	// From Claude (provider, cursor=0), ] should jump to OpenCode (cursor=3).
	p.cursor = 0
	p.Update(tea.KeyPressMsg{Code: ']', Text: "]"})
	if p.cursor != 3 {
		t.Errorf("after ] from Claude provider: cursor = %d, want 3 (OpenCode)", p.cursor)
	}

	// Go back. From proj-a (cursor=1), ] should jump to proj-b (cursor=2).
	p.cursor = 1
	p.Update(tea.KeyPressMsg{Code: ']', Text: "]"})
	if p.cursor != 2 {
		t.Errorf("after ] from proj-a: cursor = %d, want 2 (proj-b)", p.cursor)
	}
}

func TestTreeSelectPage_AltK_PrevSibling(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-b", SessionID: "s2"},
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "proj-c", SessionID: "s3"},
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	// Flat items: Claude(0), proj-a(1), proj-b(2), OpenCode(3).

	// From OpenCode (provider, cursor=3), Alt+k should jump to Claude (cursor=0).
	p.cursor = 3
	p.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModAlt})
	if p.cursor != 0 {
		t.Errorf("after Alt+k from OpenCode provider: cursor = %d, want 0 (Claude)", p.cursor)
	}

	// From proj-b (cursor=2), Alt+k should jump to proj-a (cursor=1).
	p.cursor = 2
	p.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModAlt})
	if p.cursor != 1 {
		t.Errorf("after Alt+k from proj-b: cursor = %d, want 1 (proj-a)", p.cursor)
	}

	// From Claude (cursor=0), Alt+k should be no-op.
	p.cursor = 0
	p.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModAlt})
	if p.cursor != 0 {
		t.Errorf("after Alt+k from first provider: cursor = %d, want 0 (no-op)", p.cursor)
	}
}

func TestTreeSelectPage_AltJ_NextSibling(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-b", SessionID: "s2"},
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "proj-c", SessionID: "s3"},
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	// Flat items: Claude(0), proj-a(1), proj-b(2), OpenCode(3).

	// From Claude (provider, cursor=0), Alt+j should jump to OpenCode (cursor=3).
	p.cursor = 0
	p.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModAlt})
	if p.cursor != 3 {
		t.Errorf("after Alt+j from Claude provider: cursor = %d, want 3 (OpenCode)", p.cursor)
	}

	// Go back. From proj-a (cursor=1), Alt+j should jump to proj-b (cursor=2).
	p.cursor = 1
	p.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModAlt})
	if p.cursor != 2 {
		t.Errorf("after Alt+j from proj-a: cursor = %d, want 2 (proj-b)", p.cursor)
	}
}

func TestTreeSelectPage_DateIncludesYear(t *testing.T) {
	sessions := []SessionListing{
		{
			Harness:     string(defaults.HarnessClaudeCode),
			ProjectName: "proj",
			SessionID:   "s1",
			Date:        mustParseTime("2025-03-15T10:30:00Z"),
		},
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true
	p.providers[0].remotes[0].worktrees[0].expanded = true

	view := p.View(80, 24)
	if !strings.Contains(view, "2025") {
		t.Errorf("view should contain year '2025'; got:\n%s", view)
	}
	if !strings.Contains(view, "Mar 15, 2025") {
		t.Errorf("view should contain 'Mar 15, 2025'; got:\n%s", view)
	}
}

func TestTreeSelectPage_StatusBarShowsArrowKeys(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)
	view := p.View(80, 24)
	// Status bar shows arrow-key equivalents, not vim keys.
	for _, want := range []string{"expand/collapse", "toggle", "confirm"} {
		if !strings.Contains(view, want) {
			t.Errorf("status bar should contain %q; got:\n%s", want, view)
		}
	}
	// Vim-only labels should not appear in the status bar view.
	for _, unwanted := range []string{"prev section", "next section", "search"} {
		if strings.Contains(view, unwanted) {
			t.Errorf("status bar should NOT contain power-user key %q", unwanted)
		}
	}
}

// ---------------------------------------------------------------------------
// Wizard — project selection precedes the provider filter
// ---------------------------------------------------------------------------

func TestWizard_ScopePageCombinesProjectAndHarnessAxes(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 10})
	m := NewWizard(WithProviderInventory(inventory))

	if len(m.pages) != 11 {
		t.Fatalf("expected 11 pages, got %d", len(m.pages))
	}
	if _, ok := m.pages[pageProjectSelect].(*ProjectSelectPage); !ok {
		t.Fatalf("page %d should be *ProjectSelectPage, got %T", pageProjectSelect, m.pages[pageProjectSelect])
	}

	if _, ok := m.pages[pageSessionSelect].(*ProjectScopePage); !ok {
		t.Fatalf("page %d should be *ProjectScopePage, got %T", pageSessionSelect, m.pages[pageSessionSelect])
	}
	for _, page := range m.pages {
		if page.Title() == "Select Providers" {
			t.Fatal("standalone provider page remains mounted")
		}
	}
}

// ---------------------------------------------------------------------------
// buildFilteredSelectionPage
// ---------------------------------------------------------------------------

func TestBuildFilteredSelectionPage_ImportAllPreCheckedAndCollapsed(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s2"},
	}
	selections := []ProviderSelection{
		{Harness: string(defaults.HarnessClaudeCode), ImportAll: true},
	}

	page := buildFilteredSelectionPage(sessions, selections, nil)

	// All sessions should be pre-checked.
	for pi := range page.providers {
		for ri := range page.providers[pi].remotes {
			for wi := range page.providers[pi].remotes[ri].worktrees {
				for si, checked := range page.sessionSel[pi][ri][wi] {
					if !checked {
						t.Errorf("session [%d][%d][%d][%d] should be pre-checked for import-all provider", pi, ri, wi, si)
					}
				}
			}
		}
	}
	// Provider should be collapsed.
	if page.providers[0].expanded {
		t.Error("import-all provider should be collapsed")
	}
}

func TestBuildFilteredSelectionPage_SelectSessionsExpandedNotPreChecked(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "proj-b", SessionID: "s3"},
	}
	selections := []ProviderSelection{
		{Harness: string(defaults.HarnessOpenCode), ImportAll: false},
	}

	page := buildFilteredSelectionPage(sessions, selections, nil)

	// Sessions should NOT be pre-checked.
	for pi := range page.providers {
		for ri := range page.providers[pi].remotes {
			for wi := range page.providers[pi].remotes[ri].worktrees {
				for si, checked := range page.sessionSel[pi][ri][wi] {
					if checked {
						t.Errorf("session [%d][%d][%d][%d] should not be pre-checked for select-sessions provider", pi, ri, wi, si)
					}
				}
			}
		}
	}
	// Provider should be expanded.
	if !page.providers[0].expanded {
		t.Error("select-sessions provider should be expanded")
	}
	// Remotes should be collapsed (user expands manually).
	for _, rem := range page.providers[0].remotes {
		if rem.expanded {
			t.Errorf("remote %q should be collapsed by default", rem.name)
		}
	}
}

func TestBuildFilteredSelectionPage_FiltersToCheckedProviders(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "proj-b", SessionID: "s2"},
		{Harness: string(defaults.HarnessGeminiCLI), ProjectName: "proj-c", SessionID: "s3"},
	}
	selections := []ProviderSelection{
		{Harness: string(defaults.HarnessClaudeCode), ImportAll: true},
		// opencode and gemini are NOT selected.
	}

	page := buildFilteredSelectionPage(sessions, selections, nil)

	if len(page.providers) != 1 {
		t.Fatalf("expected 1 provider after filtering, got %d", len(page.providers))
	}
	if page.providers[0].name != string(defaults.HarnessClaudeCode) {
		t.Errorf("filtered provider = %q, want %q", page.providers[0].name, string(defaults.HarnessClaudeCode))
	}
}

// ---------------------------------------------------------------------------
// Wizard — shouldSkip for page 3
// ---------------------------------------------------------------------------

func TestWizard_ShouldSkipPage3_AllImportAll(t *testing.T) {
	m := NewWizard()
	m.answers.WantImport = true
	m.answers.ProviderSelections = []ProviderSelection{
		{Harness: string(defaults.HarnessClaudeCode), ImportAll: true},
		{Harness: string(defaults.HarnessOpenCode), ImportAll: true},
	}

	if m.shouldSkip(pageSessionSelect) {
		t.Error("project-rooted narrowing remains available after project selection")
	}
}

func TestWizard_ShouldNotSkipPage3_MixedImportModes(t *testing.T) {
	m := NewWizard()
	m.answers.WantImport = true
	m.answers.ProviderSelections = []ProviderSelection{
		{Harness: string(defaults.HarnessClaudeCode), ImportAll: true},
		{Harness: string(defaults.HarnessOpenCode), ImportAll: false},
	}

	if m.shouldSkip(3) {
		t.Error("page 3 should NOT be skipped when at least one provider wants session selection")
	}
}

func TestWizard_ShouldSkipRetentionPage(t *testing.T) {
	t.Run("zero-claude-count", func(t *testing.T) {
		m := NewWizard()
		m.providerInventory[defaults.HarnessClaudeCode] = ProviderDiscovery{SessionCount: 0, Enabled: true}
		if !m.shouldSkip(pageRetention) {
			t.Error("retention page should be skipped when no Claude Code transcripts discovered")
		}
	})

	t.Run("nonzero-claude-count", func(t *testing.T) {
		m := NewWizard()
		m.providerInventory[defaults.HarnessClaudeCode] = ProviderDiscovery{SessionCount: 5, Enabled: true}
		if m.shouldSkip(pageRetention) {
			t.Error("retention page should not be skipped when Claude Code transcripts are discovered")
		}
	})
}

// ---------------------------------------------------------------------------
// TreeSelectPage — Shift+J / Shift+K page jumping
// ---------------------------------------------------------------------------

func TestTreeSelectPage_ShiftJ_PageDown(t *testing.T) {
	// Create enough sessions to test page jumping.
	var sessions []SessionListing
	for i := 0; i < 20; i++ {
		sessions = append(sessions, SessionListing{
			Harness:     string(defaults.HarnessClaudeCode),
			ProjectName: "proj",
			SessionID:   fmt.Sprintf("s%d", i),
		})
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true
	p.providers[0].remotes[0].worktrees[0].expanded = true
	p.viewHeight = 5

	// Cursor starts at 0. Shift+J should jump by viewHeight (5).
	p.Update(tea.KeyPressMsg{Code: 'J', Text: "J"})
	if p.cursor != 5 {
		t.Errorf("after Shift+J: cursor = %d, want 5", p.cursor)
	}

	// Another Shift+J: 5 + 5 = 10.
	p.Update(tea.KeyPressMsg{Code: 'J', Text: "J"})
	if p.cursor != 10 {
		t.Errorf("after 2x Shift+J: cursor = %d, want 10", p.cursor)
	}
}

func TestTreeSelectPage_ShiftK_PageUp(t *testing.T) {
	var sessions []SessionListing
	for i := 0; i < 20; i++ {
		sessions = append(sessions, SessionListing{
			Harness:     string(defaults.HarnessClaudeCode),
			ProjectName: "proj",
			SessionID:   fmt.Sprintf("s%d", i),
		})
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true
	p.providers[0].remotes[0].worktrees[0].expanded = true
	p.viewHeight = 5
	p.cursor = 15
	p.clamp(len(p.flatItems()))

	// Shift+K should jump up by viewHeight (5).
	p.Update(tea.KeyPressMsg{Code: 'K', Text: "K"})
	if p.cursor != 10 {
		t.Errorf("after Shift+K: cursor = %d, want 10", p.cursor)
	}

	// Shift+K again.
	p.Update(tea.KeyPressMsg{Code: 'K', Text: "K"})
	if p.cursor != 5 {
		t.Errorf("after 2x Shift+K: cursor = %d, want 5", p.cursor)
	}

	// Shift+K from 5 should clamp to 0.
	p.Update(tea.KeyPressMsg{Code: 'K', Text: "K"})
	if p.cursor != 0 {
		t.Errorf("after 3x Shift+K: cursor = %d, want 0 (clamped)", p.cursor)
	}
}

func TestTreeSelectPage_ShiftJ_ClampsAtEnd(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s2"},
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true
	p.providers[0].remotes[0].worktrees[0].expanded = true
	p.viewHeight = 10

	// Items: provider(0), remote(1), worktree(2), s1(3), s2(4) = 5 items.
	// Shift+J from 0 should clamp to 4 (last item).
	p.Update(tea.KeyPressMsg{Code: 'J', Text: "J"})
	if p.cursor != 4 {
		t.Errorf("after Shift+J past end: cursor = %d, want 4 (clamped)", p.cursor)
	}
}

// ---------------------------------------------------------------------------
// TreeSelectPage — search / filter
// ---------------------------------------------------------------------------

func TestTreeSelectPage_SearchMode_FKeyActivates(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	p.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	if !p.filter.Active {
		t.Error("pressing 'f' should activate search mode")
	}
}

func TestTreeSelectPage_SearchMode_TypingFiltersItems(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "alpha", SessionID: "s1"},
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "beta", SessionID: "s2"},
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	p.providers[1].expanded = true

	// Enter search mode.
	p.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	// Type "alpha" to filter.
	for _, r := range "alpha" {
		p.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	if p.filter.Text != "alpha" {
		t.Errorf("filterText = %q, want %q", p.filter.Text, "alpha")
	}

	// flatItems should only show Claude provider (matching via project "alpha").
	items := p.flatItems()
	for _, item := range items {
		if item.providerIdx == 1 {
			t.Error("OpenCode provider should be filtered out when searching for 'alpha'")
		}
	}
}

func TestTreeSelectPage_SearchMode_EscapeClearsFilter(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "alpha", SessionID: "s1"},
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "beta", SessionID: "s2"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Enter search, type, then Escape.
	p.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	p.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	p.Update(tea.KeyPressMsg{Code: tea.KeyEsc})

	if p.filter.Active {
		t.Error("Escape should exit search mode")
	}
	if p.filter.Text != "" {
		t.Errorf("Escape should clear filterText, got %q", p.filter.Text)
	}

	// All items should be visible again (2 providers).
	items := p.flatItems()
	if len(items) != 2 { // 2 collapsed providers
		t.Errorf("after Escape: expected 2 items (2 providers), got %d", len(items))
	}
}

func TestTreeSelectPage_SearchMode_EnterKeepsFilter(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "alpha", SessionID: "s1"},
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "beta", SessionID: "s2"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Enter search, type, then Enter.
	p.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	p.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if p.filter.Active {
		t.Error("Enter should exit search mode")
	}
	if p.filter.Text != "a" {
		t.Errorf("Enter should keep filterText = %q, got %q", "a", p.filter.Text)
	}
}

func TestTreeSelectPage_SearchMode_BlocksNormalKeys(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Enter search mode.
	p.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})

	// Press space — in normal mode this toggles selection; in search mode it should be text input.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if p.filter.Text != " " {
		t.Errorf("space in search mode should be text input, filterText = %q", p.filter.Text)
	}

	// Selection should NOT have been toggled.
	if p.sessionSel[0][0][0][0] {
		t.Error("space in search mode should not toggle session selection")
	}
}

func TestTreeSelectPage_FilterActive_EscapeClearsFromNormalMode(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "alpha", SessionID: "s1"},
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "beta", SessionID: "s2"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Search, type, Enter to keep filter, then Escape in normal mode.
	p.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	p.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	// Filter is active, not in search mode.
	if p.filter.Text != "b" {
		t.Fatalf("expected filterText = %q, got %q", "b", p.filter.Text)
	}

	// Escape should clear filter in normal mode too.
	p.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if p.filter.Text != "" {
		t.Errorf("Escape in normal mode should clear active filter, got %q", p.filter.Text)
	}
}

func TestTreeSelectPage_OverlayShowsPowerUserKeys(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)
	p.showingHelp = true
	view := p.View(80, 24)
	// The help overlay should show power-user keys under the Actions category.
	for _, want := range []string{"j/k", "h/l", "J/K", "H/L", "Actions"} {
		if !strings.Contains(view, want) {
			t.Errorf("help overlay should contain %q; got:\n%s", want, view)
		}
	}
}

func TestTreeSelectPage_SearchMode_NoMatches(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "alpha", SessionID: "s1"},
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "beta", SessionID: "s2"},
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	p.providers[1].expanded = true

	// Enter search mode and type a string that matches nothing.
	p.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	for _, r := range "zzzzz" {
		p.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	items := p.flatItems()
	if len(items) != 0 { // no matches
		t.Errorf("expected 0 items for non-matching search, got %d", len(items))
	}
	if p.cursor != 0 {
		t.Errorf("cursor should be 0 with no matches, got %d", p.cursor)
	}

	// Press down, up, space — should not panic.
	p.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}) // add to filter (still in search mode)
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})   // exit search mode, keep filter

	// Now in normal mode with zero visible items — navigation must not panic.
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
}

// ---------------------------------------------------------------------------
// Wizard — search mode global key suppression
// ---------------------------------------------------------------------------

func TestWizard_SearchMode_GlobalKeysPassedToPage(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-b", SessionID: "s2"},
	}
	w := NewWizard(WithSessions(sessions))

	// Navigate directly to the tree select page (index 3).
	w.current = 3
	// Rebuild the tree page so it has our sessions.
	w.pages[3] = NewTreeSelectPage("title", sessions)

	tp := w.pages[3].(*TreeSelectPage)

	// Enter search mode by pressing "f".
	m, _ := w.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	w = m.(WizardModel)
	tp = w.pages[3].(*TreeSelectPage)

	if !tp.IsSearching() {
		t.Fatal("expected tree page to be in search mode after pressing f")
	}

	// Send "b", "r", "q" — they should be typed into the filter, not handled as global keys.
	for _, ch := range []rune{'b', 'r', 'q'} {
		m, _ = w.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
		w = m.(WizardModel)
	}

	tp = w.pages[3].(*TreeSelectPage)

	if w.quitting {
		t.Error("wizard should not have quit; q should be captured by search mode")
	}
	if w.current != 3 {
		t.Errorf("wizard should still be on page 3, got %d (b triggered back)", w.current)
	}
	if tp.filter.Text != "brq" {
		t.Errorf("expected filterText %q, got %q", "brq", tp.filter.Text)
	}
}

func TestWizard_SearchMode_CtrlCStillQuits(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
	}
	w := NewWizard(WithSessions(sessions))

	// Navigate directly to the tree select page.
	w.current = 3
	w.pages[3] = NewTreeSelectPage("title", sessions)

	// Enter search mode.
	m, _ := w.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	w = m.(WizardModel)

	tp := w.pages[3].(*TreeSelectPage)
	if !tp.IsSearching() {
		t.Fatal("expected search mode")
	}

	// ctrl+c must still quit even in search mode.
	m, cmd := w.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	w = m.(WizardModel)

	if !w.quitting {
		t.Error("ctrl+c should quit even during search mode")
	}
	if cmd == nil {
		t.Error("ctrl+c should return tea.Quit command")
	}
}

func TestWizard_ConfirmMode_BackKeyDoesNotNavigate(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	w := NewWizard(WithSessions(sessions))

	// Navigate directly to the tree select page (index 3).
	w.current = 3
	w.pages[3] = NewTreeSelectPage("title", sessions)

	tp := w.pages[3].(*TreeSelectPage)

	// Select a session and enter confirm mode.
	m, _ := w.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	w = m.(WizardModel)
	m, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	w = m.(WizardModel)

	tp = w.pages[3].(*TreeSelectPage)
	if !tp.IsConfirming() {
		t.Fatal("expected tree page to be in confirm mode")
	}

	// Press 'b' — should cancel confirm overlay, NOT navigate wizard back.
	m, _ = w.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	w = m.(WizardModel)

	if w.current != 3 {
		t.Errorf("wizard should still be on page 3, got %d (b incorrectly triggered back)", w.current)
	}

	tp = w.pages[3].(*TreeSelectPage)
	if tp.IsConfirming() {
		t.Error("confirm overlay should be dismissed after pressing b")
	}
}

// ---------------------------------------------------------------------------
// Session remote labels use the same internal/projectlabel.Label formatter as
// the Home/Map picker and CLI. These cases drive the production grouping path
// over the fixture corpus rather than reimplementing the formatter.
// ---------------------------------------------------------------------------

type sessionRemoteLabelInput struct {
	Remote      string `yaml:"remote"`
	ProjectName string `yaml:"projectName"`
}

type sessionRemoteLabelExpected struct {
	Label string `yaml:"label"`
}

type sessionRemoteLabelShape struct {
	transport    string
	gitSuffix    bool
	userInfo     bool
	pathSegments int
}

var allSessionRemoteLabelShapes = []sessionRemoteLabelShape{
	{transport: "https", gitSuffix: true, pathSegments: 2},
	{transport: "https", pathSegments: 2},
	{transport: "ssh", gitSuffix: true, pathSegments: 2},
	{transport: "ssh", pathSegments: 2},
	{transport: "unparseable"},
	{transport: "https", gitSuffix: true, pathSegments: 3},
	{transport: "https", userInfo: true, pathSegments: 2},
	{transport: "https", gitSuffix: true, userInfo: true, pathSegments: 2},
}

func classifySessionRemoteLabelShape(remote string) sessionRemoteLabelShape {
	shape := sessionRemoteLabelShape{gitSuffix: strings.HasSuffix(remote, ".git")}
	remote = strings.TrimSuffix(remote, ".git")
	segmentCount := func(path string) int {
		return len(strings.FieldsFunc(path, func(r rune) bool { return r == '/' }))
	}

	switch {
	case strings.HasPrefix(remote, "https://"):
		shape.transport = "https"
		parsed, err := url.Parse(remote)
		if err == nil {
			shape.userInfo = parsed.User != nil
			shape.pathSegments = segmentCount(parsed.Path)
		}
	case strings.HasPrefix(remote, "git@"):
		shape.transport = "ssh"
		if _, path, ok := strings.Cut(remote, ":"); ok {
			shape.pathSegments = segmentCount(path)
		}
	default:
		shape.transport = "unparseable"
	}
	return shape
}

//go:embed testdata/session_remote_label.yaml
var sessionRemoteLabelYAML []byte

const expectedSessionRemoteLabelCaseCount = 8

func TestSessionRemoteLabel_MatchesSharedFormatter(t *testing.T) {
	corpus, err := testcase.LoadCorpus[sessionRemoteLabelInput, sessionRemoteLabelExpected](sessionRemoteLabelYAML)
	if err != nil {
		t.Fatalf("load session remote label fixture: %v", err)
	}
	// The floor, and then the CEILING, using the corpus's own CheckMin rather than
	// a hand-rolled comparison - the idiom four other sites already use.
	//
	// RequireMin alone is a floor, and a floor is only as good as its distance
	// from the corpus. It equals the row count today, so a deletion trips it; but
	// the moment a case is added without this constant moving, the floor acquires
	// slack and every row inside that slack becomes deletable in silence
	// afterwards. Requiring CheckMin(N+1) to ERROR is the other direction: it
	// holds only while the corpus has exactly N rows.
	testassert.RequireMin(t, corpus, expectedSessionRemoteLabelCaseCount)
	if err := corpus.CheckMin(expectedSessionRemoteLabelCaseCount + 1); err == nil {
		t.Fatalf("the corpus holds MORE than expectedSessionRemoteLabelCaseCount (%d) cases, so the floor beneath it now "+
			"has slack and every row inside that slack can be deleted without anything going red. If a case was ADDED, "+
			"raise the constant in the same commit; if one was REMOVED, say why - each of these is a remote shape that "+
			"once rendered wrong, and two of them are the only proof that a credential in a git remote is not rendered "+
			"into a display label.", expectedSessionRemoteLabelCaseCount)
	}
	testassert.RequireValid(t, corpus)

	observedShapes := make([]sessionRemoteLabelShape, 0, len(corpus.Cases))
	for _, fixtureCase := range corpus.Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			session := SessionListing{
				Harness:     "claude-code",
				ProjectName: fixtureCase.Input.ProjectName,
				GitRemote:   fixtureCase.Input.Remote,
				SessionID:   "session-1",
			}
			page := NewTreeSelectPage("select", []SessionListing{session})
			if len(page.providers) != 1 || len(page.providers[0].remotes) != 1 {
				t.Fatalf("NewTreeSelectPage grouped %+v, want exactly one provider with one remote", page.providers)
			}
			got := page.providers[0].remotes[0].name
			if got != fixtureCase.Expected.Label {
				t.Fatalf("remote group label = %q, want %q", got, fixtureCase.Expected.Label)
			}
			observedShapes = append(observedShapes, classifySessionRemoteLabelShape(fixtureCase.Input.Remote))
		})
	}
	// This dimension is derived from inputs whose production labels passed above,
	// not from a fixture-declared classification that could merely claim coverage.
	testutil.RequireClosedSetCoverage(t, "session remote label", "remote shape", allSessionRemoteLabelShapes, observedShapes)
}

// ---------------------------------------------------------------------------
// TreeSelectPage — GitRemote grouping
// ---------------------------------------------------------------------------

func TestTreeSelectPage_GitRemoteGrouping(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "my-repo", GitRemote: "git@github.com:user/my-repo.git", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "my-repo", GitRemote: "git@github.com:user/my-repo.git", SessionID: "s2"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "other", GitRemote: "https://github.com/user/other.git", SessionID: "s3"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "no-remote", SessionID: "s4"},
	}
	p := NewTreeSelectPage("title", sessions)

	if len(p.providers) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(p.providers))
	}
	prov := p.providers[0]
	if len(prov.remotes) != 3 {
		t.Fatalf("expected 3 remotes, got %d", len(prov.remotes))
	}

	// Sessions with same git remote should be grouped under the display name
	// (internal/projectlabel.Label's "host:owner/repo" form, delegating to
	// the shared cross-repo schema.RemoteLabel rule — the same full-host
	// format the Home/Map picker and CLI use).
	if prov.remotes[0].name != "github.com:user/my-repo" {
		t.Errorf("remote[0] name = %q, want %q", prov.remotes[0].name, "github.com:user/my-repo")
	}
	// Each remote has a (default) worktree; check session count there.
	if len(prov.remotes[0].worktrees) != 1 {
		t.Fatalf("expected 1 worktree under remote[0], got %d", len(prov.remotes[0].worktrees))
	}
	if len(prov.remotes[0].worktrees[0].sessions) != 2 {
		t.Errorf("remote[0] worktree[0] sessions = %d, want 2", len(prov.remotes[0].worktrees[0].sessions))
	}

	if prov.remotes[1].name != "github.com:user/other" {
		t.Errorf("remote[1] name = %q, want %q", prov.remotes[1].name, "github.com:user/other")
	}

	// Session without git remote falls back to ProjectName.
	if prov.remotes[2].name != "no-remote" {
		t.Errorf("remote[2] name = %q, want %q", prov.remotes[2].name, "no-remote")
	}
}

func TestTreeSelectPage_SameRemoteDifferentBranches(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "repo", GitRemote: "git@github.com:user/repo.git", Branch: "main", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "repo", GitRemote: "git@github.com:user/repo.git", Branch: "main", SessionID: "s2"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "repo", GitRemote: "git@github.com:user/repo.git", Branch: "feat-x", SessionID: "s3"},
	}
	p := NewTreeSelectPage("title", sessions)

	if len(p.providers) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(p.providers))
	}
	prov := p.providers[0]
	if len(prov.remotes) != 1 {
		t.Fatalf("expected 1 remote (same git URL), got %d", len(prov.remotes))
	}
	remote := prov.remotes[0]
	if len(remote.worktrees) != 2 {
		t.Fatalf("expected 2 worktrees (main + feat-x), got %d", len(remote.worktrees))
	}

	// Worktrees should be separate: one for "main" (2 sessions), one for "feat-x" (1 session).
	wtNames := map[string]int{}
	for _, wt := range remote.worktrees {
		wtNames[wt.name] = len(wt.sessions)
	}
	if wtNames["main"] != 2 {
		t.Errorf("worktree 'main' should have 2 sessions, got %d", wtNames["main"])
	}
	if wtNames["feat-x"] != 1 {
		t.Errorf("worktree 'feat-x' should have 1 session, got %d", wtNames["feat-x"])
	}
}

func TestTreeSelectPage_NonGitSessions_DefaultWorktree(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "my-project", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "my-project", SessionID: "s2"},
	}
	p := NewTreeSelectPage("title", sessions)

	prov := p.providers[0]
	if len(prov.remotes) != 1 {
		t.Fatalf("expected 1 remote for non-git project, got %d", len(prov.remotes))
	}
	// Non-git sessions use ProjectName as the remote name.
	if prov.remotes[0].name != "my-project" {
		t.Errorf("remote name = %q, want %q", prov.remotes[0].name, "my-project")
	}
	// Should have a single "(default)" worktree.
	if len(prov.remotes[0].worktrees) != 1 {
		t.Fatalf("expected 1 worktree, got %d", len(prov.remotes[0].worktrees))
	}
	if prov.remotes[0].worktrees[0].name != "(default)" {
		t.Errorf("worktree name = %q, want %q", prov.remotes[0].worktrees[0].name, "(default)")
	}
	if len(prov.remotes[0].worktrees[0].sessions) != 2 {
		t.Errorf("expected 2 sessions under (default) worktree, got %d", len(prov.remotes[0].worktrees[0].sessions))
	}
}

func TestTreeSelectPage_SessionDisplayIncludesTitle(t *testing.T) {
	sessions := []SessionListing{
		{
			Harness:     string(defaults.HarnessClaudeCode),
			ProjectName: "proj",
			SessionID:   "s1",
			Title:       "Fix pipeline bug",
			Date:        mustParseTime("2025-03-05T10:00:00Z"),
			TurnCount:   12,
		},
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true
	p.providers[0].remotes[0].worktrees[0].expanded = true

	view := p.View(120, 24)
	if !strings.Contains(view, "Fix pipeline bug") {
		t.Errorf("view should contain session title 'Fix pipeline bug'; got:\n%s", view)
	}
	if !strings.Contains(view, "Mar 05, 2025") {
		t.Errorf("view should contain formatted date; got:\n%s", view)
	}
	if !strings.Contains(view, "12 turns") {
		t.Errorf("view should contain turn count; got:\n%s", view)
	}
}

// ---------------------------------------------------------------------------
// TreeSelectPage — Space toggles, Enter confirms (opens overlay)
// ---------------------------------------------------------------------------

func TestTreeSelectPage_SpaceTogglesItem(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Provider starts collapsed; Space on provider should toggle (select all).
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if p.providerState(0) != treeChecked {
		t.Error("Space on provider should toggle all sessions to checked")
	}

	// Expand and navigate to session.
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true
	p.providers[0].remotes[0].worktrees[0].expanded = true
	p.cursor = 3 // session row: provider(0)->remote(1)->worktree(2)->session(3)
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	// Session was checked (from provider toggle above), Space should uncheck it.
	if p.sessionSel[0][0][0][0] {
		t.Error("Space on session should toggle it off")
	}

	// Toggle back on.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if !p.sessionSel[0][0][0][0] {
		t.Error("Space on session should toggle it back on")
	}

	// Space should NOT set confirmed.
	if p.IsComplete() {
		t.Error("Space should toggle, not confirm the page")
	}
}

func TestTreeSelectPage_Enter_ShowsConfirmSummary(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s2"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Select all sessions via provider toggle.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})

	// Enter should enter confirm summary mode.
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !p.confirmingSelection {
		t.Error("Enter should set confirmingSelection to true")
	}
	if p.IsComplete() {
		t.Error("Enter should not immediately complete the page")
	}
}

func TestTreeSelectPage_CtrlS_DoesNotOpenConfirm(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Select a session.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})

	// Ctrl+S should not open confirm overlay.
	p.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if p.confirmingSelection {
		t.Error("Ctrl+S should not open confirm overlay")
	}
}

func TestTreeSelectPage_ConfirmSummary_EnterConfirms(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Select and enter confirm mode.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if !p.confirmingSelection {
		t.Fatal("expected confirmingSelection to be true")
	}

	// Enter in confirm mode should complete.
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !p.IsComplete() {
		t.Error("Enter in confirm summary mode should complete the page")
	}
}

func TestTreeSelectPage_ConfirmSummary_EscapeCancels(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Select and enter confirm mode.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	// Escape should cancel confirm mode.
	p.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if p.confirmingSelection {
		t.Error("Escape should clear confirmingSelection")
	}
	if p.IsComplete() {
		t.Error("Escape should not complete the page")
	}
}

func TestTreeSelectPage_ConfirmSummary_BackCancels(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Select and enter confirm mode.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if !p.confirmingSelection {
		t.Fatal("expected confirmingSelection to be true")
	}

	// 'b' should cancel confirm mode (return to tree), not navigate back.
	p.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	if p.confirmingSelection {
		t.Error("'b' should clear confirmingSelection")
	}
	if p.IsComplete() {
		t.Error("'b' should not complete the page")
	}
}

func TestTreeSelectPage_Enter_AllowsZeroSelection(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// No sessions selected — Enter should still open confirm overlay.
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !p.confirmingSelection {
		t.Error("Enter with no selected sessions should still enter confirm mode")
	}
}

func TestTreeSelectPage_ConfirmSummary_BlocksOtherKeys(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Select and enter confirm mode.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	// Navigation keys should be blocked.
	p.cursor = 0
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.cursor != 0 {
		t.Error("down key should be blocked in confirm summary mode")
	}
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	// Should still be in confirm mode.
	if !p.confirmingSelection {
		t.Error("space should not exit confirm mode")
	}
}

func TestTreeSelectPage_ConfirmSummary_ViewShowsSummary(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s2"},
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "proj-b", SessionID: "s3"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Select all via provider toggles.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "}) // select Claude
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "}) // select OpenCode
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	view := p.View(80, 24)
	if !strings.Contains(view, "3 sessions") {
		t.Errorf("confirm summary should show total session count; got:\n%s", view)
	}
	if !strings.Contains(view, "enter: confirm") {
		t.Errorf("confirm summary should show help bar; got:\n%s", view)
	}
	if !strings.Contains(view, "esc/b: cancel") {
		t.Errorf("confirm summary should show esc/b hint; got:\n%s", view)
	}
}

func TestTreeSelectPage_PageUpPageDown_PhysicalKeys(t *testing.T) {
	var sessions []SessionListing
	for i := range 20 {
		sessions = append(sessions, SessionListing{
			Harness:     string(defaults.HarnessClaudeCode),
			ProjectName: "proj",
			SessionID:   fmt.Sprintf("s%d", i),
		})
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true
	p.providers[0].remotes[0].worktrees[0].expanded = true
	p.viewHeight = 5

	// PgDown should jump by viewHeight.
	p.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if p.cursor != 5 {
		t.Errorf("after PgDown: cursor = %d, want 5", p.cursor)
	}

	// PgUp should jump back.
	p.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if p.cursor != 0 {
		t.Errorf("after PgUp: cursor = %d, want 0", p.cursor)
	}
}

func TestTreeSelectPage_Reset_ClearsConfirmingSelection(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Select, enter confirm mode.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !p.confirmingSelection {
		t.Fatal("expected confirmingSelection to be true before reset")
	}

	p.Reset()
	if p.confirmingSelection {
		t.Error("Reset should clear confirmingSelection")
	}
}

// ---------------------------------------------------------------------------
// SelectedSessions — returns only toggled sessions
// ---------------------------------------------------------------------------

func TestTreeSelectPage_SelectedSessions(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1", Title: "Session 1", Date: mustParseTime("2025-01-01T00:00:00Z")},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s2", Title: "Session 2", Date: mustParseTime("2025-01-02T00:00:00Z")},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-b", SessionID: "s3", Title: "Session 3", Date: mustParseTime("2025-01-03T00:00:00Z")},
	}
	p := NewTreeSelectPage("title", sessions)

	// Expand provider, both remotes, and their worktrees to make sessions visible.
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true
	p.providers[0].remotes[0].worktrees[0].expanded = true
	p.providers[0].remotes[1].expanded = true
	p.providers[0].remotes[1].worktrees[0].expanded = true

	// Flat layout: provider(0) -> remote-a(1) -> wt-a(2) -> s1(3) -> s2(4) -> remote-b(5) -> wt-b(6) -> s3(7)
	// Toggle s1 (cursor=3) via space.
	p.cursor = 3
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})

	// Toggle s3 (cursor=7) via space.
	p.cursor = 7
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})

	selected := p.SelectedSessions()
	if len(selected) != 2 {
		t.Fatalf("expected 2 selected sessions, got %d", len(selected))
	}
	ids := map[string]bool{}
	for _, s := range selected {
		ids[s.SessionID] = true
	}
	if !ids["s1"] || !ids["s3"] {
		t.Errorf("expected s1 and s3 selected, got %v", selected)
	}
	// s2 should NOT be selected.
	if ids["s2"] {
		t.Error("s2 should not be selected")
	}
}

func TestTreeSelectPage_SelectedSessions_EmptyWhenNoneToggled(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	selected := p.SelectedSessions()
	if len(selected) != 0 {
		t.Errorf("expected 0 selected sessions with no toggles, got %d", len(selected))
	}
}

// ---------------------------------------------------------------------------
// sessionMatchesFilter — GitRemote field matching
// ---------------------------------------------------------------------------

func TestSessionMatchesFilter_GitRemote(t *testing.T) {
	s := SessionListing{
		Harness:     string(defaults.HarnessClaudeCode),
		ProjectName: "my-repo",
		GitRemote:   "git@github.com:user/my-repo.git",
		SessionID:   "s1",
		Title:       "Session 1",
	}

	// Should match on the derived remote label's content. The label is now
	// internal/projectlabel.Label's "host:owner/repo" full-host form (the
	// same one the Home/Map picker and CLI use, delegating to the shared
	// cross-repo schema.RemoteLabel rule), so a filter query on "github"
	// still matches as a substring of the full "github.com" hostname.
	if !sessionMatchesFilter(s, "github") {
		t.Error("expected match on remote-label substring 'github'")
	}
	if !sessionMatchesFilter(s, "user/my-repo") {
		t.Error("expected match on GitRemote substring 'user/my-repo'")
	}
	// Should not match unrelated text.
	if sessionMatchesFilter(s, "gitlab") {
		t.Error("should not match 'gitlab' in git remote")
	}

	// Credential-embedded URL: PAT fragment must NOT match; host/path still must.
	credSession := SessionListing{
		Harness:   string(defaults.HarnessClaudeCode),
		GitRemote: "https://kjhy:glpat-SomeSecretToken@gitlab.com/org/repo",
		SessionID: "s2",
	}
	if sessionMatchesFilter(credSession, "glpat-SomeSecretToken") {
		t.Error("credential fragment must not match — filter uses sanitized remote")
	}
	if !sessionMatchesFilter(credSession, "gitlab") {
		t.Error("host should still match after credential stripping")
	}
	if !sessionMatchesFilter(credSession, "org/repo") {
		t.Error("path should still match after credential stripping")
	}
}

// ---------------------------------------------------------------------------
// Regression: shift+enter does NOT confirm (not a valid bubbletea key)
// ---------------------------------------------------------------------------

func TestTreeSelectPage_ShiftEnter_DoesNotConfirm(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Select a session so confirmation would be possible.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})

	// Send a key msg that looks like "shift+enter" — bubbletea has no such key type,
	// so it should be treated as literal rune input and NOT trigger confirm.
	p.Update(tea.KeyPressMsg{Text: "shift+enter"})
	if p.confirmingSelection {
		t.Error("shift+enter should NOT trigger confirm (not a valid terminal key)")
	}
}

// ---------------------------------------------------------------------------
// Worktree-level expand/collapse via keypress (IMPORTANT-2)
// ---------------------------------------------------------------------------

func TestTreeSelectPage_ExpandCollapseWorktree(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "repo", GitRemote: "git@github.com:user/repo.git", Branch: "main", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "repo", GitRemote: "git@github.com:user/repo.git", Branch: "main", SessionID: "s2"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Expand provider and remote to reach worktree row.
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true
	// Flat: provider(0), remote(1), worktree(2), [Confirm](3).
	// Worktree starts collapsed.
	if p.providers[0].remotes[0].worktrees[0].expanded {
		t.Fatal("worktree should start collapsed")
	}

	// Navigate to worktree row (cursor=2) and press l to expand.
	p.cursor = 2
	p.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	if !p.providers[0].remotes[0].worktrees[0].expanded {
		t.Error("l on worktree row should expand it")
	}

	// Press h to collapse.
	p.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
	if p.providers[0].remotes[0].worktrees[0].expanded {
		t.Error("h on worktree row should collapse it")
	}

	// Press right arrow to expand.
	p.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if !p.providers[0].remotes[0].worktrees[0].expanded {
		t.Error("right arrow on worktree row should expand it")
	}

	// Press left arrow to collapse.
	p.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if p.providers[0].remotes[0].worktrees[0].expanded {
		t.Error("left arrow on worktree row should collapse it")
	}
}

// ---------------------------------------------------------------------------
// Toggle cascading at remote and worktree levels (IMPORTANT-3)
// ---------------------------------------------------------------------------

func TestTreeSelectPage_ToggleCascadesAtRemoteAndWorktree(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "repo", GitRemote: "git@github.com:user/repo.git", Branch: "main", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "repo", GitRemote: "git@github.com:user/repo.git", Branch: "feat", SessionID: "s2"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "repo", GitRemote: "git@github.com:user/repo.git", Branch: "feat", SessionID: "s3"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Expand to see structure: provider -> remote -> worktree(main), worktree(feat).
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true

	// Toggle the remote row (cursor=1) — all sessions under all worktrees should be selected.
	p.cursor = 1
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})

	if p.remoteState(0, 0) != treeChecked {
		t.Error("toggling remote should check all sessions under all its worktrees")
	}
	// Verify individual sessions.
	if !p.sessionSel[0][0][0][0] {
		t.Error("session s1 (main) should be checked after remote toggle")
	}
	if !p.sessionSel[0][0][1][0] || !p.sessionSel[0][0][1][1] {
		t.Error("sessions s2, s3 (feat) should be checked after remote toggle")
	}

	// Toggle worktree "feat" (cursor should be at worktree row).
	// Flat: provider(0), remote(1), wt-main(2), wt-feat(3), [Confirm](4).
	p.cursor = 3
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})

	// feat worktree should now be unchecked.
	if p.worktreeState(0, 0, 1) != treeUnchecked {
		t.Error("toggling worktree feat should uncheck all its sessions")
	}
	// main worktree should still be checked.
	if p.worktreeState(0, 0, 0) != treeChecked {
		t.Error("main worktree should remain checked")
	}
	// Remote should show partial state.
	if p.remoteState(0, 0) != treePartial {
		t.Error("remote should show partial state when one worktree is checked and another is not")
	}
	// Provider should also show partial.
	if p.providerState(0) != treePartial {
		t.Error("provider should show partial state propagated from remote")
	}
}

// ---------------------------------------------------------------------------
// PrevSibling/NextSibling at worktree level (IMPORTANT-4)
// ---------------------------------------------------------------------------

func TestTreeSelectPage_SiblingNavigation_WorktreeLevel(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "repo", GitRemote: "git@github.com:user/repo.git", Branch: "main", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "repo", GitRemote: "git@github.com:user/repo.git", Branch: "feat-a", SessionID: "s2"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "repo", GitRemote: "git@github.com:user/repo.git", Branch: "feat-b", SessionID: "s3"},
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true
	// Flat: provider(0), remote(1), wt-main(2), wt-feat-a(3), wt-feat-b(4), [Confirm](5).

	// From wt-feat-b (cursor=4), [ should jump to wt-feat-a (cursor=3).
	p.cursor = 4
	p.Update(tea.KeyPressMsg{Code: '[', Text: "["})
	if p.cursor != 3 {
		t.Errorf("after [ from wt-feat-b: cursor = %d, want 3 (wt-feat-a)", p.cursor)
	}

	// From wt-feat-a (cursor=3), [ should jump to wt-main (cursor=2).
	p.Update(tea.KeyPressMsg{Code: '[', Text: "["})
	if p.cursor != 2 {
		t.Errorf("after [ from wt-feat-a: cursor = %d, want 2 (wt-main)", p.cursor)
	}

	// From wt-main (cursor=2), [ should be no-op (no previous worktree sibling).
	p.Update(tea.KeyPressMsg{Code: '[', Text: "["})
	if p.cursor != 2 {
		t.Errorf("after [ from wt-main: cursor = %d, want 2 (no-op)", p.cursor)
	}

	// From wt-main (cursor=2), ] should jump to wt-feat-a (cursor=3).
	p.Update(tea.KeyPressMsg{Code: ']', Text: "]"})
	if p.cursor != 3 {
		t.Errorf("after ] from wt-main: cursor = %d, want 3 (wt-feat-a)", p.cursor)
	}

	// From wt-feat-a (cursor=3), ] should jump to wt-feat-b (cursor=4).
	p.Update(tea.KeyPressMsg{Code: ']', Text: "]"})
	if p.cursor != 4 {
		t.Errorf("after ] from wt-feat-a: cursor = %d, want 4 (wt-feat-b)", p.cursor)
	}

	// From wt-feat-b (cursor=4), ] should be no-op.
	p.Update(tea.KeyPressMsg{Code: ']', Text: "]"})
	if p.cursor != 4 {
		t.Errorf("after ] from wt-feat-b: cursor = %d, want 4 (no-op)", p.cursor)
	}
}

// ---------------------------------------------------------------------------
// Search/filter by branch name (IMPORTANT-5)
// ---------------------------------------------------------------------------

func TestTreeSelectPage_SearchMode_FilterByBranchName(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "repo", GitRemote: "git@github.com:user/repo.git", Branch: "main", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "repo", GitRemote: "git@github.com:user/repo.git", Branch: "feat-x", SessionID: "s2"},
		{Harness: string(defaults.HarnessOpenCode), ProjectName: "other", Branch: "develop", SessionID: "s3"},
	}
	p := NewTreeSelectPage("title", sessions)
	p.providers[0].expanded = true
	p.providers[0].remotes[0].expanded = true
	if len(p.providers) > 1 {
		p.providers[1].expanded = true
	}

	// Enter search mode and type "feat-x".
	p.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	for _, r := range "feat-x" {
		p.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	if p.filter.Text != "feat-x" {
		t.Fatalf("filterText = %q, want %q", p.filter.Text, "feat-x")
	}

	// flatItems should contain only Claude provider (matching via branch "feat-x")
	// + confirm footer.
	items := p.flatItems()
	for _, item := range items {
		if item.level == treeLevelProvider && p.providers[item.providerIdx].name == string(defaults.HarnessOpenCode) {
			t.Error("OpenCode provider should be filtered out when searching for 'feat-x'")
		}
	}

	// Verify that the branch name "feat-x" is what caused the match.
	if !sessionMatchesFilter(sessions[1], "feat-x") {
		t.Error("session with Branch='feat-x' should match filter 'feat-x'")
	}
	if sessionMatchesFilter(sessions[0], "feat-x") {
		t.Error("session with Branch='main' should not match filter 'feat-x'")
	}
}

// ---------------------------------------------------------------------------
// Enter = confirm behaviour (opens overlay from any cursor position)
// ---------------------------------------------------------------------------

func TestTreeSelectPage_EnterOpensConfirmOverlay(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Select a session first.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})

	// Press Enter from any position — should open confirm overlay.
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !p.confirmingSelection {
		t.Error("Enter should open the confirm overlay")
	}
}

func TestTreeSelectPage_SpaceTogglesDoesNotConfirm(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Press Space — should toggle, not confirm.
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if p.confirmingSelection {
		t.Error("Space should toggle selection, not open confirm overlay")
	}
}

func TestTreeSelectPage_EnterAllowsZeroSelection(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Enter without selecting anything — should still open confirm overlay.
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !p.confirmingSelection {
		t.Error("Enter with no selections should open confirm overlay")
	}
}

func TestTreeSelectPage_NoConfirmFooterRow(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	view := p.View(80, 24)
	if strings.Contains(view, "[Confirm") {
		t.Errorf("view should NOT contain [Confirm] footer row; got:\n%s", view)
	}
}

// ---------------------------------------------------------------------------
// Help overlay tests — TreeSelectPage
// ---------------------------------------------------------------------------

func TestTreeSelectPage_QuestionMarkTogglesHelpOverlay(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	if p.showingHelp {
		t.Fatal("help should start hidden")
	}

	// Press ? to open help.
	p.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	if !p.showingHelp {
		t.Error("? should open help overlay")
	}

	// Press ? again to close.
	p.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	if p.showingHelp {
		t.Error("? should toggle help overlay off")
	}
}

func TestTreeSelectPage_EscapeClosesHelpOverlay(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	p.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	if !p.showingHelp {
		t.Fatal("? should open help overlay")
	}

	p.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if p.showingHelp {
		t.Error("Escape should close help overlay")
	}
}

func TestTreeSelectPage_NavigationBlockedWhileHelpShowing(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-a", SessionID: "s1"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj-b", SessionID: "s2"},
	}
	p := NewTreeSelectPage("title", sessions)

	// Open help.
	p.Update(tea.KeyPressMsg{Code: '?', Text: "?"})

	// Record cursor before navigation attempts.
	cursorBefore := p.cursor

	// Try pressing j (down), l (expand), space (toggle).
	p.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	p.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	p.Update(tea.KeyPressMsg{Code: ' ', Text: " "})

	if p.cursor != cursorBefore {
		t.Error("navigation keys should be blocked while help is showing")
	}
	if p.providers[0].expanded {
		t.Error("expand should be blocked while help is showing")
	}
}

func TestTreeSelectPage_HelpOverlayContent(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)
	p.showingHelp = true

	view := p.View(80, 24)

	// Check that the overlay contains key binding labels from the keymap.
	expectedLabels := []string{
		p.keymap.Up.Help().Key,
		p.keymap.Down.Help().Key,
		p.keymap.Expand.Help().Key,
		p.keymap.Collapse.Help().Key,
		p.keymap.Search.Help().Key,
		p.keymap.Help.Help().Key,
	}
	for _, label := range expectedLabels {
		if !strings.Contains(view, label) {
			t.Errorf("help overlay should contain binding key %q", label)
		}
	}

	// Check category headers.
	if !strings.Contains(view, "Navigation") {
		t.Error("help overlay should contain 'Navigation' category")
	}
	if !strings.Contains(view, "Actions") {
		t.Error("help overlay should contain 'Actions' category")
	}
	if strings.Contains(view, "Power user") {
		t.Error("help overlay should NOT contain separate 'Power user' category (merged into Actions)")
	}
	if !strings.Contains(view, "Press ? or Esc to close") {
		t.Error("help overlay should contain dismiss instruction")
	}
}

func TestTreeSelectPage_StatusBarShowsEssentialKeys(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	p := NewTreeSelectPage("title", sessions)

	view := p.View(80, 24)

	// The compact status bar should show essential keys only (no vim keys).
	helpBar := p.helpBarText()
	for _, want := range []string{"space", "enter", "?"} {
		if !strings.Contains(helpBar, want) {
			t.Errorf("status bar should contain %q", want)
		}
	}
	// Vim-specific keys should NOT appear in the compact status bar.
	for _, unwanted := range []string{"j/k", "h/l", "Shift+"} {
		if strings.Contains(helpBar, unwanted) {
			t.Errorf("status bar should not contain vim key %q", unwanted)
		}
	}
	// Verify the View includes the status bar content.
	if !strings.Contains(view, "toggle") {
		t.Error("view should include status bar with toggle action")
	}
}

// ---------------------------------------------------------------------------
// Help overlay tests — ProviderSelectPage
// ---------------------------------------------------------------------------

func TestProviderSelectPage_QuestionMarkTogglesHelpOverlay(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 5})
	p := NewProviderSelectPage("title", "desc", inventory)

	if p.showingHelp {
		t.Fatal("help should start hidden")
	}

	p.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	if !p.showingHelp {
		t.Error("? should open help overlay on ProviderSelectPage")
	}

	// Escape closes it.
	p.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if p.showingHelp {
		t.Error("Escape should close help overlay on ProviderSelectPage")
	}
}

func TestProviderSelectPage_HelpOverlayContent(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 5})
	p := NewProviderSelectPage("title", "desc", inventory)
	p.showingHelp = true

	view := p.View(80, 24)

	if !strings.Contains(view, "Navigation") {
		t.Error("help overlay should contain 'Navigation' category")
	}
	if !strings.Contains(view, "Actions") {
		t.Error("help overlay should contain 'Actions' category")
	}
	if !strings.Contains(view, p.keymap.Help.Help().Key) {
		t.Error("help overlay should contain ? key label")
	}
}

// ---------------------------------------------------------------------------
// Wizard-level help guard — b key should not navigate while help is showing
// ---------------------------------------------------------------------------

func TestWizard_BKeyBlockedWhileHelpShowing(t *testing.T) {
	inventory := enabledProviderInventory(map[defaults.Harness]int{defaults.HarnessClaudeCode: 5})
	w := NewWizard(WithProviderInventory(inventory))

	// Place the wizard at the project picker while it captures search input.
	w.current = pageProjectSelect
	projectPage := w.pages[pageProjectSelect].(*ProjectSelectPage)
	projectPage.searching = true

	// Press 'b' — should NOT navigate back.
	m, _ := w.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	w = m.(WizardModel)
	if w.current != pageProjectSelect {
		t.Errorf("b key should not navigate back while help overlay is showing; current=%d", w.current)
	}

	// Close help and verify 'b' works again.
	projectPage = w.pages[pageProjectSelect].(*ProjectSelectPage)
	projectPage.searching = false
	m, _ = w.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	w = m.(WizardModel)
	if w.current >= pageProjectSelect {
		t.Errorf("b key should navigate back when help overlay is closed; current=%d", w.current)
	}
}

func TestWizard_QKeyBlockedWhileTreeHelpShowing(t *testing.T) {
	sessions := []SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "proj", SessionID: "s1"},
	}
	w := NewWizard(WithSessions(sessions))

	// Place wizard at the tree select page (page 3).
	w.current = 3
	w.pages[3] = NewTreeSelectPage("title", sessions)
	treePage := w.pages[3].(*TreeSelectPage)
	treePage.showingHelp = true

	// Press 'q' — should NOT quit.
	result, cmd := w.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	wm := result.(WizardModel)
	if wm.quitting {
		t.Error("q key should not quit while help overlay is showing")
	}
	if cmd != nil {
		t.Error("no command should be generated when q is blocked by help overlay")
	}
}

// --- buildSelectionConfig tests ---

func TestBuildSelectionConfig_NoImport(t *testing.T) {
	answers := &WizardAnswers{WantImport: false}
	got := buildSelectionConfig(answers)
	if got == nil || got.Mode != config.SelectionModeSelected || len(got.Harnesses) != 0 {
		t.Errorf("buildSelectionConfig(WantImport=false) = %+v, want selected mode with an empty allowlist", got)
	}
}

func TestBuildSelectionConfig_AllImportAll(t *testing.T) {
	answers := &WizardAnswers{
		WantImport: true,
		ProviderSelections: []ProviderSelection{
			{Harness: string(defaults.HarnessClaudeCode), ImportAll: true},
			{Harness: string(defaults.HarnessOpenCode), ImportAll: true},
		},
	}
	got := buildSelectionConfig(answers)
	if got == nil {
		t.Fatal("buildSelectionConfig: got nil, want non-nil")
	}
	if got.Mode != "all" {
		t.Errorf("Mode = %q, want %q", got.Mode, "all")
	}
	if !got.AutoIngestNewBranches {
		t.Error("AutoIngestNewBranches = false, want true for all-import-all")
	}
}

func TestBuildSelectionConfig_MixedProviders(t *testing.T) {
	answers := &WizardAnswers{
		WantImport: true,
		ProviderSelections: []ProviderSelection{
			{Harness: string(defaults.HarnessClaudeCode), ImportAll: false},
			{Harness: string(defaults.HarnessOpenCode), ImportAll: true},
		},
		SelectedSessions: []SessionListing{
			{
				Harness:     string(defaults.HarnessClaudeCode),
				GitRemote:   "https://github.com/org/repo-a.git",
				Branch:      "main",
				SessionID:   "sess-1",
				ProjectName: "repo-a",
			},
			{
				Harness:     string(defaults.HarnessClaudeCode),
				GitRemote:   "https://github.com/org/repo-a.git",
				Branch:      "develop",
				SessionID:   "sess-2",
				ProjectName: "repo-a",
			},
			{
				Harness:     string(defaults.HarnessClaudeCode),
				GitRemote:   "https://github.com/org/repo-b.git",
				Branch:      "main",
				SessionID:   "sess-3",
				ProjectName: "repo-b",
			},
		},
		AutoIngestNewBranches: true,
	}

	got := buildSelectionConfig(answers)
	if got == nil {
		t.Fatal("buildSelectionConfig: got nil")
	}
	if got.Mode != "selected" {
		t.Errorf("Mode = %q, want %q", got.Mode, "selected")
	}
	if !got.AutoIngestNewBranches {
		t.Error("AutoIngestNewBranches = false, want true")
	}

	// Claude provider should have 2 projects.
	claudeCfg, ok := got.Harnesses[string(defaults.HarnessClaudeCode)]
	if !ok {
		t.Fatal("missing claude provider in selection")
	}
	if len(claudeCfg.Projects) != 2 {
		t.Fatalf("claude projects len = %d, want 2", len(claudeCfg.Projects))
	}
	// First project: repo-a with branches [main, develop].
	if claudeCfg.Projects[0].GitRemote != "https://github.com/org/repo-a.git" {
		t.Errorf("project[0].GitRemote = %q", claudeCfg.Projects[0].GitRemote)
	}
	if len(claudeCfg.Projects[0].Branches) != 2 {
		t.Errorf("project[0].Branches len = %d, want 2", len(claudeCfg.Projects[0].Branches))
	}
	// Second project: repo-b with branch [main].
	if claudeCfg.Projects[1].GitRemote != "https://github.com/org/repo-b.git" {
		t.Errorf("project[1].GitRemote = %q", claudeCfg.Projects[1].GitRemote)
	}

	// OpenCode provider should have empty projects (import all).
	ocCfg, ok := got.Harnesses[string(defaults.HarnessOpenCode)]
	if !ok {
		t.Fatal("missing opencode provider in selection")
	}
	if len(ocCfg.Projects) != 0 {
		t.Errorf("opencode projects len = %d, want 0 (import all)", len(ocCfg.Projects))
	}
}

func TestBuildSelectionConfig_SessionWithoutProject(t *testing.T) {
	answers := &WizardAnswers{
		WantImport: true,
		ProviderSelections: []ProviderSelection{
			{Harness: string(defaults.HarnessClaudeCode), ImportAll: false},
		},
		SelectedSessions: []SessionListing{
			{
				Harness:   string(defaults.HarnessClaudeCode),
				SessionID: "orphan-sess",
				// No GitRemote, no ProjectName.
			},
		},
	}

	got := buildSelectionConfig(answers)
	if got == nil {
		t.Fatal("buildSelectionConfig: got nil")
	}
	claudeCfg := got.Harnesses[string(defaults.HarnessClaudeCode)]
	if len(claudeCfg.Sessions) != 1 || claudeCfg.Sessions[0] != "orphan-sess" {
		t.Errorf("Sessions = %v, want [orphan-sess]", claudeCfg.Sessions)
	}
	if len(claudeCfg.Projects) != 0 {
		t.Errorf("Projects len = %d, want 0 (no project context)", len(claudeCfg.Projects))
	}
}

func TestBuildSelectionConfig_LocalProjectByName(t *testing.T) {
	answers := &WizardAnswers{
		WantImport: true,
		ProviderSelections: []ProviderSelection{
			{Harness: string(defaults.HarnessClaudeCode), ImportAll: false},
		},
		SelectedSessions: []SessionListing{
			{
				Harness:     string(defaults.HarnessClaudeCode),
				ProjectName: "my-local-proj",
				Branch:      "main",
				SessionID:   "sess-local",
				// No GitRemote — local project.
			},
		},
	}

	got := buildSelectionConfig(answers)
	if got == nil {
		t.Fatal("buildSelectionConfig: got nil")
	}
	claudeCfg := got.Harnesses[string(defaults.HarnessClaudeCode)]
	if len(claudeCfg.Projects) != 1 {
		t.Fatalf("Projects len = %d, want 1", len(claudeCfg.Projects))
	}
	if claudeCfg.Projects[0].Name != "my-local-proj" {
		t.Errorf("Name = %q, want %q", claudeCfg.Projects[0].Name, "my-local-proj")
	}
	if claudeCfg.Projects[0].GitRemote != "" {
		t.Errorf("GitRemote = %q, want empty for local project", claudeCfg.Projects[0].GitRemote)
	}
}
