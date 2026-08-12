package ftue

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"gopkg.in/yaml.v3"
)

type synchronizedProgress struct {
	mu   sync.RWMutex
	data map[string]StageProgress
}

func (p *synchronizedProgress) Snapshot() map[string]StageProgress {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make(map[string]StageProgress, len(p.data))
	for stage, progress := range p.data {
		result[stage] = progress
	}
	return result
}

func (p *synchronizedProgress) set(stage string, progress StageProgress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.data[stage] = progress
}

type restartJourneyRunner struct{ calls int }

func (r *restartJourneyRunner) Run(context.Context, JourneyRequest) (JourneyResult, error) {
	r.calls++
	return JourneyResult{}, nil
}

type blockedJourneyRunner struct{ started chan struct{} }

func (r blockedJourneyRunner) Run(ctx context.Context, request JourneyRequest) (JourneyResult, error) {
	close(r.started)
	<-ctx.Done()
	return JourneyResult{Effects: mergeJourneyEffects(request.PriorEffects, []PersistedEffect{{Stage: StageIngest, Status: StatusCancelled, Detail: ctx.Err().Error()}})}, nil
}

//go:embed testdata/persistence.yaml
var persistenceFixtureYAML []byte

//go:embed testdata/execution_cancel.yaml
var executionCancelYAML []byte

type executionCancelDocument struct {
	DeclaredRows int                      `yaml:"declaredRows"`
	RequiredArms []string                 `yaml:"requiredArms"`
	Cases        []executionCancelFixture `yaml:"cases"`
}

type executionCancelFixture struct{ Name, Arm, Key string }

func loadExecutionCancelFixtures(t *testing.T) []executionCancelFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(executionCancelYAML))
	decoder.KnownFields(true)
	var doc executionCancelDocument
	if err := decoder.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("execution cancel fixture must contain exactly one document")
	}
	if doc.DeclaredRows != len(doc.Cases) || doc.DeclaredRows < 4 {
		t.Fatal("execution cancel fixture row count is not guarded")
	}
	arms := map[string]bool{}
	for _, row := range doc.Cases {
		if row.Name == "" || row.Arm == "" || row.Key == "" {
			t.Fatal("execution cancel fixture contains a vacuous row")
		}
		arms[row.Arm] = true
	}
	for _, arm := range doc.RequiredArms {
		if !arms[arm] {
			t.Fatalf("execution cancel fixture misses arm %q", arm)
		}
	}
	return doc.Cases
}

type persistenceFixtures struct {
	DeclaredRows          int                      `yaml:"declared_rows"`
	Cases                 []persistenceFixture     `yaml:"cases"`
	DeclaredSelectionRows int                      `yaml:"declared_selection_rows"`
	SelectionCases        []wizardSelectionFixture `yaml:"selection_cases"`
}

// wizardPrecheck is the closed set of states a session's checkbox can be in
// when the kickstart tree is rebuilt over a saved selection. It has THREE
// values because the box carries two independent facts: whether the session
// stays in the saved selection when the wizard is confirmed, and whether it
// will actually be ingested. A conflicted session is the case where those two
// answers differ, so a two-state box would have to lie about one of them.
type wizardPrecheck string

const (
	// wizardPrecheckUnticked is cleared: confirming the wizard drops this
	// session from the saved selection.
	wizardPrecheckUnticked wizardPrecheck = "unticked"
	// wizardPrecheckTicked is a plain tick: kept, and it will be ingested.
	wizardPrecheckTicked wizardPrecheck = "ticked"
	// wizardPrecheckFlagged is kept, but the saved selection cannot decide it,
	// so ingest withholds it. Ticked so that re-confirming the wizard does not
	// silently delete it; marked so the tick is not a promise it will run.
	wizardPrecheckFlagged wizardPrecheck = "flagged"
)

var allWizardPrechecks = []wizardPrecheck{wizardPrecheckUnticked, wizardPrecheckTicked, wizardPrecheckFlagged}

// selected is the write-back half: whether SelectedSessions returns it, i.e.
// whether confirming the wizard keeps it in the configuration.
func (p wizardPrecheck) selected(t *testing.T, caseName string) bool {
	t.Helper()
	switch p {
	case wizardPrecheckTicked, wizardPrecheckFlagged:
		return true
	case wizardPrecheckUnticked:
		return false
	default:
		t.Fatalf("wizard selection fixture %q declares unknown expected_precheck %q; use one of %v", caseName, string(p), allWizardPrechecks)
		return false
	}
}

// glyph is the display half: the box the renderer must draw. It is asserted
// SEPARATELY from selected, because the two regressions this corpus exists to
// catch are independent — dropping the tick is a destructive write-back, and
// dropping the marker is a display that lies.
func (p wizardPrecheck) glyph(t *testing.T, caseName string) string {
	t.Helper()
	switch p {
	case wizardPrecheckUnticked:
		return "[ ]"
	case wizardPrecheckTicked:
		return "[✓]"
	case wizardPrecheckFlagged:
		return "[!]"
	default:
		t.Fatalf("wizard selection fixture %q declares unknown expected_precheck %q; use one of %v", caseName, string(p), allWizardPrechecks)
		return ""
	}
}

// wizardConfiguredEntry is one project entry of the saved selection. Cases
// carry a LIST because two entries that identify the same session and disagree
// about its branch are the only way to reach the matcher's withheld state, and
// that state is where the wizard makes its decision.
type wizardConfiguredEntry struct {
	GitRemote string   `yaml:"git_remote"`
	Name      string   `yaml:"name"`
	Branches  []string `yaml:"branches"`
}

type wizardSelectionFixture struct {
	Name        string `yaml:"name"`
	GitRemote   string `yaml:"git_remote"`
	ProjectName string `yaml:"project_name"`
	Branch      string `yaml:"branch"`
	// ConfiguredEntries are the project entries the saved selection carries.
	ConfiguredEntries []wizardConfiguredEntry `yaml:"configured_entries"`
	// UnrestrictedHarness declares that the case deliberately configures the
	// harness with no project entry, so the blank-field guard does not read the
	// empty entry list as an unfinished case.
	UnrestrictedHarness   bool                      `yaml:"unrestricted_harness"`
	AutoIngestNewBranches bool                      `yaml:"auto_ingest_new_branches"`
	SessionID             string                    `yaml:"session_id"`
	ExpectedMatch         testutil.SelectionOutcome `yaml:"expected_match"`
	ExpectedPrecheck      wizardPrecheck            `yaml:"expected_precheck"`
	// ExpectedPrecheckToggled is the box after ONE space keypress on the
	// session row. It is declared per row, not derived, because the conflicted
	// row's answer is a product invariant: an explicitly cleared box
	// wins over the marker. Deriving it would let the rule be restated by
	// whatever the code does.
	ExpectedPrecheckToggled wizardPrecheck `yaml:"expected_precheck_toggled"`
}

// selection builds the saved selection this case describes.
func (f wizardSelectionFixture) selection() *config.SelectionConfig {
	projects := make([]config.ProjectSelection, 0, len(f.ConfiguredEntries))
	for _, entry := range f.ConfiguredEntries {
		projects = append(projects, config.ProjectSelection{GitRemote: entry.GitRemote, Name: entry.Name, Branches: entry.Branches})
	}
	return &config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: f.AutoIngestNewBranches,
		Harnesses: map[string]config.SelectionHarnessConfig{
			defaults.HarnessClaudeCode.String(): {Projects: projects},
		},
	}
}

// wizardEntryShape is the closed set of saved-selection shapes the corpus must
// exercise. It is COMPUTED from a case's configured entries rather than
// declared by it, so it cannot be moved onto an unrelated row to keep a
// coverage guard satisfied, and it is computed structurally (entry counts and
// which fields are populated) rather than by re-deriving the matcher's
// normalization, which is the duplication this slice exists to remove.
type wizardEntryShape string

const (
	// wizardShapeUnrestricted is a harness with no project entries at all.
	wizardShapeUnrestricted wizardEntryShape = "unrestricted"
	// wizardShapeConflicting is two or more entries that can disagree.
	wizardShapeConflicting wizardEntryShape = "conflicting"
	// wizardShapeBranchPinned is a single entry restricted to named branches.
	wizardShapeBranchPinned wizardEntryShape = "branch-pinned"
	// wizardShapeNamed is a single entry that carries a project name.
	wizardShapeNamed wizardEntryShape = "named"
	// wizardShapeRemoteOnly is a single entry identified by git remote alone.
	wizardShapeRemoteOnly wizardEntryShape = "remote-only"
)

var allWizardEntryShapes = []wizardEntryShape{
	wizardShapeUnrestricted, wizardShapeConflicting, wizardShapeBranchPinned,
	wizardShapeNamed, wizardShapeRemoteOnly,
}

func (f wizardSelectionFixture) entryShape() wizardEntryShape {
	switch {
	case f.UnrestrictedHarness:
		return wizardShapeUnrestricted
	case len(f.ConfiguredEntries) > 1:
		return wizardShapeConflicting
	case len(f.ConfiguredEntries) == 1 && len(f.ConfiguredEntries[0].Branches) > 0:
		return wizardShapeBranchPinned
	case len(f.ConfiguredEntries) == 1 && f.ConfiguredEntries[0].Name != "":
		return wizardShapeNamed
	default:
		return wizardShapeRemoteOnly
	}
}

// wizardDecision is the pair a selection case really asserts: what the canonical
// matcher answers, and what the wizard then shows. The wizard's answer is NOT a
// function of the matcher's, so the legal pairs are the closed set — and
// enumerating them is what makes each divergence a row that cannot be deleted
// without the corpus noticing.
type wizardDecision struct {
	match    testutil.SelectionOutcome
	precheck wizardPrecheck
}

var allWizardDecisions = []wizardDecision{
	{testutil.SelectionSelected, wizardPrecheckTicked},
	// The matcher selects every session of an unrestricted harness; the wizard
	// still ticks none of them.
	{testutil.SelectionSelected, wizardPrecheckUnticked},
	{testutil.SelectionRejected, wizardPrecheckUnticked},
	// A conflict is withheld by ingest, kept by the wizard, and marked.
	{testutil.SelectionWithheld, wizardPrecheckFlagged},
}

// autoBranchAxisKey identifies everything about a case EXCEPT the auto-ingest
// setting and the expectations, so the loader can require that the corpus holds
// a pair of rows differing only in that setting. Nothing in the fixture labels
// the pair; it is recognised by shape, so no label can be moved onto an
// unrelated row to keep the guard satisfied.
func (f wizardSelectionFixture) autoBranchAxisKey() string {
	entries := make([]string, len(f.ConfiguredEntries))
	for i, entry := range f.ConfiguredEntries {
		entries[i] = fmt.Sprintf("%s|%s|%v", entry.GitRemote, entry.Name, entry.Branches)
	}
	return fmt.Sprintf("%s|%s|%s|%s|%v|%s", f.GitRemote, f.ProjectName, f.Branch, f.SessionID, f.UnrestrictedHarness, strings.Join(entries, ","))
}

type persistenceFixture struct {
	Name           string `yaml:"name"`
	UserEmail      string `yaml:"user_email"`
	OutputPath     string `yaml:"output_path"`
	VillageURL     string `yaml:"village_url"`
	RedactionLevel string `yaml:"redaction_level"`
	SourcePath     string `yaml:"source_path"`
	// SessionID seeds the caller's saved selection so the mutation guard has a
	// map and a slice to watch, not only scalars.
	SessionID string `yaml:"session_id"`
}

func loadPersistenceFixtures(t *testing.T) persistenceFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(persistenceFixtureYAML))
	decoder.KnownFields(true)
	var fixtures persistenceFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode persistence fixture with strict fields: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("persistence fixture must contain exactly one YAML document: %v", err)
	}
	// Floor EQUALS the row count, the same as every sibling loader. Deleting a
	// row and decrementing the declaration still trips the floor, so the corpus
	// only ever ratchets up. The derived coverage guards below are the second
	// layer: they catch a row being swapped for a junk one at the same count,
	// which a floor cannot.
	if fixtures.DeclaredRows != len(fixtures.Cases) || fixtures.DeclaredRows < 2 {
		t.Fatalf("persistence fixture row guard failed: declared=%d actual=%d minimum=2", fixtures.DeclaredRows, len(fixtures.Cases))
	}
	if fixtures.DeclaredSelectionRows != len(fixtures.SelectionCases) || fixtures.DeclaredSelectionRows < 6 {
		t.Fatalf("wizard selection fixture row guard failed: declared=%d actual=%d minimum=6", fixtures.DeclaredSelectionRows, len(fixtures.SelectionCases))
	}
	// The preservation corpus must exercise every redaction level that is not
	// the BaseConfig default. A row carrying the default cannot distinguish
	// "SaveTo preserved the loaded level" from "SaveTo reset it", so the set of
	// levels worth fixturing is derived, not chosen.
	var preservableLevels, observedLevels []redact.RedactionLevel
	for _, level := range redact.AllRedactionLevels() {
		if level != config.BaseConfig().Redaction.Level {
			preservableLevels = append(preservableLevels, level)
		}
	}
	for _, fixture := range fixtures.Cases {
		level := redact.RedactionLevel(fixture.RedactionLevel)
		if !level.IsValid() {
			t.Fatalf("persistence fixture case %q declares redaction_level %q, which is not a redaction level; use one of %v", fixture.Name, fixture.RedactionLevel, redact.AllRedactionLevels())
		}
		if level == config.BaseConfig().Redaction.Level {
			t.Fatalf("persistence fixture case %q sets redaction_level to the BaseConfig default %q, so that field asserts nothing: the case passes whether or not SaveTo preserved it; use one of %v", fixture.Name, level, preservableLevels)
		}
		observedLevels = append(observedLevels, level)
	}
	testutil.RequireClosedSetCoverage(t, "persistence", "redaction_level", preservableLevels, observedLevels)

	seen := make(map[string]struct{}, len(fixtures.Cases)+len(fixtures.SelectionCases))
	for _, fixture := range fixtures.Cases {
		testutil.RequireFixtureFields(t, "persistence", fixture.Name, []testutil.FixtureField{
			{Key: "name", Value: fixture.Name},
			{Key: "user_email", Value: fixture.UserEmail},
			{Key: "output_path", Value: fixture.OutputPath},
			{Key: "village_url", Value: fixture.VillageURL},
			{Key: "redaction_level", Value: fixture.RedactionLevel},
			{Key: "source_path", Value: fixture.SourcePath},
			{Key: "session_id", Value: fixture.SessionID},
		})
		if _, exists := seen[fixture.Name]; exists {
			t.Fatalf("persistence fixture name %q is duplicated", fixture.Name)
		}
		seen[fixture.Name] = struct{}{}
	}
	observedDecisions := make([]wizardDecision, 0, len(fixtures.SelectionCases))
	observedShapes := make([]wizardEntryShape, 0, len(fixtures.SelectionCases))
	autoBranchAxis := make(map[string]map[bool]wizardSelectionFixture)
	for _, fixture := range fixtures.SelectionCases {
		testutil.RequireFixtureFields(t, "wizard selection", fixture.Name, []testutil.FixtureField{
			{Key: "name", Value: fixture.Name},
			{Key: "git_remote", Value: fixture.GitRemote},
			{Key: "session_id", Value: fixture.SessionID},
			{Key: "expected_match", Value: string(fixture.ExpectedMatch)},
			{Key: "expected_precheck", Value: string(fixture.ExpectedPrecheck)},
			{Key: "expected_precheck_toggled", Value: string(fixture.ExpectedPrecheckToggled)},
		})
		if len(fixture.ConfiguredEntries) == 0 && !fixture.UnrestrictedHarness {
			t.Fatalf("wizard selection fixture %q configures no project entry; add one to configured_entries, or mark it unrestricted_harness", fixture.Name)
		}
		for i, entry := range fixture.ConfiguredEntries {
			if entry.GitRemote == "" && entry.Name == "" {
				t.Fatalf("wizard selection fixture %q configured_entries[%d] carries neither git_remote nor name, so it can identify nothing", fixture.Name, i)
			}
		}
		// Every decoder fails the corpus on a token outside its closed set.
		fixture.ExpectedMatch.BranchMatch(t, "wizard selection", fixture.Name)
		fixture.ExpectedPrecheck.selected(t, fixture.Name)
		fixture.ExpectedPrecheck.glyph(t, fixture.Name)
		fixture.ExpectedPrecheckToggled.glyph(t, fixture.Name)
		if fixture.ExpectedPrecheckToggled.selected(t, fixture.Name) == fixture.ExpectedPrecheck.selected(t, fixture.Name) {
			t.Fatalf("wizard selection fixture %q expects the same tick state before and after a space keypress; a toggle that does not toggle would make the whole toggled expectation vacuous", fixture.Name)
		}
		observedDecisions = append(observedDecisions, wizardDecision{fixture.ExpectedMatch, fixture.ExpectedPrecheck})
		observedShapes = append(observedShapes, fixture.entryShape())
		key := fixture.autoBranchAxisKey()
		if autoBranchAxis[key] == nil {
			autoBranchAxis[key] = make(map[bool]wizardSelectionFixture, 2)
		}
		autoBranchAxis[key][fixture.AutoIngestNewBranches] = fixture
		if _, exists := seen[fixture.Name]; exists {
			t.Fatalf("wizard selection fixture name %q is duplicated", fixture.Name)
		}
		seen[fixture.Name] = struct{}{}
	}
	// Two coverage guards, each derived rather than chosen: the decision pairs
	// (so neither divergence can lose its only row) and the entry shapes (so
	// each way a saved selection can identify a session keeps a row). Together
	// they make every row of this corpus load-bearing, which a row-count floor
	// cannot do — a floor is decremented by the same edit that deletes a row.
	testutil.RequireClosedSetCoverage(t, "wizard selection", "expected_match/expected_precheck pair", allWizardDecisions, observedDecisions)
	testutil.RequireClosedSetCoverage(t, "wizard selection", "configured entry shape", allWizardEntryShapes, observedShapes)

	// The auto-ingest-new-branches setting only means anything as a pair: two
	// otherwise identical cases whose only difference is the setting, and whose
	// expectations differ because of it. Without the pair, hardcoding the
	// setting to a constant in the wizard changes nothing observable.
	paired := false
	for _, pair := range autoBranchAxis {
		on, hasOn := pair[true]
		off, hasOff := pair[false]
		if hasOn && hasOff && on.ExpectedMatch != off.ExpectedMatch {
			paired = true
		}
	}
	if !paired {
		t.Fatal("wizard selection fixture holds no auto_ingest_new_branches pair: it needs two cases identical except for auto_ingest_new_branches whose expected_match differs, or the wizard can stop reading the setting with the corpus green")
	}
	return fixtures
}

// sessionRowIn extracts the one SESSION line from a rendered wizard frame.
//
// The assertion is scoped to that line rather than the whole frame so the
// session's own box is read independently of the group rows above it. Those
// rows carry their own claim about the same session, asserted separately by
// requireGroupRowsOnly below and, over trees with several sessions, by the
// group-row corpus.
//
// The fixture sessions carry a zero Date, so the year the session row formats is
// what identifies it, and no group row carries a date at all.
//
// expandedFrame renders the wizard with every group open. It is the state a
// user reaches by expanding the tree — NOT the state the wizard opens in, which
// is why the drawn opening frame is asserted separately below rather than only
// here.
func expandedFrame(t *testing.T, page *TreeSelectPage) string {
	t.Helper()
	for pi := range page.providers {
		page.providers[pi].expanded = true
		for ri := range page.providers[pi].remotes {
			page.providers[pi].remotes[ri].expanded = true
			for wi := range page.providers[pi].remotes[ri].worktrees {
				page.providers[pi].remotes[ri].worktrees[wi].expanded = true
			}
		}
	}
	return page.View(80, 24)
}

func sessionRowIn(t *testing.T, frame string) string {
	t.Helper()
	var matches []string
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, "0001") {
			matches = append(matches, line)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one session row in the rendered wizard frame, found %d; the frame was:\n%s", len(matches), frame)
	}
	return matches[0]
}

// TestWizardExistingSelection_UsesCanonicalMatcher pins both halves of every
// selection case: what the canonical matcher answers for the configuration, and
// what the wizard then shows the user. Asserting only the second would let a
// case claim it reaches a conflict while configuring something that cannot
// conflict; asserting only the first would not observe the wizard at all.
func TestWizardExistingSelection_UsesCanonicalMatcher(t *testing.T) {
	for _, fixture := range loadPersistenceFixtures(t).SelectionCases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			session := SessionListing{
				Harness:     defaults.HarnessClaudeCode.String(),
				GitRemote:   fixture.GitRemote,
				ProjectName: fixture.ProjectName,
				Branch:      fixture.Branch,
				SessionID:   fixture.SessionID,
			}
			selection := fixture.selection()

			// The declared match is asserted against the real matcher, so a case
			// cannot claim an outcome its configuration does not produce.
			wantMatch := fixture.ExpectedMatch.BranchMatch(t, "wizard selection", fixture.Name)
			gotMatch := config.CompileSelectionMatcher(*selection).MatchDiscovery(
				ingest.Harness(session.Harness), session.GitRemote, session.ProjectName, session.Branch,
				ingest.SessionID(session.SessionID), selection.AutoIngestNewBranches,
			)
			if gotMatch != wantMatch {
				t.Fatalf("canonical matcher answered %v for this configuration, but the case declares expected_match %q; fix whichever is wrong before reading the pre-check result", gotMatch, fixture.ExpectedMatch)
			}

			page := buildFilteredSelectionPage([]SessionListing{session}, []ProviderSelection{{Harness: defaults.HarnessClaudeCode.String()}}, selection)

			// ASSERTION 1 — the write-back half. SelectedSessions is what the
			// wizard persists on confirm, so a cleared box DELETES the session
			// from the saved selection. Keeping a conflicted session ticked is
			// what makes re-running the wizard non-destructive.
			wantSelected := fixture.ExpectedPrecheck.selected(t, fixture.Name)
			if got := len(page.SelectedSessions()) == 1; got != wantSelected {
				t.Fatalf("the wizard would write back selected=%v for this session, want %v (matcher answered %v). Clearing a session the configuration still names deletes it from the saved selection on confirm, and with it the record that anything was ever in conflict.", got, wantSelected, gotMatch)
			}

			wantGlyph := fixture.ExpectedPrecheck.glyph(t, fixture.Name)

			// ASSERTION 2 — the OPENING frame: what is on screen before the
			// user touches anything. The wizard opens with the remote and the
			// branch closed, so for a conflicted session the only boxes drawn
			// here are the group rows above it, and a marker that lived only on
			// the session row would be invisible in the frame that is actually
			// shipped. Every box on screen stands for this one session, so
			// every box on screen must make the same claim it does.
			requireGroupRowsOnly(t, page.View(80, 24), wantGlyph)

			// ASSERTION 3 — the display half, deliberately separate. A ticked
			// box claims the session will be ingested; a withheld one will not
			// be. These two regressions are independent, so one assertion
			// covering both would let whichever it does not cover back in.
			//
			// Asserted on the FRAME the wizard draws, not on the helper that
			// builds one box. View is the mounted exit; a helper it happens to
			// call can be correct while nothing calls it.
			rendered := sessionRowIn(t, expandedFrame(t, page))
			if !strings.Contains(rendered, wantGlyph) {
				t.Fatalf("the wizard draws the session row as %q, want %q (matcher answered %v). A withheld session must not render a plain tick: the tick would promise an ingest that does not happen.", rendered, wantGlyph, gotMatch)
			}
			requireOnlyGlyph(t, rendered, wantGlyph)

			// Assertion 4: an explicitly cleared box
			// wins over the marker. Driven through the real keypress, because
			// that is the one action that produces the state (toggleItem flips
			// the tick and deliberately does NOT clear the conflict flag), and
			// it is reachable in one press from the tree the user is looking at.
			//
			// This is the arm someone deliberately decided, which makes it the
			// worst of the four to leave unguarded. The group rows are asserted
			// alongside the session row: clearing the box must retract the
			// marker from the rows above it too, or the wizard would keep
			// warning about a session the user has answered for.
			before := page.sessionSel[0][0][0][0]
			pressSpaceOnSessionRow(t, page)
			if page.sessionSel[0][0][0][0] == before {
				t.Fatalf("space did not change the session's tick, so the cursor was not on the session row; the flat-row layout must have changed")
			}
			toggledGlyph := fixture.ExpectedPrecheckToggled.glyph(t, fixture.Name)
			requireOnlyGlyph(t, sessionRowIn(t, expandedFrame(t, page)), toggledGlyph)
			requireGroupRowsOnly(t, expandedFrame(t, page), toggledGlyph)

			// And back: pressing again restores the original box, so the marker
			// survives being cleared rather than being consumed by it.
			pressSpaceOnSessionRow(t, page)
			requireOnlyGlyph(t, sessionRowIn(t, expandedFrame(t, page)), wantGlyph)
			requireGroupRowsOnly(t, expandedFrame(t, page), wantGlyph)
		})
	}
}

// requireGroupRowsOnly asserts every GROUP row of a frame carries exactly the
// wanted checkbox. It is used by the single-session cases, where each group row
// stands for that one session and so makes a claim about it: a group drawing
// [✓] over a session that will not be ingested is the same over-claim the [!]
// marker exists to prevent, just one row higher.
//
// Group rows are recognised by the expand arrow the renderer puts on them and
// on nothing else, so the helper does not depend on labels or on how many
// levels the tree happens to have.
func requireGroupRowsOnly(t *testing.T, frame, want string) {
	t.Helper()
	var rows []string
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, "▷") || strings.Contains(line, "▽") {
			rows = append(rows, line)
		}
	}
	// The corpus builds one provider over one remote over one branch, so the
	// opening frame has at least the provider and the remote on it. A frame
	// that drew no group row at all would otherwise satisfy this vacuously.
	if len(rows) < 2 {
		t.Fatalf("expected at least the provider and remote rows in the rendered wizard frame, found %d group row(s); the frame was:\n%s", len(rows), frame)
	}
	for _, row := range rows {
		requireOnlyGlyph(t, row, want)
	}
}

// requireOnlyGlyph asserts the row carries exactly the wanted checkbox and none
// of the others, so a renderer emitting several marks cannot pass.
func requireOnlyGlyph(t *testing.T, row, want string) {
	t.Helper()
	if !strings.Contains(row, want) {
		t.Fatalf("the row %q does not carry %q", row, want)
	}
	for _, other := range allBoxGlyphs {
		if other != want && strings.Contains(row, other) {
			t.Fatalf("the row %q carries %q as well as the expected %q; the box must be exactly one state", row, other, want)
		}
	}
}

// pressSpaceOnSessionRow puts the cursor on the single session and sends the
// real toggle key, rather than writing to the selection grid directly — the
// production path is what a user has, and it is what carries the precedence
// rule between the tick and the marker.
func pressSpaceOnSessionRow(t *testing.T, page *TreeSelectPage) {
	t.Helper()
	for pi := range page.providers {
		page.providers[pi].expanded = true
		for ri := range page.providers[pi].remotes {
			page.providers[pi].remotes[ri].expanded = true
			for wi := range page.providers[pi].remotes[ri].worktrees {
				page.providers[pi].remotes[ri].worktrees[wi].expanded = true
			}
		}
	}
	// provider, remote, worktree, session — the corpus builds exactly one of
	// each, and the caller asserts the tick actually moved, so a layout change
	// fails loudly instead of toggling a different row.
	page.cursor = 3
	page.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
}

func TestConfigSaveTo_ExactPathPreservesLoadedSettings(t *testing.T) {
	for _, fixture := range loadPersistenceFixtures(t).Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			defaultHome := t.TempDir()
			t.Setenv(defaults.EnvXDGConfigHome.String(), defaultHome)
			exact := filepath.Join(t.TempDir(), "chosen", "settings.yaml")
			loaded := config.BaseConfig()
			loaded.User.Email = fixture.UserEmail
			loaded.Output.BasePath = fixture.OutputPath
			loaded.Village.URL = fixture.VillageURL
			loaded.Redaction.Level = redact.RedactionLevel(fixture.RedactionLevel)
			loaded.Sources.ClaudeCode.Paths = []string{fixture.SourcePath}

			loaded.Selection = config.SelectionConfig{Mode: config.SelectionModeAll, Harnesses: map[string]config.SelectionHarnessConfig{
				defaults.HarnessOpenCode.String(): {Sessions: []string{fixture.SessionID}},
			}}
			loaded.Push.Sources = []string{defaults.HarnessOpenCode.String()}

			// Snapshot the caller's configuration, including its reference-typed
			// fields, so a write THROUGH the copy is caught and not only a write
			// to a scalar. SaveTo merges into a copy: the wizard keeps using this
			// object after a save, and a wizard restart re-reads it.
			before := *loaded
			beforePaths := slices.Clone(loaded.Sources.ClaudeCode.Paths)
			beforePushSources := slices.Clone(loaded.Push.Sources)
			beforeHarnesses := maps.Clone(loaded.Selection.Harnesses)

			wizard := &Config{
				VillageConnected: true,
				DaemonMode:       "opt-in",
				ImportMethod:     string(config.PushMethodBySource),
				ImportSources:    providerSources(defaults.HarnessClaudeCode),
				License:          config.LicenseCC0,
				Selection: &config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{
					defaults.HarnessClaudeCode.String(): {Sessions: []string{fixture.SessionID}},
				}},
			}
			if err := wizard.SaveTo(exact, loaded); err != nil {
				t.Fatalf("save wizard choices to exact path: %v", err)
			}
			for _, unchanged := range []struct {
				field  string
				stayed bool
			}{
				{"Village.Connected", loaded.Village.Connected == before.Village.Connected},
				{"Daemon.ProjectMode", loaded.Daemon.ProjectMode == before.Daemon.ProjectMode},
				{"Push.Method", loaded.Push.Method == before.Push.Method},
				{"Push.License", loaded.Push.License == before.Push.License},
				{"Push.Sources", slices.Equal(loaded.Push.Sources, beforePushSources)},
				{"Redaction.Level", loaded.Redaction.Level == before.Redaction.Level},
				{"Sources.ClaudeCode.Enabled", loaded.Sources.ClaudeCode.Enabled == before.Sources.ClaudeCode.Enabled},
				{"Sources.ClaudeCode.Paths", slices.Equal(loaded.Sources.ClaudeCode.Paths, beforePaths)},
				{"Selection.Mode", loaded.Selection.Mode == before.Selection.Mode},
				{"Selection.Harnesses", maps.EqualFunc(loaded.Selection.Harnesses, beforeHarnesses, func(a, b config.SelectionHarnessConfig) bool {
					return slices.Equal(a.Sessions, b.Sessions) && len(a.Projects) == len(b.Projects)
				})},
			} {
				if !unchanged.stayed {
					t.Fatalf("SaveTo mutated the caller's loaded configuration at %s; it must merge into a copy, because the wizard keeps using this object after a save and a restart re-reads it (before=%+v after=%+v)", unchanged.field, before, *loaded)
				}
			}
			data, err := os.ReadFile(exact)
			if err != nil {
				t.Fatalf("read exact wizard config path: %v", err)
			}
			got, err := config.Parse(data)
			if err != nil {
				t.Fatalf("parse exact wizard config: %v", err)
			}
			if got.User.Email != fixture.UserEmail || got.Output.BasePath != fixture.OutputPath || got.Village.URL != fixture.VillageURL || got.Redaction.Level.String() != fixture.RedactionLevel || len(got.Sources.ClaudeCode.Paths) != 1 || got.Sources.ClaudeCode.Paths[0] != fixture.SourcePath || !got.Village.Connected {
				t.Fatalf("saved config did not preserve loaded settings and apply wizard choices: %+v", got)
			}
			if _, err := os.Stat(defaults.ResolveConfigFilePath().String()); !os.IsNotExist(err) {
				t.Fatalf("wizard wrote default config path instead of exact path: %v", err)
			}
		})
	}
}

func TestWizardRestart_PreservesPersistenceAndSaves(t *testing.T) {
	t.Setenv(defaults.EnvXDGConfigHome.String(), t.TempDir())
	exact := filepath.Join(t.TempDir(), "chosen.yaml")
	loaded := config.BaseConfig()
	loaded.User.Email = "restart@example.test"
	wizard := NewWizard(WithConfigPersistence(exact, loaded)).restart()
	wizard.answers.DaemonMode = "opt-in"
	if err := wizard.saveConfig(); err != nil {
		t.Fatalf("save restarted wizard config: %v", err)
	}
	data, err := os.ReadFile(exact)
	if err != nil {
		t.Fatalf("read restarted wizard exact config: %v", err)
	}
	got, err := config.Parse(data)
	if err != nil || got.User.Email != loaded.User.Email {
		t.Fatalf("restarted wizard did not preserve loaded config: config=%+v err=%v", got, err)
	}
	if _, err := os.Stat(defaults.ResolveConfigFilePath().String()); !os.IsNotExist(err) {
		t.Fatalf("restarted wizard wrote default config path: %v", err)
	}
}

func TestWizardRestartPreservesJourneyRunnerWithoutExecutingIt(t *testing.T) {
	runner := &restartJourneyRunner{}
	restarted := NewWizard(WithJourneyRunner(runner)).restart()
	if restarted.journeyRunner != runner {
		t.Fatal("restart replaced the injected journey runner authority")
	}
	if runner.calls != 0 {
		t.Fatalf("restart invoked journey side effects %d time(s)", runner.calls)
	}
}

func TestWizardSkipsLegacyIngestionPageWithJourneyRunner(t *testing.T) {
	wizard := NewWizard(WithJourneyRunner(&restartJourneyRunner{}), WithIngestRunner(func(context.Context, WizardAnswers) (*IngestResult, error) {
		return &IngestResult{}, nil
	}))
	wizard.answers.WantImport = true
	if !wizard.shouldSkip(pageIngestion) {
		t.Fatal("mounted journey runner left the legacy ingestion page reachable")
	}
}

func TestWizardStreamsIngestProgressThroughOrderedJourneyOnce(t *testing.T) {
	progress := &synchronizedProgress{data: map[string]StageProgress{}}
	ingestStarted := make(chan struct{})
	releaseIngest := make(chan struct{})
	var ingestCalls atomic.Int32
	runner := OrderedJourneyRunner{Operations: map[ExecutionStage]StageOperation{
		StageIngest: func(ctx context.Context, _ JourneyRequest) ([]PersistedEffect, []RetryTarget, error) {
			ingestCalls.Add(1)
			progress.set("DISCOVER", StageProgress{Done: 1, Total: 1, Ended: true})
			progress.set("EXTRACT+WRITE", StageProgress{Done: 1, Total: 2})
			close(ingestStarted)
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-releaseIngest:
				return []PersistedEffect{{Stage: StageIngest, Status: StatusPersisted, SessionID: "session-one"}}, nil, nil
			}
		},
	}}
	wizard := NewWizard(WithJourneyRunner(runner), WithProgress(progress))
	wizard.executing = true
	finished := make(chan tea.Msg, 1)
	go func() { finished <- wizard.startJourney(nil, nil)() }()
	<-ingestStarted

	view := wizard.View().Content
	if !strings.Contains(view, "Discover") || !strings.Contains(view, "Extract") || !strings.Contains(view, "1/2") {
		t.Fatalf("ordered journey did not render detailed ingest progress: %s", view)
	}
	if ingestCalls.Load() != 1 {
		t.Fatalf("ordered journey executed ingest %d times, want exactly once", ingestCalls.Load())
	}
	close(releaseIngest)
	model, _ := wizard.Update(<-finished)
	wizard = model.(WizardModel)
	if ingestCalls.Load() != 1 || wizard.journeyResult == nil {
		t.Fatalf("ordered journey completion duplicated ingest or lost result: calls=%d result=%+v", ingestCalls.Load(), wizard.journeyResult)
	}
}

func TestWizardRetryRejectsStaleProgressTick(t *testing.T) {
	wizard := NewWizard(WithJourneyRunner(&restartJourneyRunner{}))
	wizard.journeyProgressToken = 1
	wizard.executing = true
	result := JourneyResult{Retry: []RetryTarget{{Stage: StageIngest, SessionIDs: []string{"session-one"}}}}
	model, _ := wizard.Update(journeyFinishedMsg{result: result})
	wizard = model.(WizardModel)
	if wizard.executing || wizard.journeyProgressToken != 2 {
		t.Fatalf("completed journey did not invalidate its progress generation: executing=%v token=%d", wizard.executing, wizard.journeyProgressToken)
	}

	model, retryCmd := wizard.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	wizard = model.(WizardModel)
	if retryCmd == nil || !wizard.executing || wizard.journeyProgressToken != 3 {
		t.Fatalf("retry did not start exactly one new progress generation: executing=%v token=%d cmd=%v", wizard.executing, wizard.journeyProgressToken, retryCmd != nil)
	}

	model, staleCmd := wizard.Update(journeyProgressTickMsg{token: 1})
	wizard = model.(WizardModel)
	if staleCmd != nil {
		t.Fatal("stale progress tick from completed journey rescheduled during retry")
	}
	model, currentCmd := wizard.Update(journeyProgressTickMsg{token: 3})
	wizard = model.(WizardModel)
	if currentCmd == nil {
		t.Fatal("current retry progress tick did not reschedule")
	}
}

func TestWizardExecutionCancellationWaitsForMountedRunner(t *testing.T) {
	for _, fixture := range loadExecutionCancelFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			runner := blockedJourneyRunner{started: make(chan struct{})}
			wizard := NewWizard(WithJourneyRunner(runner))
			wizard.executing = true
			prior := []PersistedEffect{{Stage: StageConfig, Status: StatusPersisted, Detail: "/tmp/config.yaml"}}
			cmd := wizard.startJourney(nil, prior)
			finished := make(chan tea.Msg, 1)
			go func() { finished <- cmd() }()
			<-runner.started
			key := tea.KeyPressMsg{Text: fixture.Key}
			if fixture.Key == "ctrl-c" {
				key = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
			}
			model, quitCmd := wizard.Update(key)
			wizard = model.(WizardModel)
			if quitCmd != nil || wizard.quitting || !wizard.executing || !wizard.cancellationRequested {
				t.Fatalf("cancellation did not wait for mounted runner: quitCmd=%v", quitCmd != nil)
			}
			model, _ = wizard.Update(<-finished)
			wizard = model.(WizardModel)
			if wizard.executing || wizard.journeyResult == nil || len(wizard.journeyResult.Effects) != 2 {
				t.Fatalf("finished cancellation did not preserve effects: %+v", wizard.journeyResult)
			}
			view := wizard.View().Content
			if !strings.Contains(view, "config:") || !strings.Contains(view, "cancelled=1") {
				t.Fatalf("cancellation receipt omitted persisted effects: %s", view)
			}
		})
	}
}
