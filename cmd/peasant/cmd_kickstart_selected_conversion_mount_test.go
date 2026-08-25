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
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/kickstart_legacy_selected_conversion_mount.yaml
var mountedLegacySelectedConversionData []byte

type mountedLegacySelectedAction string

const (
	mountedLegacySelectedCommit mountedLegacySelectedAction = "commit"
	mountedLegacySelectedNo     mountedLegacySelectedAction = "no"
	mountedLegacySelectedBack   mountedLegacySelectedAction = "back"
	mountedLegacySelectedQuit   mountedLegacySelectedAction = "quit"
	mountedLegacySelectedCancel mountedLegacySelectedAction = "cancel"
)

func (a mountedLegacySelectedAction) valid() bool {
	switch a {
	case mountedLegacySelectedCommit,
		mountedLegacySelectedNo,
		mountedLegacySelectedBack,
		mountedLegacySelectedQuit,
		mountedLegacySelectedCancel:
		return true
	default:
		return false
	}
}

type mountedLegacySelectedPathState string

const (
	mountedLegacySelectedPathDirectory mountedLegacySelectedPathState = "directory"
	mountedLegacySelectedPathSymlink   mountedLegacySelectedPathState = "symlink"
)

type mountedLegacySelectedPathFixture struct {
	Key       string                         `yaml:"key"`
	State     mountedLegacySelectedPathState `yaml:"state"`
	TargetKey string                         `yaml:"targetKey"`
}

type mountedLegacySelectedDocument struct {
	ExpectedScenarioCount int                             `yaml:"expectedScenarioCount"`
	ExpectedJourneyCount  int                             `yaml:"expectedJourneyCount"`
	ExpectedScenarioNames []string                        `yaml:"expectedScenarioNames"`
	ExpectedJourneyNames  []string                        `yaml:"expectedJourneyNames"`
	Scenarios             []mountedLegacySelectedScenario `yaml:"scenarios"`
}

type mountedLegacySelectedScenario struct {
	Name                         string                             `yaml:"name"`
	ExpectedStoredCount          *int                               `yaml:"expectedStoredCount"`
	ExpectedScanCount            *int                               `yaml:"expectedScanCount"`
	ExpectedSelectedProjectCount *int                               `yaml:"expectedSelectedProjectCount"`
	ExpectedExplicitSessionCount *int                               `yaml:"expectedExplicitSessionCount"`
	InitialSelection             config.SelectionConfig             `yaml:"initialSelection"`
	Paths                        []mountedLegacySelectedPathFixture `yaml:"paths"`
	Stored                       []mountedLegacyStoredFixture       `yaml:"stored"`
	Scan                         []mountedLegacyListingFixture      `yaml:"scan"`
	ExpectedProjects             []mountedLegacyProjectFixture      `yaml:"expectedProjects"`
	ExpectedSessions             []mountedLegacySessionsFixture     `yaml:"expectedSessions"`
	ExpectedClearPathKeys        []string                           `yaml:"expectedClearPathKeys"`
	Journeys                     []mountedLegacySelectedJourney     `yaml:"journeys"`
}

type mountedLegacySelectedJourney struct {
	Name         string                      `yaml:"name"`
	Action       mountedLegacySelectedAction `yaml:"action"`
	ExpectPrompt *bool                       `yaml:"expectPrompt"`
	ExpectCommit *bool                       `yaml:"expectCommit"`
	ExpectExit   *bool                       `yaml:"expectExit"`
	Rerun        *bool                       `yaml:"rerun"`
}

func loadMountedLegacySelectedDocument(t *testing.T) mountedLegacySelectedDocument {
	t.Helper()
	var document mountedLegacySelectedDocument
	if err := decodeMountedLegacySelectedFixture(mountedLegacySelectedConversionData, &document); err != nil {
		t.Fatalf("decode kickstart_legacy_selected_conversion_mount.yaml: %v", err)
	}
	if document.ExpectedScenarioCount != len(document.Scenarios) || document.ExpectedScenarioCount == 0 {
		t.Fatalf("expectedScenarioCount=%d but %d scenarios are present", document.ExpectedScenarioCount, len(document.Scenarios))
	}
	if len(document.ExpectedScenarioNames) != document.ExpectedScenarioCount {
		t.Fatalf("expectedScenarioNames has %d names, want %d", len(document.ExpectedScenarioNames), document.ExpectedScenarioCount)
	}
	if len(document.ExpectedJourneyNames) != document.ExpectedJourneyCount {
		t.Fatalf("expectedJourneyNames has %d names, want %d", len(document.ExpectedJourneyNames), document.ExpectedJourneyCount)
	}

	seenScenarios := map[string]struct{}{}
	seenJourneys := map[string]struct{}{}
	actualScenarioNames := make([]string, 0, len(document.Scenarios))
	actualJourneyNames := make([]string, 0, document.ExpectedJourneyCount)
	for _, scenario := range document.Scenarios {
		validateMountedLegacySelectedScenario(t, scenario, seenScenarios, seenJourneys)
		actualScenarioNames = append(actualScenarioNames, scenario.Name)
		for _, journey := range scenario.Journeys {
			actualJourneyNames = append(actualJourneyNames, scenario.Name+"/"+journey.Name)
		}
	}
	if !reflect.DeepEqual(actualScenarioNames, document.ExpectedScenarioNames) {
		t.Fatalf("mounted selected scenario names = %v, want exact manifest %v", actualScenarioNames, document.ExpectedScenarioNames)
	}
	if !reflect.DeepEqual(actualJourneyNames, document.ExpectedJourneyNames) {
		t.Fatalf("mounted selected journey names = %v, want exact manifest %v", actualJourneyNames, document.ExpectedJourneyNames)
	}
	return document
}

func decodeMountedLegacySelectedFixture(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("fixture contains a second YAML document")
		}
		return err
	}
	return nil
}

func validateMountedLegacySelectedScenario(
	t *testing.T,
	scenario mountedLegacySelectedScenario,
	seenScenarios map[string]struct{},
	seenJourneys map[string]struct{},
) {
	t.Helper()
	if scenario.Name == "" || scenario.ExpectedStoredCount == nil || scenario.ExpectedScanCount == nil ||
		scenario.ExpectedSelectedProjectCount == nil || scenario.ExpectedExplicitSessionCount == nil {
		t.Fatalf("mounted selected scenario has missing required fields: %+v", scenario)
	}
	if _, duplicate := seenScenarios[scenario.Name]; duplicate {
		t.Fatalf("mounted selected fixture repeats scenario %q", scenario.Name)
	}
	seenScenarios[scenario.Name] = struct{}{}
	if *scenario.ExpectedStoredCount != len(scenario.Stored) || *scenario.ExpectedScanCount != len(scenario.Scan) {
		t.Fatalf("scenario %q count mismatch: stored=%d/%d scan=%d/%d", scenario.Name, len(scenario.Stored), *scenario.ExpectedStoredCount, len(scenario.Scan), *scenario.ExpectedScanCount)
	}
	if *scenario.ExpectedSelectedProjectCount != len(scenario.ExpectedProjects) {
		t.Fatalf("scenario %q expectedSelectedProjectCount=%d but %d projects are present", scenario.Name, *scenario.ExpectedSelectedProjectCount, len(scenario.ExpectedProjects))
	}
	if *scenario.ExpectedExplicitSessionCount != mountedLegacyExpectedSessionCount(scenario.ExpectedSessions) {
		t.Fatalf("scenario %q expectedExplicitSessionCount=%d but %d sessions are present", scenario.Name, *scenario.ExpectedExplicitSessionCount, mountedLegacyExpectedSessionCount(scenario.ExpectedSessions))
	}
	if scenario.InitialSelection.Mode != config.SelectionModeSelected || !mountedSelectionHasPathlessProject(scenario.InitialSelection) {
		t.Fatalf("scenario %q must start with selected-mode pathless project rules", scenario.Name)
	}
	if len(scenario.Paths) == 0 || len(scenario.Stored) == 0 || len(scenario.Scan) == 0 || len(scenario.ExpectedClearPathKeys) == 0 || len(scenario.Journeys) == 0 {
		t.Fatalf("scenario %q must include paths, store rows, scanner rows, clear-path controls, and journeys", scenario.Name)
	}

	pathStates := make(map[string]mountedLegacyPathState, len(scenario.Paths))
	pathFixtures := make(map[string]mountedLegacySelectedPathFixture, len(scenario.Paths))
	for _, path := range scenario.Paths {
		if path.Key == "" || filepath.IsAbs(path.Key) || filepath.Clean(path.Key) != path.Key || strings.HasPrefix(path.Key, "..") {
			t.Fatalf("scenario %q has unsafe path key %q", scenario.Name, path.Key)
		}
		if path.State != mountedLegacySelectedPathDirectory && path.State != mountedLegacySelectedPathSymlink {
			t.Fatalf("scenario %q path %q has invalid state %q", scenario.Name, path.Key, path.State)
		}
		if _, duplicate := pathStates[path.Key]; duplicate {
			t.Fatalf("scenario %q repeats path key %q", scenario.Name, path.Key)
		}
		if path.State == mountedLegacySelectedPathDirectory && path.TargetKey != "" {
			t.Fatalf("scenario %q directory %q cannot have symlink target %q", scenario.Name, path.Key, path.TargetKey)
		}
		if path.State == mountedLegacySelectedPathSymlink && path.TargetKey == "" {
			t.Fatalf("scenario %q symlink %q has no targetKey", scenario.Name, path.Key)
		}
		pathStates[path.Key] = mountedLegacyPathDirectory
		pathFixtures[path.Key] = path
	}
	for _, path := range scenario.Paths {
		if path.State != mountedLegacySelectedPathSymlink {
			continue
		}
		target, present := pathFixtures[path.TargetKey]
		if !present || target.State != mountedLegacySelectedPathDirectory {
			t.Fatalf("scenario %q symlink %q targets unknown or non-directory path %q", scenario.Name, path.Key, path.TargetKey)
		}
	}
	for _, stored := range scenario.Stored {
		validateMountedLegacyStoredFixture(t, scenario.Name, stored, pathStates)
	}
	for _, listing := range scenario.Scan {
		if listing.SessionID == "" || !listing.Harness.IsKnown() || listing.PathKey == "" || listing.Date.IsZero() {
			t.Fatalf("scenario %q has an incomplete scanner row: %+v", scenario.Name, listing)
		}
		requireMountedLegacyPath(t, scenario.Name, pathStates, listing.PathKey)
		if pathStates[listing.PathKey] != mountedLegacyPathDirectory {
			t.Fatalf("scenario %q scanner row %q uses unavailable path %q", scenario.Name, listing.SessionID, listing.PathKey)
		}
	}
	for _, project := range scenario.ExpectedProjects {
		if !project.Harness.IsKnown() || len(project.PathKeys) == 0 {
			t.Fatalf("scenario %q has an incomplete expected project: %+v", scenario.Name, project)
		}
		for _, key := range project.PathKeys {
			requireMountedLegacyPath(t, scenario.Name, pathStates, key)
		}
	}
	for _, sessions := range scenario.ExpectedSessions {
		if !sessions.Harness.IsKnown() || len(sessions.IDs) == 0 {
			t.Fatalf("scenario %q has an incomplete expected session group: %+v", scenario.Name, sessions)
		}
	}
	for _, key := range scenario.ExpectedClearPathKeys {
		requireMountedLegacyPath(t, scenario.Name, pathStates, key)
	}
	for _, journey := range scenario.Journeys {
		fullName := scenario.Name + "/" + journey.Name
		if journey.Name == "" || !journey.Action.valid() || journey.ExpectPrompt == nil || journey.ExpectCommit == nil || journey.ExpectExit == nil || journey.Rerun == nil {
			t.Fatalf("scenario %q has an incomplete journey: %+v", scenario.Name, journey)
		}
		if _, duplicate := seenJourneys[fullName]; duplicate {
			t.Fatalf("mounted selected fixture repeats journey %q", fullName)
		}
		seenJourneys[fullName] = struct{}{}
		if *journey.Rerun && !*journey.ExpectCommit {
			t.Fatalf("journey %q requests a rerun without a committed first run", fullName)
		}
	}
}

func mountedSelectionHasPathlessProject(selection config.SelectionConfig) bool {
	for _, configured := range selection.Harnesses {
		for _, project := range configured.Projects {
			if len(project.ClonePaths) == 0 {
				return true
			}
		}
	}
	return false
}

func TestMountedLegacySelectedConversion_ConsentCancellationAndRerun(t *testing.T) {
	document := loadMountedLegacySelectedDocument(t)
	for _, scenario := range document.Scenarios {
		scenario := scenario
		for _, journey := range scenario.Journeys {
			journey := journey
			t.Run(scenario.Name+"/"+journey.Name, func(t *testing.T) {
				paths := materializeMountedLegacySelectedPaths(t, scenario.Paths)
				dataHome := t.TempDir()
				seedMountedLegacyStore(t, dataHome, scenario.Stored, paths)
				configPath := filepath.Join(t.TempDir(), "config.yaml")
				baseline := config.BaseConfig()
				baseline.Selection = scenario.InitialSelection
				if err := config.SaveAtomic(configPath, baseline); err != nil {
					t.Fatalf("save selected legacy config: %v", err)
				}
				before := mountedLegacyReadFile(t, configPath)
				listings := mountedLegacyListings(scenario.Scan, paths)

				mounted, ingestCalls := runMountedLegacySelectedJourney(
					t,
					dataHome,
					configPath,
					listings,
					before,
					journey,
				)
				if mounted.Program().Committed() != *journey.ExpectCommit {
					t.Fatalf("committed=%t, want %t", mounted.Program().Committed(), *journey.ExpectCommit)
				}
				if mounted.Program().Exited() != *journey.ExpectExit {
					t.Fatalf("exited=%t, want %t", mounted.Program().Exited(), *journey.ExpectExit)
				}
				wantIngestCalls := 0
				if *journey.ExpectCommit {
					wantIngestCalls = 1
				}
				if ingestCalls != wantIngestCalls {
					t.Fatalf("ingest calls=%d, want %d", ingestCalls, wantIngestCalls)
				}

				if !*journey.ExpectCommit {
					after := mountedLegacyReadFile(t, configPath)
					if !bytes.Equal(after, before) {
						t.Fatalf("cancelled selected migration changed config bytes\n before: %s\n after: %s", before, after)
					}
					return
				}

				committedBytes := mountedLegacyReadFile(t, configPath)
				committed, err := config.Parse(committedBytes)
				if err != nil {
					t.Fatalf("parse committed selected migration: %v", err)
				}
				want := mountedLegacySelectedExpectedSelection(scenario, paths)
				if !reflect.DeepEqual(committed.Selection, want) {
					t.Fatalf("committed selected migration mismatch\n got: %#v\nwant: %#v", committed.Selection, want)
				}
				assertMountedLegacyClearPaths(t, committed.Selection, scenario.ExpectedClearPathKeys, paths)
				if mountedSelectionHasPathlessProject(committed.Selection) {
					t.Fatal("committed selected migration retained a pathless project rule")
				}
				if *journey.Rerun {
					runMountedLegacySelectedExactRerun(t, dataHome, configPath, listings, committedBytes)
				}
			})
		}
	}
}

func materializeMountedLegacySelectedPaths(
	t *testing.T,
	fixtures []mountedLegacySelectedPathFixture,
) map[string]string {
	t.Helper()
	root := t.TempDir()
	paths := make(map[string]string, len(fixtures))
	for _, fixture := range fixtures {
		paths[fixture.Key] = filepath.Join(root, fixture.Key)
	}
	for _, fixture := range fixtures {
		if fixture.State != mountedLegacySelectedPathDirectory {
			continue
		}
		if err := os.MkdirAll(paths[fixture.Key], 0o755); err != nil {
			t.Fatalf("create mounted selected directory %q: %v", fixture.Key, err)
		}
	}
	for _, fixture := range fixtures {
		if fixture.State != mountedLegacySelectedPathSymlink {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(paths[fixture.Key]), 0o755); err != nil {
			t.Fatalf("create mounted selected symlink parent %q: %v", fixture.Key, err)
		}
		if err := os.Symlink(paths[fixture.TargetKey], paths[fixture.Key]); err != nil {
			t.Fatalf("create mounted selected symlink %q -> %q: %v", fixture.Key, fixture.TargetKey, err)
		}
	}
	return paths
}

func runMountedLegacySelectedJourney(
	t *testing.T,
	dataHome string,
	configPath string,
	listings []ftue.SessionListing,
	before []byte,
	journey mountedLegacySelectedJourney,
) (kickstart.Model, int) {
	t.Helper()
	var mounted kickstart.Model
	ingestCalls := 0
	deps := defaultKickstartCommandDeps()
	deps.flowIngest = func(context.Context) (*ftue.IngestResult, error) {
		ingestCalls++
		return &ftue.IngestResult{}, nil
	}
	deps.runModel = func(model tea.Model) error {
		var ok bool
		mounted, ok = model.(kickstart.Model)
		if !ok {
			return fmt.Errorf("runKickstartFlow mounted %T, want kickstart.Model", model)
		}
		mounted = startKickstartGateModel(t, mounted)
		if current := mountedLegacyReadFile(t, configPath); !bytes.Equal(current, before) {
			return fmt.Errorf("selected legacy config changed before consent\n before: %s\ncurrent: %s", before, current)
		}
		mounted = advanceKickstartGateModelToReceipt(t, mounted)
		if !mounted.Program().OnReceipt() {
			return fmt.Errorf("selected legacy flow did not reach review and save")
		}
		mounted = updateKickstartGateModel(t, mounted, commitGateKeyEnter)
		if mounted.Program().ConfirmingNoProjects() != *journey.ExpectPrompt {
			return fmt.Errorf("no-project confirmation visible=%t, want %t", mounted.Program().ConfirmingNoProjects(), *journey.ExpectPrompt)
		}
		switch journey.Action {
		case mountedLegacySelectedCommit:
			mounted = updateKickstartGateModel(t, mounted, commitGateKeyEnter)
		case mountedLegacySelectedNo:
			mounted = updateKickstartGateModel(t, mounted, commitGateKeyEnter)
		case mountedLegacySelectedBack:
			mounted = updateKickstartGateModel(t, mounted, commitGateKeyBack)
		case mountedLegacySelectedQuit:
			mounted = updateKickstartGateModel(t, mounted, commitGateKeyCancel)
		case mountedLegacySelectedCancel:
			mounted = updateKickstartGateModel(t, mounted, commitGateKeyInterrupt)
		default:
			return fmt.Errorf("unsupported selected migration journey action %q", journey.Action)
		}
		return nil
	}
	if err := runKickstartFlow(mountTestCmd(t, dataHome), deps, configPath, nil, listings, nil); err != nil {
		t.Fatalf("run mounted selected legacy flow: %v", err)
	}
	return mounted, ingestCalls
}

func mountedLegacySelectedExpectedSelection(
	scenario mountedLegacySelectedScenario,
	paths map[string]string,
) config.SelectionConfig {
	selection := config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: scenario.InitialSelection.AutoIngestNewBranches,
	}
	if len(scenario.ExpectedProjects) > 0 || len(scenario.ExpectedSessions) > 0 {
		selection.Harnesses = map[string]config.SelectionHarnessConfig{}
	}
	for _, fixture := range scenario.ExpectedProjects {
		configured := selection.Harnesses[fixture.Harness.String()]
		project := config.ProjectSelection{
			GitRemote: fixture.GitRemote,
			Name:      fixture.Name,
			Branches:  append([]string(nil), fixture.Branches...),
		}
		for _, key := range fixture.PathKeys {
			project.ClonePaths = append(project.ClonePaths, paths[key])
		}
		configured.Projects = append(configured.Projects, project)
		selection.Harnesses[fixture.Harness.String()] = configured
	}
	for _, fixture := range scenario.ExpectedSessions {
		configured := selection.Harnesses[fixture.Harness.String()]
		configured.Sessions = append(configured.Sessions, fixture.IDs...)
		selection.Harnesses[fixture.Harness.String()] = configured
	}
	return selection
}

func runMountedLegacySelectedExactRerun(
	t *testing.T,
	dataHome string,
	configPath string,
	listings []ftue.SessionListing,
	before []byte,
) {
	t.Helper()
	journey := mountedLegacySelectedJourney{
		Name:         "exact-rerun",
		Action:       mountedLegacySelectedCommit,
		ExpectPrompt: mountedLegacySelectedBool(false),
		ExpectCommit: mountedLegacySelectedBool(true),
		ExpectExit:   mountedLegacySelectedBool(false),
		Rerun:        mountedLegacySelectedBool(false),
	}
	mounted, ingestCalls := runMountedLegacySelectedJourney(t, dataHome, configPath, listings, before, journey)
	if !mounted.Program().Committed() || ingestCalls != 1 {
		t.Fatalf("exact rerun committed=%t ingest calls=%d, want true/1", mounted.Program().Committed(), ingestCalls)
	}
	after := mountedLegacyReadFile(t, configPath)
	if !bytes.Equal(after, before) {
		t.Fatalf("exact selected rerun changed config bytes\n before: %s\n after: %s", before, after)
	}
}

func mountedLegacySelectedBool(value bool) *bool { return &value }

func TestMountedLegacySelectedFixtureRejectsUnknownInitialSelectionKey(t *testing.T) {
	malformed := bytes.Replace(
		mountedLegacySelectedConversionData,
		[]byte("initialSelection:"),
		[]byte("initialSelectionTypo:"),
		1,
	)
	if bytes.Equal(malformed, mountedLegacySelectedConversionData) {
		t.Fatal("mounted selected conversion fixture has no initialSelection key to mutate")
	}
	var document mountedLegacySelectedDocument
	if err := decodeMountedLegacySelectedFixture(malformed, &document); err == nil {
		t.Fatal("mounted selected conversion fixture decoder accepted an unknown initial selection key")
	}
}
