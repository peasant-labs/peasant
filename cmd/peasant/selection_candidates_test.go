package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/selection_candidate_cohorts.yaml
var selectionCandidateCohortYAML []byte

type candidateMultiplicityFixture string

const (
	candidateMultiplicityUnique    candidateMultiplicityFixture = "unique"
	candidateMultiplicityAmbiguous candidateMultiplicityFixture = "ambiguous"
)

var allCandidateMultiplicities = []candidateMultiplicityFixture{
	candidateMultiplicityUnique,
	candidateMultiplicityAmbiguous,
}

type selectionCandidateFixtureDocument struct {
	DeclaredCases int                             `yaml:"declared_cases"`
	DeclaredRows  int                             `yaml:"declared_rows"`
	Cases         []selectionCandidateFixtureCase `yaml:"cases"`
}

type selectionCandidateFixtureCase struct {
	Name             string                    `yaml:"name"`
	Harness          ingest.Harness            `yaml:"harness"`
	Projects         []config.ProjectSelection `yaml:"projects"`
	ExplicitSessions []string                  `yaml:"explicit_sessions"`
	Rows             []selectionCandidateRow   `yaml:"rows"`
}

type selectionCandidateRow struct {
	SessionID                  string                       `yaml:"session_id"`
	GitRemote                  string                       `yaml:"git_remote"`
	ProjectName                string                       `yaml:"project_name"`
	ProjectHash                string                       `yaml:"project_hash"`
	ProjectPath                string                       `yaml:"project_path"`
	ResolvedPath               string                       `yaml:"resolved_path"`
	ResolutionFails            bool                         `yaml:"resolution_fails"`
	Branch                     string                       `yaml:"branch"`
	ExpectedClonePath          string                       `yaml:"expected_clone_path"`
	ExpectedRemoteMultiplicity candidateMultiplicityFixture `yaml:"expected_remote_multiplicity"`
	ExpectedNameMultiplicity   candidateMultiplicityFixture `yaml:"expected_name_multiplicity"`
	ExpectedBranchMatch        testutil.SelectionOutcome    `yaml:"expected_branch_match"`
	ExpectedProjectMatch       bool                         `yaml:"expected_project_match"`
}

func loadSelectionCandidateFixtures(t *testing.T) selectionCandidateFixtureDocument {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(selectionCandidateCohortYAML))
	decoder.KnownFields(true)
	var document selectionCandidateFixtureDocument
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode selection candidate fixtures with strict fields: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("selection candidate fixtures must contain exactly one YAML document: %v", err)
	}
	if document.DeclaredCases != len(document.Cases) || document.DeclaredCases < 9 {
		t.Fatalf("selection candidate case guard failed: declared=%d actual=%d minimum=9", document.DeclaredCases, len(document.Cases))
	}
	rows := 0
	seenCases := make(map[string]bool, len(document.Cases))
	seenSessions := make(map[string]bool)
	var outcomes []testutil.SelectionOutcome
	var multiplicities []candidateMultiplicityFixture
	for _, fixture := range document.Cases {
		if fixture.Name == "" || seenCases[fixture.Name] {
			t.Fatalf("selection candidate fixture case name %q is empty or duplicated", fixture.Name)
		}
		seenCases[fixture.Name] = true
		if fixture.Harness == "" || len(fixture.Rows) == 0 {
			t.Fatalf("selection candidate fixture %q needs a harness and at least one row", fixture.Name)
		}
		resolvedByRaw := make(map[string]string)
		for _, row := range fixture.Rows {
			rows++
			if row.SessionID == "" || seenSessions[row.SessionID] {
				t.Fatalf("selection candidate fixture session ID %q is empty or duplicated", row.SessionID)
			}
			seenSessions[row.SessionID] = true
			if _, err := ingest.NewSessionID(row.SessionID); err != nil {
				t.Fatalf("selection candidate fixture %q has invalid session ID %q: %v", fixture.Name, row.SessionID, err)
			}
			if row.ProjectPath == "" && (row.ResolvedPath != "" || row.ResolutionFails) {
				t.Fatalf("selection candidate fixture %q session %s describes resolution without a project_path", fixture.Name, row.SessionID)
			}
			if row.ResolutionFails == (row.ResolvedPath != "") {
				t.Fatalf("selection candidate fixture %q session %s must choose exactly one of resolved_path or resolution_fails", fixture.Name, row.SessionID)
			}
			if prior, exists := resolvedByRaw[row.ProjectPath]; exists && prior != row.ResolvedPath {
				t.Fatalf("selection candidate fixture %q resolves raw path %q to both %q and %q", fixture.Name, row.ProjectPath, prior, row.ResolvedPath)
			}
			resolvedByRaw[row.ProjectPath] = row.ResolvedPath
			outcomes = append(outcomes, row.ExpectedBranchMatch)
			row.ExpectedBranchMatch.BranchMatch(t, "selection candidate", fixture.Name)
			multiplicities = append(multiplicities, row.ExpectedRemoteMultiplicity, row.ExpectedNameMultiplicity)
			candidateMultiplicity(t, fixture.Name, row.ExpectedRemoteMultiplicity)
			candidateMultiplicity(t, fixture.Name, row.ExpectedNameMultiplicity)
		}
	}
	if document.DeclaredRows != rows || document.DeclaredRows < 16 {
		t.Fatalf("selection candidate row guard failed: declared=%d actual=%d minimum=16", document.DeclaredRows, rows)
	}
	testutil.RequireClosedSetCoverage(t, "selection candidate", "branch outcome", testutil.AllSelectionOutcomes, outcomes)
	testutil.RequireClosedSetCoverage(t, "selection candidate", "identity multiplicity", allCandidateMultiplicities, multiplicities)
	return document
}

type selectionCandidateFixtureResolver struct {
	resolved map[string]ingest.ClonePath
	failed   map[string]bool
}

func (r selectionCandidateFixtureResolver) Resolve(raw string) (ingest.ClonePath, error) {
	if r.failed[raw] {
		return "", fmt.Errorf("fixture path %q cannot be resolved", raw)
	}
	path, ok := r.resolved[raw]
	if !ok {
		return "", fmt.Errorf("fixture path %q has no resolution", raw)
	}
	return path, nil
}

func candidateMultiplicity(t *testing.T, caseName string, value candidateMultiplicityFixture) ingest.DiscoveryIdentityMultiplicity {
	t.Helper()
	switch value {
	case candidateMultiplicityUnique:
		return ingest.DiscoveryIdentityUnique
	case candidateMultiplicityAmbiguous:
		return ingest.DiscoveryIdentityAmbiguous
	default:
		t.Fatalf("selection candidate fixture %q declares unknown multiplicity %q", caseName, value)
		return ingest.DiscoveryIdentityUnproven
	}
}

func TestSelectionCandidateCohortsDriveCommandBoundaries(t *testing.T) {
	for _, fixture := range loadSelectionCandidateFixtures(t).Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			resolver := selectionCandidateFixtureResolver{
				resolved: make(map[string]ingest.ClonePath),
				failed:   make(map[string]bool),
			}
			inputs := make([]selectionCandidateInput, len(fixture.Rows))
			pushRows := make([]ingest.PushSessionRow, len(fixture.Rows))
			pruneRows := make([]ingest.PruneSessionRow, len(fixture.Rows))
			for index, row := range fixture.Rows {
				if row.ResolutionFails {
					resolver.failed[row.ProjectPath] = true
				} else {
					resolver.resolved[row.ProjectPath] = ingest.ClonePath(row.ResolvedPath)
				}
				sessionID := ingest.SessionID(row.SessionID)
				inputs[index] = selectionCandidateInput{
					Harness:     fixture.Harness,
					GitRemote:   row.GitRemote,
					ProjectName: row.ProjectName,
					ProjectHash: row.ProjectHash,
					ProjectPath: row.ProjectPath,
					Branch:      row.Branch,
					SessionID:   sessionID,
				}
				var branch *string
				if row.Branch != "" {
					value := row.Branch
					branch = &value
				}
				pushRows[index] = ingest.PushSessionRow{
					SessionID:    row.SessionID,
					ModelHarness: fixture.Harness.String(),
					ProjectHash:  row.ProjectHash,
					ProjectName:  row.ProjectName,
					ProjectPath:  row.ProjectPath,
					GitRemote:    row.GitRemote,
					GitBranch:    branch,
				}
				pruneRows[index] = ingest.PruneSessionRow{
					SessionID:   sessionID,
					Harness:     fixture.Harness,
					ProjectHash: row.ProjectHash,
					ProjectName: row.ProjectName,
					ProjectPath: row.ProjectPath,
					GitRemote:   row.GitRemote,
				}
			}

			cfg := config.BaseConfig()
			cfg.Selection = config.SelectionConfig{
				Mode: config.SelectionModeSelected,
				Harnesses: map[string]config.SelectionHarnessConfig{
					fixture.Harness.String(): {
						Projects: fixture.Projects,
						Sessions: fixture.ExplicitSessions,
					},
				},
			}
			matcher := cfg.SelectionMatcher()
			candidates, err := prepareSelectionCandidates(context.Background(), inputs, resolver)
			if err != nil {
				t.Fatalf("prepareSelectionCandidates: %v", err)
			}
			for index, candidate := range candidates {
				row := fixture.Rows[index]
				if candidate.ClonePath.String() != row.ExpectedClonePath {
					t.Errorf("session %s ClonePath = %q, want %q", row.SessionID, candidate.ClonePath, row.ExpectedClonePath)
				}
				if candidate.RemoteMultiplicity != candidateMultiplicity(t, fixture.Name, row.ExpectedRemoteMultiplicity) {
					t.Errorf("session %s RemoteMultiplicity = %d, want %q", row.SessionID, candidate.RemoteMultiplicity, row.ExpectedRemoteMultiplicity)
				}
				if candidate.NameMultiplicity != candidateMultiplicity(t, fixture.Name, row.ExpectedNameMultiplicity) {
					t.Errorf("session %s NameMultiplicity = %d, want %q", row.SessionID, candidate.NameMultiplicity, row.ExpectedNameMultiplicity)
				}
				wantBranch := row.ExpectedBranchMatch.BranchMatch(t, "selection candidate", fixture.Name)
				if got := matcher.MatchBranchCandidate(candidate); got != wantBranch {
					t.Errorf("session %s MatchBranchCandidate = %v, want %v", row.SessionID, got, wantBranch)
				}
				if got := matcher.MatchesCandidate(candidate); got != row.ExpectedProjectMatch {
					t.Errorf("session %s MatchesCandidate = %v, want %v", row.SessionID, got, row.ExpectedProjectMatch)
				}
			}

			store := &testutil.StubPushStore{AllSessions: pushRows}
			pushSelection, err := preparePushSelection(context.Background(), store, matcher, resolver)
			if err != nil {
				t.Fatalf("preparePushSelection: %v", err)
			}
			kept, withheld := push.ApplySelection(pushRows, pushSelection)
			assertPreparedPushPartition(t, fixture, kept, withheld, pushSelection)

			unselected, err := unselectedPruneSessions(context.Background(), pruneRows, matcher, resolver)
			if err != nil {
				t.Fatalf("unselectedPruneSessions: %v", err)
			}
			unselectedIDs := make(map[string]bool, len(unselected))
			for _, row := range unselected {
				unselectedIDs[row.SessionID.String()] = true
			}
			for _, row := range fixture.Rows {
				if got, want := unselectedIDs[row.SessionID], !row.ExpectedProjectMatch; got != want {
					t.Errorf("session %s prune-unselected membership = %v, want %v", row.SessionID, got, want)
				}
			}
		})
	}
}

func TestHarvestSelectionPreparesClonePathsBeforeLookup(t *testing.T) {
	fixtures := loadSelectionCandidateFixtures(t)
	var fixture selectionCandidateFixtureCase
	for _, candidate := range fixtures.Cases {
		if candidate.Name == "exact clone path selects one clone of an ambiguous remote" {
			fixture = candidate
			break
		}
	}
	if fixture.Name == "" {
		t.Fatal("clone-path harvest fixture is missing")
	}

	resolver := selectionCandidateFixtureResolver{
		resolved: make(map[string]ingest.ClonePath),
		failed:   make(map[string]bool),
	}
	sessions := make([]ingest.DiscoveredSession, len(fixture.Rows))
	for index, row := range fixture.Rows {
		resolver.resolved[row.ProjectPath] = ingest.ClonePath(row.ResolvedPath)
		sessions[index] = ingest.DiscoveredSession{
			SessionID:   ingest.SessionID(row.SessionID),
			Harness:     fixture.Harness,
			ProjectName: row.ProjectName,
			Branch:      row.Branch,
			CWD:         row.ProjectPath,
		}
	}
	cfg := config.BaseConfig()
	cfg.Selection = config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			fixture.Harness.String(): {Projects: fixture.Projects},
		},
	}
	git := &stubBranchGitResolver{remote: fixture.Rows[0].GitRemote, branch: fixture.Rows[0].Branch}
	filter, _ := buildSelectionFilterWithResolver(cfg, git, resolver)
	if filter.Match(sessions[0]) {
		t.Fatal("harvest selection matched before the complete cohort was prepared")
	}
	if err := filter.Prepare(context.Background(), sessions); err != nil {
		t.Fatalf("prepare harvest selection: %v", err)
	}
	for index, session := range sessions {
		want := fixture.Rows[index].ExpectedBranchMatch == testutil.SelectionSelected
		if got := filter.Match(session); got != want {
			t.Errorf("session %s harvest selection = %v, want %v", session.SessionID, got, want)
		}
	}
	mutated := sessions[0]
	mutated.CWD = fixture.Rows[1].ProjectPath
	mutated.ProjectName = "different-after-preparation"
	if !filter.Match(mutated) {
		t.Error("harvest lookup recomputed mutable session evidence instead of using the prepared SessionID decision")
	}
}

func assertPreparedPushPartition(
	t *testing.T,
	fixture selectionCandidateFixtureCase,
	kept []ingest.PushSessionRow,
	withheld []ingest.PushSessionRow,
	selection *push.SessionSelection,
) {
	t.Helper()
	keptIDs := make(map[string]bool, len(kept))
	withheldIDs := make(map[string]bool, len(withheld))
	for _, row := range kept {
		keptIDs[row.SessionID] = true
	}
	for _, row := range withheld {
		withheldIDs[row.SessionID] = true
	}
	for _, row := range fixture.Rows {
		want := row.ExpectedBranchMatch.BranchMatch(t, "selection candidate", fixture.Name)
		if got := selection.Decision(ingest.SessionID(row.SessionID)); got != want {
			t.Errorf("session %s prepared push decision = %v, want %v", row.SessionID, got, want)
		}
		if keptIDs[row.SessionID] != (want == ingest.BranchMatchYes) {
			t.Errorf("session %s kept membership = %v for decision %v", row.SessionID, keptIDs[row.SessionID], want)
		}
		if withheldIDs[row.SessionID] != (want == ingest.BranchMatchWithheldConflict) {
			t.Errorf("session %s withheld membership = %v for decision %v", row.SessionID, withheldIDs[row.SessionID], want)
		}
		// Reuse the complete-cohort decisions against one eligibility row. This is
		// the force invariant: changing pushed-at eligibility cannot recalculate
		// multiplicity and turn an excluded sibling into a selected session.
		singleKept, singleWithheld := push.ApplySelection([]ingest.PushSessionRow{{SessionID: row.SessionID}}, selection)
		if (len(singleKept) == 1) != (want == ingest.BranchMatchYes) ||
			(len(singleWithheld) == 1) != (want == ingest.BranchMatchWithheldConflict) {
			t.Errorf("session %s changed decision after eligibility narrowed: kept=%d withheld=%d want=%v", row.SessionID, len(singleKept), len(singleWithheld), want)
		}
	}
}

func TestSessionSelectionMissingRowFailsClosed(t *testing.T) {
	selection := push.NewSessionSelection(map[ingest.SessionID]ingest.BranchMatch{
		"11111111-1111-4111-8111-111111111111": ingest.BranchMatchYes,
	})
	if got := selection.Decision("22222222-2222-4222-8222-222222222222"); got != ingest.BranchMatchNo {
		t.Fatalf("missing prepared session decision = %v, want fail-closed %v", got, ingest.BranchMatchNo)
	}
}
