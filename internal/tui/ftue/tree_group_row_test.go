package ftue

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/projectlabel"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/group_rows.yaml
var groupRowFixtureYAML []byte

// groupGlyph is the closed set of boxes a GROUP row (provider, remote, branch)
// can draw. It is four-valued because a group carries one fact a session row
// does not — how its children combine — and one the session row also carries:
// whether anything under it is selected but undecidable.
type groupGlyph string

const (
	// groupGlyphEmpty: nothing under this row is selected.
	groupGlyphEmpty groupGlyph = "[ ]"
	// groupGlyphPartial: some but not all of it is selected.
	groupGlyphPartial groupGlyph = "[~]"
	// groupGlyphChecked: all of it is selected and all of it will be ingested.
	groupGlyphChecked groupGlyph = "[✓]"
	// groupGlyphConflict: something under it is selected but cannot be decided,
	// so it will not be ingested. Beats [✓] and [~], which are the two claims a
	// hidden conflict falsifies.
	groupGlyphConflict groupGlyph = "[!]"
)

var allGroupGlyphs = []groupGlyph{groupGlyphEmpty, groupGlyphPartial, groupGlyphChecked, groupGlyphConflict}

// allBoxGlyphs is every box ANY row can draw, derived from the group set
// because that is the widest — a session row draws three of these four. It is
// the exclusion set behind "this row carries exactly one box", so a renderer
// gaining a state without this set gaining it is what would weaken that check.
var allBoxGlyphs = func() []string {
	out := make([]string, 0, len(allGroupGlyphs))
	for _, glyph := range allGroupGlyphs {
		out = append(out, string(glyph))
	}
	return out
}()

func (g groupGlyph) validate(t *testing.T, caseName, field string) groupGlyph {
	t.Helper()
	if !slices.Contains(allGroupGlyphs, g) {
		t.Fatalf("group rows fixture case %q declares %s %q, which is not a box the tree can draw; use one of %v", caseName, field, string(g), allGroupGlyphs)
	}
	return g
}

type groupRowFixtures struct {
	DeclaredRows int               `yaml:"declared_rows"`
	Cases        []groupRowFixture `yaml:"cases"`
}

type groupRowFixture struct {
	Name string `yaml:"name"`
	// GitRemote and ProjectName are the project a session belongs to unless it
	// names its own, so a case that needs two projects says so on the one
	// session that differs rather than repeating the common one on every row.
	GitRemote   string `yaml:"git_remote"`
	ProjectName string `yaml:"project_name"`
	// ConfiguredEntries and ConfiguredSessions are the saved selection the
	// wizard is re-run over. Sessions are the explicit-ID allowlist, which is
	// the only way to make one session of a branch decidable while another on
	// the same branch is not — the case where the branch row would otherwise
	// draw a plain tick over a conflict.
	ConfiguredEntries     []wizardConfiguredEntry `yaml:"configured_entries"`
	ConfiguredSessions    []string                `yaml:"configured_sessions"`
	AutoIngestNewBranches bool                    `yaml:"auto_ingest_new_branches"`
	Sessions              []groupRowSession       `yaml:"sessions"`

	ExpectedProviderGlyph groupGlyph `yaml:"expected_provider_glyph"`
	// ExpectedRemoteGlyphs and ExpectedWorktreeGlyphs are keyed by the remote a
	// session declares and by branch, and must name every group the case
	// produces and no others, so a case cannot quietly stop asserting one.
	ExpectedRemoteGlyphs   map[string]groupGlyph `yaml:"expected_remote_glyphs"`
	ExpectedWorktreeGlyphs map[string]groupGlyph `yaml:"expected_worktree_glyphs"`
}

type groupRowSession struct {
	Branch string `yaml:"branch"`
	// GitRemote and ProjectName override the case's project for this session.
	GitRemote        string                    `yaml:"git_remote"`
	ProjectName      string                    `yaml:"project_name"`
	SessionID        string                    `yaml:"session_id"`
	ExpectedMatch    testutil.SelectionOutcome `yaml:"expected_match"`
	ExpectedPrecheck wizardPrecheck            `yaml:"expected_precheck"`
}

// project resolves the project a session belongs to.
func (f groupRowFixture) project(sess groupRowSession) (gitRemote, projectName string) {
	gitRemote, projectName = sess.GitRemote, sess.ProjectName
	if gitRemote == "" {
		gitRemote = f.GitRemote
	}
	if projectName == "" {
		projectName = f.ProjectName
	}
	return gitRemote, projectName
}

func (f groupRowFixture) selection() *config.SelectionConfig {
	projects := make([]config.ProjectSelection, 0, len(f.ConfiguredEntries))
	for _, entry := range f.ConfiguredEntries {
		projects = append(projects, config.ProjectSelection{GitRemote: entry.GitRemote, Name: entry.Name, Branches: entry.Branches})
	}
	return &config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: f.AutoIngestNewBranches,
		Harnesses: map[string]config.SelectionHarnessConfig{
			defaults.HarnessClaudeCode.String(): {Projects: projects, Sessions: f.ConfiguredSessions},
		},
	}
}

// listings builds the discovered sessions this case describes, in declaration
// order — which is the order the tree groups them in. The session ID is used as
// the title so each session row can be found in the drawn frame by something
// the fixture states, rather than by its position.
func (f groupRowFixture) listings() []SessionListing {
	out := make([]SessionListing, 0, len(f.Sessions))
	for _, sess := range f.Sessions {
		gitRemote, projectName := f.project(sess)
		out = append(out, SessionListing{
			Harness:     defaults.HarnessClaudeCode.String(),
			GitRemote:   gitRemote,
			ProjectName: projectName,
			Branch:      sess.Branch,
			SessionID:   sess.SessionID,
			Title:       sess.SessionID,
		})
	}
	return out
}

// remotes lists the distinct project remotes of a case in the order the tree
// draws them; branchesOf does the same for one remote's branches. Order is part
// of what these cases assert: a rule that only inspected a group's FIRST child
// is only visible when the interesting child is not first.
func (f groupRowFixture) remotes() []string {
	var out []string
	for _, sess := range f.Sessions {
		gitRemote, _ := f.project(sess)
		if !slices.Contains(out, gitRemote) {
			out = append(out, gitRemote)
		}
	}
	return out
}

func (f groupRowFixture) branchesOf(gitRemote string) []string {
	var out []string
	for _, sess := range f.Sessions {
		remote, _ := f.project(sess)
		if remote != gitRemote || slices.Contains(out, sess.Branch) {
			continue
		}
		out = append(out, sess.Branch)
	}
	return out
}

// remoteLabel is the label the tree draws for a remote's row, derived the way
// the page derives it, from the first session that belongs to that remote.
func (f groupRowFixture) remoteLabel(t *testing.T, gitRemote string) string {
	t.Helper()
	for _, sess := range f.Sessions {
		remote, projectName := f.project(sess)
		if remote == gitRemote {
			return projectlabel.Label(remote, projectName)
		}
	}
	t.Fatalf("group rows fixture case %q asks for the row label of remote %q, which none of its sessions belongs to", f.Name, gitRemote)
	return ""
}

func (f groupRowFixture) branches() []string {
	var out []string
	for _, gitRemote := range f.remotes() {
		out = append(out, f.branchesOf(gitRemote)...)
	}
	return out
}

func loadGroupRowFixtures(t *testing.T) groupRowFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(groupRowFixtureYAML))
	decoder.KnownFields(true)
	var fixtures groupRowFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode group rows fixture with strict fields: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("group rows fixture must contain exactly one YAML document: %v", err)
	}
	// Floor EQUALS the row count: deleting a row and decrementing the
	// declaration still trips it, so the corpus only ratchets up. The derived
	// guards below are the second layer, because a floor cannot see a row
	// swapped for a junk one at the same count.
	if fixtures.DeclaredRows != len(fixtures.Cases) || fixtures.DeclaredRows < 8 {
		t.Fatalf("group rows fixture row guard failed: declared=%d actual=%d minimum=8", fixtures.DeclaredRows, len(fixtures.Cases))
	}

	var (
		// Coverage is tracked PER LEVEL, not pooled: a level is a different row
		// with a different aggregation behind it, and pooling lets one level's
		// rows stand in for another's, which leaves whole cases deletable.
		observedProviderGlyphs []groupGlyph
		observedRemoteGlyphs   []groupGlyph
		observedBranchGlyphs   []groupGlyph
		observedPrechecks      []wizardPrecheck
		observedMatches        []testutil.SelectionOutcome
		// The properties that make this corpus able to catch a propagation
		// defect, each RECOGNISED from the declared data rather than labelled by
		// a row — a label could be moved onto an unrelated case to keep a guard
		// satisfied.
		aggregatesRatherThanCopies bool
		conflictBeatsATick         bool
		conflictBeatsAPartial      bool
		conflictSparesASibling     bool
		conflictPastAFirstSession  bool
		conflictPastAFirstBranch   bool
		conflictPastAFirstRemote   bool
	)
	seen := make(map[string]struct{}, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		testutil.RequireFixtureFields(t, "group rows", fixture.Name, []testutil.FixtureField{
			{Key: "name", Value: fixture.Name},
			{Key: "git_remote", Value: fixture.GitRemote},
			{Key: "project_name", Value: fixture.ProjectName},
			{Key: "expected_provider_glyph", Value: string(fixture.ExpectedProviderGlyph)},
		})
		if _, exists := seen[fixture.Name]; exists {
			t.Fatalf("group rows fixture name %q is duplicated", fixture.Name)
		}
		seen[fixture.Name] = struct{}{}
		if len(fixture.ConfiguredEntries) == 0 {
			t.Fatalf("group rows fixture case %q configures no project entry, so the harness is unrestricted and the wizard pre-checks nothing whatever the rule under test does; add an entry to configured_entries", fixture.Name)
		}
		for i, entry := range fixture.ConfiguredEntries {
			if entry.GitRemote == "" && entry.Name == "" {
				t.Fatalf("group rows fixture case %q configured_entries[%d] carries neither git_remote nor name, so it can identify nothing", fixture.Name, i)
			}
		}
		// A group row only aggregates when it has something to aggregate: over a
		// single session it would pass whether the rule walks the subtree or
		// copies its only child.
		if len(fixture.Sessions) < 2 {
			t.Fatalf("group rows fixture case %q declares %d session(s); a group row over one session cannot distinguish aggregation from copying its only child, so every case needs at least two", fixture.Name, len(fixture.Sessions))
		}

		fixture.ExpectedProviderGlyph.validate(t, fixture.Name, "expected_provider_glyph")
		observedProviderGlyphs = append(observedProviderGlyphs, fixture.ExpectedProviderGlyph)

		remotes := fixture.remotes()
		if len(fixture.ExpectedRemoteGlyphs) != len(remotes) {
			t.Fatalf("group rows fixture case %q produces %d remote row(s) but declares %d expected_remote_glyphs; every remote row the wizard draws must be asserted, and no key may name a remote the case does not have", fixture.Name, len(remotes), len(fixture.ExpectedRemoteGlyphs))
		}
		if len(fixture.ExpectedWorktreeGlyphs) != len(fixture.branches()) {
			t.Fatalf("group rows fixture case %q produces %d branch row(s) but declares %d expected_worktree_glyphs; every branch row the wizard draws must be asserted, and no key may name a branch the case does not have", fixture.Name, len(fixture.branches()), len(fixture.ExpectedWorktreeGlyphs))
		}

		var remoteGlyphs []groupGlyph
		for remoteIdx, gitRemote := range remotes {
			remoteGlyph, ok := fixture.ExpectedRemoteGlyphs[gitRemote]
			if !ok {
				t.Fatalf("group rows fixture case %q has sessions on remote %q but declares no expected_remote_glyphs entry for it", fixture.Name, gitRemote)
			}
			remoteGlyph.validate(t, fixture.Name, "expected_remote_glyphs["+gitRemote+"]")
			remoteGlyphs = append(remoteGlyphs, remoteGlyph)
			observedRemoteGlyphs = append(observedRemoteGlyphs, remoteGlyph)

			branches := fixture.branchesOf(gitRemote)
			var branchGlyphs []groupGlyph
			for branchIdx, branch := range branches {
				branchGlyph, ok := fixture.ExpectedWorktreeGlyphs[branch]
				if !ok {
					t.Fatalf("group rows fixture case %q has sessions on branch %q but declares no expected_worktree_glyphs entry for it", fixture.Name, branch)
				}
				branchGlyph.validate(t, fixture.Name, "expected_worktree_glyphs["+branch+"]")
				branchGlyphs = append(branchGlyphs, branchGlyph)
				observedBranchGlyphs = append(observedBranchGlyphs, branchGlyph)
				if branchGlyph == groupGlyphConflict && branchIdx > 0 {
					conflictPastAFirstBranch = true
				}
			}
			if !slices.Contains(branchGlyphs, remoteGlyph) {
				aggregatesRatherThanCopies = true
			}
			if remoteGlyph == groupGlyphConflict && slices.Contains(branchGlyphs, groupGlyphChecked) {
				conflictSparesASibling = true
			}
			if remoteGlyph == groupGlyphConflict && slices.Contains(branchGlyphs, groupGlyphEmpty) {
				// A remote whose only selected session is undecidable: its tick
				// state is partial, so [~] is what it would draw unmarked.
				conflictBeatsAPartial = true
			}
			if remoteGlyph == groupGlyphConflict && remoteIdx > 0 {
				conflictPastAFirstRemote = true
			}
		}
		if fixture.ExpectedProviderGlyph == groupGlyphConflict && slices.Contains(remoteGlyphs, groupGlyphChecked) {
			conflictSparesASibling = true
		}

		for i, sess := range fixture.Sessions {
			testutil.RequireFixtureFields(t, "group rows", fixture.Name, []testutil.FixtureField{
				{Key: "sessions[].branch", Value: sess.Branch},
				{Key: "sessions[].session_id", Value: sess.SessionID},
				{Key: "sessions[].expected_match", Value: string(sess.ExpectedMatch)},
				{Key: "sessions[].expected_precheck", Value: string(sess.ExpectedPrecheck)},
			})
			for _, earlier := range fixture.Sessions[:i] {
				if earlier.SessionID == sess.SessionID {
					t.Fatalf("group rows fixture case %q reuses session_id %q; session rows are located in the drawn frame by that id, so a duplicate would assert on an ambiguous row", fixture.Name, sess.SessionID)
				}
				earlierRemote, _ := fixture.project(earlier)
				thisRemote, _ := fixture.project(sess)
				if earlier.Branch == sess.Branch && earlierRemote != thisRemote {
					t.Fatalf("group rows fixture case %q puts branch %q under two different remotes; branch rows are keyed by name, so the case could not say which of the two a glyph belongs to", fixture.Name, sess.Branch)
				}
			}
			// Every token is decoded through its own closed set, so a typo fails
			// the corpus rather than weakening an expectation that still passes.
			sess.ExpectedMatch.BranchMatch(t, "group rows", fixture.Name)
			sess.ExpectedPrecheck.glyph(t, fixture.Name)
			// The pair a session states must be one the wizard is allowed to
			// make: allWizardDecisions is the allowed map between what the
			// matcher answers and what the wizard shows.
			pair := wizardDecision{sess.ExpectedMatch, sess.ExpectedPrecheck}
			if !slices.Contains(allWizardDecisions, pair) {
				t.Fatalf("group rows fixture case %q pairs expected_match %q with expected_precheck %q, which is not one of the allowed wizard decisions %v", fixture.Name, sess.ExpectedMatch, sess.ExpectedPrecheck, allWizardDecisions)
			}
			observedPrechecks = append(observedPrechecks, sess.ExpectedPrecheck)
			observedMatches = append(observedMatches, sess.ExpectedMatch)
		}

		for _, branch := range fixture.branches() {
			var flagged, ticked, position int
			for _, sess := range fixture.Sessions {
				if sess.Branch != branch {
					continue
				}
				switch sess.ExpectedPrecheck {
				case wizardPrecheckFlagged:
					flagged++
					if position > 0 {
						conflictPastAFirstSession = true
					}
				case wizardPrecheckTicked:
					ticked++
				}
				position++
			}
			if flagged > 0 && ticked > 0 {
				conflictBeatsATick = true
			}
		}
	}

	// Coverage layer: every box a group row can draw, every box a session row
	// can draw, and every answer the matcher can give must be exercised.
	testutil.RequireClosedSetCoverage(t, "group rows", "provider row glyph", allGroupGlyphs, observedProviderGlyphs)
	testutil.RequireClosedSetCoverage(t, "group rows", "remote row glyph", allGroupGlyphs, observedRemoteGlyphs)
	testutil.RequireClosedSetCoverage(t, "group rows", "branch row glyph", allGroupGlyphs, observedBranchGlyphs)
	testutil.RequireClosedSetCoverage(t, "group rows", "session expected_precheck", allWizardPrechecks, observedPrechecks)
	testutil.RequireClosedSetCoverage(t, "group rows", "session expected_match", testutil.AllSelectionOutcomes, observedMatches)

	for _, required := range []struct {
		held bool
		why  string
	}{
		{aggregatesRatherThanCopies, "no case whose remote box is a value none of its branch rows draws: without one, a remote row that simply copied a child's box would satisfy the whole corpus"},
		{conflictBeatsATick, "no branch carrying both a flagged and a plainly ticked session: without one, nothing shows the marker beating [✓] inside a single group, which is the claim the opening frame makes"},
		{conflictBeatsAPartial, "no group mixing a flagged session with an unselected one: without it, nothing shows the marker beating [~], and a group whose only selected session is undecidable would still read as 'some of these will be ingested'"},
		{conflictSparesASibling, "no marked group with a plainly ticked sibling under it: without one, a rule that marked every row as soon as anything conflicted would pass, and the marker would stop meaning anything"},
		{conflictPastAFirstSession, "no conflict on any session but a branch's first: without one, a rule that only inspected a branch's first session would leave the branch row over-claiming and still pass"},
		{conflictPastAFirstBranch, "no conflict under any branch but a remote's first: without one, a rule that only inspected a remote's first branch would leave the remote row over-claiming and still pass"},
		{conflictPastAFirstRemote, "no conflict under any remote but the first: without one, a rule that only inspected the first remote would leave the provider row — the row that is always on screen — over-claiming and still pass"},
	} {
		if !required.held {
			t.Fatalf("group rows fixture holds %s", required.why)
		}
	}
	return fixtures
}

// TestWizardGroupRows_DisclosesConflictInTheDrawnFrame asserts the boxes of the
// provider, remote and branch rows.
//
// It asserts them FIRST on the frame the wizard actually opens with — no
// test-side expansion — because that is the frame every user of a re-run
// kickstart sees, and it is the one a conflicted session can be invisible in:
// the wizard opens with the remotes and branches closed, so a marker that lived
// only on the session row would be one to two keypresses out of sight while the
// rows on screen drew plain ticks over it.
//
// The deeper rows are then opened through the production key handler, so every
// row this test reads is a row the wizard drew for a state a user can reach.
func TestWizardGroupRows_DisclosesConflictInTheDrawnFrame(t *testing.T) {
	for _, fixture := range loadGroupRowFixtures(t).Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			selection := fixture.selection()
			matcher := config.CompileSelectionMatcher(*selection)

			// Ground every declared outcome in the real matcher first, so a case
			// cannot claim a conflict its configuration does not produce and
			// then "prove" the marker over a state the wizard never reaches.
			for _, sess := range fixture.Sessions {
				gitRemote, projectName := fixture.project(sess)
				want := sess.ExpectedMatch.BranchMatch(t, "group rows", fixture.Name)
				got := matcher.MatchDiscovery(
					ingest.Harness(defaults.HarnessClaudeCode.String()), gitRemote, projectName,
					sess.Branch, ingest.SessionID(sess.SessionID), selection.AutoIngestNewBranches,
				)
				if got != want {
					t.Fatalf("canonical matcher answered %v for session %s on branch %q of %q, but the case declares expected_match %q; fix whichever is wrong before reading the drawn frame", got, sess.SessionID, sess.Branch, gitRemote, sess.ExpectedMatch)
				}
			}

			page := buildFilteredSelectionPage(
				fixture.listings(),
				[]ProviderSelection{{Harness: defaults.HarnessClaudeCode.String()}},
				selection,
			)

			// THE FRAME THE WIZARD ACTUALLY DRAWS — nothing expanded by the test.
			opening := page.View(80, 24)
			providerNeedle := string(schema.HarnessDisplayName(defaults.HarnessClaudeCode)) + " ("
			requireOnlyGlyph(t, rowWith(t, opening, providerNeedle, "provider"), string(fixture.ExpectedProviderGlyph))
			for _, gitRemote := range fixture.remotes() {
				needle := fixture.remoteLabel(t, gitRemote) + " ("
				requireOnlyGlyph(t, rowWith(t, opening, needle, "remote "+gitRemote), string(fixture.ExpectedRemoteGlyphs[gitRemote]))
			}

			// Then open everything the way a user does, and read the rows the
			// opening frame kept closed.
			expandAllViaKeypresses(t, page)
			opened := page.View(80, 24)

			// The rows that were already on screen must not change their claim
			// by being opened: a marker that only appears once the group is
			// expanded is the defect, not the fix.
			requireOnlyGlyph(t, rowWith(t, opened, providerNeedle, "provider"), string(fixture.ExpectedProviderGlyph))
			for _, gitRemote := range fixture.remotes() {
				needle := fixture.remoteLabel(t, gitRemote) + " ("
				requireOnlyGlyph(t, rowWith(t, opened, needle, "remote "+gitRemote), string(fixture.ExpectedRemoteGlyphs[gitRemote]))
			}
			for _, branch := range fixture.branches() {
				requireOnlyGlyph(t, rowWith(t, opened, branch+" (", "branch "+branch), string(fixture.ExpectedWorktreeGlyphs[branch]))
			}
			for _, sess := range fixture.Sessions {
				want := sess.ExpectedPrecheck.glyph(t, fixture.Name)
				requireOnlyGlyph(t, rowWith(t, opened, sess.SessionID, "session "+sess.SessionID), want)
			}

			// The marker is a DISPLAY distinction, so it must not change what
			// the space key does to a group: a group whose sessions are all
			// ticked still clears in one press, marked or not. Folding the
			// marker into the tick state would silently invert this for exactly
			// the conflicted groups, and nothing above would notice.
			requireGroupToggleStillReadsTickState(t, page, fixture)
		})
	}
}

// requireGroupToggleStillReadsTickState presses the real toggle key on a
// case's FIRST remote row and asserts the sessions under it end up where the
// pre-existing rule puts them: a group that is entirely ticked clears, and any
// other group fills. The expectation is derived from the declared pre-check
// states, not from what the page reports, so a rule change has to move the
// fixture rather than the assertion.
func requireGroupToggleStillReadsTickState(t *testing.T, page *TreeSelectPage, fixture groupRowFixture) {
	t.Helper()
	gitRemote := fixture.remotes()[0]
	var under []string
	allTicked := true
	for _, sess := range fixture.Sessions {
		remote, _ := fixture.project(sess)
		if remote != gitRemote {
			continue
		}
		under = append(under, sess.SessionID)
		if !sess.ExpectedPrecheck.selected(t, fixture.Name) {
			allTicked = false
		}
	}

	pressed := false
	for i, item := range page.flatItems() {
		if item.level != treeLevelRemote || item.remoteIdx != 0 {
			continue
		}
		page.cursor = i
		page.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
		pressed = true
		break
	}
	if !pressed {
		t.Fatalf("found no remote row to press space on in case %q; the flat-row layout must have changed", fixture.Name)
	}

	selected := make(map[string]bool, len(page.SelectedSessions()))
	for _, sess := range page.SelectedSessions() {
		selected[sess.SessionID] = true
	}
	for _, sessionID := range under {
		// A press on an entirely ticked group clears it; on any other group it
		// fills it. That rule predates the marker and must survive it.
		if want := !allTicked; selected[sessionID] != want {
			t.Fatalf("after one space press on the remote row of case %q, session %s is selected=%v, want %v; pressing space on a group must still be decided by whether the group is entirely ticked, which the conflict marker does not change", fixture.Name, sessionID, selected[sessionID], want)
		}
	}
}

// rowWith returns the single line of a rendered frame carrying needle, failing
// with the whole frame when it is missing or ambiguous — a lookup that silently
// matched nothing would make every assertion built on it vacuous.
func rowWith(t *testing.T, frame, needle, what string) string {
	t.Helper()
	var matches []string
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, needle) {
			matches = append(matches, line)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one %s row containing %q in the rendered wizard frame, found %d; the frame was:\n%s", what, needle, len(matches), frame)
	}
	return matches[0]
}

// expandAllViaKeypresses opens every closed group with the production expand
// key, one row at a time, re-reading the row layout after each press because
// opening a row changes it. Writing the expansion flags directly would render a
// state no keypress produces, which is how the gap this test exists for stayed
// invisible.
func expandAllViaKeypresses(t *testing.T, page *TreeSelectPage) {
	t.Helper()
	for range 64 {
		items := page.flatItems()
		pressed := false
		for i, item := range items {
			if rowExpanded(page, item) {
				continue
			}
			page.cursor = i
			page.Update(tea.KeyPressMsg{Code: tea.KeyRight})
			if !rowExpanded(page, item) {
				t.Fatalf("the expand key left row %d (level %v) closed; the tree key handling must have changed", i, item.level)
			}
			pressed = true
			break
		}
		if !pressed {
			return
		}
	}
	t.Fatal("gave up expanding the tree after 64 keypresses; the fixture trees are small, so this means a press stopped opening the row it was on")
}

func rowExpanded(page *TreeSelectPage, item treeFlatItem) bool {
	switch item.level {
	case treeLevelProvider:
		return page.providers[item.providerIdx].expanded
	case treeLevelRemote:
		return page.providers[item.providerIdx].remotes[item.remoteIdx].expanded
	case treeLevelWorktree:
		return page.providers[item.providerIdx].remotes[item.remoteIdx].worktrees[item.worktreeIdx].expanded
	default:
		return true
	}
}
