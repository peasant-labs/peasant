package ingest_test

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
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	metricspkg "github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	expectedCanonicalSelectionCases     = 9
	expectedCanonicalSelectionMutations = 5
)

type canonicalFixtureRepresentation string

const (
	canonicalFixtureJSON    canonicalFixtureRepresentation = "legacy_json"
	canonicalFixtureLegacy  canonicalFixtureRepresentation = "legacy_sqlite"
	canonicalFixtureCurrent canonicalFixtureRepresentation = "current_sqlite"
)

func (representation canonicalFixtureRepresentation) production() (ingest.OpenCodeCanonicalRepresentation, error) {
	switch representation {
	case canonicalFixtureJSON:
		return ingest.OpenCodeRepresentationLegacyJSON, nil
	case canonicalFixtureLegacy:
		return ingest.OpenCodeRepresentationLegacySQLite, nil
	case canonicalFixtureCurrent:
		return ingest.OpenCodeRepresentationCurrentSQLite, nil
	default:
		return 0, fmt.Errorf("unknown canonical fixture representation %q", representation)
	}
}

type canonicalSelectionFixture struct {
	DeclaredCases           int                                `yaml:"declared_cases"`
	SourceFixture           string                             `yaml:"source_fixture"`
	JSONMTimeMS             int64                              `yaml:"json_mtime_ms"`
	Cases                   []canonicalSelectionCase           `yaml:"cases"`
	LoaderMutations         []canonicalSelectionLoaderMutation `yaml:"loader_mutations"`
	DeclaredLoaderMutations int                                `yaml:"declared_loader_mutations"`
}

type canonicalSelectionCase struct {
	Name                string                           `yaml:"name"`
	SessionID           string                           `yaml:"session_id"`
	Representations     []canonicalFixtureRepresentation `yaml:"representations"`
	Expected            canonicalFixtureRepresentation   `yaml:"expected"`
	Marker              string                           `yaml:"marker"`
	ExpectedFreshnessMS int64                            `yaml:"expected_freshness_ms"`
	MissingParent       string                           `yaml:"missing_parent"`
	LinkedParent        string                           `yaml:"linked_parent"`
	LinkedChild         string                           `yaml:"linked_child"`
	MissingEntry        string                           `yaml:"missing_entry"`
	ToolCallID          string                           `yaml:"tool_call_id"`
	OrphanEntry         string                           `yaml:"orphan_entry"`
	OrphanParent        string                           `yaml:"orphan_parent"`
	OrphanMarker        string                           `yaml:"orphan_marker"`
}

type canonicalSelectionLoaderMutation struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"`
}

//go:embed testdata/opencode_canonical_selection.yaml
var canonicalSelectionYAML []byte

func loadCanonicalSelectionFixture(data []byte) (canonicalSelectionFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture canonicalSelectionFixture
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode canonical OpenCode selection fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixture, errors.New("canonical OpenCode selection fixture must contain exactly one YAML document")
	}
	if fixture.DeclaredCases != expectedCanonicalSelectionCases || len(fixture.Cases) != expectedCanonicalSelectionCases || fixture.DeclaredLoaderMutations != expectedCanonicalSelectionMutations || len(fixture.LoaderMutations) != expectedCanonicalSelectionMutations || fixture.SourceFixture == "" || fixture.JSONMTimeMS <= 0 {
		return fixture, errors.New("canonical OpenCode selection fixture count or source guard failed")
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || testCase.SessionID == "" || testCase.Marker == "" || testCase.ExpectedFreshnessMS <= 0 || seen[testCase.Name] || len(testCase.Representations) == 0 {
			return fixture, fmt.Errorf("canonical OpenCode selection fixture contains an incomplete or duplicate case %+v", testCase)
		}
		seen[testCase.Name] = true
		hasExpected := false
		for _, representation := range testCase.Representations {
			if _, err := representation.production(); err != nil {
				return fixture, err
			}
			if representation == testCase.Expected {
				hasExpected = true
			}
		}
		if _, err := testCase.Expected.production(); err != nil || !hasExpected {
			return fixture, fmt.Errorf("canonical OpenCode selection case %q has an invalid expected representation", testCase.Name)
		}
	}
	for _, mutation := range fixture.LoaderMutations {
		if mutation.Name == "" {
			return fixture, errors.New("canonical OpenCode selection fixture has an unnamed loader mutation")
		}
		switch mutation.Kind {
		case "unknown_field", "wrong_count", "duplicate_name", "unknown_representation", "trailing_document":
		default:
			return fixture, fmt.Errorf("canonical OpenCode selection fixture has unknown loader mutation %q", mutation.Kind)
		}
	}
	return fixture, nil
}

func TestCanonicalOpenCodeSelectionMountedMatrix(t *testing.T) {
	fixture, err := loadCanonicalSelectionFixture(canonicalSelectionYAML)
	if err != nil {
		t.Fatal(err)
	}
	materialized := testfixture.MaterializeByName(t, fixture.SourceFixture)
	rootPath := filepath.Dir(materialized.Path)
	writeCanonicalJSONFixtures(t, rootPath, fixture)
	root, err := ingest.NewResolvedPath(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	environment := mountedCurrentEnvironment{"OPENCODE_DB": materialized.Path}
	adapterFactory := canonicalAdapterFactory(t, environment)
	recorder := newCanonicalFreshnessRecorder()
	filesystem := &canonicalFreshnessFileSystem{OSFileSystem: &ingest.OSFileSystem{}, sessionStats: make(map[string]int)}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", environment, filesystem, canonicalRecordingOpener(recorder), ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatal(err)
	}
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != len(fixture.Cases) {
		t.Fatalf("canonical discovery returned %d sessions, want %d unique raw IDs", len(discovered), len(fixture.Cases))
	}
	assertOnlyCanonicalFreshnessConsulted(t, fixture, rootPath, recorder, filesystem)
	repeated, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil || !reflect.DeepEqual(discovered, repeated) {
		t.Fatalf("canonical selection was not deterministic across repeated discovery: equal=%t error=%v", reflect.DeepEqual(discovered, repeated), err)
	}
	byID := make(map[string]ingest.DiscoveredSession, len(discovered))
	for _, session := range discovered {
		if _, duplicate := byID[string(session.SessionID)]; duplicate {
			t.Fatalf("canonical discovery duplicated raw session ID %q", session.SessionID)
		}
		byID[string(session.SessionID)] = session
	}
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true))
	for _, testCase := range fixture.Cases {
		session, ok := byID[testCase.SessionID]
		if !ok {
			t.Errorf("canonical discovery omitted session %q", testCase.SessionID)
			continue
		}
		wantRepresentation, _ := testCase.Expected.production()
		if got := representationForOrigin(session.TranscriptOrigin); got != wantRepresentation {
			t.Errorf("session %q selected representation %d, want %d", testCase.SessionID, got, wantRepresentation)
		}
		if session.ModTime.UnixMilli() != testCase.ExpectedFreshnessMS {
			t.Errorf("session %q selected freshness=%d, want %d", testCase.SessionID, session.ModTime.UnixMilli(), testCase.ExpectedFreshnessMS)
		}
		var metadata *ingest.UnifiedMetadata
		var entriesMarker []byte
		if session.TranscriptOrigin == ingest.TranscriptOriginFile {
			metadata, err = adapter.ExtractMetadata(t.Context(), session)
			if err == nil {
				var entriesErr error
				entries, entriesErr := indexer.IndexTranscript(t.Context(), session)
				err = entriesErr
				entriesMarker, _ = json.Marshal(entries)
			}
		} else {
			metadata, entriesMarker, err = adapter.MaterializeTranscript(t.Context(), session)
		}
		if err != nil {
			t.Errorf("materialize selected session %q: %v", testCase.SessionID, err)
			continue
		}
		if !bytes.Contains(entriesMarker, []byte(testCase.Marker)) {
			t.Errorf("selected session %q does not contain marker %q", testCase.SessionID, testCase.Marker)
		}
		if testCase.MissingParent != "" && !hasMissingParentWarning(metadata.Diagnostics.Warnings, testCase.MissingParent) {
			t.Errorf("selected session %q lacks actionable missing-parent diagnostic: warnings=%+v projection=%s", testCase.SessionID, metadata.Diagnostics.Warnings, entriesMarker)
		}
		if testCase.LinkedParent != "" {
			assertCanonicalGraphAndToolPairing(t, indexer, session, entriesMarker, testCase)
		}
		if testCase.OrphanEntry != "" {
			assertCanonicalOrphanPart(t, indexer, session, entriesMarker, testCase)
			if !hasMissingParentWarning(metadata.Diagnostics.Warnings, testCase.OrphanParent) {
				t.Errorf("selected session %q lacks orphan-part parent diagnostic for %q: %+v", testCase.SessionID, testCase.OrphanParent, metadata.Diagnostics.Warnings)
			}
		}
	}

	output, err := ingest.NewResolvedPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &testutil.StubSessionStore{}
	metrics := testutil.NewStubMetricsStore()
	pipeline, err := ingest.NewPipeline(&ingest.OSFileSystem{}, testutil.NoGitResolver(), map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessOpenCode: adapterFactory}, ingest.PipelineConfig{Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessOpenCode: {Enabled: true, Paths: []ingest.ResolvedPath{root}}}, OutputDir: output, Parallelism: 1}, ingest.WithStore(store), ingest.WithMetricsStore(metrics), ingest.WithAnalyzer(metricspkg.NewEngine(metrics)), ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessOpenCode: indexer}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.New != len(fixture.Cases) || len(store.InsertedEntries) != len(fixture.Cases) || len(metrics.IndexedEntries) != len(fixture.Cases) {
		t.Fatalf("mounted canonical ingest summary=%+v store=%d indexed=%d, want %d unique sessions", result.Summary, len(store.InsertedEntries), len(metrics.IndexedEntries), len(fixture.Cases))
	}
}

type canonicalFreshnessRecorder struct {
	mu      sync.Mutex
	current map[string]int
	legacy  map[string]int
}

func newCanonicalFreshnessRecorder() *canonicalFreshnessRecorder {
	return &canonicalFreshnessRecorder{current: make(map[string]int), legacy: make(map[string]int)}
}

type canonicalRecordingSource struct {
	ingest.OpenCodeSQLiteSource
	recorder *canonicalFreshnessRecorder
}

func (source canonicalRecordingSource) CurrentSessionFreshness(ctx context.Context, id ingest.OpenCodeCurrentSessionID) (time.Time, error) {
	source.recorder.mu.Lock()
	source.recorder.current[id.String()]++
	source.recorder.mu.Unlock()
	return source.OpenCodeSQLiteSource.CurrentSessionFreshness(ctx, id)
}

func (source canonicalRecordingSource) LegacySessionFreshness(ctx context.Context, id ingest.OpenCodeLegacySessionID) (time.Time, error) {
	source.recorder.mu.Lock()
	source.recorder.legacy[id.String()]++
	source.recorder.mu.Unlock()
	return source.OpenCodeSQLiteSource.LegacySessionFreshness(ctx, id)
}

func canonicalRecordingOpener(recorder *canonicalFreshnessRecorder) ingest.OpenCodeSQLiteSourceOpener {
	return func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
		source, err := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
		if err != nil {
			return nil, err
		}
		return canonicalRecordingSource{OpenCodeSQLiteSource: source, recorder: recorder}, nil
	}
}

type canonicalFreshnessFileSystem struct {
	*ingest.OSFileSystem
	mu           sync.Mutex
	sessionStats map[string]int
}

func (filesystem *canonicalFreshnessFileSystem) Stat(path string) (os.FileInfo, error) {
	if strings.HasSuffix(path, ".json") && strings.Contains(path, string(filepath.Separator)+"session"+string(filepath.Separator)) {
		filesystem.mu.Lock()
		filesystem.sessionStats[filepath.Clean(path)]++
		filesystem.mu.Unlock()
	}
	return filesystem.OSFileSystem.Stat(path)
}

func assertOnlyCanonicalFreshnessConsulted(t testing.TB, fixture canonicalSelectionFixture, root string, recorder *canonicalFreshnessRecorder, filesystem *canonicalFreshnessFileSystem) {
	t.Helper()
	recorder.mu.Lock()
	current := cloneIntMap(recorder.current)
	legacy := cloneIntMap(recorder.legacy)
	recorder.mu.Unlock()
	filesystem.mu.Lock()
	stats := cloneIntMap(filesystem.sessionStats)
	filesystem.mu.Unlock()
	for _, testCase := range fixture.Cases {
		wantCurrent, wantLegacy, wantJSON := 0, 0, 0
		switch testCase.Expected {
		case canonicalFixtureCurrent:
			wantCurrent = 1
		case canonicalFixtureLegacy:
			wantLegacy = 1
		case canonicalFixtureJSON:
			wantJSON = 1
		}
		jsonPath := filepath.Join(root, "storage", "session", "synthetic", testCase.SessionID+".json")
		if current[testCase.SessionID] != wantCurrent || legacy[testCase.SessionID] != wantLegacy || stats[jsonPath] != wantJSON {
			t.Fatalf("freshness consultations for %q current=%d legacy=%d json=%d, want %d/%d/%d from winner %q", testCase.SessionID, current[testCase.SessionID], legacy[testCase.SessionID], stats[jsonPath], wantCurrent, wantLegacy, wantJSON, testCase.Expected)
		}
	}
}

func cloneIntMap(source map[string]int) map[string]int {
	cloned := make(map[string]int, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func assertCanonicalOrphanPart(t testing.TB, indexer *ingest.OpenCodeIndexer, session ingest.DiscoveredSession, managed []byte, testCase canonicalSelectionCase) {
	t.Helper()
	entries, err := indexer.IndexTranscriptBytes(t.Context(), session, managed)
	if err != nil {
		t.Fatalf("index canonical orphan session %q: %v", session.SessionID, err)
	}
	for _, entry := range entries {
		if entry.EntryID != nil && *entry.EntryID == testCase.OrphanEntry {
			if entry.ParentIndex != nil || entry.Depth != 0 || entry.ContentPreview == nil || !strings.Contains(*entry.ContentPreview, testCase.OrphanMarker) {
				t.Fatalf("orphan part was not retained at root with content: %+v", entry)
			}
			return
		}
	}
	t.Fatalf("orphan part %q was dropped from selected projection", testCase.OrphanEntry)
}

func assertCanonicalGraphAndToolPairing(t testing.TB, indexer *ingest.OpenCodeIndexer, session ingest.DiscoveredSession, managed []byte, testCase canonicalSelectionCase) {
	t.Helper()
	entries, err := indexer.IndexTranscriptBytes(t.Context(), session, managed)
	if err != nil {
		t.Fatalf("index canonical graph session %q: %v", session.SessionID, err)
	}
	indexes := make(map[string]int)
	toolPaired := false
	for index, entry := range entries {
		if entry.EntryID != nil {
			indexes[*entry.EntryID] = index
		}
		if entry.EntryID != nil && *entry.EntryID == testCase.ToolCallID && entry.ToolInput != nil && entry.ToolOutput != nil {
			toolPaired = true
		}
	}
	parentIndex, parentFound := indexes[testCase.LinkedParent]
	childIndex, childFound := indexes[testCase.LinkedChild]
	missingIndex, missingFound := indexes[testCase.MissingEntry]
	if !parentFound || !childFound || entries[childIndex].ParentIndex == nil || *entries[childIndex].ParentIndex != parentIndex || entries[childIndex].Depth != entries[parentIndex].Depth+1 {
		t.Fatalf("selected parent graph was not preserved: parent=%d/%t child=%d/%t child_entry=%+v", parentIndex, parentFound, childIndex, childFound, entries[childIndex])
	}
	if !missingFound || entries[missingIndex].ParentIndex != nil || entries[missingIndex].Depth != 0 {
		t.Fatalf("missing-parent entry was not retained at root: index=%d found=%t entry=%+v", missingIndex, missingFound, entries[missingIndex])
	}
	if !toolPaired {
		t.Fatalf("tool call/result %q was not paired on the selected graph", testCase.ToolCallID)
	}
}

func TestCanonicalOpenCodeSelectedSourceFreshness(t *testing.T) {
	fixture, err := loadCanonicalSelectionFixture(canonicalSelectionYAML)
	if err != nil {
		t.Fatal(err)
	}
	materialized := testfixture.MaterializeByName(t, fixture.SourceFixture)
	rootPath := filepath.Dir(materialized.Path)
	writeCanonicalJSONFixtures(t, rootPath, fixture)
	root, _ := ingest.NewResolvedPath(rootPath)
	adapterFactory := canonicalAdapterFactory(t, mountedCurrentEnvironment{"OPENCODE_DB": materialized.Path})
	discover := func() ingest.DiscoveredSession {
		adapter := adapterFactory(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
		sessions, discoverErr := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
		if discoverErr != nil {
			t.Fatal(discoverErr)
		}
		for _, session := range sessions {
			if session.SessionID == "ses_3cd91f52effeXd3QAJ54jOyzv5" {
				return session
			}
		}
		t.Fatal("selected freshness session was not discovered")
		return ingest.DiscoveredSession{}
	}
	selected := discover()
	ingestedMS := time.Now().UnixMilli()
	location := ingest.SessionLocation{IngestedMs: &ingestedMS, SchemaVersion: int(ingest.CurrentSchemaVersion)}
	updateSyntheticSelectionRow(t, materialized.Path, "message", "msg_legacy_all", ingestedMS+10_000)
	nonSelectedChanged := discover()
	if !nonSelectedChanged.ModTime.Equal(selected.ModTime) || ingest.ClassifyAgainstStore(nonSelectedChanged, location, 0) != ingest.DiffUnchanged {
		t.Fatalf("non-selected legacy change altered selected freshness: before=%s after=%s", selected.ModTime, nonSelectedChanged.ModTime)
	}
	updateSyntheticSelectionRow(t, materialized.Path, "session_message", "msg_current_all", ingestedMS+20_000)
	selectedChanged := discover()
	if !selectedChanged.ModTime.After(time.UnixMilli(ingestedMS)) || ingest.ClassifyAgainstStore(selectedChanged, location, 0) != ingest.DiffUpdated {
		t.Fatalf("selected current change did not trigger re-ingest: selected freshness=%s ingested=%s", selectedChanged.ModTime, time.UnixMilli(ingestedMS))
	}
}

func TestCanonicalOpenCodeSelectionFixtureRejectsMutations(t *testing.T) {
	fixture, err := loadCanonicalSelectionFixture(canonicalSelectionYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range fixture.LoaderMutations {
		mutated := append([]byte(nil), canonicalSelectionYAML...)
		switch mutation.Kind {
		case "unknown_field":
			mutated = bytes.Replace(mutated, []byte("source_fixture:"), []byte("unexpected:"), 1)
		case "wrong_count":
			mutated = bytes.Replace(mutated, []byte("declared_cases: 9"), []byte("declared_cases: 8"), 1)
		case "duplicate_name":
			mutated = bytes.Replace(mutated, []byte("current-and-legacy-prefers-current"), []byte("all-three-prefers-current"), 1)
		case "unknown_representation":
			mutated = bytes.Replace(mutated, []byte("current_sqlite"), []byte("event_history"), 1)
		case "trailing_document":
			mutated = append(mutated, []byte("\n---\nextra: true\n")...)
		}
		if _, err := loadCanonicalSelectionFixture(mutated); err == nil {
			t.Errorf("loader mutation %q was accepted", mutation.Name)
		}
	}
}

func canonicalAdapterFactory(t testing.TB, environment mountedCurrentEnvironment) ingest.AdapterFactory {
	t.Helper()
	return func(filesystem ingest.FileSystem, git ingest.GitResolver, installationSalt salt.Salt) ingest.SourceAdapter {
		candidateFS, ok := filesystem.(ingest.OpenCodeCandidateFileSystem)
		if !ok {
			t.Fatal("production filesystem lacks OpenCode candidate capability")
		}
		adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, git, installationSalt, "latest", environment, candidateFS, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions())
		if err != nil {
			t.Fatal(err)
		}
		return adapter
	}
}

func writeCanonicalJSONFixtures(t testing.TB, root string, fixture canonicalSelectionFixture) {
	t.Helper()
	wanted := map[string]string{"ses_3cd91f52effeXd3QAJ54jOyzv5": "JSON_ALL", "ses_3cd91f52effeXd3QAJ54jOyzv7": "JSON_CURRENT", "ses_3cd91f52effeXd3QAJ54jOyzv8": "JSON_LEGACY", "ses_3cd91f52effeXd3QAJ54jOyzvB": "JSON_ONLY"}
	caseNames := make([]string, 0, len(wanted))
	for sessionID := range wanted {
		caseNames = append(caseNames, sessionID)
	}
	sort.Strings(caseNames)
	for index, sessionID := range caseNames {
		sessionDir := filepath.Join(root, "storage", "session", "synthetic")
		messageDir := filepath.Join(root, "storage", "message", sessionID)
		partDir := filepath.Join(root, "storage", "part", "msg_json_"+sessionID)
		for _, directory := range []string{sessionDir, messageDir, partDir} {
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		sessionPath := filepath.Join(sessionDir, sessionID+".json")
		sessionJSON := fmt.Sprintf(`{"id":%q,"version":"synthetic","directory":"/synthetic/selection","title":%q,"time":{"created":%d,"updated":%d}}`, sessionID, sessionID, 3000+index, 3010+index)
		messageID := "msg_json_" + sessionID
		messageJSON := fmt.Sprintf(`{"id":%q,"sessionID":%q,"role":"user","path":{"cwd":"/synthetic/selection"},"time":{"created":%d},"content":%q}`, messageID, sessionID, 3000+index, wanted[sessionID])
		partJSON := fmt.Sprintf(`{"id":%q,"messageID":%q,"type":"text","text":%q,"time":{"created":%d}}`, "part_json_"+sessionID, messageID, wanted[sessionID], 3001+index)
		if err := os.WriteFile(sessionPath, []byte(sessionJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(messageDir, messageID+".json"), []byte(messageJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(partDir, "part_json_"+sessionID+".json"), []byte(partJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		modified := time.UnixMilli(fixture.JSONMTimeMS)
		if err := os.Chtimes(sessionPath, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
}

func representationForOrigin(origin ingest.TranscriptOrigin) ingest.OpenCodeCanonicalRepresentation {
	switch origin {
	case ingest.TranscriptOriginOpenCodeCurrentSQLite:
		return ingest.OpenCodeRepresentationCurrentSQLite
	case ingest.TranscriptOriginOpenCodeLegacySQLite:
		return ingest.OpenCodeRepresentationLegacySQLite
	default:
		return ingest.OpenCodeRepresentationLegacyJSON
	}
}

func hasMissingParentWarning(warnings []ingest.DiagnosticEntry, parentID string) bool {
	for _, warning := range warnings {
		if warning.ErrorType == string(ingest.OpenCodeGraphMissingParent) && strings.Contains(warning.Message, parentID) && warning.Location != "" && warning.Remediation != "" {
			return true
		}
	}
	return false
}

func updateSyntheticSelectionRow(t testing.TB, path, table, id string, modified int64) {
	t.Helper()
	connection, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	statement := "UPDATE " + table + " SET time_updated = ? WHERE id = ?"
	if err := sqlitex.Execute(connection, statement, &sqlitex.ExecOptions{Args: []any{modified, id}}); err != nil {
		t.Fatal(err)
	}
}
