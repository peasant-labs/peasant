package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"gopkg.in/yaml.v3"
)

const mountedLegacyConversionFixtureCaseCount = 2

//go:embed testdata/kickstart_legacy_conversion_mount.yaml
var mountedLegacyConversionFixtureData []byte

type mountedLegacyPathState string

const (
	mountedLegacyPathDirectory mountedLegacyPathState = "directory"
	mountedLegacyPathMissing   mountedLegacyPathState = "missing"
)

type mountedLegacyConversionDocument struct {
	ExpectedCaseCount int                           `yaml:"expectedCaseCount"`
	Cases             []mountedLegacyConversionCase `yaml:"cases"`
}

type mountedLegacyConversionCase struct {
	Name                          string                         `yaml:"name"`
	ExpectedStoredCount           *int                           `yaml:"expectedStoredCount"`
	ExpectedScanCount             *int                           `yaml:"expectedScanCount"`
	ExpectedSelectedProjectCount  *int                           `yaml:"expectedSelectedProjectCount"`
	ExpectedUnmatchedSessionCount *int                           `yaml:"expectedUnmatchedSessionCount"`
	AutoIngestNewBranches         *bool                          `yaml:"autoIngestNewBranches"`
	Paths                         []mountedLegacyPathFixture     `yaml:"paths"`
	Stored                        []mountedLegacyStoredFixture   `yaml:"stored"`
	Scan                          []mountedLegacyListingFixture  `yaml:"scan"`
	ExpectedProjects              []mountedLegacyProjectFixture  `yaml:"expectedProjects"`
	ExpectedSessions              []mountedLegacySessionsFixture `yaml:"expectedSessions"`
	ExpectedClearPathKeys         []string                       `yaml:"expectedClearPathKeys"`
}

type mountedLegacyPathFixture struct {
	Key   string                 `yaml:"key"`
	State mountedLegacyPathState `yaml:"state"`
}

type mountedLegacyStoredFixture struct {
	SessionID           string           `yaml:"sessionId"`
	Harness             defaults.Harness `yaml:"harness"`
	ProjectHash         string           `yaml:"projectHash"`
	HostSlug            string           `yaml:"hostSlug"`
	GitRemote           string           `yaml:"gitRemote"`
	Branch              string           `yaml:"branch"`
	GitWorktreePathKey  string           `yaml:"gitWorktreePathKey"`
	CanonicalCwdPathKey string           `yaml:"canonicalCwdPathKey"`
	Title               string           `yaml:"title"`
	IngestedMs          int64            `yaml:"ingestedMs"`
}

type mountedLegacyListingFixture struct {
	SessionID   string           `yaml:"sessionId"`
	Harness     defaults.Harness `yaml:"harness"`
	ProjectName string           `yaml:"projectName"`
	GitRemote   string           `yaml:"gitRemote"`
	PathKey     string           `yaml:"pathKey"`
	Branch      string           `yaml:"branch"`
	Title       string           `yaml:"title"`
	Date        time.Time        `yaml:"date"`
}

type mountedLegacyProjectFixture struct {
	Harness   defaults.Harness `yaml:"harness"`
	GitRemote string           `yaml:"gitRemote"`
	Name      string           `yaml:"name"`
	PathKeys  []string         `yaml:"pathKeys"`
	Branches  []string         `yaml:"branches"`
}

type mountedLegacySessionsFixture struct {
	Harness defaults.Harness `yaml:"harness"`
	IDs     []string         `yaml:"ids"`
}

func loadMountedLegacyConversionDocument(t *testing.T) mountedLegacyConversionDocument {
	t.Helper()
	var document mountedLegacyConversionDocument
	decoder := yaml.NewDecoder(bytes.NewReader(mountedLegacyConversionFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode testdata/kickstart_legacy_conversion_mount.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("kickstart_legacy_conversion_mount.yaml must hold exactly one document: %v", err)
	}
	if document.ExpectedCaseCount != mountedLegacyConversionFixtureCaseCount || len(document.Cases) != mountedLegacyConversionFixtureCaseCount {
		t.Fatalf(
			"mounted legacy conversion fixture declares %d cases and contains %d, want exactly %d",
			document.ExpectedCaseCount,
			len(document.Cases),
			mountedLegacyConversionFixtureCaseCount,
		)
	}
	seen := map[string]struct{}{}
	for _, testCase := range document.Cases {
		validateMountedLegacyConversionCase(t, testCase, seen)
	}
	return document
}

func validateMountedLegacyConversionCase(
	t *testing.T,
	testCase mountedLegacyConversionCase,
	seen map[string]struct{},
) {
	t.Helper()
	testutil.RequireFixtureFields(t, "mounted legacy conversion", testCase.Name, []testutil.FixtureField{
		{Key: "name", Value: testCase.Name},
		{Key: "expectedStoredCount", Value: mountedLegacyIntValue(testCase.ExpectedStoredCount)},
		{Key: "expectedScanCount", Value: mountedLegacyIntValue(testCase.ExpectedScanCount)},
		{Key: "expectedSelectedProjectCount", Value: mountedLegacyIntValue(testCase.ExpectedSelectedProjectCount)},
		{Key: "expectedUnmatchedSessionCount", Value: mountedLegacyIntValue(testCase.ExpectedUnmatchedSessionCount)},
		{Key: "autoIngestNewBranches", Value: mountedLegacyBoolValue(testCase.AutoIngestNewBranches)},
	})
	if _, duplicate := seen[testCase.Name]; duplicate {
		t.Fatalf("mounted legacy conversion fixture repeats case name %q", testCase.Name)
	}
	seen[testCase.Name] = struct{}{}
	if *testCase.ExpectedStoredCount != len(testCase.Stored) {
		t.Fatalf("case %q expectedStoredCount=%d but has %d stored rows", testCase.Name, *testCase.ExpectedStoredCount, len(testCase.Stored))
	}
	if *testCase.ExpectedScanCount != len(testCase.Scan) || len(testCase.Scan) == 0 {
		t.Fatalf("case %q expectedScanCount=%d but has %d scanner rows", testCase.Name, *testCase.ExpectedScanCount, len(testCase.Scan))
	}
	if *testCase.ExpectedSelectedProjectCount != len(testCase.ExpectedProjects) || len(testCase.ExpectedProjects) == 0 {
		t.Fatalf("case %q expectedSelectedProjectCount=%d but has %d expected projects", testCase.Name, *testCase.ExpectedSelectedProjectCount, len(testCase.ExpectedProjects))
	}
	if *testCase.ExpectedUnmatchedSessionCount != mountedLegacyExpectedSessionCount(testCase.ExpectedSessions) {
		t.Fatalf(
			"case %q expectedUnmatchedSessionCount=%d but has %d expected explicit sessions",
			testCase.Name,
			*testCase.ExpectedUnmatchedSessionCount,
			mountedLegacyExpectedSessionCount(testCase.ExpectedSessions),
		)
	}
	if len(testCase.Paths) == 0 || len(testCase.ExpectedClearPathKeys) == 0 {
		t.Fatalf("case %q must define physical paths and at least one path that stays clear", testCase.Name)
	}

	pathStates := make(map[string]mountedLegacyPathState, len(testCase.Paths))
	for _, path := range testCase.Paths {
		if path.Key == "" || filepath.IsAbs(path.Key) || filepath.Clean(path.Key) != path.Key || strings.HasPrefix(path.Key, "..") {
			t.Fatalf("case %q has unsafe path key %q", testCase.Name, path.Key)
		}
		if path.State != mountedLegacyPathDirectory && path.State != mountedLegacyPathMissing {
			t.Fatalf("case %q path %q has unknown state %q", testCase.Name, path.Key, path.State)
		}
		if _, duplicate := pathStates[path.Key]; duplicate {
			t.Fatalf("case %q repeats path key %q", testCase.Name, path.Key)
		}
		pathStates[path.Key] = path.State
	}
	for _, stored := range testCase.Stored {
		validateMountedLegacyStoredFixture(t, testCase.Name, stored, pathStates)
	}
	for _, listing := range testCase.Scan {
		if listing.SessionID == "" || !listing.Harness.IsKnown() || listing.ProjectName == "" || listing.PathKey == "" || listing.Date.IsZero() {
			t.Fatalf("case %q has an incomplete scanner row for session %q", testCase.Name, listing.SessionID)
		}
		requireMountedLegacyPath(t, testCase.Name, pathStates, listing.PathKey)
		if pathStates[listing.PathKey] != mountedLegacyPathDirectory {
			t.Fatalf("case %q scanner row %q uses unavailable path %q", testCase.Name, listing.SessionID, listing.PathKey)
		}
	}
	for _, expected := range testCase.ExpectedProjects {
		if !expected.Harness.IsKnown() || len(expected.PathKeys) == 0 {
			t.Fatalf("case %q has an incomplete expected project for harness %q", testCase.Name, expected.Harness)
		}
		for _, key := range expected.PathKeys {
			requireMountedLegacyPath(t, testCase.Name, pathStates, key)
		}
	}
	for _, expected := range testCase.ExpectedSessions {
		if !expected.Harness.IsKnown() || len(expected.IDs) == 0 {
			t.Fatalf("case %q has an incomplete expected explicit-session group", testCase.Name)
		}
	}
	for _, key := range testCase.ExpectedClearPathKeys {
		requireMountedLegacyPath(t, testCase.Name, pathStates, key)
	}
}

func validateMountedLegacyStoredFixture(
	t *testing.T,
	caseName string,
	fixture mountedLegacyStoredFixture,
	pathStates map[string]mountedLegacyPathState,
) {
	t.Helper()
	if fixture.SessionID == "" || !fixture.Harness.IsKnown() || fixture.ProjectHash == "" || fixture.HostSlug == "" || fixture.IngestedMs <= 0 {
		t.Fatalf("case %q has an incomplete stored row for session %q", caseName, fixture.SessionID)
	}
	if _, err := ingest.NewSessionID(fixture.SessionID); err != nil {
		t.Fatalf("case %q has invalid stored session ID %q: %v", caseName, fixture.SessionID, err)
	}
	if _, err := ingest.NewProjectHash(fixture.ProjectHash); err != nil {
		t.Fatalf("case %q has invalid project hash for session %q: %v", caseName, fixture.SessionID, err)
	}
	if _, err := ingest.NewHostSlug(fixture.HostSlug); err != nil {
		t.Fatalf("case %q has invalid host slug for session %q: %v", caseName, fixture.SessionID, err)
	}
	if fixture.GitWorktreePathKey != "" {
		requireMountedLegacyPath(t, caseName, pathStates, fixture.GitWorktreePathKey)
	}
	if fixture.CanonicalCwdPathKey != "" {
		requireMountedLegacyPath(t, caseName, pathStates, fixture.CanonicalCwdPathKey)
	}
}

func requireMountedLegacyPath(t *testing.T, caseName string, paths map[string]mountedLegacyPathState, key string) {
	t.Helper()
	if _, ok := paths[key]; !ok {
		t.Fatalf("case %q references unknown path key %q", caseName, key)
	}
}

func mountedLegacyIntValue(value *int) string {
	if value == nil {
		return ""
	}
	return strconv.Itoa(*value)
}

func mountedLegacyBoolValue(value *bool) string {
	if value == nil {
		return ""
	}
	return strconv.FormatBool(*value)
}

func mountedLegacyExpectedSessionCount(groups []mountedLegacySessionsFixture) int {
	count := 0
	for _, group := range groups {
		count += len(group.IDs)
	}
	return count
}

func TestMountedLegacyConversionFixtureRejectsUnknownStoredIdentityKey(t *testing.T) {
	malformed := bytes.Replace(
		mountedLegacyConversionFixtureData,
		[]byte("gitWorktreePathKey:"),
		[]byte("gitWorktreePathTypo:"),
		1,
	)
	if bytes.Equal(malformed, mountedLegacyConversionFixtureData) {
		t.Fatal("mounted legacy conversion fixture has no gitWorktreePathKey to mutate")
	}
	var document mountedLegacyConversionDocument
	decoder := yaml.NewDecoder(bytes.NewReader(malformed))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err == nil {
		t.Fatal("mounted legacy conversion fixture decoder accepted an unknown stored identity key")
	}
}

func TestRunKickstartFlowLegacyAllRejectsUnreadableStoredEvidence(t *testing.T) {
	dataHome := t.TempDir()
	dbPath := defaults.ResolveDBFilePathWith(dataHome).String()
	if err := os.MkdirAll(filepath.Dir(dbPath), defaults.PrivateDirPerm); err != nil {
		t.Fatalf("create unreadable-store parent: %v", err)
	}
	if err := os.WriteFile(dbPath, []byte("not a sqlite database"), defaults.PrivateFilePerm); err != nil {
		t.Fatalf("write invalid store: %v", err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	legacy := config.BaseConfig()
	legacy.Selection = config.SelectionConfig{Mode: config.SelectionModeAll, AutoIngestNewBranches: true}
	if err := config.SaveAtomic(configPath, legacy); err != nil {
		t.Fatalf("save legacy config: %v", err)
	}
	before := mountedLegacyReadFile(t, configPath)
	runnerCalled := false
	err := runKickstartFlow(
		mountTestCmd(t, dataHome),
		kickstartCommandDeps{runFlow: func(tea.Model) error {
			runnerCalled = true
			return nil
		}},
		configPath,
		nil,
		nil,
	)
	if err == nil {
		t.Fatal("legacy conversion opened the selection flow without complete stored evidence")
	}
	if runnerCalled {
		t.Fatal("legacy conversion started the selection flow after the existing store failed to open")
	}
	if after := mountedLegacyReadFile(t, configPath); !bytes.Equal(after, before) {
		t.Fatalf("legacy config changed after the existing store failed to open\n before: %s\n after: %s", before, after)
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "what:") || !strings.Contains(message, "why:") ||
		!strings.Contains(message, "where:") || !strings.Contains(message, "when:") ||
		!strings.Contains(message, "meaning:") || !strings.Contains(message, "fix:") {
		t.Fatalf("unreadable-store error is not actionable: %v", err)
	}
}

// TestRunKickstartFlowConvertsLegacyAllFromStoredEvidence drives the real
// runKickstartFlow mount through its production store reader, physical path
// resolver, scanner, settings.Flow, and atomic user commit. The YAML corpus
// keeps the identity combinations out of Go code.
func TestRunKickstartFlowConvertsLegacyAllFromStoredEvidence(t *testing.T) {
	document := loadMountedLegacyConversionDocument(t)
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			paths := materializeMountedLegacyPaths(t, testCase.Paths)
			dataHome := t.TempDir()
			seedMountedLegacyStore(t, dataHome, testCase.Stored, paths)

			configPath := filepath.Join(t.TempDir(), "config.yaml")
			baseline := config.BaseConfig()
			baseline.Selection = config.SelectionConfig{
				Mode:                  config.SelectionModeAll,
				AutoIngestNewBranches: *testCase.AutoIngestNewBranches,
			}
			if err := config.SaveAtomic(configPath, baseline); err != nil {
				t.Fatalf("save legacy mode-all config: %v", err)
			}
			before := mountedLegacyReadFile(t, configPath)
			listings := mountedLegacyListings(testCase.Scan, paths)
			cmd := mountTestCmd(t, dataHome)
			committed := false
			deps := kickstartCommandDeps{
				runFlow: func(model tea.Model) error {
					mounted, ok := model.(kickstart.Model)
					if !ok {
						return fmt.Errorf("runKickstartFlow mounted %T, want kickstart.Model", model)
					}
					program := mounted.Program()
					program.SetSize(120, 30)
					var load tea.Cmd
					if program.Phase() == kickstart.PhaseOAuth {
						program, load = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
					} else {
						load = program.Init()
					}
					program = drainMountedLegacyProgram(t, program, load)
					if program.Phase() != kickstart.PhaseFlow {
						return fmt.Errorf("mounted kickstart phase after scanner load = %s, want flow", program.Phase())
					}
					if current := mountedLegacyReadFile(t, configPath); !bytes.Equal(current, before) {
						return fmt.Errorf("legacy config changed before the user commit\n before: %s\ncurrent: %s", before, current)
					}
					for range 8 {
						program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyTab})
					}
					program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
					if !program.Committed() {
						return fmt.Errorf("mounted kickstart did not commit the converted selection; phase=%s", program.Phase())
					}
					committed = true
					return nil
				},
			}

			if err := runKickstartFlow(cmd, deps, configPath, nil, listings); err != nil {
				t.Fatalf("run mounted kickstart flow: %v", err)
			}
			if !committed {
				t.Fatal("mounted flow runner returned without a user commit")
			}
			after := mountedLegacyReadFile(t, configPath)
			if bytes.Equal(after, before) {
				t.Fatal("user commit left the legacy mode-all file unchanged")
			}
			got, err := config.Parse(after)
			if err != nil {
				t.Fatalf("parse committed converted config: %v", err)
			}
			want := mountedLegacyExpectedSelection(testCase, paths)
			if !reflect.DeepEqual(got.Selection, want) {
				t.Fatalf("committed mounted conversion mismatch\n got: %#v\nwant: %#v", got.Selection, want)
			}
			assertMountedLegacyClearPaths(t, got.Selection, testCase.ExpectedClearPathKeys, paths)
		})
	}
}

func materializeMountedLegacyPaths(t *testing.T, fixtures []mountedLegacyPathFixture) map[string]string {
	t.Helper()
	root := t.TempDir()
	paths := make(map[string]string, len(fixtures))
	for _, fixture := range fixtures {
		path := filepath.Join(root, fixture.Key)
		paths[fixture.Key] = path
		if fixture.State == mountedLegacyPathDirectory {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("create mounted legacy path %q: %v", fixture.Key, err)
			}
		}
	}
	return paths
}

func seedMountedLegacyStore(
	t *testing.T,
	dataHome string,
	fixtures []mountedLegacyStoredFixture,
	paths map[string]string,
) {
	t.Helper()
	dbPath := defaults.ResolveDBFilePathWith(dataHome).String()
	if err := os.MkdirAll(filepath.Dir(dbPath), defaults.PrivateDirPerm); err != nil {
		t.Fatalf("create mounted legacy data directory: %v", err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open mounted legacy store: %v", err)
	}
	for index, fixture := range fixtures {
		entry := mountedLegacyStoreEntry(t, fixture, paths, index)
		if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
			_ = db.Close()
			t.Fatalf("insert mounted legacy session %q: %v", fixture.SessionID, err)
		}
		if fixture.Title != "" {
			title := fixture.Title
			metrics := ingest.SessionMetrics{SessionID: entry.Metadata.SessionID}
			metrics.TitleGenerated = &title
			if err := db.SaveMetrics(t.Context(), &metrics); err != nil {
				_ = db.Close()
				t.Fatalf("save mounted legacy title for %q: %v", fixture.SessionID, err)
			}
		}
	}
	assertMountedLegacyStoredRows(t, db, fixtures, paths)
	if err := db.Close(); err != nil {
		t.Fatalf("close mounted legacy seed store: %v", err)
	}
}

func assertMountedLegacyStoredRows(
	t *testing.T,
	db *store.Store,
	fixtures []mountedLegacyStoredFixture,
	paths map[string]string,
) {
	t.Helper()
	rows, err := db.AllIngestedSessions(t.Context())
	if err != nil {
		t.Fatalf("read mounted legacy stored evidence: %v", err)
	}
	if len(rows) != len(fixtures) {
		t.Fatalf("mounted legacy store returned %d rows, want %d fixture rows", len(rows), len(fixtures))
	}
	byID := make(map[string]store.IngestedSessionRow, len(rows))
	for _, row := range rows {
		byID[row.SessionID] = row
	}
	for _, fixture := range fixtures {
		row, ok := byID[fixture.SessionID]
		if !ok {
			t.Fatalf("mounted legacy store omitted session %q", fixture.SessionID)
		}
		want := store.IngestedSessionRow{
			SessionID:     fixture.SessionID,
			Harness:       fixture.Harness.String(),
			GitRemote:     fixture.GitRemote,
			Branch:        fixture.Branch,
			GitWorktree:   mountedLegacyOptionalPath(paths, fixture.GitWorktreePathKey),
			CanonicalCwd:  mountedLegacyOptionalPath(paths, fixture.CanonicalCwdPathKey),
			Title:         fixture.Title,
			IngestedMs:    fixture.IngestedMs,
			SchemaVersion: ingest.CurrentSchemaVersion,
		}
		if row != want {
			t.Fatalf("mounted legacy stored row for %q = %+v, want %+v", fixture.SessionID, row, want)
		}
	}
}

func mountedLegacyStoreEntry(
	t *testing.T,
	fixture mountedLegacyStoredFixture,
	paths map[string]string,
	index int,
) ingest.StoreEntry {
	t.Helper()
	sessionID, err := ingest.NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatalf("construct mounted legacy session ID: %v", err)
	}
	projectHash, err := ingest.NewProjectHash(fixture.ProjectHash)
	if err != nil {
		t.Fatalf("construct mounted legacy project hash: %v", err)
	}
	hostSlug, err := ingest.NewHostSlug(fixture.HostSlug)
	if err != nil {
		t.Fatalf("construct mounted legacy host slug: %v", err)
	}
	model, err := ingest.NewModelID("fixture-model")
	if err != nil {
		t.Fatalf("construct mounted legacy model: %v", err)
	}
	sourcePath, err := ingest.NewResolvedPath(filepath.Join(t.TempDir(), fixture.SessionID+".jsonl"))
	if err != nil {
		t.Fatalf("construct mounted legacy source path: %v", err)
	}
	remote := fixture.GitRemote
	branch := fixture.Branch
	worktree := mountedLegacyOptionalPath(paths, fixture.GitWorktreePathKey)
	canonical := mountedLegacyOptionalPath(paths, fixture.CanonicalCwdPathKey)
	start := fixture.IngestedMs - int64(index+1)
	return ingest.StoreEntry{Metadata: &ingest.UnifiedMetadata{
		SchemaVersion: ingest.CurrentSchemaVersion,
		SessionID:     sessionID,
		ModelHarness:  fixture.Harness,
		Model:         model,
		HostSlug:      hostSlug,
		Timestamp:     ingest.TimestampInfo{Start: start, End: fixture.IngestedMs, Ingested: &fixture.IngestedMs},
		Project:       ingest.ProjectInfo{Hash: projectHash, Name: fixture.Title, FilePath: canonical},
		Source:        ingest.SourceInfo{FilePath: string(sourcePath), Format: ingest.SourceFormatJSONL},
		Git: ingest.GitContext{
			Remote:   mountedLegacyOptionalString(remote),
			Branch:   mountedLegacyOptionalString(branch),
			Worktree: mountedLegacyOptionalString(worktree),
		},
	}}
}

func mountedLegacyOptionalPath(paths map[string]string, key string) string {
	if key == "" {
		return ""
	}
	return paths[key]
}

func mountedLegacyOptionalString(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func mountedLegacyListings(fixtures []mountedLegacyListingFixture, paths map[string]string) []ftue.SessionListing {
	listings := make([]ftue.SessionListing, 0, len(fixtures))
	for _, fixture := range fixtures {
		listings = append(listings, ftue.SessionListing{
			SessionID:   fixture.SessionID,
			Harness:     fixture.Harness.String(),
			ProjectName: fixture.ProjectName,
			GitRemote:   fixture.GitRemote,
			WorkingDir:  paths[fixture.PathKey],
			Branch:      fixture.Branch,
			Title:       fixture.Title,
			Date:        fixture.Date,
		})
	}
	return listings
}

func mountedLegacyExpectedSelection(
	testCase mountedLegacyConversionCase,
	paths map[string]string,
) config.SelectionConfig {
	selection := config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: *testCase.AutoIngestNewBranches,
		Harnesses:             map[string]config.SelectionHarnessConfig{},
	}
	for _, fixture := range testCase.ExpectedProjects {
		harness := selection.Harnesses[fixture.Harness.String()]
		project := config.ProjectSelection{
			GitRemote: fixture.GitRemote,
			Name:      fixture.Name,
			Branches:  append([]string(nil), fixture.Branches...),
		}
		for _, key := range fixture.PathKeys {
			project.ClonePaths = append(project.ClonePaths, paths[key])
		}
		harness.Projects = append(harness.Projects, project)
		selection.Harnesses[fixture.Harness.String()] = harness
	}
	for _, fixture := range testCase.ExpectedSessions {
		harness := selection.Harnesses[fixture.Harness.String()]
		harness.Sessions = append(harness.Sessions, fixture.IDs...)
		selection.Harnesses[fixture.Harness.String()] = harness
	}
	if len(selection.Harnesses) == 0 {
		selection.Harnesses = nil
	}
	return selection
}

func assertMountedLegacyClearPaths(
	t *testing.T,
	selection config.SelectionConfig,
	clearPathKeys []string,
	paths map[string]string,
) {
	t.Helper()
	selected := map[string]struct{}{}
	for _, harness := range selection.Harnesses {
		for _, project := range harness.Projects {
			for _, clonePath := range project.ClonePaths {
				selected[clonePath] = struct{}{}
			}
		}
	}
	for _, key := range clearPathKeys {
		if _, widened := selected[paths[key]]; widened {
			t.Errorf("scanner project %q started selected without exact stored path evidence", key)
		}
	}
}

func mountedLegacyReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read mounted legacy config %q: %v", path, err)
	}
	return data
}

func drainMountedLegacyProgram(t *testing.T, program kickstart.Program, cmd tea.Cmd) kickstart.Program {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for iteration := 0; len(queue) > 0 && iteration < 256; iteration++ {
		nextCommand := queue[0]
		queue = queue[1:]
		if nextCommand == nil {
			continue
		}
		message := nextCommand()
		switch typed := message.(type) {
		case tea.BatchMsg:
			queue = append(queue, typed...)
		case nil:
		default:
			var follow tea.Cmd
			program, follow = program.Update(typed)
			if follow != nil {
				queue = append(queue, follow)
			}
		}
	}
	if len(queue) != 0 {
		t.Fatalf("mounted legacy flow did not settle after 256 commands")
	}
	return program
}
