package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

const (
	kickstartStoredGateCaseCount       = 4
	kickstartStoredGateStoredRowCount  = 4
	kickstartStoredGateScannerRowCount = 1
	kickstartStoredGateHostSlug        = "github.com-example-kickstart-stored-gate"
)

type kickstartStoredGateBehavior string

const (
	kickstartStoredGateExplicit    kickstartStoredGateBehavior = "stored-explicit"
	kickstartStoredGateUnresolved  kickstartStoredGateBehavior = "stored-unresolved-explicit"
	kickstartStoredGateEmpty       kickstartStoredGateBehavior = "empty-control"
	kickstartStoredGateReadFailure kickstartStoredGateBehavior = "store-read-failure"
)

type kickstartStoredGateStoreState string

const (
	kickstartStoredGateReadable     kickstartStoredGateStoreState = "readable"
	kickstartStoredGateQueryFailure kickstartStoredGateStoreState = "query-failure"
)

type kickstartStoredGatePathSource string

const (
	kickstartStoredGateGitWorktree  kickstartStoredGatePathSource = "git-worktree"
	kickstartStoredGateCanonicalCwd kickstartStoredGatePathSource = "canonical-cwd"
)

type kickstartStoredGateOutcome string

const (
	kickstartStoredGateNone    kickstartStoredGateOutcome = "none"
	kickstartStoredGateConfirm kickstartStoredGateOutcome = "confirm-no-projects"
	kickstartStoredGateError   kickstartStoredGateOutcome = "error"
)

type kickstartStoredGateDocument struct {
	DeclaredCases       int                       `yaml:"declaredCases"`
	DeclaredStoredRows  int                       `yaml:"declaredStoredRows"`
	DeclaredScannerRows int                       `yaml:"declaredScannerRows"`
	Cases               []kickstartStoredGateCase `yaml:"cases"`
}

type kickstartStoredGateCase struct {
	Name                    string                        `yaml:"name"`
	Behavior                kickstartStoredGateBehavior   `yaml:"behavior"`
	StoreState              kickstartStoredGateStoreState `yaml:"storeState"`
	SelectedSessionID       string                        `yaml:"selectedSessionId"`
	Paths                   []kickstartGatePath           `yaml:"paths"`
	StoredRows              []kickstartStoredGateRow      `yaml:"storedRows"`
	ScannerRows             []kickstartStoredGateRow      `yaml:"scannerRows"`
	ExpectedGate            kickstartStoredGateOutcome    `yaml:"expectedGate"`
	ExpectedCandidateCount  int                           `yaml:"expectedCandidateCount"`
	ExpectedDescendantCount int                           `yaml:"expectedDescendantCount"`
	ExpectedViewerProjects  int                           `yaml:"expectedViewerProjects"`
	ExpectedViewerSessions  int                           `yaml:"expectedViewerSessions"`
	ExpectedPushSessionIDs  []string                      `yaml:"expectedPushSessionIds"`
}

type kickstartStoredGateRow struct {
	Harness     defaults.Harness              `yaml:"harness"`
	SessionID   string                        `yaml:"sessionId"`
	ProjectName string                        `yaml:"projectName"`
	GitRemote   string                        `yaml:"gitRemote"`
	Branch      string                        `yaml:"branch"`
	PathKey     string                        `yaml:"pathKey"`
	PathSource  kickstartStoredGatePathSource `yaml:"pathSource"`
}

//go:embed testdata/kickstart_stored_gate_alignment.yaml
var kickstartStoredGateFixture []byte

func loadKickstartStoredGateDocument(t *testing.T) kickstartStoredGateDocument {
	t.Helper()
	var document kickstartStoredGateDocument
	decoder := yaml.NewDecoder(bytes.NewReader(kickstartStoredGateFixture))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode stored gate alignment fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("stored gate alignment fixture must contain exactly one YAML document: %v", err)
	}
	validateKickstartStoredGateDocument(t, document)
	return document
}

func validateKickstartStoredGateDocument(t *testing.T, document kickstartStoredGateDocument) {
	t.Helper()
	if document.DeclaredCases != kickstartStoredGateCaseCount || len(document.Cases) != kickstartStoredGateCaseCount {
		t.Fatalf("stored gate fixture cases: declared=%d actual=%d required=%d", document.DeclaredCases, len(document.Cases), kickstartStoredGateCaseCount)
	}
	seenNames := make(map[string]struct{}, len(document.Cases))
	seenBehaviors := make(map[kickstartStoredGateBehavior]struct{}, len(document.Cases))
	storedRows := 0
	scannerRows := 0
	for _, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" {
			t.Fatal("stored gate fixture has an empty case name")
		}
		if _, duplicate := seenNames[testCase.Name]; duplicate {
			t.Fatalf("stored gate fixture repeats case %q", testCase.Name)
		}
		seenNames[testCase.Name] = struct{}{}
		if !testCase.Behavior.valid() || !testCase.StoreState.valid() || !testCase.ExpectedGate.valid() {
			t.Fatalf("stored gate fixture case %q has an unknown behavior, store state, or gate outcome", testCase.Name)
		}
		if _, duplicate := seenBehaviors[testCase.Behavior]; duplicate {
			t.Fatalf("stored gate fixture repeats behavior %q", testCase.Behavior)
		}
		seenBehaviors[testCase.Behavior] = struct{}{}
		if _, err := ingest.NewSessionID(testCase.SelectedSessionID); err != nil {
			t.Fatalf("stored gate fixture case %q has invalid selected session ID %q: %v", testCase.Name, testCase.SelectedSessionID, err)
		}
		pathStates := validateKickstartGatePaths(t, testCase.Name, testCase.Paths)
		validateKickstartStoredGateRows(
			t,
			testCase.Name,
			testCase.StoredRows,
			pathStates,
			true,
			testCase.Behavior == kickstartStoredGateUnresolved,
		)
		validateKickstartStoredGateRows(t, testCase.Name, testCase.ScannerRows, pathStates, false, false)
		validateKickstartStoredGateCaseSemantics(t, testCase)
		storedRows += len(testCase.StoredRows)
		scannerRows += len(testCase.ScannerRows)
	}
	if storedRows != document.DeclaredStoredRows || storedRows != kickstartStoredGateStoredRowCount {
		t.Fatalf("stored gate fixture stored rows: declared=%d actual=%d required=%d", document.DeclaredStoredRows, storedRows, kickstartStoredGateStoredRowCount)
	}
	if scannerRows != document.DeclaredScannerRows || scannerRows != kickstartStoredGateScannerRowCount {
		t.Fatalf("stored gate fixture scanner rows: declared=%d actual=%d required=%d", document.DeclaredScannerRows, scannerRows, kickstartStoredGateScannerRowCount)
	}
	for _, behavior := range []kickstartStoredGateBehavior{
		kickstartStoredGateExplicit,
		kickstartStoredGateUnresolved,
		kickstartStoredGateEmpty,
		kickstartStoredGateReadFailure,
	} {
		if _, ok := seenBehaviors[behavior]; !ok {
			t.Fatalf("stored gate fixture does not cover behavior %q", behavior)
		}
	}
}

func validateKickstartStoredGateRows(
	t *testing.T,
	caseName string,
	rows []kickstartStoredGateRow,
	pathStates map[string]commitGatePathState,
	stored bool,
	allowMissing bool,
) {
	t.Helper()
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if !row.Harness.IsKnown() || row.ProjectName == "" || row.GitRemote == "" || row.Branch == "" {
			t.Fatalf("stored gate fixture case %q has an incomplete row: %#v", caseName, row)
		}
		if _, err := ingest.NewSessionID(row.SessionID); err != nil {
			t.Fatalf("stored gate fixture case %q has invalid row session ID %q: %v", caseName, row.SessionID, err)
		}
		if _, duplicate := seen[row.SessionID]; duplicate {
			t.Fatalf("stored gate fixture case %q repeats row session ID %q", caseName, row.SessionID)
		}
		seen[row.SessionID] = struct{}{}
		pathState := pathStates[row.PathKey]
		if pathState != commitGatePathDirectory && !(allowMissing && pathState == commitGatePathMissing) {
			t.Fatalf("stored gate fixture case %q row %q uses unavailable path %q", caseName, row.SessionID, row.PathKey)
		}
		if stored {
			if row.PathSource != kickstartStoredGateGitWorktree && row.PathSource != kickstartStoredGateCanonicalCwd {
				t.Fatalf("stored gate fixture case %q row %q has unknown path source %q", caseName, row.SessionID, row.PathSource)
			}
		} else if row.PathSource != "" {
			t.Fatalf("stored gate fixture case %q scanner row %q must not define pathSource", caseName, row.SessionID)
		}
	}
}

func validateKickstartStoredGateCaseSemantics(t *testing.T, testCase kickstartStoredGateCase) {
	t.Helper()
	switch testCase.Behavior {
	case kickstartStoredGateExplicit:
		if testCase.StoreState != kickstartStoredGateReadable || testCase.ExpectedGate != kickstartStoredGateNone ||
			len(testCase.StoredRows) != 2 || len(testCase.ScannerRows) != 1 ||
			testCase.ExpectedCandidateCount != 2 || testCase.ExpectedDescendantCount != 2 ||
			testCase.ExpectedViewerProjects != 1 || testCase.ExpectedViewerSessions != 1 ||
			len(testCase.ExpectedPushSessionIDs) != 1 || testCase.ExpectedPushSessionIDs[0] != testCase.SelectedSessionID {
			t.Fatalf("stored gate fixture alignment case %q does not encode the required cross-surface outcome", testCase.Name)
		}
		if testCase.StoredRows[0].SessionID != testCase.SelectedSessionID {
			t.Fatalf("stored gate fixture alignment case %q must select its DB-only first row", testCase.Name)
		}
		if testCase.ScannerRows[0].SessionID != testCase.StoredRows[1].SessionID ||
			testCase.ScannerRows[0].PathKey != testCase.StoredRows[1].PathKey {
			t.Fatalf("stored gate fixture alignment case %q must duplicate the unselected sibling across scanner and store", testCase.Name)
		}
		if testCase.ScannerRows[0].SessionID == testCase.SelectedSessionID {
			t.Fatalf("stored gate fixture alignment case %q selected session must remain absent from scanner discovery", testCase.Name)
		}
		if testCase.StoredRows[0].GitRemote != testCase.StoredRows[1].GitRemote ||
			testCase.StoredRows[0].PathKey == testCase.StoredRows[1].PathKey {
			t.Fatalf("stored gate fixture alignment case %q must contain distinct same-remote physical clones", testCase.Name)
		}
	case kickstartStoredGateUnresolved:
		if testCase.StoreState != kickstartStoredGateReadable || testCase.ExpectedGate != kickstartStoredGateNone ||
			len(testCase.StoredRows) != 1 || len(testCase.ScannerRows) != 0 ||
			testCase.StoredRows[0].SessionID != testCase.SelectedSessionID ||
			testCase.StoredRows[0].PathSource != kickstartStoredGateGitWorktree ||
			len(testCase.Paths) != 1 || testCase.Paths[0].State != commitGatePathMissing ||
			testCase.ExpectedCandidateCount != 1 || testCase.ExpectedDescendantCount != 1 ||
			testCase.ExpectedViewerProjects != 1 || testCase.ExpectedViewerSessions != 1 ||
			len(testCase.ExpectedPushSessionIDs) != 1 || testCase.ExpectedPushSessionIDs[0] != testCase.SelectedSessionID {
			t.Fatalf("stored gate fixture unresolved case %q does not retain exact session evidence without path matching", testCase.Name)
		}
	case kickstartStoredGateEmpty:
		if testCase.StoreState != kickstartStoredGateReadable || testCase.ExpectedGate != kickstartStoredGateConfirm ||
			len(testCase.StoredRows) != 1 || len(testCase.ScannerRows) != 0 ||
			testCase.StoredRows[0].PathSource != kickstartStoredGateCanonicalCwd ||
			testCase.StoredRows[0].SessionID == testCase.SelectedSessionID ||
			testCase.ExpectedCandidateCount != 1 || testCase.ExpectedDescendantCount != 1 ||
			testCase.ExpectedViewerProjects != 0 || testCase.ExpectedViewerSessions != 0 ||
			len(testCase.ExpectedPushSessionIDs) != 0 {
			t.Fatalf("stored gate fixture empty control %q does not encode the required no-effective-descendant outcome", testCase.Name)
		}
	case kickstartStoredGateReadFailure:
		if testCase.StoreState != kickstartStoredGateQueryFailure || testCase.ExpectedGate != kickstartStoredGateError ||
			len(testCase.StoredRows) != 0 || len(testCase.ScannerRows) != 0 ||
			testCase.ExpectedCandidateCount != 0 || testCase.ExpectedDescendantCount != 0 ||
			testCase.ExpectedViewerProjects != 0 || testCase.ExpectedViewerSessions != 0 ||
			len(testCase.ExpectedPushSessionIDs) != 0 {
			t.Fatalf("stored gate fixture failure case %q does not encode fail-closed selected-mode behavior", testCase.Name)
		}
	}
}

func (b kickstartStoredGateBehavior) valid() bool {
	return b == kickstartStoredGateExplicit || b == kickstartStoredGateUnresolved ||
		b == kickstartStoredGateEmpty || b == kickstartStoredGateReadFailure
}

func (s kickstartStoredGateStoreState) valid() bool {
	return s == kickstartStoredGateReadable || s == kickstartStoredGateQueryFailure
}

func (o kickstartStoredGateOutcome) valid() bool {
	return o == kickstartStoredGateNone || o == kickstartStoredGateConfirm || o == kickstartStoredGateError
}

func TestKickstartStoredGateFixtureRejectsUnknownStoreStateKey(t *testing.T) {
	mutated := bytes.Replace(kickstartStoredGateFixture, []byte("storeState:"), []byte("storeStatus:"), 1)
	if bytes.Equal(mutated, kickstartStoredGateFixture) {
		t.Fatal("stored gate fixture has no storeState key to mutate")
	}
	var document kickstartStoredGateDocument
	decoder := yaml.NewDecoder(bytes.NewReader(mutated))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err == nil {
		t.Fatal("stored gate fixture decoder accepted an unknown store-state key")
	}
}

type kickstartStoredGateWorld struct {
	DataHome   string
	ConfigPath string
	DBPath     string
	OutputBase string
	Paths      map[string]string
}

func seedKickstartStoredGateWorld(t *testing.T, testCase kickstartStoredGateCase) kickstartStoredGateWorld {
	t.Helper()
	dataHome := t.TempDir()
	world := kickstartStoredGateWorld{
		DataHome:   dataHome,
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		DBPath:     defaults.ResolveDBFilePathWith(dataHome).String(),
		OutputBase: filepath.Join(defaults.ResolveDataDirPathWith(dataHome).String(), "peasant-sync"),
		Paths:      materializeKickstartGatePaths(t, testCase.Paths),
	}

	configured := config.BaseConfig()
	configured.Output.BasePath = world.OutputBase
	configured.Selection = config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: true,
		Harnesses: map[string]config.SelectionHarnessConfig{
			defaults.HarnessClaudeCode.String(): {Sessions: []string{testCase.SelectedSessionID}},
		},
	}
	if err := config.SaveAtomic(world.ConfigPath, configured); err != nil {
		t.Fatalf("save stored gate config: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(world.DBPath), defaults.PrivateDirPerm); err != nil {
		t.Fatalf("create stored gate data directory: %v", err)
	}
	db, err := store.Open(world.DBPath)
	if err != nil {
		t.Fatalf("open stored gate database: %v", err)
	}
	entries := make([]ingest.StoreEntry, 0, len(testCase.StoredRows))
	for index, row := range testCase.StoredRows {
		projectPath := world.Paths[row.PathKey]
		entry := makeCmdStoreEntry(
			t,
			row.SessionID,
			kickstartStoredGateHostSlug,
			row.GitRemote,
			row.Branch,
			1_706_000_000_000+int64(index)*60_000,
			projectPath,
		)
		entry.Metadata.ModelHarness = row.Harness
		entry.Metadata.Project.Name = row.ProjectName
		entry.Session.Harness = row.Harness
		if row.PathSource == kickstartStoredGateCanonicalCwd {
			entry.Metadata.Git.Worktree = nil
		}
		entries = append(entries, entry)
	}
	if len(entries) > 0 {
		if err := db.InsertSessions(t.Context(), entries); err != nil {
			db.Close()
			t.Fatalf("insert stored gate sessions: %v", err)
		}
		writeKickstartStoredGateMetadata(t, world.OutputBase, entries)
	}
	if testCase.StoreState == kickstartStoredGateQueryFailure {
		conn, err := db.Pool().Take(t.Context())
		if err != nil {
			db.Close()
			t.Fatalf("take stored gate database connection: %v", err)
		}
		if err := sqlitex.ExecuteTransient(conn, `DROP TABLE session_metrics`, nil); err != nil {
			db.Pool().Put(conn)
			db.Close()
			t.Fatalf("break stored gate query boundary: %v", err)
		}
		db.Pool().Put(conn)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close stored gate database after seeding: %v", err)
	}
	return world
}

func writeKickstartStoredGateMetadata(t *testing.T, outputBase string, entries []ingest.StoreEntry) {
	t.Helper()
	for _, entry := range entries {
		metadataPath := ingest.SessionMetadataPath(
			outputBase,
			entry.Metadata.HostSlug.String(),
			entry.Metadata.SessionID.String(),
			"",
		)
		if err := os.MkdirAll(filepath.Dir(metadataPath), defaults.PrivateDirPerm); err != nil {
			t.Fatalf("create stored gate metadata directory: %v", err)
		}
		encoded, err := json.Marshal(entry.Metadata)
		if err != nil {
			t.Fatalf("encode stored gate metadata: %v", err)
		}
		if err := os.WriteFile(metadataPath, encoded, defaults.PrivateFilePerm); err != nil {
			t.Fatalf("write stored gate metadata: %v", err)
		}
	}
}

func kickstartStoredGateListings(testCase kickstartStoredGateCase, paths map[string]string) []ftue.SessionListing {
	listings := make([]ftue.SessionListing, 0, len(testCase.ScannerRows))
	for _, row := range testCase.ScannerRows {
		listings = append(listings, ftue.SessionListing{
			Harness:     row.Harness.String(),
			ProjectName: row.ProjectName,
			GitRemote:   row.GitRemote,
			Branch:      row.Branch,
			SessionID:   row.SessionID,
			Title:       row.ProjectName,
			WorkingDir:  paths[row.PathKey],
		})
	}
	return listings
}

func TestMountedKickstartStoredGateAlignsViewerAndPush(t *testing.T) {
	document := loadKickstartStoredGateDocument(t)
	for _, testCase := range document.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			world := seedKickstartStoredGateWorld(t, testCase)
			before := snapshotKickstartGateFile(t, world.ConfigPath)
			listings := kickstartStoredGateListings(testCase, world.Paths)
			var mounted kickstart.Model
			flowMounted := false
			ingestCalls := 0
			deps := defaultKickstartCommandDeps()
			deps.flowIngest = func(context.Context) (*ftue.IngestResult, error) {
				ingestCalls++
				return &ftue.IngestResult{}, nil
			}
			deps.runFlow = func(model tea.Model) error {
				flowMounted = true
				var ok bool
				mounted, ok = model.(kickstart.Model)
				if !ok {
					return fmt.Errorf("stored gate mounted model has type %T, want kickstart.Model", model)
				}
				mounted = startKickstartGateModel(t, mounted)
				for index := 0; index < 12 && !mounted.Program().OnReceipt(); index++ {
					mounted = updateKickstartGateModel(t, mounted, commitGateKey("tab"))
				}
				if !mounted.Program().OnReceipt() {
					return fmt.Errorf("stored gate flow did not reach review and save")
				}
				mounted = updateKickstartGateModel(t, mounted, commitGateKeyEnter)
				wantPrompt := testCase.ExpectedGate == kickstartStoredGateConfirm
				if got := mounted.Program().ConfirmingNoProjects(); got != wantPrompt {
					return fmt.Errorf("stored gate confirmation visible=%t, want %t", got, wantPrompt)
				}
				if wantPrompt {
					mounted = updateKickstartGateModel(t, mounted, commitGateKeyEnter)
				}
				return nil
			}

			err := runKickstartFlow(
				mountTestCmd(t, world.DataHome),
				deps,
				world.ConfigPath,
				ftue.ProviderInventory{},
				listings,
			)
			if testCase.ExpectedGate == kickstartStoredGateError {
				assertKickstartStoredGateFailure(t, err, flowMounted, ingestCalls, before, world.ConfigPath)
				return
			}
			if err != nil {
				t.Fatalf("run mounted stored gate flow: %v", err)
			}
			if !flowMounted {
				t.Fatal("stored gate flow returned without mounting the production program")
			}
			wantCommitted := testCase.ExpectedGate == kickstartStoredGateNone
			if mounted.Program().Committed() != wantCommitted {
				t.Fatalf("stored gate committed=%t, want %t", mounted.Program().Committed(), wantCommitted)
			}
			wantIngestCalls := 0
			if wantCommitted {
				wantIngestCalls = 1
			}
			if ingestCalls != wantIngestCalls {
				t.Fatalf("stored gate ingest calls=%d, want %d", ingestCalls, wantIngestCalls)
			}
			if !wantCommitted {
				after := snapshotKickstartGateFile(t, world.ConfigPath)
				if !bytes.Equal(after.Bytes, before.Bytes) || after.Exists != before.Exists {
					t.Fatalf("stored gate default No changed config bytes\n before=%#v\n after=%#v", before, after)
				}
			}

			assertKickstartStoredGateCandidates(t, testCase, world, listings)
			assertKickstartStoredGateCrossSurfaces(t, testCase, world)
		})
	}
}

func assertKickstartStoredGateFailure(
	t *testing.T,
	err error,
	flowMounted bool,
	ingestCalls int,
	before kickstartGateFileSnapshot,
	configPath string,
) {
	t.Helper()
	if err == nil {
		t.Fatal("selected-mode stored gate evidence failure returned no error")
	}
	if flowMounted || ingestCalls != 0 {
		t.Fatalf("stored gate evidence failure mounted flow=%t and ingest calls=%d, want false and zero", flowMounted, ingestCalls)
	}
	after := snapshotKickstartGateFile(t, configPath)
	if !bytes.Equal(after.Bytes, before.Bytes) || after.Exists != before.Exists {
		t.Fatalf("stored gate evidence failure changed config bytes\n before=%#v\n after=%#v", before, after)
	}
	message := strings.ToLower(err.Error())
	for _, field := range []string{"what:", "why:", "where:", "when:", "meaning:", "fix:"} {
		if !strings.Contains(message, field) {
			t.Fatalf("stored gate evidence error lacks %q: %v", field, err)
		}
	}
}

func assertKickstartStoredGateCandidates(
	t *testing.T,
	testCase kickstartStoredGateCase,
	world kickstartStoredGateWorld,
	listings []ftue.SessionListing,
) {
	t.Helper()
	db, err := store.Open(world.DBPath)
	if err != nil {
		t.Fatalf("open stored gate database for candidate assertion: %v", err)
	}
	defer db.Close()
	storedRows, err := db.AllIngestedSessions(t.Context())
	if err != nil {
		t.Fatalf("read stored gate rows for candidate assertion: %v", err)
	}
	source := kickstart.NewScannerTreeSource(
		listings,
		kickstart.WithPathIdentityResolver(ingest.NewPhysicalPathResolver()),
	)
	candidates, err := source.CommitGateCandidates(storedRows)
	if err != nil {
		t.Fatalf("prepare scanner and stored gate candidate union: %v", err)
	}
	if len(candidates) != testCase.ExpectedCandidateCount {
		t.Fatalf("stored gate candidates=%d, want %d", len(candidates), testCase.ExpectedCandidateCount)
	}
	descendants := make(map[string]int)
	for _, candidate := range candidates {
		if candidate.Harness != ingest.Harness(defaults.HarnessClaudeCode) {
			t.Fatalf("stored gate candidate harness=%q, want %q", candidate.Harness, defaults.HarnessClaudeCode)
		}
		unresolved := testCase.Behavior == kickstartStoredGateUnresolved
		if unresolved {
			if candidate.ClonePath != "" || candidate.GitRemote != "" || candidate.ProjectName != "" {
				t.Fatalf("unresolved stored session gained project matching evidence: %#v", candidate)
			}
			wantParentID := "stored-session:" + candidate.Harness.String() + ":" + testCase.SelectedSessionID
			if string(candidate.ParentProjectID) != wantParentID {
				t.Fatalf("unresolved stored gate ParentProjectID=%q, want stable synthetic identity %q", candidate.ParentProjectID, wantParentID)
			}
		} else {
			if candidate.ClonePath == "" {
				t.Fatal("stored gate readable candidate lost its resolved physical path")
			}
			wantParentID := (kickstart.ProjectIdentity{Harness: candidate.Harness, ClonePath: candidate.ClonePath}).String()
			if string(candidate.ParentProjectID) != wantParentID {
				t.Fatalf("stored gate ParentProjectID=%q, want stable ProjectIdentity %q", candidate.ParentProjectID, wantParentID)
			}
		}
		if testCase.Behavior == kickstartStoredGateExplicit && candidate.GitRemote != "" {
			t.Fatalf("same-remote stored gate candidate retained ambiguous fallback %q", candidate.GitRemote)
		}
		for _, descendant := range candidate.Descendants {
			descendants[descendant.SessionID.String()]++
			if descendant.ClonePath != candidate.ClonePath || descendant.ParentSessionID != "" {
				t.Fatalf("stored gate descendant carrier=%#v, want project clone path and no guessed parent", descendant)
			}
		}
	}
	if len(descendants) != testCase.ExpectedDescendantCount {
		t.Fatalf("stored gate distinct descendants=%d, want %d", len(descendants), testCase.ExpectedDescendantCount)
	}
	for _, row := range testCase.StoredRows {
		if descendants[row.SessionID] != 1 {
			t.Fatalf("stored gate session %q appears %d times after scanner/store deduplication, want once", row.SessionID, descendants[row.SessionID])
		}
	}
}

func assertKickstartStoredGateCrossSurfaces(t *testing.T, testCase kickstartStoredGateCase, world kickstartStoredGateWorld) {
	t.Helper()
	configured, err := loadConfig(world.ConfigPath)
	if err != nil {
		t.Fatalf("load stored gate config for cross-surface assertions: %v", err)
	}
	visibility, err := sessionvisibility.New(configured.Selection)
	if err != nil {
		t.Fatalf("build stored gate visibility policy: %v", err)
	}
	db, err := store.Open(world.DBPath)
	if err != nil {
		t.Fatalf("open stored gate database for cross-surface assertions: %v", err)
	}
	defer db.Close()

	viewer := codemap.NewService(
		db,
		func(path string) gitops.Repository { return gitops.NewExecGitRepository(path) },
		codegraph.NewGraphBuilder(),
		visibility,
	)
	summaries, err := viewer.ProjectSummaries(t.Context())
	if err != nil {
		t.Fatalf("load real stored gate ProjectSummaries: %v", err)
	}
	if len(summaries.Projects) != testCase.ExpectedViewerProjects {
		t.Fatalf("stored gate viewer projects=%d, want %d", len(summaries.Projects), testCase.ExpectedViewerProjects)
	}
	visibleSessions := 0
	for _, project := range summaries.Projects {
		visibleSessions += project.Sessions
	}
	if visibleSessions != testCase.ExpectedViewerSessions {
		t.Fatalf("stored gate viewer sessions=%d, want %d", visibleSessions, testCase.ExpectedViewerSessions)
	}

	selection, err := preparePushSelection(t.Context(), db, configured.SelectionMatcher(), ingest.NewPhysicalPathResolver())
	if err != nil {
		t.Fatalf("prepare real stored gate push selection: %v", err)
	}
	wizardSessions, err := buildPushWizardSessions(
		t.Context(),
		db,
		&ingest.OSFileSystem{},
		configured.Output.BasePath,
		push.PushCandidateQuery{Method: configured.Push.Method, Sources: configured.Push.Sources},
		selection,
	)
	if err != nil {
		t.Fatalf("build real stored gate push chooser: %v", err)
	}
	gotPushIDs := make([]string, 0, len(wizardSessions))
	for _, session := range wizardSessions {
		gotPushIDs = append(gotPushIDs, session.Row.SessionID)
	}
	sort.Strings(gotPushIDs)
	wantPushIDs := append([]string(nil), testCase.ExpectedPushSessionIDs...)
	sort.Strings(wantPushIDs)
	if strings.Join(gotPushIDs, "\n") != strings.Join(wantPushIDs, "\n") {
		t.Fatalf("stored gate push chooser IDs=%v, want %v", gotPushIDs, wantPushIDs)
	}
}
