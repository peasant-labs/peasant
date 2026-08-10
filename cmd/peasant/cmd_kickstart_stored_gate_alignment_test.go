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
	"github.com/peasant-labs/peasant/internal/selectionprojection"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

const (
	kickstartStoredGateCaseCount       = 8
	kickstartStoredGateStoredRowCount  = 9
	kickstartStoredGateScannerRowCount = 1
	kickstartStoredGateHostSlug        = "github.com-example-kickstart-stored-gate"
)

type kickstartStoredGateBehavior string

const (
	kickstartStoredGateExplicit     kickstartStoredGateBehavior = "stored-explicit"
	kickstartStoredGateUnresolved   kickstartStoredGateBehavior = "stored-unresolved-explicit"
	kickstartStoredGateClone        kickstartStoredGateBehavior = "stored-canonical-clone"
	kickstartStoredGateUniqueName   kickstartStoredGateBehavior = "stored-unique-name"
	kickstartStoredGateNameConflict kickstartStoredGateBehavior = "stored-ambiguous-name"
	kickstartStoredGateEmpty        kickstartStoredGateBehavior = "empty-control"
	kickstartStoredGateReadFailure  kickstartStoredGateBehavior = "store-read-failure"
	kickstartStoredGateInvalidHash  kickstartStoredGateBehavior = "store-invalid-project-hash"
)

type kickstartStoredGateStoreState string

const (
	kickstartStoredGateReadable     kickstartStoredGateStoreState = "readable"
	kickstartStoredGateQueryFailure kickstartStoredGateStoreState = "query-failure"
	kickstartStoredGateHashInvalid  kickstartStoredGateStoreState = "invalid-project-hash"
)

type kickstartStoredGateSelectionKind string

const (
	kickstartStoredGateSelectSession kickstartStoredGateSelectionKind = "explicit-session"
	kickstartStoredGateSelectClone   kickstartStoredGateSelectionKind = "clone-path"
	kickstartStoredGateSelectName    kickstartStoredGateSelectionKind = "project-name"
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
	CorruptProjectHash      string                        `yaml:"corruptProjectHash"`
	Selection               kickstartStoredGateSelection  `yaml:"selection"`
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

type kickstartStoredGateSelection struct {
	Kind         kickstartStoredGateSelectionKind `yaml:"kind"`
	SessionID    string                           `yaml:"sessionId"`
	ProjectName  string                           `yaml:"projectName"`
	ClonePathKey string                           `yaml:"clonePathKey"`
}

type kickstartStoredGateRow struct {
	Harness          defaults.Harness `yaml:"harness"`
	SessionID        string           `yaml:"sessionId"`
	ProjectHash      string           `yaml:"projectHash"`
	ProjectName      string           `yaml:"projectName"`
	GitRemote        string           `yaml:"gitRemote"`
	Branch           string           `yaml:"branch"`
	CanonicalPathKey string           `yaml:"canonicalPathKey"`
	WorktreePathKey  string           `yaml:"worktreePathKey"`
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
		pathStates := validateKickstartGatePaths(t, testCase.Name, testCase.Paths)
		validateKickstartStoredGateSelection(t, testCase.Name, testCase.Selection, pathStates)
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
		kickstartStoredGateClone,
		kickstartStoredGateUniqueName,
		kickstartStoredGateNameConflict,
		kickstartStoredGateEmpty,
		kickstartStoredGateReadFailure,
		kickstartStoredGateInvalidHash,
	} {
		if _, ok := seenBehaviors[behavior]; !ok {
			t.Fatalf("stored gate fixture does not cover behavior %q", behavior)
		}
	}
}

func validateKickstartStoredGateSelection(
	t *testing.T,
	caseName string,
	selection kickstartStoredGateSelection,
	pathStates map[string]commitGatePathState,
) {
	t.Helper()
	switch selection.Kind {
	case kickstartStoredGateSelectSession:
		if _, err := ingest.NewSessionID(selection.SessionID); err != nil {
			t.Fatalf("stored gate fixture case %q has invalid selected session ID %q: %v", caseName, selection.SessionID, err)
		}
		if selection.ProjectName != "" || selection.ClonePathKey != "" {
			t.Fatalf("stored gate fixture case %q explicit-session selection includes project fields: %#v", caseName, selection)
		}
	case kickstartStoredGateSelectClone:
		if selection.SessionID != "" || selection.ProjectName != "" || pathStates[selection.ClonePathKey] != commitGatePathDirectory {
			t.Fatalf("stored gate fixture case %q has invalid clone-path selection: %#v", caseName, selection)
		}
	case kickstartStoredGateSelectName:
		if selection.SessionID != "" || strings.TrimSpace(selection.ProjectName) == "" || selection.ClonePathKey != "" {
			t.Fatalf("stored gate fixture case %q has invalid project-name selection: %#v", caseName, selection)
		}
	default:
		t.Fatalf("stored gate fixture case %q has unknown selection kind %q", caseName, selection.Kind)
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
		if !row.Harness.IsKnown() || row.ProjectName == "" || row.Branch == "" {
			t.Fatalf("stored gate fixture case %q has an incomplete row: %#v", caseName, row)
		}
		if _, err := ingest.NewSessionID(row.SessionID); err != nil {
			t.Fatalf("stored gate fixture case %q has invalid row session ID %q: %v", caseName, row.SessionID, err)
		}
		if _, duplicate := seen[row.SessionID]; duplicate {
			t.Fatalf("stored gate fixture case %q repeats row session ID %q", caseName, row.SessionID)
		}
		seen[row.SessionID] = struct{}{}
		if stored {
			if _, err := ingest.NewProjectHash(row.ProjectHash); err != nil {
				t.Fatalf("stored gate fixture case %q row %q has invalid project hash %q: %v", caseName, row.SessionID, row.ProjectHash, err)
			}
			if row.CanonicalPathKey == "" {
				t.Fatalf("stored gate fixture case %q row %q has no canonical path carrier", caseName, row.SessionID)
			}
			for _, pathKey := range []string{row.CanonicalPathKey, row.WorktreePathKey} {
				if pathKey == "" {
					continue
				}
				pathState := pathStates[pathKey]
				if pathState != commitGatePathDirectory && !(allowMissing && pathState == commitGatePathMissing) {
					t.Fatalf("stored gate fixture case %q row %q uses unavailable path %q", caseName, row.SessionID, pathKey)
				}
			}
		} else {
			if row.ProjectHash != "" || row.CanonicalPathKey != "" || pathStates[row.WorktreePathKey] != commitGatePathDirectory {
				t.Fatalf("stored gate fixture case %q scanner row %q has invalid scanner-only identity fields: %#v", caseName, row.SessionID, row)
			}
		}
	}
}

func validateKickstartStoredGateCaseSemantics(t *testing.T, testCase kickstartStoredGateCase) {
	t.Helper()
	switch testCase.Behavior {
	case kickstartStoredGateExplicit:
		if testCase.StoreState != kickstartStoredGateReadable || testCase.ExpectedGate != kickstartStoredGateNone ||
			testCase.Selection.Kind != kickstartStoredGateSelectSession ||
			len(testCase.StoredRows) != 2 || len(testCase.ScannerRows) != 1 ||
			testCase.ExpectedCandidateCount != 2 || testCase.ExpectedDescendantCount != 2 ||
			testCase.ExpectedViewerProjects != 1 || testCase.ExpectedViewerSessions != 1 ||
			len(testCase.ExpectedPushSessionIDs) != 1 || testCase.ExpectedPushSessionIDs[0] != testCase.Selection.SessionID {
			t.Fatalf("stored gate fixture alignment case %q does not encode the required cross-surface outcome", testCase.Name)
		}
		if testCase.StoredRows[0].SessionID != testCase.Selection.SessionID {
			t.Fatalf("stored gate fixture alignment case %q must select its DB-only first row", testCase.Name)
		}
		if testCase.ScannerRows[0].SessionID != testCase.StoredRows[1].SessionID ||
			testCase.ScannerRows[0].WorktreePathKey != testCase.StoredRows[1].WorktreePathKey {
			t.Fatalf("stored gate fixture alignment case %q must duplicate the unselected sibling across scanner and store", testCase.Name)
		}
		if testCase.ScannerRows[0].SessionID == testCase.Selection.SessionID {
			t.Fatalf("stored gate fixture alignment case %q selected session must remain absent from scanner discovery", testCase.Name)
		}
		if testCase.StoredRows[0].GitRemote != testCase.StoredRows[1].GitRemote ||
			testCase.StoredRows[0].CanonicalPathKey == testCase.StoredRows[1].CanonicalPathKey ||
			testCase.StoredRows[0].WorktreePathKey == testCase.StoredRows[1].WorktreePathKey {
			t.Fatalf("stored gate fixture alignment case %q must contain distinct same-remote physical clones", testCase.Name)
		}
	case kickstartStoredGateUnresolved:
		if testCase.StoreState != kickstartStoredGateReadable || testCase.ExpectedGate != kickstartStoredGateNone ||
			testCase.Selection.Kind != kickstartStoredGateSelectSession ||
			len(testCase.StoredRows) != 1 || len(testCase.ScannerRows) != 0 ||
			testCase.StoredRows[0].SessionID != testCase.Selection.SessionID ||
			len(testCase.Paths) != 2 || testCase.Paths[0].State != commitGatePathMissing || testCase.Paths[1].State != commitGatePathMissing ||
			testCase.ExpectedCandidateCount != 1 || testCase.ExpectedDescendantCount != 1 ||
			testCase.ExpectedViewerProjects != 1 || testCase.ExpectedViewerSessions != 1 ||
			len(testCase.ExpectedPushSessionIDs) != 1 || testCase.ExpectedPushSessionIDs[0] != testCase.Selection.SessionID {
			t.Fatalf("stored gate fixture unresolved case %q does not retain exact session evidence without path matching", testCase.Name)
		}
	case kickstartStoredGateClone:
		if testCase.StoreState != kickstartStoredGateReadable || testCase.ExpectedGate != kickstartStoredGateNone ||
			testCase.Selection.Kind != kickstartStoredGateSelectClone || len(testCase.StoredRows) != 1 || len(testCase.ScannerRows) != 0 ||
			testCase.StoredRows[0].CanonicalPathKey != testCase.Selection.ClonePathKey || testCase.StoredRows[0].WorktreePathKey == "" ||
			testCase.StoredRows[0].CanonicalPathKey == testCase.StoredRows[0].WorktreePathKey ||
			testCase.ExpectedCandidateCount != 1 || testCase.ExpectedDescendantCount != 1 ||
			testCase.ExpectedViewerProjects != 1 || testCase.ExpectedViewerSessions != 0 || len(testCase.ExpectedPushSessionIDs) != 0 {
			t.Fatalf("stored gate fixture clone case %q does not preserve distinct parent and descendant carriers", testCase.Name)
		}
	case kickstartStoredGateUniqueName:
		if testCase.StoreState != kickstartStoredGateReadable || testCase.ExpectedGate != kickstartStoredGateNone ||
			testCase.Selection.Kind != kickstartStoredGateSelectName || len(testCase.StoredRows) != 1 || len(testCase.ScannerRows) != 0 ||
			testCase.StoredRows[0].GitRemote != "" || testCase.StoredRows[0].WorktreePathKey != "" ||
			filepath.Base(testCase.StoredRows[0].CanonicalPathKey) != testCase.Selection.ProjectName ||
			testCase.ExpectedCandidateCount != 1 || testCase.ExpectedDescendantCount != 1 ||
			testCase.ExpectedViewerProjects != 1 || testCase.ExpectedViewerSessions != 1 ||
			len(testCase.ExpectedPushSessionIDs) != 1 || testCase.ExpectedPushSessionIDs[0] != testCase.StoredRows[0].SessionID {
			t.Fatalf("stored gate fixture unique-name case %q does not align DB-only non-Git behavior", testCase.Name)
		}
	case kickstartStoredGateNameConflict:
		if testCase.StoreState != kickstartStoredGateReadable || testCase.ExpectedGate != kickstartStoredGateConfirm ||
			testCase.Selection.Kind != kickstartStoredGateSelectName || len(testCase.StoredRows) != 2 || len(testCase.ScannerRows) != 0 ||
			testCase.StoredRows[0].ProjectHash == testCase.StoredRows[1].ProjectHash ||
			testCase.StoredRows[0].CanonicalPathKey == testCase.StoredRows[1].CanonicalPathKey ||
			filepath.Base(testCase.StoredRows[0].CanonicalPathKey) != testCase.Selection.ProjectName ||
			filepath.Base(testCase.StoredRows[1].CanonicalPathKey) != testCase.Selection.ProjectName ||
			testCase.ExpectedCandidateCount != 2 || testCase.ExpectedDescendantCount != 2 ||
			testCase.ExpectedViewerProjects != 0 || testCase.ExpectedViewerSessions != 0 || len(testCase.ExpectedPushSessionIDs) != 0 {
			t.Fatalf("stored gate fixture same-name case %q does not encode fail-closed physical ambiguity", testCase.Name)
		}
	case kickstartStoredGateEmpty:
		if testCase.StoreState != kickstartStoredGateReadable || testCase.ExpectedGate != kickstartStoredGateConfirm ||
			testCase.Selection.Kind != kickstartStoredGateSelectSession ||
			len(testCase.StoredRows) != 1 || len(testCase.ScannerRows) != 0 ||
			testCase.StoredRows[0].WorktreePathKey != "" ||
			testCase.StoredRows[0].SessionID == testCase.Selection.SessionID ||
			testCase.ExpectedCandidateCount != 1 || testCase.ExpectedDescendantCount != 1 ||
			testCase.ExpectedViewerProjects != 0 || testCase.ExpectedViewerSessions != 0 ||
			len(testCase.ExpectedPushSessionIDs) != 0 {
			t.Fatalf("stored gate fixture empty control %q does not encode the required no-effective-descendant outcome", testCase.Name)
		}
	case kickstartStoredGateReadFailure:
		if testCase.StoreState != kickstartStoredGateQueryFailure || testCase.ExpectedGate != kickstartStoredGateError ||
			testCase.Selection.Kind != kickstartStoredGateSelectSession ||
			len(testCase.StoredRows) != 0 || len(testCase.ScannerRows) != 0 ||
			testCase.ExpectedCandidateCount != 0 || testCase.ExpectedDescendantCount != 0 ||
			testCase.ExpectedViewerProjects != 0 || testCase.ExpectedViewerSessions != 0 ||
			len(testCase.ExpectedPushSessionIDs) != 0 {
			t.Fatalf("stored gate fixture failure case %q does not encode fail-closed selected-mode behavior", testCase.Name)
		}
	case kickstartStoredGateInvalidHash:
		if testCase.StoreState != kickstartStoredGateHashInvalid || testCase.ExpectedGate != kickstartStoredGateError ||
			testCase.Selection.Kind != kickstartStoredGateSelectSession || len(testCase.StoredRows) != 1 || len(testCase.ScannerRows) != 0 ||
			strings.TrimSpace(testCase.CorruptProjectHash) == "" || testCase.CorruptProjectHash == testCase.StoredRows[0].ProjectHash ||
			testCase.ExpectedCandidateCount != 0 || testCase.ExpectedDescendantCount != 0 ||
			testCase.ExpectedViewerProjects != 0 || testCase.ExpectedViewerSessions != 0 || len(testCase.ExpectedPushSessionIDs) != 0 {
			t.Fatalf("stored gate fixture invalid-hash case %q does not encode boundary rejection", testCase.Name)
		}
		if _, err := ingest.NewProjectHash(testCase.CorruptProjectHash); err == nil {
			t.Fatalf("stored gate fixture invalid-hash case %q uses valid corruption value %q", testCase.Name, testCase.CorruptProjectHash)
		}
	}
	if testCase.Behavior != kickstartStoredGateInvalidHash && testCase.CorruptProjectHash != "" {
		t.Fatalf("stored gate fixture case %q has unexpected corruptProjectHash %q", testCase.Name, testCase.CorruptProjectHash)
	}
}

func (b kickstartStoredGateBehavior) valid() bool {
	return b == kickstartStoredGateExplicit || b == kickstartStoredGateUnresolved ||
		b == kickstartStoredGateClone || b == kickstartStoredGateUniqueName ||
		b == kickstartStoredGateNameConflict || b == kickstartStoredGateEmpty ||
		b == kickstartStoredGateReadFailure || b == kickstartStoredGateInvalidHash
}

func (s kickstartStoredGateStoreState) valid() bool {
	return s == kickstartStoredGateReadable || s == kickstartStoredGateQueryFailure || s == kickstartStoredGateHashInvalid
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
	configured.Selection = kickstartStoredGateSelectionConfig(t, testCase.Selection, world.Paths)
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
		canonicalPath := world.Paths[row.CanonicalPathKey]
		entry := makeCmdStoreEntry(
			t,
			row.SessionID,
			kickstartStoredGateHostSlug,
			row.GitRemote,
			row.Branch,
			1_706_000_000_000+int64(index)*60_000,
			canonicalPath,
		)
		projectHash, err := ingest.NewProjectHash(row.ProjectHash)
		if err != nil {
			db.Close()
			t.Fatalf("construct stored gate project hash: %v", err)
		}
		entry.Metadata.ModelHarness = row.Harness
		entry.Metadata.Project = ingest.ProjectInfo{Hash: projectHash, Name: row.ProjectName, FilePath: canonicalPath}
		entry.Session.Harness = row.Harness
		if row.WorktreePathKey == "" {
			entry.Metadata.Git.Worktree = nil
		} else {
			worktree := world.Paths[row.WorktreePathKey]
			entry.Metadata.Git.Worktree = &worktree
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
	if testCase.StoreState == kickstartStoredGateHashInvalid {
		conn, err := db.Pool().Take(t.Context())
		if err != nil {
			db.Close()
			t.Fatalf("take stored gate database connection for project-hash corruption: %v", err)
		}
		if err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys = OFF`, nil); err != nil {
			db.Pool().Put(conn)
			db.Close()
			t.Fatalf("disable foreign keys for stored gate corruption fixture: %v", err)
		}
		err = sqlitex.ExecuteTransient(conn, `UPDATE sessions SET project_hash = ?`, &sqlitex.ExecOptions{
			Args: []any{testCase.CorruptProjectHash},
		})
		changed := conn.Changes()
		db.Pool().Put(conn)
		if err != nil {
			db.Close()
			t.Fatalf("corrupt stored gate project hash: %v", err)
		}
		if changed != 1 {
			db.Close()
			t.Fatalf("corrupt stored gate project hash changed %d rows, want 1", changed)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close stored gate database after seeding: %v", err)
	}
	return world
}

func kickstartStoredGateSelectionConfig(
	t *testing.T,
	selection kickstartStoredGateSelection,
	paths map[string]string,
) config.SelectionConfig {
	t.Helper()
	harness := config.SelectionHarnessConfig{}
	switch selection.Kind {
	case kickstartStoredGateSelectSession:
		harness.Sessions = []string{selection.SessionID}
	case kickstartStoredGateSelectClone:
		harness.Projects = []config.ProjectSelection{{ClonePaths: []string{paths[selection.ClonePathKey]}}}
	case kickstartStoredGateSelectName:
		harness.Projects = []config.ProjectSelection{{Name: selection.ProjectName}}
	default:
		t.Fatalf("build stored gate config from unknown selection kind %q", selection.Kind)
	}
	return config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: true,
		Harnesses: map[string]config.SelectionHarnessConfig{
			defaults.HarnessClaudeCode.String(): harness,
		},
	}
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
			WorkingDir:  paths[row.WorktreePathKey],
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
				assertKickstartStoredGateFailure(t, testCase, err, flowMounted, ingestCalls, before, world.ConfigPath)
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
	testCase kickstartStoredGateCase,
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
	if testCase.Behavior == kickstartStoredGateInvalidHash && !strings.Contains(message, "invalid project hash") {
		t.Fatalf("stored gate project-hash error does not identify the invalid boundary value: %v", err)
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
	rowsBySession := make(map[string]kickstartStoredGateRow, len(testCase.StoredRows))
	for _, row := range testCase.StoredRows {
		rowsBySession[row.SessionID] = row
	}
	descendants := make(map[string]int)
	for _, candidate := range candidates {
		if len(candidate.Descendants) != 1 {
			t.Fatalf("stored gate candidate for parent %q has %d descendants, want one direct stored carrier", candidate.ParentProjectID, len(candidate.Descendants))
		}
		descendant := candidate.Descendants[0]
		row, ok := rowsBySession[descendant.SessionID.String()]
		if !ok {
			t.Fatalf("stored gate retained scanner-only candidate for session %q after union deduplication", descendant.SessionID)
		}
		descendants[descendant.SessionID.String()]++
		wantProjectPath := resolvedKickstartStoredGateFixturePath(testCase.Paths, world.Paths, row.CanonicalPathKey)
		wantSessionPath := resolvedKickstartStoredGateFixturePath(testCase.Paths, world.Paths, row.WorktreePathKey)
		if candidate.ParentProjectID != selectionprojection.ParentProjectID(row.ProjectHash) ||
			candidate.Harness != ingest.Harness(row.Harness) ||
			candidate.GitRemote != row.GitRemote ||
			candidate.ProjectName != world.Paths[row.CanonicalPathKey] ||
			candidate.ClonePath != wantProjectPath {
			t.Fatalf("stored gate project carrier for session %q = %#v, want hash=%q harness=%q remote=%q name=%q clone=%q", row.SessionID, candidate, row.ProjectHash, row.Harness, row.GitRemote, world.Paths[row.CanonicalPathKey], wantProjectPath)
		}
		if descendant.Branch != row.Branch || descendant.ClonePath != wantSessionPath || descendant.ParentSessionID != "" {
			t.Fatalf("stored gate descendant carrier for session %q = %#v, want branch=%q worktree=%q and no stored parent-session carrier", row.SessionID, descendant, row.Branch, wantSessionPath)
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

func resolvedKickstartStoredGateFixturePath(
	paths []kickstartGatePath,
	materialized map[string]string,
	key string,
) ingest.ClonePath {
	if key == "" {
		return ""
	}
	for _, path := range paths {
		if path.Key == key && path.State == commitGatePathDirectory {
			return ingest.ClonePath(materialized[key])
		}
	}
	return ""
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
