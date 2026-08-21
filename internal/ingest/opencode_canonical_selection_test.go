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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	metricspkg "github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	expectedCanonicalSelectionCases     = 9
	expectedCanonicalSelectionMutations = 7
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

// canonicalSelectionMutationKind is the closed set of loader mutations the
// selection fixture proves non-vacuous.
type canonicalSelectionMutationKind string

const (
	canonicalMutationUnknownField          canonicalSelectionMutationKind = "unknown_field"
	canonicalMutationWrongCount            canonicalSelectionMutationKind = "wrong_count"
	canonicalMutationDuplicateName         canonicalSelectionMutationKind = "duplicate_name"
	canonicalMutationUnknownRepresentation canonicalSelectionMutationKind = "unknown_representation"
	canonicalMutationTrailingDocument      canonicalSelectionMutationKind = "trailing_document"
	canonicalMutationOrphanJSONSession     canonicalSelectionMutationKind = "orphan_json_session"
	canonicalMutationDroppedPartWithoutRow canonicalSelectionMutationKind = "dropped_part_without_row"
)

func (kind canonicalSelectionMutationKind) validate() error {
	switch kind {
	case canonicalMutationUnknownField, canonicalMutationWrongCount, canonicalMutationDuplicateName, canonicalMutationUnknownRepresentation, canonicalMutationTrailingDocument, canonicalMutationOrphanJSONSession, canonicalMutationDroppedPartWithoutRow:
		return nil
	default:
		return fmt.Errorf("canonical OpenCode selection fixture has unknown loader mutation %q", kind)
	}
}

type canonicalSelectionFixture struct {
	DeclaredCases           int                                `yaml:"declared_cases"`
	SourceFixture           string                             `yaml:"source_fixture"`
	JSONMTimeMS             int64                              `yaml:"json_mtime_ms"`
	JSONSessions            []canonicalSelectionJSONSession    `yaml:"json_sessions"`
	ParentLinks             []canonicalSelectionParentLink     `yaml:"parent_links"`
	StrayOrphanParts        []canonicalSelectionStrayPart      `yaml:"stray_orphan_parts"`
	Freshness               canonicalSelectionFreshness        `yaml:"freshness"`
	Cases                   []canonicalSelectionCase           `yaml:"cases"`
	LoaderMutations         []canonicalSelectionLoaderMutation `yaml:"loader_mutations"`
	DeclaredLoaderMutations int                                `yaml:"declared_loader_mutations"`
}

type canonicalSelectionJSONSession struct {
	SessionID string `yaml:"session_id"`
	Marker    string `yaml:"marker"`
}

type canonicalSelectionParentLink struct {
	SessionID string `yaml:"session_id"`
	ParentID  string `yaml:"parent_id"`
}

// canonicalSelectionStrayPart is one part row inserted beside the corpus. Its
// data may be malformed on purpose, which the corpus validator would reject.
type canonicalSelectionStrayPart struct {
	ID          string `yaml:"id"`
	MessageID   string `yaml:"message_id"`
	SessionID   string `yaml:"session_id"`
	TimeCreated int64  `yaml:"time_created"`
	Data        string `yaml:"data"`
	Dropped     bool   `yaml:"dropped"`
}

type canonicalSelectionRow struct {
	Table string `yaml:"table"`
	ID    string `yaml:"id"`
}

type canonicalSelectionDeletion struct {
	Session string `yaml:"session"`
	Table   string `yaml:"table"`
	ID      string `yaml:"id"`
}

type canonicalSelectionFreshness struct {
	SelectedSession string                     `yaml:"selected_session"`
	NonSelectedRow  canonicalSelectionRow      `yaml:"non_selected_row"`
	SelectedRow     canonicalSelectionRow      `yaml:"selected_row"`
	Deletion        canonicalSelectionDeletion `yaml:"deletion"`
}

type canonicalSelectionCase struct {
	Name                string                           `yaml:"name"`
	SessionID           string                           `yaml:"session_id"`
	Representations     []canonicalFixtureRepresentation `yaml:"representations"`
	Expected            canonicalFixtureRepresentation   `yaml:"expected"`
	Marker              string                           `yaml:"marker"`
	ExpectedFreshnessMS int64                            `yaml:"expected_freshness_ms"`
	ExpectedParent      string                           `yaml:"expected_parent"`
	MissingParent       string                           `yaml:"missing_parent"`
	LinkedParent        string                           `yaml:"linked_parent"`
	LinkedChild         string                           `yaml:"linked_child"`
	MissingEntry        string                           `yaml:"missing_entry"`
	ToolCallID          string                           `yaml:"tool_call_id"`
	OrphanEntry         string                           `yaml:"orphan_entry"`
	OrphanParent        string                           `yaml:"orphan_parent"`
	OrphanMarker        string                           `yaml:"orphan_marker"`
	OrphanToolEntry     string                           `yaml:"orphan_tool_entry"`
	DroppedOrphanParts  []string                         `yaml:"dropped_orphan_parts"`
}

type canonicalSelectionLoaderMutation struct {
	Name string                         `yaml:"name"`
	Kind canonicalSelectionMutationKind `yaml:"kind"`
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
	if fixture.Freshness.SelectedSession == "" || fixture.Freshness.NonSelectedRow.ID == "" || fixture.Freshness.SelectedRow.ID == "" || fixture.Freshness.Deletion.ID == "" || fixture.Freshness.Deletion.Session == "" {
		return fixture, errors.New("canonical OpenCode selection fixture has an incomplete freshness probe")
	}
	cases := make(map[string]canonicalSelectionCase, len(fixture.Cases))
	seen := make(map[string]bool, len(fixture.Cases))
	strayIDs := make(map[string]bool, len(fixture.StrayOrphanParts))
	for _, part := range fixture.StrayOrphanParts {
		if part.ID == "" || part.MessageID == "" || part.SessionID == "" || part.Data == "" || strayIDs[part.ID] {
			return fixture, fmt.Errorf("canonical OpenCode selection fixture has an incomplete or duplicate stray part %+v", part)
		}
		strayIDs[part.ID] = true
	}
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || testCase.SessionID == "" || testCase.Marker == "" || testCase.ExpectedFreshnessMS <= 0 || seen[testCase.Name] || len(testCase.Representations) == 0 {
			return fixture, fmt.Errorf("canonical OpenCode selection fixture contains an incomplete or duplicate case %+v", testCase)
		}
		seen[testCase.Name] = true
		cases[testCase.SessionID] = testCase
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
		for _, dropped := range testCase.DroppedOrphanParts {
			if !strayIDs[dropped] {
				return fixture, fmt.Errorf("canonical OpenCode selection case %q expects dropped part %q that no stray row defines", testCase.Name, dropped)
			}
		}
		if testCase.OrphanToolEntry != "" && !strayIDs[testCase.OrphanToolEntry] {
			return fixture, fmt.Errorf("canonical OpenCode selection case %q expects orphan tool entry %q that no stray row defines", testCase.Name, testCase.OrphanToolEntry)
		}
	}
	for _, session := range fixture.JSONSessions {
		testCase, known := cases[session.SessionID]
		if session.SessionID == "" || session.Marker == "" || !known || !hasCanonicalRepresentation(testCase, canonicalFixtureJSON) {
			return fixture, fmt.Errorf("canonical OpenCode selection fixture JSON session %+v does not match a case with a legacy JSON representation", session)
		}
	}
	for _, link := range fixture.ParentLinks {
		testCase, known := cases[link.SessionID]
		if link.SessionID == "" || link.ParentID == "" || !known || testCase.ExpectedParent != link.ParentID {
			return fixture, fmt.Errorf("canonical OpenCode selection fixture parent link %+v does not match its case expectation", link)
		}
	}
	for _, mutation := range fixture.LoaderMutations {
		if mutation.Name == "" {
			return fixture, errors.New("canonical OpenCode selection fixture has an unnamed loader mutation")
		}
		if err := mutation.Kind.validate(); err != nil {
			return fixture, err
		}
	}
	return fixture, nil
}

func hasCanonicalRepresentation(testCase canonicalSelectionCase, wanted canonicalFixtureRepresentation) bool {
	for _, representation := range testCase.Representations {
		if representation == wanted {
			return true
		}
	}
	return false
}

// prepareCanonicalSelectionRoot materializes the corpus, writes the JSON
// fixtures, and inserts the parent links and stray parts.
func prepareCanonicalSelectionRoot(t testing.TB, fixture canonicalSelectionFixture) (string, string) {
	t.Helper()
	materialized := testfixture.MaterializeByName(t, fixture.SourceFixture)
	rootPath := filepath.Dir(materialized.Path)
	writeCanonicalJSONFixtures(t, rootPath, fixture)
	withCanonicalConnection(t, materialized.Path, func(connection *sqlite.Conn) error {
		for _, link := range fixture.ParentLinks {
			if err := sqlitex.Execute(connection, `INSERT OR IGNORE INTO session(id) VALUES (?1)`, &sqlitex.ExecOptions{Args: []any{link.SessionID}}); err != nil {
				return err
			}
			if err := sqlitex.Execute(connection, `UPDATE session SET parent_id = ?2 WHERE id = ?1`, &sqlitex.ExecOptions{Args: []any{link.SessionID, link.ParentID}}); err != nil {
				return err
			}
		}
		for _, part := range fixture.StrayOrphanParts {
			if err := sqlitex.Execute(connection, `INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES (?1, ?2, ?3, ?4, ?4, ?5)`, &sqlitex.ExecOptions{Args: []any{part.ID, part.MessageID, part.SessionID, part.TimeCreated, part.Data}}); err != nil {
				return err
			}
		}
		return nil
	})
	return rootPath, materialized.Path
}

func withCanonicalConnection(t testing.TB, path string, work func(*sqlite.Conn) error) {
	t.Helper()
	connection, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	workErr := work(connection)
	closeErr := connection.Close()
	if workErr != nil || closeErr != nil {
		t.Fatalf("write canonical selection synthetic rows: %v", errors.Join(workErr, closeErr))
	}
}

func TestCanonicalOpenCodeSelectionMountedMatrix(t *testing.T) {
	fixture, err := loadCanonicalSelectionFixture(canonicalSelectionYAML)
	if err != nil {
		t.Fatal(err)
	}
	rootPath, databasePath := prepareCanonicalSelectionRoot(t, fixture)
	root, err := ingest.NewResolvedPath(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	environment := mountedCurrentEnvironment{"OPENCODE_DB": databasePath}
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
	shallowIndexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{})
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
		assertCanonicalParentLink(t, session, testCase)
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
			assertCanonicalOrphanPart(t, indexer, shallowIndexer, session, entriesMarker, testCase)
			if !hasMissingParentWarning(metadata.Diagnostics.Warnings, testCase.OrphanParent) {
				t.Errorf("selected session %q lacks orphan-part parent diagnostic for %q: %+v", testCase.SessionID, testCase.OrphanParent, metadata.Diagnostics.Warnings)
			}
		}
		assertCanonicalDroppedOrphanParts(t, metadata, entriesMarker, testCase)
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

func assertCanonicalParentLink(t testing.TB, session ingest.DiscoveredSession, testCase canonicalSelectionCase) {
	t.Helper()
	if testCase.ExpectedParent == "" {
		if session.ParentUUID != nil {
			t.Errorf("session %q carries parent %q, want a root session", testCase.SessionID, *session.ParentUUID)
		}
		return
	}
	if session.ParentUUID == nil || string(*session.ParentUUID) != testCase.ExpectedParent {
		t.Errorf("session %q parent=%v, want session.parent_id link %q", testCase.SessionID, session.ParentUUID, testCase.ExpectedParent)
	}
}

type canonicalFreshnessRecorder struct {
	mu           sync.Mutex
	current      map[string]int
	legacy       map[string]int
	currentBatch int
	legacyBatch  int
}

func newCanonicalFreshnessRecorder() *canonicalFreshnessRecorder {
	return &canonicalFreshnessRecorder{current: make(map[string]int), legacy: make(map[string]int)}
}

type canonicalRecordingSource struct {
	ingest.OpenCodeSQLiteSource
	recorder *canonicalFreshnessRecorder
}

var _ ingest.OpenCodeSQLiteSource = canonicalRecordingSource{}

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

func (source canonicalRecordingSource) CurrentFreshnessBySession(ctx context.Context) (map[string]time.Time, error) {
	source.recorder.mu.Lock()
	source.recorder.currentBatch++
	source.recorder.mu.Unlock()
	return source.OpenCodeSQLiteSource.CurrentFreshnessBySession(ctx)
}

func (source canonicalRecordingSource) LegacyFreshnessBySession(ctx context.Context) (map[string]time.Time, error) {
	source.recorder.mu.Lock()
	source.recorder.legacyBatch++
	source.recorder.mu.Unlock()
	return source.OpenCodeSQLiteSource.LegacyFreshnessBySession(ctx)
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

var _ ingest.FileSystem = (*canonicalFreshnessFileSystem)(nil)
var _ ingest.OpenCodeCandidateFileSystem = (*canonicalFreshnessFileSystem)(nil)

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
	currentBatch := recorder.currentBatch
	legacyBatch := recorder.legacyBatch
	recorder.mu.Unlock()
	filesystem.mu.Lock()
	stats := cloneIntMap(filesystem.sessionStats)
	filesystem.mu.Unlock()
	// Row freshness is read per table, never per session, so no per-session
	// aggregate is consulted and the batch statement count is bounded by the
	// present representations rather than by the number of sessions.
	if len(current) != 0 || len(legacy) != 0 {
		t.Fatalf("per-session freshness was consulted: current=%v legacy=%v", current, legacy)
	}
	wantCurrentBatch, wantLegacyBatch := 0, 0
	for _, testCase := range fixture.Cases {
		switch testCase.Expected {
		case canonicalFixtureCurrent:
			wantCurrentBatch = 1
		case canonicalFixtureLegacy:
			wantLegacyBatch = 1
		}
	}
	if currentBatch != wantCurrentBatch || legacyBatch != wantLegacyBatch {
		t.Fatalf("row freshness batch statements current=%d legacy=%d, want %d/%d bounded by present representations", currentBatch, legacyBatch, wantCurrentBatch, wantLegacyBatch)
	}
	for _, testCase := range fixture.Cases {
		wantJSON := 0
		if testCase.Expected == canonicalFixtureJSON {
			wantJSON = 1
		}
		jsonPath := filepath.Join(root, "storage", "session", "synthetic", testCase.SessionID+".json")
		if stats[jsonPath] != wantJSON {
			t.Fatalf("JSON freshness stat for %q = %d, want %d from winner %q", testCase.SessionID, stats[jsonPath], wantJSON, testCase.Expected)
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

// assertCanonicalOrphanPart checks that a usable orphan part stays at the root
// as an inert system note, that an orphan tool part never becomes a tool turn,
// and that orphans follow the same depth gate as ordinary parts.
func assertCanonicalOrphanPart(t testing.TB, indexer, shallowIndexer *ingest.OpenCodeIndexer, session ingest.DiscoveredSession, managed []byte, testCase canonicalSelectionCase) {
	t.Helper()
	entries, err := indexer.IndexTranscriptBytes(t.Context(), session, managed)
	if err != nil {
		t.Fatalf("index canonical orphan session %q: %v", session.SessionID, err)
	}
	found := false
	toolFound := testCase.OrphanToolEntry == ""
	for _, entry := range entries {
		if entry.EntryID == nil {
			continue
		}
		if *entry.EntryID == testCase.OrphanEntry {
			if entry.ParentIndex != nil || entry.Depth != 0 || entry.Role != schema.RoleSystem || entry.EntryType != schema.EntryTypeSystem || entry.ContentPreview == nil || !strings.Contains(*entry.ContentPreview, testCase.OrphanMarker) {
				t.Fatalf("orphan part was not retained at root as an inert note with content: %+v", entry)
			}
			found = true
		}
		if testCase.OrphanToolEntry != "" && *entry.EntryID == testCase.OrphanToolEntry {
			if entry.ParentIndex != nil || entry.Depth != 0 || entry.Role != schema.RoleSystem || entry.EntryType != schema.EntryTypeSystem || entry.HasToolUse || entry.ToolCallID != nil || entry.ToolInput != nil || entry.ContentPreview == nil || *entry.ContentPreview == "" {
				t.Fatalf("orphan tool part surfaced as a tool turn instead of an inert note: %+v", entry)
			}
			toolFound = true
		}
	}
	if !found {
		t.Fatalf("orphan part %q was dropped from selected projection", testCase.OrphanEntry)
	}
	if !toolFound {
		t.Fatalf("orphan tool part %q was dropped from selected projection", testCase.OrphanToolEntry)
	}
	shallow, err := shallowIndexer.IndexTranscriptBytes(t.Context(), session, managed)
	if err != nil {
		t.Fatalf("index canonical orphan session %q without part depth: %v", session.SessionID, err)
	}
	for _, entry := range shallow {
		if entry.EntryID != nil && (*entry.EntryID == testCase.OrphanEntry || *entry.EntryID == testCase.OrphanToolEntry) {
			t.Fatalf("orphan part %q was emitted without the part depth gate: %+v", *entry.EntryID, entry)
		}
	}
}

// assertCanonicalDroppedOrphanParts checks that unusable orphan rows are
// dropped with a warning and never fail the session.
func assertCanonicalDroppedOrphanParts(t testing.TB, metadata *ingest.UnifiedMetadata, managed []byte, testCase canonicalSelectionCase) {
	t.Helper()
	for _, dropped := range testCase.DroppedOrphanParts {
		if bytes.Contains(managed, []byte(dropped)) {
			t.Errorf("session %q still carries unusable orphan part %q in its transcript", testCase.SessionID, dropped)
		}
		warned := false
		for _, warning := range metadata.Diagnostics.Warnings {
			if warning.ErrorType == string(ingest.OpenCodeGraphOrphanPartDropped) && strings.Contains(warning.Location, dropped) && warning.Message != "" && warning.Remediation != "" {
				warned = true
			}
		}
		if !warned {
			t.Errorf("session %q lacks a dropped-orphan warning for %q: %+v", testCase.SessionID, dropped, metadata.Diagnostics.Warnings)
		}
	}
}

// assertCanonicalGraphAndToolPairing checks the parent link by entry index
// while both messages keep Depth 0. Depth 0 and Depth 1 remain the message
// and part discriminators that every consumer relies on.
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
		if entry.EntryID != nil && *entry.EntryID == testCase.ToolCallID && entry.Depth == 1 && entry.ToolInput != nil && entry.ToolOutput != nil {
			toolPaired = true
		}
	}
	parentIndex, parentFound := indexes[testCase.LinkedParent]
	childIndex, childFound := indexes[testCase.LinkedChild]
	missingIndex, missingFound := indexes[testCase.MissingEntry]
	if !parentFound || !childFound || entries[childIndex].ParentIndex == nil || *entries[childIndex].ParentIndex != parentIndex || entries[childIndex].Depth != 0 || entries[parentIndex].Depth != 0 {
		t.Fatalf("selected parent graph was not preserved at message depth: parent=%d/%t child=%d/%t child_entry=%+v", parentIndex, parentFound, childIndex, childFound, entries[childIndex])
	}
	if !missingFound || entries[missingIndex].ParentIndex != nil || entries[missingIndex].Depth != 0 {
		t.Fatalf("missing-parent entry was not retained at root: index=%d found=%t entry=%+v", missingIndex, missingFound, entries[missingIndex])
	}
	if !toolPaired {
		t.Fatalf("tool call/result %q was not paired on the selected graph", testCase.ToolCallID)
	}
}

// TestCanonicalOpenCodeSelectedSourceFreshness proves three freshness rules.
// A losing representation's row change does not move the winner. The winner's
// row change moves it. A row deletion moves it through the upstream session
// clock even though the surviving rows' times went down.
func TestCanonicalOpenCodeSelectedSourceFreshness(t *testing.T) {
	fixture, err := loadCanonicalSelectionFixture(canonicalSelectionYAML)
	if err != nil {
		t.Fatal(err)
	}
	rootPath, databasePath := prepareCanonicalSelectionRoot(t, fixture)
	root, _ := ingest.NewResolvedPath(rootPath)
	adapterFactory := canonicalAdapterFactory(t, mountedCurrentEnvironment{"OPENCODE_DB": databasePath})
	discover := func(sessionID string) ingest.DiscoveredSession {
		adapter := adapterFactory(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
		sessions, discoverErr := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
		if discoverErr != nil {
			t.Fatal(discoverErr)
		}
		for _, session := range sessions {
			if string(session.SessionID) == sessionID {
				return session
			}
		}
		t.Fatalf("selected freshness session %q was not discovered", sessionID)
		return ingest.DiscoveredSession{}
	}
	probe := fixture.Freshness
	selected := discover(probe.SelectedSession)
	ingestedMS := time.Now().UnixMilli()
	location := ingest.SessionLocation{IngestedMs: &ingestedMS, SchemaVersion: int(ingest.CurrentSchemaVersion)}

	updateSyntheticSelectionRow(t, databasePath, probe.NonSelectedRow.Table, probe.NonSelectedRow.ID, ingestedMS+10_000)
	nonSelectedChanged := discover(probe.SelectedSession)
	if !nonSelectedChanged.ModTime.Equal(selected.ModTime) || ingest.ClassifyAgainstStore(nonSelectedChanged, location, 0) != ingest.DiffUnchanged {
		t.Fatalf("non-selected legacy change altered selected freshness: before=%s after=%s", selected.ModTime, nonSelectedChanged.ModTime)
	}

	updateSyntheticSelectionRow(t, databasePath, probe.SelectedRow.Table, probe.SelectedRow.ID, ingestedMS+20_000)
	selectedChanged := discover(probe.SelectedSession)
	if !selectedChanged.ModTime.Equal(time.UnixMilli(ingestedMS+20_000)) || ingest.ClassifyAgainstStore(selectedChanged, location, 0) != ingest.DiffUpdated {
		t.Fatalf("selected current change did not trigger re-ingest: selected freshness=%s ingested=%s", selectedChanged.ModTime, time.UnixMilli(ingestedMS))
	}

	beforeDeletion := discover(probe.Deletion.Session)
	if ingest.ClassifyAgainstStore(beforeDeletion, location, 0) != ingest.DiffUnchanged {
		t.Fatalf("deletion probe session %q was not unchanged before the deletion: freshness=%s", probe.Deletion.Session, beforeDeletion.ModTime)
	}
	// OpenCode deletes rows on revert and undo and moves session.time_updated
	// in the same flow. The surviving rows' own times go down, so only the
	// session clock can report the change.
	deleteSyntheticSelectionRow(t, databasePath, probe.Deletion.Table, probe.Deletion.ID)
	deletedAt := time.UnixMilli(ingestedMS + 10_000)
	updateSyntheticSessionClock(t, databasePath, probe.Deletion.Session, deletedAt.UnixMilli())
	afterDeletion := discover(probe.Deletion.Session)
	if !afterDeletion.ModTime.Equal(deletedAt) || ingest.ClassifyAgainstStore(afterDeletion, location, 0) != ingest.DiffUpdated {
		t.Fatalf("row deletion did not trigger re-ingest: freshness=%s ingested=%s", afterDeletion.ModTime, time.UnixMilli(ingestedMS))
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
		case canonicalMutationUnknownField:
			mutated = bytes.Replace(mutated, []byte("source_fixture:"), []byte("unexpected:"), 1)
		case canonicalMutationWrongCount:
			mutated = bytes.Replace(mutated, []byte("declared_cases: 9"), []byte("declared_cases: 8"), 1)
		case canonicalMutationDuplicateName:
			mutated = bytes.Replace(mutated, []byte("current-and-legacy-prefers-current"), []byte("all-three-prefers-current"), 1)
		case canonicalMutationUnknownRepresentation:
			mutated = bytes.Replace(mutated, []byte("current_sqlite"), []byte("event_history"), 1)
		case canonicalMutationTrailingDocument:
			mutated = append(mutated, []byte("\n---\nextra: true\n")...)
		case canonicalMutationOrphanJSONSession:
			mutated = bytes.Replace(mutated, []byte("{session_id: ses_3cd91f52effeXd3QAJ54jOyzvB, marker: JSON_ONLY}"), []byte("{session_id: ses_3cd91f52effeXd3QAJ54jOyzvA, marker: JSON_ONLY}"), 1)
		case canonicalMutationDroppedPartWithoutRow:
			mutated = bytes.Replace(mutated, []byte("dropped_orphan_parts: [part_legacy_truncated, part_legacy_step_start]"), []byte("dropped_orphan_parts: [part_legacy_truncated, part_legacy_unknown]"), 1)
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
	for index, session := range fixture.JSONSessions {
		sessionID := session.SessionID
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
		messageJSON := fmt.Sprintf(`{"id":%q,"sessionID":%q,"role":"user","path":{"cwd":"/synthetic/selection"},"time":{"created":%d},"content":%q}`, messageID, sessionID, 3000+index, session.Marker)
		partJSON := fmt.Sprintf(`{"id":%q,"messageID":%q,"type":"text","text":%q,"time":{"created":%d}}`, "part_json_"+sessionID, messageID, session.Marker, 3001+index)
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
	withCanonicalConnection(t, path, func(connection *sqlite.Conn) error {
		return sqlitex.Execute(connection, "UPDATE "+table+" SET time_updated = ? WHERE id = ?", &sqlitex.ExecOptions{Args: []any{modified, id}})
	})
}

func updateSyntheticSessionClock(t testing.TB, path, sessionID string, modified int64) {
	t.Helper()
	withCanonicalConnection(t, path, func(connection *sqlite.Conn) error {
		return sqlitex.Execute(connection, "UPDATE session SET time_updated = ? WHERE id = ?", &sqlitex.ExecOptions{Args: []any{modified, sessionID}})
	})
}

func deleteSyntheticSelectionRow(t testing.TB, path, table, id string) {
	t.Helper()
	withCanonicalConnection(t, path, func(connection *sqlite.Conn) error {
		return sqlitex.Execute(connection, "DELETE FROM "+table+" WHERE id = ?", &sqlitex.ExecOptions{Args: []any{id}})
	})
}
