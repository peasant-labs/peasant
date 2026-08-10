package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
)

const expectedKickstartCommitGateCaseCount = 12
const expectedKickstartCommitGateCandidateCaseCount = 2
const expectedKickstartCommitGateCandidateCheckCount = 3

const (
	commitGateParentSessionID = "11111111-1111-4111-8111-111111111111"
	commitGateChildSessionID  = "22222222-2222-4222-8222-222222222222"
)

type commitGateRunState string

const (
	commitGateFirstRun commitGateRunState = "first"
	commitGateLaterRun commitGateRunState = "later"
)

type commitGateSelection string

const (
	commitGateSelectionAll            commitGateSelection = "all"
	commitGateSelectionEmpty          commitGateSelection = "empty"
	commitGateSelectionExplicitParent commitGateSelection = "explicit-parent"
	commitGateSelectionWholeProject   commitGateSelection = "whole-project"
)

type commitGatePathState string

const (
	commitGatePathDirectory commitGatePathState = "directory"
	commitGatePathMissing   commitGatePathState = "missing"
)

type commitGateKey string

const (
	commitGateKeySelectAll commitGateKey = "a"
	commitGateKeyDown      commitGateKey = "j"
	commitGateKeyRight     commitGateKey = "right"
	commitGateKeySpace     commitGateKey = "space"
	commitGateKeyLeft      commitGateKey = "left"
	commitGateKeyEnter     commitGateKey = "enter"
	commitGateKeyBack      commitGateKey = "esc"
	commitGateKeyCancel    commitGateKey = "q"
	commitGateKeyInterrupt commitGateKey = "ctrl+c"
)

type kickstartCommitGateDocument struct {
	DeclaredCaseCount           int                          `yaml:"declaredCaseCount"`
	DeclaredCandidateCaseCount  int                          `yaml:"declaredCandidateCaseCount"`
	DeclaredCandidateCheckCount int                          `yaml:"declaredCandidateCheckCount"`
	MessageBlock                string                       `yaml:"messageBlock"`
	Paths                       []kickstartGatePath          `yaml:"paths"`
	Listings                    []kickstartGateListing       `yaml:"listings"`
	Cases                       []kickstartCommitGateCase    `yaml:"cases"`
	CandidateCases              []kickstartGateCandidateCase `yaml:"candidateCases"`
}

type kickstartGatePath struct {
	Key   string              `yaml:"key"`
	State commitGatePathState `yaml:"state"`
}

type kickstartGateListing struct {
	Harness     defaults.Harness `yaml:"harness"`
	ProjectName string           `yaml:"projectName"`
	GitRemote   string           `yaml:"gitRemote"`
	PathKey     string           `yaml:"pathKey"`
	Branch      string           `yaml:"branch"`
	SessionID   string           `yaml:"sessionId"`
	Title       string           `yaml:"title"`
	SubagentIDs []string         `yaml:"subagentIds"`
}

type kickstartCommitGateCase struct {
	Name             string              `yaml:"name"`
	RunState         commitGateRunState  `yaml:"runState"`
	InitialSelection commitGateSelection `yaml:"initialSelection"`
	EditKeys         []commitGateKey     `yaml:"editKeys"`
	AnswerKeys       []commitGateKey     `yaml:"answerKeys"`
	ExpectPrompt     bool                `yaml:"expectPrompt"`
	ExpectCommit     bool                `yaml:"expectCommit"`
	ExpectExit       bool                `yaml:"expectExit"`
	ExpectSelection  commitGateSelection `yaml:"expectSelection"`
}

type commitGateExpectedGate string

const (
	commitGateExpectedNone              commitGateExpectedGate = "none"
	commitGateExpectedConfirmNoProjects commitGateExpectedGate = "confirm-no-projects"
)

type kickstartGateCandidateCase struct {
	Name                   string                           `yaml:"name"`
	ExpectedCandidateCount int                              `yaml:"expectedCandidateCount"`
	Paths                  []kickstartGatePath              `yaml:"paths"`
	Listings               []kickstartGateListing           `yaml:"listings"`
	ExpectedCandidates     []kickstartGateExpectedCandidate `yaml:"expectedCandidates"`
	GateChecks             []kickstartGateCandidateCheck    `yaml:"gateChecks"`
}

type kickstartGateExpectedCandidate struct {
	Harness     defaults.Harness                  `yaml:"harness"`
	PathKey     string                            `yaml:"pathKey"`
	GitRemote   string                            `yaml:"gitRemote"`
	ProjectName string                            `yaml:"projectName"`
	Descendants []kickstartGateExpectedDescendant `yaml:"descendants"`
}

type kickstartGateExpectedDescendant struct {
	SessionID       string `yaml:"sessionId"`
	ParentSessionID string `yaml:"parentSessionId"`
}

type kickstartGateCandidateCheck struct {
	Name                       string                          `yaml:"name"`
	Selection                  kickstartGateCandidateSelection `yaml:"selection"`
	ExpectedEditorProjectCount int                             `yaml:"expectedEditorProjectCount"`
	ExpectedGate               commitGateExpectedGate          `yaml:"expectedGate"`
}

type kickstartGateCandidateSelection struct {
	Harness     defaults.Harness `yaml:"harness"`
	GitRemote   string           `yaml:"gitRemote"`
	ProjectName string           `yaml:"projectName"`
	PathKey     string           `yaml:"pathKey"`
}

//go:embed testdata/kickstart_commit_gate.yaml
var kickstartCommitGateData []byte

func loadKickstartCommitGateDocument(t *testing.T) kickstartCommitGateDocument {
	t.Helper()
	var document kickstartCommitGateDocument
	decoder := yaml.NewDecoder(bytes.NewReader(kickstartCommitGateData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode testdata/kickstart_commit_gate.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("kickstart_commit_gate.yaml must hold exactly one document: %v", err)
	}
	if document.DeclaredCaseCount != expectedKickstartCommitGateCaseCount || len(document.Cases) != expectedKickstartCommitGateCaseCount {
		t.Fatalf(
			"declaredCaseCount=%d and actual cases=%d; both must equal required count %d",
			document.DeclaredCaseCount,
			len(document.Cases),
			expectedKickstartCommitGateCaseCount,
		)
	}
	if document.DeclaredCandidateCaseCount != expectedKickstartCommitGateCandidateCaseCount ||
		len(document.CandidateCases) != expectedKickstartCommitGateCandidateCaseCount {
		t.Fatalf(
			"declaredCandidateCaseCount=%d and actual candidate cases=%d; both must equal required count %d",
			document.DeclaredCandidateCaseCount,
			len(document.CandidateCases),
			expectedKickstartCommitGateCandidateCaseCount,
		)
	}
	validateKickstartCommitGateDocument(t, document)
	return document
}

func validateKickstartCommitGateDocument(t *testing.T, document kickstartCommitGateDocument) {
	t.Helper()
	messageLines := strings.Split(document.MessageBlock, "\n")
	if len(messageLines) != 6 || strings.Join(messageLines, "\n") != document.MessageBlock {
		t.Fatalf("messageBlock has %d rows, want one ordered block with the six accepted sentences", len(messageLines))
	}
	pathKeys := validateKickstartGatePaths(t, "mounted lifecycle", document.Paths)
	if len(document.Listings) < 2 {
		t.Fatal("fixture must include an available parent and nested descendant")
	}
	validateKickstartGateListings(t, "mounted lifecycle", document.Listings, pathKeys)

	caseNames := make(map[string]struct{}, len(document.Cases))
	pairs := make(map[string]int, len(document.Cases))
	runCounts := make(map[commitGateRunState]int)
	outcomeCounts := make(map[string]int)
	for _, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" {
			t.Fatal("fixture case has an empty name")
		}
		if _, duplicate := caseNames[testCase.Name]; duplicate {
			t.Fatalf("fixture repeats case name %q", testCase.Name)
		}
		caseNames[testCase.Name] = struct{}{}
		if !testCase.RunState.valid() || !testCase.InitialSelection.valid() || !testCase.ExpectSelection.valid() {
			t.Fatalf("fixture case %q has an unknown run state or selection", testCase.Name)
		}
		for _, key := range append(append([]commitGateKey(nil), testCase.EditKeys...), testCase.AnswerKeys...) {
			if !key.valid() {
				t.Fatalf("fixture case %q has unknown key %q", testCase.Name, key)
			}
		}
		if testCase.ExpectCommit && testCase.ExpectExit {
			t.Fatalf("fixture case %q cannot both commit and exit without saving", testCase.Name)
		}
		outcome := commitGateOutcome(testCase)
		if outcome == "unknown" {
			t.Fatalf("fixture case %q has no supported confirmation outcome", testCase.Name)
		}
		pairs[string(testCase.RunState)+"/"+outcome]++
		runCounts[testCase.RunState]++
		outcomeCounts[outcome]++
	}
	if len(pairs) != expectedKickstartCommitGateCaseCount {
		t.Fatalf("fixture has %d distinct run/outcome pairs, want %d", len(pairs), expectedKickstartCommitGateCaseCount)
	}
	for pair, count := range pairs {
		if count != 1 {
			t.Fatalf("fixture repeats run/outcome pair %q %d times", pair, count)
		}
	}
	if runCounts[commitGateFirstRun] != 6 || runCounts[commitGateLaterRun] != 6 {
		t.Fatalf("fixture run counts = first:%d later:%d, want 6 each", runCounts[commitGateFirstRun], runCounts[commitGateLaterRun])
	}
	if outcomeCounts["suppressed"] != 2 || outcomeCounts["yes"] != 2 || outcomeCounts["no"] != 2 ||
		outcomeCounts["back"] != 2 || outcomeCounts["cancel"] != 2 || outcomeCounts["interrupt"] != 2 {
		t.Fatalf("fixture must cover every confirmation outcome once per run state; got %v", outcomeCounts)
	}

	checkCount := 0
	candidateNames := make(map[string]struct{}, len(document.CandidateCases))
	for _, candidateCase := range document.CandidateCases {
		validateKickstartGateCandidateCase(t, candidateCase, candidateNames)
		checkCount += len(candidateCase.GateChecks)
	}
	if document.DeclaredCandidateCheckCount != expectedKickstartCommitGateCandidateCheckCount ||
		checkCount != expectedKickstartCommitGateCandidateCheckCount {
		t.Fatalf(
			"declaredCandidateCheckCount=%d and actual checks=%d; both must equal required count %d",
			document.DeclaredCandidateCheckCount,
			checkCount,
			expectedKickstartCommitGateCandidateCheckCount,
		)
	}
}

func validateKickstartGatePaths(t *testing.T, scope string, paths []kickstartGatePath) map[string]commitGatePathState {
	t.Helper()
	if len(paths) == 0 {
		t.Fatalf("%s fixture has no paths", scope)
	}
	pathStates := make(map[string]commitGatePathState, len(paths))
	for _, path := range paths {
		if path.Key == "" || filepath.IsAbs(path.Key) || filepath.Clean(path.Key) != path.Key || strings.HasPrefix(path.Key, "..") {
			t.Fatalf("%s fixture has unsafe path key %q", scope, path.Key)
		}
		if path.State != commitGatePathDirectory && path.State != commitGatePathMissing {
			t.Fatalf("%s fixture path %q has unknown state %q", scope, path.Key, path.State)
		}
		if _, duplicate := pathStates[path.Key]; duplicate {
			t.Fatalf("%s fixture repeats path key %q", scope, path.Key)
		}
		pathStates[path.Key] = path.State
	}
	return pathStates
}

func validateKickstartGateListings(
	t *testing.T,
	scope string,
	listings []kickstartGateListing,
	pathStates map[string]commitGatePathState,
) {
	t.Helper()
	listingIDs := make(map[string]struct{}, len(listings))
	for _, listing := range listings {
		if !listing.Harness.IsKnown() || listing.SessionID == "" || listing.PathKey == "" {
			t.Fatalf("%s fixture listing lacks a known harness, session ID, or path: %#v", scope, listing)
		}
		if _, ok := pathStates[listing.PathKey]; !ok {
			t.Fatalf("%s fixture listing %q references unknown path key %q", scope, listing.SessionID, listing.PathKey)
		}
		if _, err := ingest.NewSessionID(listing.SessionID); err != nil {
			t.Fatalf("%s fixture listing has invalid session ID %q: %v", scope, listing.SessionID, err)
		}
		if _, duplicate := listingIDs[listing.SessionID]; duplicate {
			t.Fatalf("%s fixture repeats session ID %q", scope, listing.SessionID)
		}
		listingIDs[listing.SessionID] = struct{}{}
		for _, childID := range listing.SubagentIDs {
			if _, err := ingest.NewSessionID(childID); err != nil {
				t.Fatalf("%s fixture listing %q has invalid child session ID %q: %v", scope, listing.SessionID, childID, err)
			}
		}
	}
}

func validateKickstartGateCandidateCase(
	t *testing.T,
	testCase kickstartGateCandidateCase,
	seen map[string]struct{},
) {
	t.Helper()
	if strings.TrimSpace(testCase.Name) == "" {
		t.Fatal("candidate fixture case has an empty name")
	}
	if _, duplicate := seen[testCase.Name]; duplicate {
		t.Fatalf("candidate fixture repeats case name %q", testCase.Name)
	}
	seen[testCase.Name] = struct{}{}
	if testCase.ExpectedCandidateCount != len(testCase.ExpectedCandidates) || testCase.ExpectedCandidateCount == 0 {
		t.Fatalf(
			"candidate fixture %q expectedCandidateCount=%d but has %d expected candidates",
			testCase.Name,
			testCase.ExpectedCandidateCount,
			len(testCase.ExpectedCandidates),
		)
	}
	pathStates := validateKickstartGatePaths(t, testCase.Name, testCase.Paths)
	validateKickstartGateListings(t, testCase.Name, testCase.Listings, pathStates)
	for _, expected := range testCase.ExpectedCandidates {
		if !expected.Harness.IsKnown() || pathStates[expected.PathKey] != commitGatePathDirectory || len(expected.Descendants) == 0 {
			t.Fatalf("candidate fixture %q has an incomplete expected candidate: %#v", testCase.Name, expected)
		}
		for _, descendant := range expected.Descendants {
			if _, err := ingest.NewSessionID(descendant.SessionID); err != nil {
				t.Fatalf("candidate fixture %q has invalid expected session ID %q: %v", testCase.Name, descendant.SessionID, err)
			}
			if descendant.ParentSessionID != "" {
				if _, err := ingest.NewSessionID(descendant.ParentSessionID); err != nil {
					t.Fatalf("candidate fixture %q has invalid expected parent session ID %q: %v", testCase.Name, descendant.ParentSessionID, err)
				}
			}
		}
	}
	if len(testCase.GateChecks) == 0 {
		t.Fatalf("candidate fixture %q has no editor and gate checks", testCase.Name)
	}
	checkNames := make(map[string]struct{}, len(testCase.GateChecks))
	for _, check := range testCase.GateChecks {
		if strings.TrimSpace(check.Name) == "" || !check.Selection.Harness.IsKnown() {
			t.Fatalf("candidate fixture %q has an incomplete gate check: %#v", testCase.Name, check)
		}
		if _, duplicate := checkNames[check.Name]; duplicate {
			t.Fatalf("candidate fixture %q repeats gate check %q", testCase.Name, check.Name)
		}
		checkNames[check.Name] = struct{}{}
		if check.Selection.PathKey != "" && pathStates[check.Selection.PathKey] != commitGatePathDirectory {
			t.Fatalf("candidate fixture %q check %q uses unavailable path %q", testCase.Name, check.Name, check.Selection.PathKey)
		}
		if check.ExpectedEditorProjectCount < 0 || check.ExpectedEditorProjectCount > testCase.ExpectedCandidateCount {
			t.Fatalf("candidate fixture %q check %q has invalid editor project count %d", testCase.Name, check.Name, check.ExpectedEditorProjectCount)
		}
		if !check.ExpectedGate.valid() {
			t.Fatalf("candidate fixture %q check %q has unknown gate %q", testCase.Name, check.Name, check.ExpectedGate)
		}
	}
}

func (s commitGateRunState) valid() bool {
	return s == commitGateFirstRun || s == commitGateLaterRun
}

func (s commitGateSelection) valid() bool {
	switch s {
	case commitGateSelectionAll, commitGateSelectionEmpty, commitGateSelectionExplicitParent, commitGateSelectionWholeProject:
		return true
	default:
		return false
	}
}

func (k commitGateKey) valid() bool {
	switch k {
	case commitGateKeySelectAll, commitGateKeyDown, commitGateKeyRight, commitGateKeySpace, commitGateKeyLeft,
		commitGateKeyEnter, commitGateKeyBack, commitGateKeyCancel, commitGateKeyInterrupt:
		return true
	default:
		return false
	}
}

func (g commitGateExpectedGate) valid() bool {
	return g == commitGateExpectedNone || g == commitGateExpectedConfirmNoProjects
}

func (g commitGateExpectedGate) value() settings.CommitGate {
	if g == commitGateExpectedConfirmNoProjects {
		return settings.CommitGateConfirmNoProjects
	}
	return settings.CommitGateNone
}

func commitGateOutcome(testCase kickstartCommitGateCase) string {
	if !testCase.ExpectPrompt {
		return "suppressed"
	}
	if len(testCase.AnswerKeys) == 2 && testCase.AnswerKeys[0] == commitGateKeyLeft && testCase.AnswerKeys[1] == commitGateKeyEnter {
		return "yes"
	}
	if len(testCase.AnswerKeys) != 1 {
		return "unknown"
	}
	switch testCase.AnswerKeys[0] {
	case commitGateKeyEnter:
		return "no"
	case commitGateKeyBack:
		return "back"
	case commitGateKeyCancel:
		return "cancel"
	case commitGateKeyInterrupt:
		return "interrupt"
	default:
		return "unknown"
	}
}

func TestKickstartCommitGateCandidateCohortsPreservePhysicalIdentityAndAmbiguity(t *testing.T) {
	t.Parallel()
	document := loadKickstartCommitGateDocument(t)
	for _, testCase := range document.CandidateCases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			paths := materializeKickstartGatePaths(t, testCase.Paths)
			listings := kickstartGateListings(testCase.Listings, paths)
			source := kickstart.NewScannerTreeSource(
				listings,
				kickstart.WithPathIdentityResolver(ingest.NewPhysicalPathResolver()),
			)

			candidates := source.CommitGateCandidates()
			assertKickstartGateCandidates(t, candidates, testCase.ExpectedCandidates, paths)
			for _, check := range testCase.GateChecks {
				check := check
				t.Run(check.Name, func(t *testing.T) {
					selection := kickstartGateCandidateSelectionConfig(check.Selection, paths)
					roots, err := source.Load(t.Context())
					if err != nil {
						t.Fatalf("load production scanner tree: %v", err)
					}
					settings.PrepopulateSelection(roots, selection)
					if got := selectedKickstartGateEditorProjects(roots); got != check.ExpectedEditorProjectCount {
						t.Fatalf("checked editor projects=%d, want %d", got, check.ExpectedEditorProjectCount)
					}
					gotGate := settings.NewCommitGateEvaluator(candidates)(selection)
					if gotGate != check.ExpectedGate.value() {
						t.Fatalf("commit gate=%d, want %d", gotGate, check.ExpectedGate.value())
					}
				})
			}
		})
	}
}

func assertKickstartGateCandidates(
	t *testing.T,
	candidates []selectionprojection.ProjectCandidate,
	expected []kickstartGateExpectedCandidate,
	paths map[string]string,
) {
	t.Helper()
	if len(candidates) != len(expected) {
		t.Fatalf("commit-gate candidate projects=%d, want %d", len(candidates), len(expected))
	}
	byParentID := make(map[selectionprojection.ParentProjectID]selectionprojection.ProjectCandidate, len(candidates))
	for _, candidate := range candidates {
		if _, duplicate := byParentID[candidate.ParentProjectID]; duplicate {
			t.Fatalf("commit-gate candidates repeat ParentProjectID %q", candidate.ParentProjectID)
		}
		byParentID[candidate.ParentProjectID] = candidate
	}
	for _, want := range expected {
		physicalPath := ingest.ClonePath(paths[want.PathKey])
		wantParentID := selectionprojection.ParentProjectID((kickstart.ProjectIdentity{
			Harness:   ingest.Harness(want.Harness),
			ClonePath: physicalPath,
		}).String())
		candidate, ok := byParentID[wantParentID]
		if !ok {
			t.Fatalf("missing candidate for stable ParentProjectID %q", wantParentID)
		}
		if candidate.Harness != ingest.Harness(want.Harness) || candidate.ClonePath != physicalPath {
			t.Fatalf("candidate carrier=(%q,%q), want (%q,%q)", candidate.Harness, candidate.ClonePath, want.Harness, physicalPath)
		}
		if candidate.GitRemote != want.GitRemote || candidate.ProjectName != want.ProjectName {
			t.Fatalf(
				"candidate fallback identity=(%q,%q), want (%q,%q)",
				candidate.GitRemote,
				candidate.ProjectName,
				want.GitRemote,
				want.ProjectName,
			)
		}
		assertKickstartGateDescendants(t, candidate.Descendants, want.Descendants, physicalPath)
	}
}

func assertKickstartGateDescendants(
	t *testing.T,
	got []selectionprojection.SessionCandidate,
	want []kickstartGateExpectedDescendant,
	physicalPath ingest.ClonePath,
) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("candidate descendants=%d, want %d", len(got), len(want))
	}
	got = append([]selectionprojection.SessionCandidate(nil), got...)
	sort.Slice(got, func(i, j int) bool { return got[i].SessionID < got[j].SessionID })
	want = append([]kickstartGateExpectedDescendant(nil), want...)
	sort.Slice(want, func(i, j int) bool { return want[i].SessionID < want[j].SessionID })
	for index, expected := range want {
		if got[index].SessionID != ingest.SessionID(expected.SessionID) ||
			got[index].ParentSessionID != ingest.SessionID(expected.ParentSessionID) ||
			got[index].ClonePath != physicalPath {
			t.Fatalf(
				"descendant[%d]=(%q,%q,%q), want (%q,%q,%q)",
				index,
				got[index].SessionID,
				got[index].ParentSessionID,
				got[index].ClonePath,
				expected.SessionID,
				expected.ParentSessionID,
				physicalPath,
			)
		}
	}
}

func kickstartGateCandidateSelectionConfig(
	selection kickstartGateCandidateSelection,
	paths map[string]string,
) config.SelectionConfig {
	project := config.ProjectSelection{
		GitRemote: selection.GitRemote,
		Name:      selection.ProjectName,
	}
	if selection.PathKey != "" {
		project.ClonePaths = []string{paths[selection.PathKey]}
	}
	return config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: true,
		Harnesses: map[string]config.SelectionHarnessConfig{
			selection.Harness.String(): {Projects: []config.ProjectSelection{project}},
		},
	}
}

func selectedKickstartGateEditorProjects(roots []*kit.TreeNode) int {
	selected := 0
	for _, root := range roots {
		if root.State != kit.Unchecked {
			selected++
		}
	}
	return selected
}

func TestMountedKickstartCommitGateFirstAndLaterRuns(t *testing.T) {
	document := loadKickstartCommitGateDocument(t)
	for _, testCase := range document.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			paths := materializeKickstartGatePaths(t, document.Paths)
			listings := kickstartGateListings(document.Listings, paths)
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			seedKickstartGateConfig(t, configPath, testCase.RunState, testCase.InitialSelection, paths)
			before := snapshotKickstartGateFile(t, configPath)

			var ingestCalls int
			var mounted kickstart.Model
			flowIngest := func(context.Context) (*ftue.IngestResult, error) {
				ingestCalls++
				return &ftue.IngestResult{}, nil
			}
			driveFlow := func(model tea.Model) error {
				var ok bool
				mounted, ok = model.(kickstart.Model)
				if !ok {
					return fmt.Errorf("mounted model has type %T, want kickstart.Model", model)
				}
				mounted = startKickstartGateModel(t, mounted)
				for _, key := range testCase.EditKeys {
					mounted = updateKickstartGateModel(t, mounted, key)
				}
				for index := 0; index < 12 && !mounted.Program().OnReceipt(); index++ {
					mounted = updateKickstartGateModel(t, mounted, commitGateKey("tab"))
				}
				if !mounted.Program().OnReceipt() {
					return fmt.Errorf("kickstart flow did not reach review and save")
				}

				mounted = updateKickstartGateModel(t, mounted, commitGateKeyEnter)
				if got := mounted.Program().ConfirmingNoProjects(); got != testCase.ExpectPrompt {
					return fmt.Errorf("no-project confirmation visible=%t, want %t", got, testCase.ExpectPrompt)
				}
				if testCase.ExpectPrompt {
					if mounted.Program().Confirming() {
						return fmt.Errorf("no-project save used the no-save exit modal")
					}
					view := normalizeKickstartGateTerminalBlock(mounted.Program().View())
					message := normalizeKickstartGateTerminalBlock(document.MessageBlock)
					if !strings.Contains(view, message) {
						return fmt.Errorf("confirmation does not contain the accepted ordered message block; view:\n%s", view)
					}
				}
				for _, key := range testCase.AnswerKeys {
					mounted = updateKickstartGateModel(t, mounted, key)
				}
				if testCase.ExpectCommit {
					mounted = updateKickstartGateModel(t, mounted, commitGateKeyEnter)
				}
				return nil
			}

			deps := defaultKickstartCommandDeps()
			deps.flowIngest = flowIngest
			deps.runFlow = driveFlow
			cmd := mountTestCmd(t, t.TempDir())
			if err := runKickstartFlow(cmd, deps, configPath, ftue.ProviderInventory{}, listings); err != nil {
				t.Fatalf("run %s mounted kickstart flow: %v", testCase.RunState, err)
			}
			program := mounted.Program()
			if program.Committed() != testCase.ExpectCommit {
				t.Fatalf("committed=%t, want %t", program.Committed(), testCase.ExpectCommit)
			}
			if program.Exited() != testCase.ExpectExit {
				t.Fatalf("exited=%t, want %t", program.Exited(), testCase.ExpectExit)
			}
			wantIngestCalls := 0
			if testCase.ExpectCommit {
				wantIngestCalls = 1
			}
			if ingestCalls != wantIngestCalls {
				t.Fatalf("ingest calls=%d, want %d", ingestCalls, wantIngestCalls)
			}
			if testCase.ExpectCommit {
				assertKickstartGateCommittedSelection(t, configPath, testCase.ExpectSelection, paths)
				return
			}
			after := snapshotKickstartGateFile(t, configPath)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("declined or interrupted confirmation changed config bytes\n before=%#v\n after=%#v", before, after)
			}
		})
	}
}

func materializeKickstartGatePaths(t *testing.T, fixtures []kickstartGatePath) map[string]string {
	t.Helper()
	root := t.TempDir()
	paths := make(map[string]string, len(fixtures))
	for _, fixture := range fixtures {
		path := filepath.Join(root, fixture.Key)
		if fixture.State == commitGatePathDirectory {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("create physical path %q: %v", fixture.Key, err)
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				t.Fatalf("resolve physical path %q: %v", fixture.Key, err)
			}
			path = resolved
		}
		var err error
		paths[fixture.Key], err = filepath.Abs(path)
		if err != nil {
			t.Fatalf("make physical path %q absolute: %v", fixture.Key, err)
		}
	}
	return paths
}

func kickstartGateListings(fixtures []kickstartGateListing, paths map[string]string) []ftue.SessionListing {
	listings := make([]ftue.SessionListing, 0, len(fixtures))
	for _, fixture := range fixtures {
		listings = append(listings, ftue.SessionListing{
			Harness:     fixture.Harness.String(),
			ProjectName: fixture.ProjectName,
			GitRemote:   fixture.GitRemote,
			Branch:      fixture.Branch,
			Title:       fixture.Title,
			SessionID:   fixture.SessionID,
			SubagentIDs: append([]string(nil), fixture.SubagentIDs...),
			WorkingDir:  paths[fixture.PathKey],
		})
	}
	return listings
}

func seedKickstartGateConfig(
	t *testing.T,
	path string,
	runState commitGateRunState,
	selection commitGateSelection,
	paths map[string]string,
) {
	t.Helper()
	if runState == commitGateFirstRun {
		return
	}
	configured := config.BaseConfig()
	configured.Selection = kickstartGateSelection(selection, paths)
	if err := config.SaveAtomic(path, configured); err != nil {
		t.Fatalf("seed later-run config: %v", err)
	}
}

func kickstartGateSelection(selection commitGateSelection, paths map[string]string) config.SelectionConfig {
	switch selection {
	case commitGateSelectionAll:
		return config.SelectionConfig{Mode: config.SelectionModeAll, AutoIngestNewBranches: true}
	case commitGateSelectionExplicitParent:
		return config.SelectionConfig{
			Mode:                  config.SelectionModeSelected,
			AutoIngestNewBranches: true,
			Harnesses: map[string]config.SelectionHarnessConfig{
				string(defaults.HarnessClaudeCode): {Sessions: []string{commitGateParentSessionID}},
			},
		}
	case commitGateSelectionWholeProject:
		return config.SelectionConfig{
			Mode:                  config.SelectionModeSelected,
			AutoIngestNewBranches: true,
			Harnesses: map[string]config.SelectionHarnessConfig{
				defaults.HarnessClaudeCode.String(): {
					Projects: []config.ProjectSelection{{
						GitRemote:  "git@github.com:acme/tool.git",
						Name:       "acme/tool",
						ClonePaths: []string{paths["worktree"]},
					}},
				},
			},
		}
	case commitGateSelectionEmpty:
		return config.SelectionConfig{Mode: config.SelectionModeSelected, AutoIngestNewBranches: true}
	default:
		return config.SelectionConfig{}
	}
}

func startKickstartGateModel(t *testing.T, model kickstart.Model) kickstart.Model {
	t.Helper()
	model = settleKickstartGateModel(t, model, model.Init())
	if model.Program().Phase() == kickstart.PhaseOAuth {
		model = updateKickstartGateModel(t, model, commitGateKeyEnter)
	}
	if model.Program().Phase() != kickstart.PhaseFlow {
		t.Fatalf("mounted kickstart phase=%s, want flow", model.Program().Phase())
	}
	return model
}

func updateKickstartGateModel(t *testing.T, model kickstart.Model, key commitGateKey) kickstart.Model {
	t.Helper()
	next, cmd := model.Update(kickstartGateKeyMessage(t, key))
	updated, ok := next.(kickstart.Model)
	if !ok {
		t.Fatalf("updated model has type %T, want kickstart.Model", next)
	}
	return settleKickstartGateModel(t, updated, cmd)
}

func settleKickstartGateModel(t *testing.T, model kickstart.Model, first tea.Cmd) kickstart.Model {
	t.Helper()
	queue := []tea.Cmd{first}
	for iterations := 0; len(queue) > 0 && iterations < 256; iterations++ {
		cmd := queue[0]
		queue = queue[1:]
		if cmd == nil {
			continue
		}
		message := cmd()
		if batch, ok := message.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		if message == nil {
			continue
		}
		next, follow := model.Update(message)
		updated, ok := next.(kickstart.Model)
		if !ok {
			t.Fatalf("async update model has type %T, want kickstart.Model", next)
		}
		model = updated
		if follow != nil {
			queue = append(queue, follow)
		}
		if model.Program().Phase() == kickstart.PhaseDone {
			return model
		}
	}
	if len(queue) > 0 {
		t.Fatal("mounted kickstart command queue did not settle")
	}
	return model
}

func kickstartGateKeyMessage(t *testing.T, key commitGateKey) tea.KeyPressMsg {
	t.Helper()
	switch key {
	case commitGateKeySelectAll:
		return tea.KeyPressMsg{Code: 'a', Text: "a"}
	case commitGateKeyDown:
		return tea.KeyPressMsg{Code: 'j', Text: "j"}
	case commitGateKeyRight:
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case commitGateKeySpace:
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case commitGateKeyLeft:
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case commitGateKeyEnter:
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case commitGateKeyBack:
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case commitGateKeyCancel:
		return tea.KeyPressMsg{Code: 'q', Text: "q"}
	case commitGateKeyInterrupt:
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	default:
		t.Fatalf("no key message for fixture token %q", key)
		return tea.KeyPressMsg{}
	}
}

type kickstartGateFileSnapshot struct {
	Exists bool
	Bytes  []byte
}

func snapshotKickstartGateFile(t *testing.T, path string) kickstartGateFileSnapshot {
	t.Helper()
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return kickstartGateFileSnapshot{}
	}
	if err != nil {
		t.Fatalf("snapshot config %q: %v", path, err)
	}
	return kickstartGateFileSnapshot{Exists: true, Bytes: content}
}

func assertKickstartGateCommittedSelection(
	t *testing.T,
	path string,
	want commitGateSelection,
	paths map[string]string,
) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed config: %v", err)
	}
	configured, err := config.Parse(content)
	if err != nil {
		t.Fatalf("parse committed config: %v", err)
	}
	got := normalizeKickstartGateSelection(configured.Selection)
	expected := normalizeKickstartGateSelection(kickstartGateSelection(want, paths))
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("committed selection mismatch\n got: %#v\nwant: %#v", got, expected)
	}
}

func normalizeKickstartGateTerminalBlock(value string) string {
	value = ansiPattern.ReplaceAllString(value, "")
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	for index := range lines {
		lines[index] = strings.TrimSpace(lines[index])
	}
	return strings.Join(lines, "\n")
}

func normalizeKickstartGateSelection(selection config.SelectionConfig) config.SelectionConfig {
	selection.DeprecatedProviders = nil
	if len(selection.Harnesses) == 0 {
		selection.Harnesses = nil
		return selection
	}
	for harness, configured := range selection.Harnesses {
		if len(configured.Sessions) == 0 {
			configured.Sessions = nil
		}
		if len(configured.Projects) == 0 {
			configured.Projects = nil
		}
		selection.Harnesses[harness] = configured
	}
	return selection
}
