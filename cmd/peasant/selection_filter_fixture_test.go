package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/selection_filter.yaml
var selectionFilterFixtureYAML []byte

type selectionFilterFixtures struct {
	DeclaredRows    int                      `yaml:"declared_rows"`
	Cases           []selectionFilterFixture `yaml:"cases"`
	MountedConflict mountedConflictFixture   `yaml:"mounted_conflict"`
	Overflow        overflowFixture          `yaml:"overflow"`
}

// overflowFixture drives the notice past its per-run cap. The summary line is a
// disclosure about how much was withheld, so an unpinned one can tell a user
// less than the truth about their own data.
type overflowFixture struct {
	DeclaredConflicts int             `yaml:"declared_conflicts"`
	ExpectedListed    int             `yaml:"expected_listed"`
	SummaryContains   overflowSummary `yaml:"summary_contains"`
}

type overflowSummary struct {
	Count  string `yaml:"count"`
	Reason string `yaml:"reason"`
	Remedy string `yaml:"remedy"`
}

func (s overflowSummary) axes() []testutil.FixtureField {
	return []testutil.FixtureField{
		{Key: "summary_contains.count", Value: s.Count},
		{Key: "summary_contains.reason", Value: s.Reason},
		{Key: "summary_contains.remedy", Value: s.Remedy},
	}
}

// mountedConflictFixture drives the real harvest command against a real
// repository, so the withheld-conflict disclosure is proven where the user reads
// it rather than only on the recorder that feeds it.
type mountedConflictFixture struct {
	ClaudeSlug      string         `yaml:"claude_slug"`
	SessionID       string         `yaml:"session_id"`
	GitRemote       string         `yaml:"git_remote"`
	AdmittedBranch  string         `yaml:"admitted_branch"`
	RejectedBranch  string         `yaml:"rejected_branch"`
	UnrelatedRemote string         `yaml:"unrelated_remote"`
	UnrelatedBranch string         `yaml:"unrelated_branch"`
	Notice          conflictNotice `yaml:"notice"`
}

// conflictNotice is the closed set of questions an actionable warning has to
// answer. Naming each axis instead of listing fragments means a dropped axis is
// a missing field rather than a shorter list nothing counts.
type conflictNotice struct {
	What  string `yaml:"what"`
	Why   string `yaml:"why"`
	Means string `yaml:"means"`
	Fix   string `yaml:"fix"`
}

// axes returns the notice fragments keyed by the question each one answers.
func (n conflictNotice) axes() []testutil.FixtureField {
	return []testutil.FixtureField{
		{Key: "notice.what", Value: n.What},
		{Key: "notice.why", Value: n.Why},
		{Key: "notice.means", Value: n.Means},
		{Key: "notice.fix", Value: n.Fix},
	}
}

// selectionGitExit is the closed set of ways discovery decides whether resolving
// git can change the answer. Each row DECLARES one and the loader DERIVES the
// same value from the row's configuration, so a row cannot carry the name of an
// exit it never reaches.
type selectionGitExit string

const (
	gitExitHarnessAbsent   selectionGitExit = "harness-absent"
	gitExitNoProjects      selectionGitExit = "no-project-entries"
	gitExitExplicitSession selectionGitExit = "explicit-session"
	gitExitResolves        selectionGitExit = "resolves"
)

type selectionFilterFixture struct {
	Name             string                    `yaml:"name"`
	Harness          ingest.Harness            `yaml:"harness"`
	Configured       string                    `yaml:"configured_harness"`
	Projects         []config.ProjectSelection `yaml:"projects"`
	ExplicitSessions []string                  `yaml:"explicit_sessions"`
	ProjectName      string                    `yaml:"project_name"`
	SessionID        string                    `yaml:"session_id"`
	SecondSessionID  string                    `yaml:"second_session_id"`
	OriginalRoot     string                    `yaml:"original_root"`
	GitExit          selectionGitExit          `yaml:"git_exit"`
	ExpectedMatch    testutil.SelectionOutcome `yaml:"expected_match"`
	ExpectedGitCalls int                       `yaml:"expected_git_calls"`
}

// derivedGitExit computes which exit this row's configuration reaches, from the
// configuration alone. It is compared against the declared git_exit so the
// declaration cannot drift onto a row that takes a different exit.
func (f selectionFilterFixture) derivedGitExit() selectionGitExit {
	switch {
	case f.Harness.String() != f.Configured:
		return gitExitHarnessAbsent
	case len(f.Projects) == 0:
		return gitExitNoProjects
	case slices.Contains(f.ExplicitSessions, f.SessionID):
		return gitExitExplicitSession
	default:
		return gitExitResolves
	}
}

// entryIdentity and entryBranchScope are the two independent axes on which
// ingest.SelectionEntry.String() branches. Every arm has to be exercised by a
// case that actually RENDERS an entry, or the warning's wording is only ever
// seen in one of the shapes a real configuration produces.
//
// Both are COMPUTED from the configured entry, never declared, so neither can
// be moved onto a row that does not have the shape.
type entryIdentity string

const (
	identityRemoteOnly    entryIdentity = "remote-only"
	identityNameOnly      entryIdentity = "name-only"
	identityRemoteAndName entryIdentity = "remote-and-name"
)

var allEntryIdentities = []entryIdentity{identityRemoteOnly, identityNameOnly, identityRemoteAndName}

type entryBranchScope string

const (
	branchScopeAll    entryBranchScope = "all-branches"
	branchScopePinned entryBranchScope = "pinned-branches"
)

var allEntryBranchScopes = []entryBranchScope{branchScopeAll, branchScopePinned}

func identityOf(project config.ProjectSelection) entryIdentity {
	switch {
	case project.GitRemote != "" && project.Name != "":
		return identityRemoteAndName
	case project.Name != "":
		return identityNameOnly
	default:
		return identityRemoteOnly
	}
}

func branchScopeOf(project config.ProjectSelection) entryBranchScope {
	if len(project.Branches) == 0 {
		return branchScopeAll
	}
	return branchScopePinned
}

// selectionFilterCoverage is the (exit, answer) pair a row asserts.
type selectionFilterCoverage struct {
	exit  selectionGitExit
	match testutil.SelectionOutcome
}

// allSelectionFilterCoverage enumerates every pair the corpus must exercise:
// each exit of the git short-circuit, and each answer the filter can give.
var allSelectionFilterCoverage = []selectionFilterCoverage{
	{gitExitHarnessAbsent, testutil.SelectionRejected},
	{gitExitNoProjects, testutil.SelectionSelected},
	{gitExitNoProjects, testutil.SelectionRejected},
	{gitExitExplicitSession, testutil.SelectionSelected},
	{gitExitResolves, testutil.SelectionSelected},
	{gitExitResolves, testutil.SelectionWithheld},
}

func loadSelectionFilterFixtures(t *testing.T) selectionFilterFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(selectionFilterFixtureYAML))
	decoder.KnownFields(true)
	var fixtures selectionFilterFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode selection filter fixture with strict fields: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("selection filter fixture must contain exactly one YAML document: %v", err)
	}
	// Floor EQUALS the row count: deleting a row and decrementing the
	// declaration still trips it, so the corpus only ratchets up. The pair
	// coverage below is the second layer, for a swap at the same count.
	if fixtures.DeclaredRows != len(fixtures.Cases) || fixtures.DeclaredRows < 8 {
		t.Fatalf("selection filter fixture row guard failed: declared=%d actual=%d minimum=8", fixtures.DeclaredRows, len(fixtures.Cases))
	}
	seen := make(map[string]struct{}, len(fixtures.Cases))
	observed := make([]selectionFilterCoverage, 0, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		testutil.RequireFixtureFields(t, "selection filter", fixture.Name, []testutil.FixtureField{
			{Key: "name", Value: fixture.Name},
			{Key: "harness", Value: fixture.Harness.String()},
			{Key: "configured_harness", Value: fixture.Configured},
			{Key: "session_id", Value: fixture.SessionID},
			{Key: "git_exit", Value: string(fixture.GitExit)},
			{Key: "expected_match", Value: string(fixture.ExpectedMatch)},
		})
		fixture.ExpectedMatch.BranchMatch(t, "selection filter", fixture.Name)
		if derived := fixture.derivedGitExit(); derived != fixture.GitExit {
			t.Fatalf("selection filter fixture %q declares git_exit %q but its configuration reaches %q; the row is named for an exit it does not take, so it proves something other than what it claims", fixture.Name, fixture.GitExit, derived)
		}
		if fixture.GitExit != gitExitResolves && fixture.ExpectedGitCalls != 0 {
			t.Fatalf("selection filter fixture %q short-circuits at %q but expects %d git call(s); a short-circuit that still resolves git is not a short-circuit", fixture.Name, fixture.GitExit, fixture.ExpectedGitCalls)
		}
		observed = append(observed, selectionFilterCoverage{fixture.GitExit, fixture.ExpectedMatch})
		if _, exists := seen[fixture.Name]; exists {
			t.Fatalf("selection filter fixture name %q is duplicated; every case must name exactly one scenario", fixture.Name)
		}
		seen[fixture.Name] = struct{}{}
	}
	// Derived coverage instead of a row-count floor: a floor is decremented by
	// the same edit that removes the row it protected, while an uncovered
	// (exit, answer) pair is reported by name.
	testutil.RequireClosedSetCoverage(t, "selection filter", "git_exit/expected_match pair", allSelectionFilterCoverage, observed)

	// Coverage over the shapes of entries that are actually RENDERED. Scoped to
	// withheld rows and the mounted block on purpose: an entry on a row that
	// never produces a warning would satisfy a coverage count while exercising
	// none of the rendering it is supposed to stand for.
	var renderedIdentities []entryIdentity
	var renderedScopes []entryBranchScope
	for _, fixture := range fixtures.Cases {
		if fixture.ExpectedMatch != testutil.SelectionWithheld {
			continue
		}
		for _, project := range fixture.Projects {
			renderedIdentities = append(renderedIdentities, identityOf(project))
			renderedScopes = append(renderedScopes, branchScopeOf(project))
		}
	}
	testutil.RequireClosedSetCoverage(t, "selection filter", "rendered entry identity", allEntryIdentities, renderedIdentities)
	testutil.RequireClosedSetCoverage(t, "selection filter", "rendered entry branch scope", allEntryBranchScopes, renderedScopes)

	// Two entries in one case must not be confusable in the rendered warning,
	// because every assertion about one of them runs against text that contains
	// the others.
	//
	// This guard previously compared a name only against remotes. That FORCED a
	// rename — and the rename collided two entries on the same name, which the
	// guard could not see, so a dropped rendering field went green. A guard that
	// constrains fixture values has to reject every collision it can push an
	// author into, not only the one it was written for.
	for _, fixture := range fixtures.Cases {
		identities := map[string]string{}
		for _, project := range fixture.Projects {
			for label, value := range map[string]string{"gitRemote": project.GitRemote, "name": project.Name} {
				if value == "" {
					continue
				}
				token := fmt.Sprintf("%s %q", label, value)
				if _, duplicated := identities[token]; duplicated {
					t.Fatalf("selection filter fixture %q has two entries that both render %s; each entry's assertion would then be satisfiable by the other, so give them distinct identities", fixture.Name, token)
				}
				identities[token] = fixture.Name
			}
			if project.Name == "" {
				continue
			}
			for _, other := range fixture.Projects {
				if other.GitRemote != "" && strings.Contains(other.GitRemote, project.Name) {
					t.Fatalf("selection filter fixture %q configures the name %q, which appears inside the remote %q in the same case; an assertion about that name can be satisfied by that URL — pick a name that appears in no remote here", fixture.Name, project.Name, other.GitRemote)
				}
			}
		}
		// The per-entry assertion splits the notice on the renderer's own
		// separator, so a value containing it would break that boundary and
		// quietly restore the whole-notice search this replaced.
		for _, project := range fixture.Projects {
			for _, value := range append([]string{project.GitRemote, project.Name}, project.Branches...) {
				if strings.Contains(value, entrySeparator) {
					t.Fatalf("selection filter fixture %q configures the value %q, which contains the separator %q the warning joins entries with; that would merge two entries into one segment and undo the per-entry scoping", fixture.Name, value, entrySeparator)
				}
			}
		}
	}

	mounted := fixtures.MountedConflict
	testutil.RequireFixtureFields(t, "selection filter", "mounted_conflict", []testutil.FixtureField{
		{Key: "mounted_conflict.claude_slug", Value: mounted.ClaudeSlug},
		{Key: "mounted_conflict.session_id", Value: mounted.SessionID},
		{Key: "mounted_conflict.git_remote", Value: mounted.GitRemote},
		{Key: "mounted_conflict.admitted_branch", Value: mounted.AdmittedBranch},
		{Key: "mounted_conflict.rejected_branch", Value: mounted.RejectedBranch},
		{Key: "mounted_conflict.unrelated_remote", Value: mounted.UnrelatedRemote},
		{Key: "mounted_conflict.unrelated_branch", Value: mounted.UnrelatedBranch},
	})
	if mounted.AdmittedBranch == mounted.RejectedBranch {
		t.Fatalf("mounted_conflict needs two different branches to disagree about; both are %q", mounted.AdmittedBranch)
	}
	if mounted.UnrelatedRemote == mounted.GitRemote {
		t.Fatalf("mounted_conflict.unrelated_remote %q is the conflicting remote; the case cannot tell 'named only the disagreeing entries' from 'named every entry' unless the third entry is a different project", mounted.UnrelatedRemote)
	}
	// Each axis is required by name, so dropping one is a blank field rather
	// than a shorter list that nothing counts.
	testutil.RequireFixtureFields(t, "selection filter", "mounted_conflict.notice", mounted.Notice.axes())
	fragments := make(map[string]string, 4)
	for _, axis := range mounted.Notice.axes() {
		if previous, duplicated := fragments[axis.Value]; duplicated {
			t.Fatalf("mounted_conflict.notice reuses the fragment %q for both %s and %s; each axis must pin its own wording or one of them asserts nothing", axis.Value, previous, axis.Key)
		}
		fragments[axis.Value] = axis.Key
	}
	overflow := fixtures.Overflow
	// Without this, deleting an axis leaves an empty needle and
	// strings.Contains against "" is always true — the exact vacuity that makes
	// a disclosure assertion pass while disclosing nothing.
	testutil.RequireFixtureFields(t, "selection filter", "overflow", overflow.SummaryContains.axes())
	if overflow.DeclaredConflicts <= overflow.ExpectedListed {
		t.Fatalf("overflow.declared_conflicts (%d) must exceed overflow.expected_listed (%d), or the summary arm is never reached and the case asserts nothing about it", overflow.DeclaredConflicts, overflow.ExpectedListed)
	}
	// The declared remainder has to FOLLOW from the case, not be asserted
	// beside it: a count that does not follow would pass while the notice
	// reported the wrong number of withheld sessions.
	remainder := fmt.Sprintf("%d additional", overflow.DeclaredConflicts-overflow.ExpectedListed)
	if !strings.HasPrefix(overflow.SummaryContains.Count, remainder) {
		t.Fatalf("overflow.summary_contains.count is %q but this case withholds %d beyond the cap, so it must start with %q", overflow.SummaryContains.Count, overflow.DeclaredConflicts-overflow.ExpectedListed, remainder)
	}
	return fixtures
}

type countingGitResolver struct {
	*testutil.StubGitResolver
	remoteCalls int
	branchCalls int
}

func (r *countingGitResolver) RemoteURL(ctx context.Context, dir string) (string, error) {
	r.remoteCalls++
	return r.StubGitResolver.RemoteURL(ctx, dir)
}

func (r *countingGitResolver) Branch(ctx context.Context, dir string) (string, error) {
	r.branchCalls++
	return r.StubGitResolver.Branch(ctx, dir)
}

func TestBuildSelectionFilter_Fixtures(t *testing.T) {
	for _, fixture := range loadSelectionFilterFixtures(t).Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			cfg := config.BaseConfig()
			cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{
				fixture.Configured: {Projects: fixture.Projects, Sessions: fixture.ExplicitSessions},
			}}
			git := &countingGitResolver{StubGitResolver: &testutil.StubGitResolver{Remote: "https://github.com/acme/tool.git", BranchName: "main"}}
			filter, recorder := buildSelectionFilterWithRecorder(cfg, git)
			sessionID, err := ingest.NewSessionID(fixture.SessionID)
			if err != nil {
				t.Fatalf("fixture session ID: %v", err)
			}
			session := ingest.DiscoveredSession{Harness: fixture.Harness, SessionID: sessionID, ProjectName: fixture.ProjectName, OriginalRoot: ingest.ResolvedPath(fixture.OriginalRoot)}
			wantSelected := fixture.ExpectedMatch == testutil.SelectionSelected
			if got := filter(session); got != wantSelected {
				t.Fatalf("selection result = %v, want %v (expected_match %q)", got, wantSelected, fixture.ExpectedMatch)
			}
			if fixture.SecondSessionID != "" {
				secondID, idErr := ingest.NewSessionID(fixture.SecondSessionID)
				if idErr != nil {
					t.Fatalf("fixture second session ID: %v", idErr)
				}
				session.SessionID = secondID
				if got := filter(session); got != wantSelected {
					t.Fatalf("second selection result = %v, want %v", got, wantSelected)
				}
			}
			if got := git.remoteCalls + git.branchCalls; got != fixture.ExpectedGitCalls {
				t.Fatalf("git calls = %d, want %d (this row reaches the %q exit)", got, fixture.ExpectedGitCalls, fixture.GitExit)
			}
			var notice bytes.Buffer
			recorder.notice(&notice, "/config/chosen.yaml")
			if fixture.ExpectedMatch != testutil.SelectionWithheld {
				if notice.Len() != 0 {
					t.Fatalf("case expects no withheld conflict but one was reported: %s", notice.String())
				}
				return
			}
			// The warning must name the disagreeing entries by the identity the
			// user wrote, and the branch rules that disagree.
			// The harness is part of WHICH session was withheld: a user running
			// several agents needs it to know where to look, and it was the one
			// field of this line nothing pinned.
			for _, needle := range []string{"withheld", fixture.SessionID, fixture.Harness.String(), "/config/chosen.yaml"} {
				if !strings.Contains(notice.String(), needle) {
					t.Fatalf("conflict notice does not name %q: %s", needle, notice.String())
				}
			}
			// The warning renders the disagreeing entries into ONE string, so an
			// assertion searching the whole notice can be satisfied by a
			// SIBLING's rendering. Asserting the labelled, quoted form defeats
			// accidental matches from unrelated text but NOT from a sibling
			// rendering the same token — that is how dropping the name from the
			// both-fields arm went unnoticed once. So each entry is checked
			// against ONE segment of the notice: every field the user wrote for
			// an entry has to land together, in the same segment, which no other
			// entry can supply.
			segments := strings.Split(notice.String(), entrySeparator)
			for _, project := range fixture.Projects {
				var required []string
				for _, field := range []struct{ label, value string }{
					{"gitRemote", project.GitRemote},
					{"name", project.Name},
				} {
					if field.value != "" {
						required = append(required, fmt.Sprintf("%s %q", field.label, field.value))
					}
				}
				if !slices.ContainsFunc(segments, func(segment string) bool {
					for _, needle := range required {
						if !strings.Contains(segment, needle) {
							return false
						}
					}
					return true
				}) {
					t.Fatalf("no single part of the conflict notice carries this entry's whole identity %v — every field the user wrote has to appear together, or they cannot tell which configured entry the warning means: %s", required, notice.String())
				}
				// The branch RULE is what disagrees, so an unrestricted entry
				// has to say so rather than render as a bare identity — that is
				// the difference between "this one allows everything" and "this
				// one has rules I cannot see".
				// Scoped to the entry's own segment for the same reason the
				// identity is: the notice's own prose says
				// `disagree on branch "main"`, so searching the whole string
				// lets an entry whose branch list IS the disagreeing branch be
				// satisfied by text no entry produced.
				branchNeedle := "all branches"
				if len(project.Branches) > 0 {
					branchNeedle = fmt.Sprintf("branches %v", project.Branches)
				}
				if !slices.ContainsFunc(segments, func(segment string) bool {
					for _, needle := range append(required, branchNeedle) {
						if !strings.Contains(segment, needle) {
							return false
						}
					}
					return true
				}) {
					t.Fatalf("no single part of the conflict notice carries this entry's identity %v together with its branch rule %q — the branch rule is what disagrees, so it has to be attached to the entry it belongs to: %s", required, branchNeedle, notice.String())
				}
			}
		})
	}
}

// TestSelectionConflictNotice_CapsTheListAndSaysHowManyItHeldBack pins the
// overflow arm. Past the cap the notice stops listing sessions and summarises
// the rest — and that summary is the only place a user learns that MORE of
// their work was withheld than the lines they can see. A cap that quietly
// under-reports, or a summary that names the wrong number, tells them less than
// the truth about their own data.
func TestSelectionConflictNotice_CapsTheListAndSaysHowManyItHeldBack(t *testing.T) {
	fixtures := loadSelectionFilterFixtures(t)
	overflow := fixtures.Overflow

	cfg := config.BaseConfig()
	cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{
		defaults.HarnessClaudeCode.String(): {Projects: []config.ProjectSelection{
			{GitRemote: "https://github.com/acme/tool.git", Branches: []string{"main"}},
			{Name: "satchel", Branches: []string{"other"}},
		}},
	}}
	git := &countingGitResolver{StubGitResolver: &testutil.StubGitResolver{Remote: "https://github.com/acme/tool.git", BranchName: "main"}}
	filter, recorder := buildSelectionFilterWithRecorder(cfg, git)
	for i := 0; i < overflow.DeclaredConflicts; i++ {
		sessionID, err := ingest.NewSessionID(fmt.Sprintf("%08d-1111-4111-8111-111111111111", i))
		if err != nil {
			t.Fatalf("build conflicting session %d: %v", i, err)
		}
		if filter(ingest.DiscoveredSession{
			Harness:      defaults.HarnessClaudeCode,
			SessionID:    sessionID,
			ProjectName:  "satchel",
			OriginalRoot: ingest.ResolvedPath("/workspace/tool"),
		}) {
			t.Fatalf("session %d was selected; the fixture must configure a conflict for every one of them", i)
		}
	}
	if len(recorder.conflicts) != overflow.DeclaredConflicts {
		t.Fatalf("recorded %d conflicts, want %d; the case cannot exercise the cap unless every session actually conflicts", len(recorder.conflicts), overflow.DeclaredConflicts)
	}

	var notice bytes.Buffer
	recorder.notice(&notice, "/config/chosen.yaml")
	listed := strings.Count(notice.String(), "was withheld during ingest")
	if listed != overflow.ExpectedListed {
		t.Errorf("the notice lists %d sessions individually, want %d; a cap that lists a different number than it claims to is the one thing this arm exists to get right", listed, overflow.ExpectedListed)
	}
	for _, axis := range overflow.SummaryContains.axes() {
		if !strings.Contains(notice.String(), axis.Value) {
			t.Errorf("the overflow summary does not answer %s (%q); a user told nothing about the remainder does not know how much was withheld:\n%s", axis.Key, axis.Value, notice.String())
		}
	}
	if !strings.Contains(notice.String(), "/config/chosen.yaml") {
		t.Errorf("the overflow summary does not name the config file to edit:\n%s", notice.String())
	}
}

// TestHarvestCmd_ReportsWithheldSelectionConflict proves the disclosure on the
// mounted path: the real harvest command, a real git repository, and a real
// config file written by the production writer. Asserting only on the recorder
// leaves the notice reachable in tests while unreachable for the user.
func TestHarvestCmd_ReportsWithheldSelectionConflict(t *testing.T) {
	fixture := loadSelectionFilterFixtures(t).MountedConflict
	dir := t.TempDir()
	sourceDir := initConflictedSourceRepo(t, fixture)

	configPath := filepath.Join(dir, string(defaults.Config.FileName))
	cfg := config.BaseConfig()
	for _, harness := range defaults.AllHarnesses {
		if source, ok := cfg.Sources.Provider(harness); ok {
			source.Enabled = harness == defaults.HarnessClaudeCode
		}
	}
	cfg.Sources.ClaudeCode.Paths = []string{sourceDir}
	cfg.Selection = config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			defaults.HarnessClaudeCode.String(): {Projects: []config.ProjectSelection{
				{GitRemote: fixture.GitRemote, Branches: []string{fixture.AdmittedBranch}},
				{GitRemote: fixture.GitRemote, Branches: []string{fixture.RejectedBranch}},
				// Configured for the same harness, but for another project, so
				// the warning has something it must NOT name.
				{GitRemote: fixture.UnrelatedRemote, Branches: []string{fixture.UnrelatedBranch}},
			}},
		},
	}
	if err := config.SaveAtomic(configPath, cfg); err != nil {
		t.Fatalf("write conflicted selection config: %v", err)
	}

	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	root.AddCommand(BuildHarvestCommand())
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	// --include-active keeps the just-written session out of the debounce skip so
	// it reaches the selection filter. --all is deliberately NOT used because it
	// clears the selection filter this test exists to exercise, and --dry-run is
	// not used because the dry-run preview returns before the filter stage runs.
	outputDir := t.TempDir()
	root.SetArgs([]string{"harvest", "--include-active", "--output", outputDir, "--data-dir", dir, "--config-dir", dir, "--config", configPath})
	if err := root.Execute(); err != nil {
		t.Fatalf("harvest with a conflicted selection: %v\n%s", err, buf.String())
	}

	output := buf.String()
	for _, axis := range fixture.Notice.axes() {
		if !strings.Contains(output, axis.Value) {
			t.Errorf("harvest output does not answer %s (%q), so the warning is not actionable:\n%s", axis.Key, axis.Value, output)
		}
	}
	for _, named := range []struct{ what, value string }{
		{what: "the withheld session", value: fixture.SessionID},
		{what: "the branch the entries disagree about", value: fixture.AdmittedBranch},
		{what: "the branch rule that rejects it", value: fixture.RejectedBranch},
		{what: "the project both disagreeing entries identify", value: fixture.GitRemote},
		{what: "the config file to edit", value: configPath},
	} {
		if !strings.Contains(output, named.value) {
			t.Errorf("harvest output does not name %s (%q):\n%s", named.what, named.value, output)
		}
	}
	// The other direction, and the one that inverts with scale: a user with
	// twenty configured projects and two that disagree must be shown the two.
	if strings.Contains(output, fixture.UnrelatedRemote) {
		t.Errorf("harvest output names %q, a configured entry that takes no part in the disagreement; the warning must name the entries that conflict, not every entry the harness carries:\n%s", fixture.UnrelatedRemote, output)
	}

	// Withheld means withheld: the disclosure is not a substitute for the session
	// staying out of the store's output tree.
	if err := filepath.WalkDir(outputDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(path, fixture.SessionID) {
			return fmt.Errorf("withheld session was written to %q", path)
		}
		return nil
	}); err != nil {
		t.Errorf("withheld session must not be ingested: %v", err)
	}
}

// initConflictedSourceRepo builds a Claude harness root that is itself a git
// repository carrying the fixture's remote and branch, holding one session under
// a project slug that decodes to nothing — so the discovery filter resolves git
// against the harness root rather than a decoded project path.
func initConflictedSourceRepo(t *testing.T, fixture mountedConflictFixture) string {
	t.Helper()
	sourceDir := t.TempDir()
	if decoded := decodeClaudeSlugToPath(fixture.ClaudeSlug); decoded != "" {
		t.Fatalf("fixture claude_slug %q decodes to an existing directory %q; pick a slug that cannot resolve", fixture.ClaudeSlug, decoded)
	}
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "selection@example.test"},
		{"git", "config", "user.name", "Selection Fixture"},
		{"git", "config", "commit.gpgsign", "false"},
		{"git", "remote", "add", "origin", fixture.GitRemote},
		{"git", "commit", "--allow-empty", "-m", "init"},
		{"git", "branch", "-M", fixture.AdmittedBranch},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = sourceDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git setup %v: %v\n%s", args, err, out)
		}
	}

	sessionFile := filepath.Join(sourceDir, fixture.ClaudeSlug, fixture.SessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o755); err != nil {
		t.Fatalf("create claude project slug directory: %v", err)
	}
	line := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"hi"},"uuid":%q,"sessionId":%q,"timestamp":"2026-05-30T04:29:24.992Z","cwd":"/tmp/conflicted","gitBranch":%q,"version":"1.0","userType":"external"}`,
		fixture.SessionID, fixture.SessionID, fixture.AdmittedBranch)
	if err := os.WriteFile(sessionFile, []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("write claude session fixture: %v", err)
	}
	return sourceDir
}
