package config_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/selection_policy.yaml
var selectionPolicyFixtureYAML []byte

type selectionPolicyFixtures struct {
	DeclaredRows int                      `yaml:"declared_rows"`
	Cases        []selectionPolicyFixture `yaml:"cases"`
}

type selectionPolicyFixture struct {
	Name               string                  `yaml:"name"`
	Harness            string                  `yaml:"harness"`
	ConfiguredHarness  string                  `yaml:"configured_harness"`
	Mode               config.SelectionMode    `yaml:"mode"`
	ConfiguredProjects []selectionProject      `yaml:"configured_projects"`
	ExplicitSessions   []string                `yaml:"explicit_sessions"`
	AutoNewBranches    bool                    `yaml:"auto_new_branches"`
	CandidateRemote    string                  `yaml:"candidate_remote"`
	CandidateProject   string                  `yaml:"candidate_project"`
	CandidateBranch    string                  `yaml:"candidate_branch"`
	CandidateSession   string                  `yaml:"candidate_session"`
	ExpectedDiscovery  selectionExpectedResult `yaml:"expected_discovery"`
}

type selectionProject struct {
	GitRemote string   `yaml:"git_remote"`
	Name      string   `yaml:"name"`
	Branches  []string `yaml:"branches"`
}

type selectionExpectedResult string

const (
	selectionExpectedSelected selectionExpectedResult = "selected"
	selectionExpectedRejected selectionExpectedResult = "rejected"
	selectionExpectedWithheld selectionExpectedResult = "withheld"
)

func loadSelectionPolicyFixtures(t *testing.T) selectionPolicyFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(selectionPolicyFixtureYAML))
	decoder.KnownFields(true)
	var fixtures selectionPolicyFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode selection policy fixture with strict fields: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("selection policy fixture must contain exactly one YAML document: %v", err)
	}
	if fixtures.DeclaredRows != len(fixtures.Cases) || fixtures.DeclaredRows < 13 {
		t.Fatalf("selection policy fixture row guard failed: declared=%d actual=%d minimum=13", fixtures.DeclaredRows, len(fixtures.Cases))
	}
	seen := make(map[string]struct{}, len(fixtures.Cases))
	observedOutcomes := make([]testutil.SelectionOutcome, 0, len(fixtures.Cases))
	observedPairs := make([]policyCoverage, 0, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		observedOutcomes = append(observedOutcomes, testutil.SelectionOutcome(fixture.ExpectedDiscovery))
		observedPairs = append(observedPairs, policyCoverage{fixture.entryShape(), testutil.SelectionOutcome(fixture.ExpectedDiscovery)})
		testutil.RequireFixtureFields(t, "selection policy", fixture.Name, []testutil.FixtureField{
			{Key: "name", Value: fixture.Name},
			{Key: "harness", Value: fixture.Harness},
			{Key: "candidate_session", Value: fixture.CandidateSession},
			{Key: "expected_discovery", Value: string(fixture.ExpectedDiscovery)},
		})
		if fixture.CandidateRemote == "" && fixture.CandidateProject == "" && len(fixture.ExplicitSessions) == 0 {
			t.Fatalf("selection policy fixture %q gives the candidate neither a remote nor a project name and lists no explicit session; nothing can identify it", fixture.Name)
		}
		if _, exists := seen[fixture.Name]; exists {
			t.Fatalf("selection policy fixture name %q is duplicated", fixture.Name)
		}
		seen[fixture.Name] = struct{}{}
		switch fixture.ExpectedDiscovery {
		case selectionExpectedSelected, selectionExpectedRejected, selectionExpectedWithheld:
		default:
			t.Fatalf("selection policy fixture %q has unknown expected_discovery %q; use selected, rejected, or withheld", fixture.Name, fixture.ExpectedDiscovery)
		}
	}
	// Layer two, beside the floor above. Several behaviours in this corpus have
	// exactly one row — the only withheld outcome, the only mode-all case, the
	// only normalisation case — and losing one is a two-integer edit that reads
	// as a data change. The floor catches the count dropping; these catch the
	// behaviour disappearing, including when a row is swapped at the same count.
	testutil.RequireClosedSetCoverage(t, "selection policy", "expected_discovery", testutil.AllSelectionOutcomes, observedOutcomes)
	testutil.RequireClosedSetCoverage(t, "selection policy", "entry shape/outcome pair", allPolicyCoverage, observedPairs)

	// The pair list is written by hand, so it can fall behind the shapes. A
	// shape with no pair is a configuration the corpus would never be required
	// to exercise — the coverage guard above would pass while saying nothing
	// about it. This is what keeps the enumeration honest as shapes are added.
	pairedShapes := make([]policyEntryShape, 0, len(allPolicyCoverage))
	for _, pair := range allPolicyCoverage {
		pairedShapes = append(pairedShapes, pair.shape)
	}
	testutil.RequireClosedSetCoverage(t, "selection policy", "entry shape named by allPolicyCoverage", allPolicyEntryShapes, pairedShapes)
	return fixtures
}

// policyEntryShape is the closed set of ways a case configures the matcher. It
// is COMPUTED from the row, never declared, so it cannot be moved onto a row
// that does not have the shape.
type policyEntryShape string

const (
	policyShapeUnrestrictedMode policyEntryShape = "mode-all"
	policyShapeHarnessMismatch  policyEntryShape = "harness-mismatch"
	policyShapeExplicitSessions policyEntryShape = "explicit-sessions"
	policyShapeMultipleEntries  policyEntryShape = "multiple-entries"
	// Branch pinning splits on whether the candidate's branch is KNOWN, because
	// that is the whole discovery/stored-row distinction: an unknown branch
	// cannot safely pass an explicit allowlist during discovery.
	policyShapePinnedKnownBranch   policyEntryShape = "branch-pinned-known-branch"
	policyShapePinnedUnknownBranch policyEntryShape = "branch-pinned-unknown-branch"
	policyShapeRemoteAndName       policyEntryShape = "remote-and-name"
	policyShapeNameOnly            policyEntryShape = "name-only"
	policyShapeRemoteOnly          policyEntryShape = "remote-only"
)

var allPolicyEntryShapes = []policyEntryShape{
	policyShapeUnrestrictedMode, policyShapeHarnessMismatch, policyShapeExplicitSessions,
	policyShapeMultipleEntries, policyShapePinnedKnownBranch, policyShapePinnedUnknownBranch,
	policyShapeRemoteAndName, policyShapeNameOnly, policyShapeRemoteOnly,
}

func (f selectionPolicyFixture) entryShape() policyEntryShape {
	single := len(f.ConfiguredProjects) == 1
	switch {
	case f.Mode == config.SelectionModeAll:
		return policyShapeUnrestrictedMode
	case f.ConfiguredHarness != "" && f.ConfiguredHarness != f.Harness:
		return policyShapeHarnessMismatch
	case len(f.ExplicitSessions) > 0:
		return policyShapeExplicitSessions
	case len(f.ConfiguredProjects) > 1:
		return policyShapeMultipleEntries
	case single && len(f.ConfiguredProjects[0].Branches) > 0 && f.CandidateBranch != "":
		return policyShapePinnedKnownBranch
	case single && len(f.ConfiguredProjects[0].Branches) > 0:
		return policyShapePinnedUnknownBranch
	case single && f.ConfiguredProjects[0].Name != "" && f.ConfiguredProjects[0].GitRemote != "":
		return policyShapeRemoteAndName
	case single && f.ConfiguredProjects[0].Name != "":
		return policyShapeNameOnly
	default:
		return policyShapeRemoteOnly
	}
}

// policyCoverage is the (shape, outcome) pair a row asserts — the same idiom the
// selection-filter corpus uses. The pair is what makes most rows individually
// undeletable: sharing a shape is fine as long as the outcomes differ.
type policyCoverage struct {
	shape   policyEntryShape
	outcome testutil.SelectionOutcome
}

// allPolicyCoverage enumerates the pairs this corpus must exercise. It does NOT
// enumerate one entry per row: two rows legitimately share
// (remote-and-name, selected) because they differ in which candidate field did
// the matching, and separating those would mean re-deriving the matcher's own
// normalisation here — the duplication this slice exists to remove. Those two
// stay protected by the row-count floor, which is the layer for exactly that.
var allPolicyCoverage = []policyCoverage{
	{policyShapeUnrestrictedMode, testutil.SelectionSelected},
	{policyShapeHarnessMismatch, testutil.SelectionRejected},
	{policyShapeExplicitSessions, testutil.SelectionSelected},
	{policyShapeExplicitSessions, testutil.SelectionRejected},
	{policyShapeMultipleEntries, testutil.SelectionWithheld},
	{policyShapePinnedKnownBranch, testutil.SelectionSelected},
	{policyShapePinnedKnownBranch, testutil.SelectionRejected},
	{policyShapePinnedUnknownBranch, testutil.SelectionRejected},
	{policyShapeRemoteAndName, testutil.SelectionSelected},
	{policyShapeNameOnly, testutil.SelectionSelected},
	{policyShapeNameOnly, testutil.SelectionRejected},
	{policyShapeRemoteOnly, testutil.SelectionSelected},
}

func TestSelectionMatcher_DiscoveryFixtures(t *testing.T) {
	fixtures := loadSelectionPolicyFixtures(t)
	for _, fixture := range fixtures.Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			configuredHarness := fixture.ConfiguredHarness
			if configuredHarness == "" {
				configuredHarness = fixture.Harness
			}
			projects := make([]config.ProjectSelection, len(fixture.ConfiguredProjects))
			for i, project := range fixture.ConfiguredProjects {
				projects[i] = config.ProjectSelection{GitRemote: project.GitRemote, Name: project.Name, Branches: project.Branches}
			}
			cfg := config.BaseConfig()
			mode := fixture.Mode
			if mode == "" {
				mode = config.SelectionModeSelected
			}
			cfg.Selection = config.SelectionConfig{
				Mode:                  mode,
				AutoIngestNewBranches: fixture.AutoNewBranches,
				Harnesses: map[string]config.SelectionHarnessConfig{
					configuredHarness: {
						Projects: projects,
						Sessions: fixture.ExplicitSessions,
					},
				},
			}
			sessionID, err := ingest.NewSessionID(fixture.CandidateSession)
			if err != nil {
				t.Fatalf("fixture candidate session ID: %v", err)
			}
			got := cfg.SelectionMatcher().MatchDiscovery(
				ingest.Harness(fixture.Harness), fixture.CandidateRemote, fixture.CandidateProject, fixture.CandidateBranch, sessionID, fixture.AutoNewBranches,
			)
			want := ingest.BranchMatchNo
			switch fixture.ExpectedDiscovery {
			case selectionExpectedSelected:
				want = ingest.BranchMatchYes
			case selectionExpectedWithheld:
				want = ingest.BranchMatchWithheldConflict
			}
			if got != want {
				t.Fatalf("discovery match = %v, want %v", got, want)
			}

			// The decision carries the entries behind the answer, and
			// Conflicting() is documented as empty for every answer except a
			// withheld conflict. Every consumer reads it to decide whether to
			// name entries to a user, so the emptiness is a contract rather
			// than an implementation detail — asserted here across all three
			// answers, which this corpus is guaranteed to cover.
			decision := cfg.SelectionMatcher().MatchDiscoveryDecision(
				ingest.Harness(fixture.Harness), fixture.CandidateRemote, fixture.CandidateProject, fixture.CandidateBranch, sessionID, fixture.AutoNewBranches,
			)
			if decision.Match != want {
				t.Fatalf("MatchDiscoveryDecision answered %v but MatchDiscovery answered %v; they must be the same call", decision.Match, want)
			}
			conflicting := decision.Conflicting()
			if want != ingest.BranchMatchWithheldConflict {
				if len(conflicting) != 0 {
					t.Fatalf("a %v decision reported %d conflicting entries: %v; naming entries for an answer that holds no disagreement would tell a user two rules clash when none do", want, len(conflicting), conflicting)
				}
				return
			}
			if len(decision.Admitting) == 0 || len(decision.Rejecting) == 0 {
				t.Fatalf("a withheld conflict must have entries on both sides; admitting=%v rejecting=%v", decision.Admitting, decision.Rejecting)
			}
			if len(conflicting) != len(decision.Admitting)+len(decision.Rejecting) {
				t.Fatalf("Conflicting() returned %d entries but the decision holds %d admitting and %d rejecting; the disagreeing set must be all of them and nothing else", len(conflicting), len(decision.Admitting), len(decision.Rejecting))
			}
			for _, entry := range conflicting {
				if !slices.ContainsFunc(fixture.ConfiguredProjects, func(project selectionProject) bool {
					return project.GitRemote == entry.GitRemote && project.Name == entry.Name
				}) {
					t.Fatalf("Conflicting() named %v, which is not one of the entries this case configured (%v); the warning must quote the user's own configuration", entry, fixture.ConfiguredProjects)
				}
			}
		})
	}
}

func TestSaveAtomic_PreservesLoadedConfigAtExactPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "custom", "chosen.yaml")
	cfg := config.BaseConfig()
	cfg.User.Email = "person@example.test"
	cfg.Output.BasePath = "/custom/output"
	cfg.Village.URL = "https://village.example.test"
	cfg.Selection.Mode = config.SelectionModeSelected

	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read exact config path: %v", err)
	}
	loaded, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse atomically saved config: %v", err)
	}
	if loaded.User.Email != cfg.User.Email || loaded.Output.BasePath != cfg.Output.BasePath || loaded.Village.URL != cfg.Village.URL {
		t.Fatalf("unrelated loaded config fields were not preserved: got=%+v", loaded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat exact config path: %v", err)
	}
	if info.Mode().Perm() != defaults.PublicFilePerm {
		t.Fatalf("config mode = %o, want %o", info.Mode().Perm(), defaults.PublicFilePerm)
	}
}

func TestSaveAtomic_RenameFailurePreservesDestination(t *testing.T) {
	t.Parallel()
	destination := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.Mkdir(destination, defaults.PublicDirPerm); err != nil {
		t.Fatalf("create destination directory: %v", err)
	}
	err := config.SaveAtomic(destination, config.BaseConfig())
	if err == nil {
		t.Fatal("SaveAtomic unexpectedly replaced a directory")
	}
	info, statErr := os.Stat(destination)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("failed atomic save changed destination: info=%v err=%v", info, statErr)
	}

	// A rename failure is the one failure that happens AFTER the temporary file
	// exists, so it is the only place the cleanup path can be observed without
	// a filesystem seam. The sibling validation test cannot reach it: validation
	// runs before the temp file is created, so its residue check passes whether
	// or not cleanup works.
	//
	// Leftover files matter here specifically because they land BESIDE the
	// user's config, named like config, in a directory they read by hand.
	entries, readErr := os.ReadDir(filepath.Dir(destination))
	if readErr != nil {
		t.Fatalf("read config directory after the failed replacement: %v", readErr)
	}
	for _, entry := range entries {
		if matched, _ := filepath.Match(".config-*.yaml.tmp", entry.Name()); matched {
			t.Fatalf("the failed replacement left the temporary file %q beside the user's configuration; a partial write must not survive the failure that caused it", entry.Name())
		}
	}
}

func TestSaveAtomic_ValidatesBeforeReplacementAndRenames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	initial := config.BaseConfig()
	initial.User.Email = "before@example.test"
	if err := config.SaveAtomic(path, initial); err != nil {
		t.Fatalf("seed atomic config: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat seeded config: %v", err)
	}
	beforeStat, supported := beforeInfo.Sys().(*syscall.Stat_t)
	if !supported {
		t.Skip("inode replacement assertion is unsupported on this platform")
	}

	invalid := config.BaseConfig()
	invalid.Push.Method = config.PushMethod("invalid")
	if err := config.SaveAtomic(path, invalid); err == nil {
		t.Fatal("SaveAtomic accepted a config that cannot pass Parse validation")
	}
	afterInvalid, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(afterInvalid, before) {
		t.Fatalf("validation failure changed destination: equal=%v err=%v", bytes.Equal(afterInvalid, before), err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read config directory after validation failure: %v", err)
	}
	for _, entry := range entries {
		if matched, _ := filepath.Match(".config-*.yaml.tmp", entry.Name()); matched {
			t.Fatalf("validation failure left temporary file %q", entry.Name())
		}
	}

	replacement := config.BaseConfig()
	replacement.User.Email = "after@example.test"
	if err := config.SaveAtomic(path, replacement); err != nil {
		t.Fatalf("atomically replace valid config: %v", err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat replaced config: %v", err)
	}
	afterStat := afterInfo.Sys().(*syscall.Stat_t)
	if beforeStat.Ino == afterStat.Ino {
		t.Fatal("config inode did not change; SaveAtomic must replace by rename, not truncate in place")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read replaced config: %v", err)
	}
	parsed, err := config.Parse(data)
	if err != nil || parsed.User.Email != replacement.User.Email {
		t.Fatalf("replacement config is invalid or stale: config=%+v err=%v", parsed, err)
	}
}
