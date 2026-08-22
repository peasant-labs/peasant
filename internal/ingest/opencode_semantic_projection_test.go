package ingest

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
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

const (
	expectedOpenCodeSemanticCases     = 1
	expectedOpenCodeSemanticNegatives = 11
	expectedOpenCodeSemanticMutations = 4
	expectedOpenCodeCurrentVariants   = 8
	expectedOpenCodeSemanticMutants   = 15
	expectedOpenCodeManagedErrors     = 11
)

type openCodeSemanticFixture struct {
	DeclaredCases             int                              `yaml:"declared_cases"`
	Cases                     []openCodeSemanticCase           `yaml:"cases"`
	DeclaredNegativeCases     int                              `yaml:"declared_negative_cases"`
	NegativeCases             []openCodeSemanticNegativeCase   `yaml:"negative_cases"`
	DeclaredLoaderMutations   int                              `yaml:"declared_loader_mutations"`
	LoaderMutations           []openCodeSemanticLoaderMutation `yaml:"loader_mutations"`
	DeclaredCurrentVariants   int                              `yaml:"declared_current_variants"`
	CurrentVariants           []openCodeSemanticCurrentRow     `yaml:"current_variants"`
	ExpectedCurrentIdentities []string                         `yaml:"expected_current_identities"`
	DeclaredSemanticMutants   int                              `yaml:"declared_semantic_mutants"`
	SemanticMutants           []openCodeSemanticMutant         `yaml:"semantic_mutants"`
	DeclaredSourceMutations   int                              `yaml:"declared_source_mutations"`
	SourceMutations           []openCodeSemanticSourceMutation `yaml:"source_mutations"`
	DeclaredManagedErrors     int                              `yaml:"declared_managed_errors"`
	ManagedErrors             []openCodeManagedErrorCase       `yaml:"managed_errors"`
}

type openCodeSemanticCase struct {
	Name                  string                        `yaml:"name"`
	LegacyFixture         string                        `yaml:"legacy_fixture"`
	CurrentFixture        string                        `yaml:"current_fixture"`
	SessionID             string                        `yaml:"session_id"`
	ExpectedEntryIDs      []string                      `yaml:"expected_entry_ids"`
	ExpectedRoles         []string                      `yaml:"expected_roles"`
	ExpectedTypes         []string                      `yaml:"expected_types"`
	ExpectedDepths        []int                         `yaml:"expected_depths"`
	ExpectedParentIndexes []int                         `yaml:"expected_parent_indexes"`
	ExpectedTimestamps    []int64                       `yaml:"expected_timestamps"`
	ExpectedToolCallID    string                        `yaml:"expected_tool_call_id"`
	ExpectedModel         string                        `yaml:"expected_model"`
	ExpectedTokensIn      int                           `yaml:"expected_tokens_in"`
	ExpectedTokensOut     int                           `yaml:"expected_tokens_out"`
	ExpectedStartMS       int64                         `yaml:"expected_start_ms"`
	ExpectedEndMS         int64                         `yaml:"expected_end_ms"`
	ForbiddenMarkers      []string                      `yaml:"forbidden_markers"`
	JSONMessages          []openCodeSemanticJSONMessage `yaml:"json_messages"`
	ExpectedMetadataTurns int                           `yaml:"expected_metadata_turns"`
	ExpectedMetadataTools int                           `yaml:"expected_metadata_tools"`
}

type openCodeSemanticJSONMessage struct {
	ID    string                     `yaml:"id"`
	Data  string                     `yaml:"data"`
	Parts []openCodeSemanticJSONPart `yaml:"parts"`
}

type openCodeSemanticJSONPart struct {
	ID   string `yaml:"id"`
	Data string `yaml:"data"`
}

type openCodeSemanticNegativeCase struct {
	Name          string                       `yaml:"name"`
	Rows          []openCodeSemanticCurrentRow `yaml:"rows"`
	ErrorContains string                       `yaml:"error_contains"`
}

type openCodeSemanticCurrentRow struct {
	ID      string `yaml:"id"`
	RowType string `yaml:"row_type"`
	Data    string `yaml:"data"`
}

type openCodeSemanticLoaderMutation struct {
	Name          string `yaml:"name"`
	Kind          string `yaml:"kind"`
	ErrorContains string `yaml:"error_contains"`
}

type openCodeSemanticMutant struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"`
	Axis string `yaml:"axis"`
}
type openCodeSemanticSourceMutation struct {
	Name   string `yaml:"name"`
	Source string `yaml:"source"`
	Old    string `yaml:"old"`
	New    string `yaml:"new"`
}

type openCodeSemanticMutationSource string

const (
	openCodeSemanticMutationSourceFile    openCodeSemanticMutationSource = "file"
	openCodeSemanticMutationSourceLegacy  openCodeSemanticMutationSource = "legacy"
	openCodeSemanticMutationSourceCurrent openCodeSemanticMutationSource = "current"
)

type openCodeManagedErrorCase struct {
	Name             string `yaml:"name"`
	Data             string `yaml:"data"`
	Origin           string `yaml:"origin"`
	ErrorContains    string `yaml:"error_contains"`
	ErrorType        string `yaml:"error_type"`
	RecognitionError bool   `yaml:"recognition_error"`
}

//go:embed testdata/opencode_semantic_parity.yaml
var openCodeSemanticFixtureYAML []byte

func loadOpenCodeSemanticFixture(data []byte) (openCodeSemanticFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture openCodeSemanticFixture
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode OpenCode semantic parity fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixture, fmt.Errorf("decode OpenCode semantic parity fixture: expected exactly one YAML document: %w", err)
	}
	if fixture.DeclaredCases != expectedOpenCodeSemanticCases || len(fixture.Cases) != expectedOpenCodeSemanticCases || fixture.DeclaredNegativeCases != expectedOpenCodeSemanticNegatives || len(fixture.NegativeCases) != expectedOpenCodeSemanticNegatives || fixture.DeclaredLoaderMutations != expectedOpenCodeSemanticMutations || len(fixture.LoaderMutations) != expectedOpenCodeSemanticMutations || fixture.DeclaredCurrentVariants != expectedOpenCodeCurrentVariants || len(fixture.CurrentVariants) != expectedOpenCodeCurrentVariants || len(fixture.ExpectedCurrentIdentities) == 0 || fixture.DeclaredSemanticMutants != expectedOpenCodeSemanticMutants || len(fixture.SemanticMutants) != expectedOpenCodeSemanticMutants || fixture.DeclaredSourceMutations != 3 || len(fixture.SourceMutations) != 3 || fixture.DeclaredManagedErrors != expectedOpenCodeManagedErrors || len(fixture.ManagedErrors) != expectedOpenCodeManagedErrors {
		return fixture, fmt.Errorf("validate OpenCode semantic parity fixture row guard: cases=%d/%d negatives=%d/%d mutations=%d/%d", fixture.DeclaredCases, len(fixture.Cases), fixture.DeclaredNegativeCases, len(fixture.NegativeCases), fixture.DeclaredLoaderMutations, len(fixture.LoaderMutations))
	}
	names := make(map[string]struct{})
	for _, item := range appendSemanticFixtureNames(fixture) {
		if item == "" {
			return fixture, errors.New("validate OpenCode semantic parity fixture: empty case name")
		}
		if _, duplicate := names[item]; duplicate {
			return fixture, fmt.Errorf("validate OpenCode semantic parity fixture: duplicate case name %q", item)
		}
		names[item] = struct{}{}
	}
	for _, testCase := range fixture.Cases {
		count := len(testCase.ExpectedEntryIDs)
		if testCase.LegacyFixture == "" || testCase.CurrentFixture == "" || testCase.SessionID == "" || len(testCase.JSONMessages) == 0 || count == 0 || len(testCase.ExpectedRoles) != count || len(testCase.ExpectedTypes) != count || len(testCase.ExpectedDepths) != count || len(testCase.ExpectedParentIndexes) != count || len(testCase.ExpectedTimestamps) != count || testCase.ExpectedMetadataTurns <= 0 || testCase.ExpectedMetadataTools <= 0 {
			return fixture, fmt.Errorf("validate OpenCode semantic parity fixture %q: expected vectors are incomplete or have unequal lengths", testCase.Name)
		}
	}
	for _, negative := range fixture.NegativeCases {
		if len(negative.Rows) == 0 || negative.ErrorContains == "" {
			return fixture, fmt.Errorf("validate OpenCode semantic parity negative %q: rows and error substring are required", negative.Name)
		}
		for _, row := range negative.Rows {
			if row.ID == "" || row.RowType == "" || !json.Valid([]byte(row.Data)) {
				return fixture, fmt.Errorf("validate OpenCode semantic parity negative %q: each row requires id, type, and valid JSON data", negative.Name)
			}
		}
	}
	for _, row := range fixture.CurrentVariants {
		if row.ID == "" || row.RowType == "" || !json.Valid([]byte(row.Data)) {
			return fixture, errors.New("validate OpenCode current variant fixture: id, type, and valid JSON data are required")
		}
	}
	for _, mutant := range fixture.SemanticMutants {
		if mutant.Name == "" || mutant.Kind == "" || mutant.Axis == "" {
			return fixture, errors.New("validate OpenCode semantic mutant fixture: name, kind, and axis are required")
		}
	}
	for _, mutant := range fixture.SourceMutations {
		source := openCodeSemanticMutationSource(mutant.Source)
		if mutant.Name == "" || (source != openCodeSemanticMutationSourceFile && source != openCodeSemanticMutationSourceLegacy && source != openCodeSemanticMutationSourceCurrent) || mutant.Old == "" || len(mutant.Old) != len(mutant.New) {
			return fixture, errors.New("validate OpenCode source mutation fixture: name, source, and equal-width replacement are required")
		}
	}
	for _, item := range fixture.ManagedErrors {
		if item.Name == "" || item.Data == "" || item.ErrorContains == "" || item.ErrorType == "" || (item.Origin != "" && item.Origin != "current" && item.Origin != "unknown") {
			return fixture, errors.New("validate managed OpenCode error fixture: name, data, error substring, error type, and a supported origin selector are required")
		}
	}
	return fixture, nil
}

func TestOpenCodeCurrentPinnedSessionMessageVariants(t *testing.T) {
	fixture, err := loadOpenCodeSemanticFixture(openCodeSemanticFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	rows := semanticCurrentRows(t, fixture.CurrentVariants)
	sessionID, _ := NewOpenCodeCurrentSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
	pageSize, _ := NewOpenCodeCurrentPageSize(32)
	projection, err := readOpenCodeCurrentProjection(t.Context(), semanticNegativeSource{rows: rows}, sessionID, pageSize)
	if err != nil {
		t.Fatalf("normalize pinned upstream SessionMessage variants: %v", err)
	}
	if len(projection.Messages) != expectedOpenCodeCurrentVariants {
		t.Fatalf("normalized messages=%d want %d", len(projection.Messages), expectedOpenCodeCurrentVariants)
	}
	data, _ := json.Marshal(projection)
	indexer := NewOpenCodeIndexer(&OSFileSystem{}, WithOpenCodeFullDepth(true), WithOpenCodeFullContent(true))
	peasantID, _ := NewSessionID(sessionID.String())
	entries, err := indexer.IndexTranscriptBytes(t.Context(), DiscoveredSession{SessionID: peasantID, TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite}, data)
	if err != nil {
		t.Fatalf("index pinned upstream variants: %v", err)
	}
	identities := make([]string, 0, len(entries)*2)
	for _, entry := range entries {
		if entry.EntryID != nil {
			identities = append(identities, *entry.EntryID)
		}
		if entry.ToolCallID != nil {
			identities = append(identities, *entry.ToolCallID)
		}
	}
	joined := strings.Join(identities, ",")
	for _, required := range fixture.ExpectedCurrentIdentities {
		if !strings.Contains(joined, required) {
			t.Errorf("indexed variants omit stable identity %q", required)
		}
	}
}

func appendSemanticFixtureNames(fixture openCodeSemanticFixture) []string {
	names := make([]string, 0, len(fixture.Cases)+len(fixture.NegativeCases)+len(fixture.LoaderMutations)+len(fixture.SemanticMutants))
	for _, item := range fixture.Cases {
		names = append(names, item.Name)
	}
	for _, item := range fixture.NegativeCases {
		names = append(names, item.Name)
	}
	for _, item := range fixture.LoaderMutations {
		names = append(names, item.Name)
	}
	for _, item := range fixture.SemanticMutants {
		names = append(names, item.Name)
	}
	for _, item := range fixture.ManagedErrors {
		names = append(names, item.Name)
	}
	return names
}

func TestOpenCodeManagedCurrentDiagnosticsPreserveCauses(t *testing.T) {
	fixture, err := loadOpenCodeSemanticFixture(openCodeSemanticFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, _ := NewSessionID(fixture.Cases[0].SessionID)
	indexer := NewOpenCodeIndexer(&OSFileSystem{})
	for _, testCase := range fixture.ManagedErrors {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			origin := TranscriptOriginOpenCodeCurrentSQLite
			if testCase.Origin == "unknown" {
				origin = TranscriptOrigin(99)
			}
			_, err := indexer.IndexTranscriptBytes(t.Context(), DiscoveredSession{SessionID: sessionID, TranscriptOrigin: origin}, []byte(testCase.Data))
			if err == nil || !strings.Contains(err.Error(), testCase.ErrorContains) || strings.Contains(err.Error(), "managed legacy") {
				t.Fatalf("current diagnostic=%v want source-neutral actionable substring %q", err, testCase.ErrorContains)
			}
			switch testCase.ErrorType {
			case "syntax":
				var target *json.SyntaxError
				if !errors.As(err, &target) {
					t.Fatalf("syntax cause not preserved: %v", err)
				}
			case "type":
				var target *json.UnmarshalTypeError
				if !errors.As(err, &target) {
					t.Fatalf("type cause not preserved: %v", err)
				}
			case "identity", "strict", "origin":
			default:
				t.Fatalf("unknown managed error type %q", testCase.ErrorType)
			}
		})
	}
}

func TestOpenCodeManagedProjectionRecognitionRejectsCorruptMarkers(t *testing.T) {
	fixture, err := loadOpenCodeSemanticFixture(openCodeSemanticFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, _ := NewSessionID(fixture.Cases[0].SessionID)
	for _, testCase := range fixture.ManagedErrors {
		if !testCase.RecognitionError {
			continue
		}
		t.Run(testCase.Name, func(t *testing.T) {
			origin, recognitionErr := recognizeManagedOpenCodeProjection([]byte(testCase.Data), sessionID)
			if recognitionErr == nil || origin != TranscriptOriginFile || !strings.Contains(recognitionErr.Error(), "recovery stopped before legacy fallback") {
				t.Fatalf("recognition origin=%d error=%v; corrupt managed marker must fail closed before legacy fallback", origin, recognitionErr)
			}
		})
	}
}

func TestOpenCodeSemanticParityMutantsChangeOwnedAxis(t *testing.T) {
	fixture, err := loadOpenCodeSemanticFixture(openCodeSemanticFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	testCase := fixture.Cases[0]
	materialized := testfixture.MaterializeByName(t, testCase.CurrentFixture)
	source := openSemanticSource(t, materialized.Path)
	currentID, _ := NewOpenCodeCurrentSessionID(testCase.SessionID)
	pageSize, _ := NewOpenCodeCurrentPageSize(MaxOpenCodeCurrentPageSize)
	projection, err := readOpenCodeCurrentProjection(t.Context(), source, currentID, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	_ = source.Close(t.Context())
	sessionID, _ := NewSessionID(testCase.SessionID)
	session := DiscoveredSession{SessionID: sessionID, TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite}
	baselineBytes, _ := json.Marshal(projection)
	fullIndexer := NewOpenCodeIndexer(&OSFileSystem{}, WithOpenCodeFullDepth(true), WithOpenCodeFullContent(true))
	baseline, err := fullIndexer.IndexTranscriptBytes(t.Context(), session, baselineBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutant := range fixture.SemanticMutants {
		mutant := mutant
		t.Run(mutant.Name, func(t *testing.T) {
			mutated := cloneCurrentProjection(t, projection)
			indexer := fullIndexer
			applySemanticMutant(t, mutant.Kind, &mutated, &indexer)
			data, _ := json.Marshal(mutated)
			got, err := indexer.IndexTranscriptBytes(t.Context(), session, data)
			if err != nil {
				return
			}
			if reflect.DeepEqual(canonicalSemanticEntries(baseline), canonicalSemanticEntries(got)) {
				t.Fatalf("mutant %q survived without changing intended %s axis", mutant.Kind, mutant.Axis)
			}
		})
	}
}

func cloneCurrentProjection(t testing.TB, projection openCodeCurrentProjection) openCodeCurrentProjection {
	t.Helper()
	data, _ := json.Marshal(projection)
	var clone openCodeCurrentProjection
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func applySemanticMutant(t testing.TB, kind string, projection *openCodeCurrentProjection, indexer **OpenCodeIndexer) {
	t.Helper()
	mutateJSON := func(raw *json.RawMessage, old, next string) { *raw = bytes.Replace(*raw, []byte(old), []byte(next), 1) }
	switch kind {
	case "timestamp":
		projection.Messages[0].TimeCreated++
	case "skill":
		mutateJSON(&projection.Messages[3].Parts[0].Data, `"name":"skill"`, `"name":"read"`)
	case "subtask":
		mutateJSON(&projection.Messages[5].Parts[0].Data, `"type":"subtask"`, `"type":"text"`)
	case "agent":
		mutateJSON(&projection.Messages[4].Parts[0].Data, `"type":"agent"`, `"type":"text"`)
	case "compaction":
		mutateJSON(&projection.Messages[2].Parts[0].Data, `"type":"compaction"`, `"type":"text"`)
	case "reasoning":
		mutateJSON(&projection.Messages[1].Parts[1].Data, `synthetic reasoning`, `mutated reasoning`)
	case "stable_id":
		projection.Messages[0].ID = "msg_mutated"
	case "tool_pairing":
		mutateJSON(&projection.Messages[1].Parts[0].Data, `call_semantic`, `call_mutated`)
	case "role":
		mutateJSON(&projection.Messages[0].Data, `"role":"user"`, `"role":"system"`)
	case "model":
		mutateJSON(&projection.Messages[1].Data, `synthetic-model`, `mutated-model`)
	case "tokens":
		mutateJSON(&projection.Messages[1].Data, `"input":7`, `"input":70`)
	case "order":
		projection.Messages[0], projection.Messages[1] = projection.Messages[1], projection.Messages[0]
	case "depth_parent":
		*indexer = NewOpenCodeIndexer(&OSFileSystem{}, WithOpenCodeFullDepth(false), WithOpenCodeFullContent(true))
	case "metadata":
		projection.Messages[1].Parts = projection.Messages[1].Parts[:1]
	case "turns_detail_metrics":
		projection.Messages = projection.Messages[1:]
	default:
		t.Fatalf("unknown semantic mutant kind %q", kind)
	}
}

func TestOpenCodeThreeSourceSemanticProjectionParity(t *testing.T) {
	fixture, err := loadOpenCodeSemanticFixture(openCodeSemanticFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			legacyMaterialized := testfixture.MaterializeByName(t, testCase.LegacyFixture)
			currentMaterialized := testfixture.MaterializeByName(t, testCase.CurrentFixture)
			legacySource := openSemanticSource(t, legacyMaterialized.Path)
			legacyID, _ := NewOpenCodeLegacySessionID(testCase.SessionID)
			legacyPageSize, _ := NewOpenCodeLegacyPageSize(MaxOpenCodeLegacyPageSize)
			legacyProjection, err := readOpenCodeLegacyProjection(t.Context(), legacySource, legacyID, legacyPageSize)
			if err != nil {
				t.Fatalf("load production legacy projection: %v", err)
			}
			if err := legacySource.Close(t.Context()); err != nil {
				t.Fatalf("close legacy source: %v", err)
			}

			currentSource := openSemanticSource(t, currentMaterialized.Path)
			currentID, _ := NewOpenCodeCurrentSessionID(testCase.SessionID)
			currentPageSize, _ := NewOpenCodeCurrentPageSize(MaxOpenCodeCurrentPageSize)
			currentProjection, err := readOpenCodeCurrentProjection(t.Context(), currentSource, currentID, currentPageSize)
			if err != nil {
				t.Fatalf("load production current projection: %v", err)
			}
			if err := currentSource.Close(t.Context()); err != nil {
				t.Fatalf("close current source: %v", err)
			}

			sessionID, _ := NewSessionID(testCase.SessionID)
			indexer := NewOpenCodeIndexer(&OSFileSystem{}, WithOpenCodeFullDepth(true), WithOpenCodeFullContent(true))
			legacyBytes, _ := json.Marshal(legacyProjection)
			legacySession := DiscoveredSession{SessionID: sessionID, Harness: HarnessOpenCode, SourceFormat: SourceFormatJSON, TranscriptOrigin: TranscriptOriginOpenCodeLegacySQLite}
			legacyEntries, err := indexer.IndexTranscriptBytes(t.Context(), legacySession, legacyBytes)
			if err != nil {
				t.Fatalf("index production legacy projection: %v", err)
			}

			currentBytes, _ := json.Marshal(currentProjection)
			currentSession := DiscoveredSession{SessionID: sessionID, Harness: HarnessOpenCode, SourceFormat: SourceFormatJSON, TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite}
			currentEntries, err := indexer.IndexTranscriptBytes(t.Context(), currentSession, currentBytes)
			if err != nil {
				t.Fatalf("index production current projection: %v", err)
			}

			jsonSession := materializeSemanticJSONTree(t, testCase)
			jsonEntries, err := indexer.IndexTranscript(t.Context(), jsonSession)
			if err != nil {
				t.Fatalf("index production JSON tree: %v", err)
			}

			assertSemanticEntries(t, testCase, legacyEntries)
			if !reflect.DeepEqual(canonicalSemanticEntries(legacyEntries), canonicalSemanticEntries(currentEntries)) || !reflect.DeepEqual(canonicalSemanticEntries(legacyEntries), canonicalSemanticEntries(jsonEntries)) {
				legacyCanonical, _ := json.Marshal(canonicalSemanticEntries(legacyEntries))
				currentCanonical, _ := json.Marshal(canonicalSemanticEntries(currentEntries))
				jsonCanonical, _ := json.Marshal(canonicalSemanticEntries(jsonEntries))
				t.Fatalf("three source loaders diverged\nlegacy=%s\ncurrent=%s\njson=%s", legacyCanonical, currentCanonical, jsonCanonical)
			}
			for _, marker := range testCase.ForbiddenMarkers {
				if bytes.Contains(currentBytes, []byte(marker)) {
					t.Fatalf("managed current projection leaked forbidden marker %q", marker)
				}
			}
			adapter := NewOpenCodeAdapter(&OSFileSystem{}, semanticNoGit{}, salt.Salt{})
			legacySession.CWD, currentSession.CWD = "/synthetic/parity", "/synthetic/parity"
			legacyMetadata, err := adapter.metadataFromManagedProjection(t.Context(), legacySession, legacyProjection)
			if err != nil {
				t.Fatal(err)
			}
			currentManaged := openCodeLegacyProjection{Format: currentProjection.Format, Version: currentProjection.Version, SessionID: currentProjection.SessionID, Messages: currentProjection.Messages}
			currentMetadata, err := adapter.metadataFromManagedProjection(t.Context(), currentSession, currentManaged)
			if err != nil {
				t.Fatal(err)
			}
			jsonMetadata, err := adapter.ExtractMetadata(t.Context(), jsonSession)
			if err != nil {
				t.Fatal(err)
			}
			assertSemanticMetadata(t, testCase, legacyMetadata, currentMetadata, jsonMetadata)
		})
	}
}

func TestOpenCodeThreeSourceSemanticProjectionMutationsChangeCanonicalArtifact(t *testing.T) {
	fixture, err := loadOpenCodeSemanticFixture(openCodeSemanticFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	testCase := fixture.Cases[0]
	sessionID, _ := NewSessionID(testCase.SessionID)
	indexer := NewOpenCodeIndexer(&OSFileSystem{}, WithOpenCodeFullDepth(true), WithOpenCodeFullContent(true))
	baseline := indexSemanticParityCurrent(t, testfixture.MaterializeByName(t, testCase.CurrentFixture).Path, sessionID, testCase.SessionID, indexer)
	for _, mutant := range fixture.SourceMutations {
		mutant := mutant
		t.Run(mutant.Name, func(t *testing.T) {
			var got []schema.SessionEntry
			switch openCodeSemanticMutationSource(mutant.Source) {
			case openCodeSemanticMutationSourceCurrent:
				materialized := testfixture.MaterializeByName(t, testCase.CurrentFixture)
				mutateSyntheticSourceBytes(t, materialized.Path, mutant.Old, mutant.New)
				got = indexSemanticParityCurrent(t, materialized.Path, sessionID, testCase.SessionID, indexer)
			case openCodeSemanticMutationSourceLegacy:
				materialized := testfixture.MaterializeByName(t, testCase.LegacyFixture)
				mutateSyntheticSourceBytes(t, materialized.Path, mutant.Old, mutant.New)
				got = indexSemanticParityLegacy(t, materialized.Path, sessionID, testCase.SessionID, indexer)
			case openCodeSemanticMutationSourceFile:
				mutatedCase := testCase
				mutatedCase.JSONMessages = append([]openCodeSemanticJSONMessage(nil), testCase.JSONMessages...)
				mutatedCase.JSONMessages[0].Data = strings.Replace(mutatedCase.JSONMessages[0].Data, mutant.Old, mutant.New, 1)
				got, err = indexer.IndexTranscript(t.Context(), materializeSemanticJSONTree(t, mutatedCase))
				if err != nil {
					t.Fatal(err)
				}
			}
			if reflect.DeepEqual(canonicalSemanticEntries(baseline), canonicalSemanticEntries(got)) {
				t.Fatalf("%s source mutation did not change its canonical artifact", mutant.Source)
			}
		})
	}
}

func mutateSyntheticSourceBytes(t testing.TB, path, old, next string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutated := bytes.Replace(data, []byte(old), []byte(next), 1)
	if bytes.Equal(data, mutated) {
		t.Fatalf("synthetic source does not contain mutation marker %q", old)
	}
	if err := os.WriteFile(path, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
}

func indexSemanticParityCurrent(t testing.TB, path string, sessionID SessionID, rawSessionID string, indexer *OpenCodeIndexer) []schema.SessionEntry {
	t.Helper()
	source := openSemanticSource(t, path)
	currentID, _ := NewOpenCodeCurrentSessionID(rawSessionID)
	pageSize, _ := NewOpenCodeCurrentPageSize(MaxOpenCodeCurrentPageSize)
	projection, err := readOpenCodeCurrentProjection(context.Background(), source, currentID, pageSize)
	if closeErr := source.Close(context.Background()); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(projection)
	entries, err := indexer.IndexTranscriptBytes(context.Background(), DiscoveredSession{SessionID: sessionID, TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite}, data)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func indexSemanticParityLegacy(t testing.TB, path string, sessionID SessionID, rawSessionID string, indexer *OpenCodeIndexer) []schema.SessionEntry {
	t.Helper()
	source := openSemanticSource(t, path)
	legacyID, _ := NewOpenCodeLegacySessionID(rawSessionID)
	pageSize, _ := NewOpenCodeLegacyPageSize(MaxOpenCodeLegacyPageSize)
	projection, err := readOpenCodeLegacyProjection(context.Background(), source, legacyID, pageSize)
	if closeErr := source.Close(context.Background()); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(projection)
	entries, err := indexer.IndexTranscriptBytes(context.Background(), DiscoveredSession{SessionID: sessionID, TranscriptOrigin: TranscriptOriginOpenCodeLegacySQLite}, data)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func openSemanticSource(t testing.TB, path string) OpenCodeSQLiteSource {
	t.Helper()
	typedPath, err := NewOpenCodeSQLiteSourcePath(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := OpenOpenCodeSQLiteSource(context.Background(), typedPath, DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func materializeSemanticJSONTree(t testing.TB, testCase openCodeSemanticCase) DiscoveredSession {
	t.Helper()
	root := t.TempDir()
	storage := filepath.Join(root, defaults.OpenCodeDirStorage.String())
	sessionPath := filepath.Join(storage, defaults.OpenCodeDirSession.String(), "project", testCase.SessionID+defaults.ExtJSON.String())
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	sessionJSON := fmt.Sprintf(`{"id":%q,"version":"fixture-version","directory":"/synthetic/parity","time":{"created":%d,"updated":%d}}`, testCase.SessionID, testCase.ExpectedStartMS, testCase.ExpectedEndMS)
	if err := os.WriteFile(sessionPath, []byte(sessionJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, message := range testCase.JSONMessages {
		messageDir := filepath.Join(storage, defaults.OpenCodeDirMessage.String(), testCase.SessionID)
		if err := os.MkdirAll(messageDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(messageDir, message.ID+defaults.ExtJSON.String()), []byte(message.Data), 0o600); err != nil {
			t.Fatal(err)
		}
		partDir := filepath.Join(storage, defaults.OpenCodeDirPart.String(), message.ID)
		if err := os.MkdirAll(partDir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, part := range message.Parts {
			if err := os.WriteFile(filepath.Join(partDir, part.ID+defaults.ExtJSON.String()), []byte(part.Data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	sessionID, _ := NewSessionID(testCase.SessionID)
	return DiscoveredSession{SessionID: sessionID, Harness: HarnessOpenCode, SourcePath: ResolvedPath(sessionPath), SourceFormat: SourceFormatJSON, OriginalRoot: ResolvedPath(root), TranscriptOrigin: TranscriptOriginFile}
}

type semanticNoGit struct{}

func (semanticNoGit) RemoteURL(context.Context, string) (string, error) {
	return "", errors.New("fixture has no git")
}
func (semanticNoGit) Branch(context.Context, string) (string, error) {
	return "", errors.New("fixture has no git")
}
func (semanticNoGit) Worktree(context.Context, string) (string, error) {
	return "", errors.New("fixture has no git")
}
func (semanticNoGit) TrackingBranch(context.Context, string) (string, error) {
	return "", errors.New("fixture has no git")
}
func (semanticNoGit) UserEmail(context.Context) (string, error) {
	return "", errors.New("fixture has no git")
}
func (semanticNoGit) WalkUpRemoteURL(context.Context, string) (string, string, error) {
	return "", "", nil
}

func assertSemanticMetadata(t testing.TB, testCase openCodeSemanticCase, values ...*UnifiedMetadata) {
	t.Helper()
	for _, metadata := range values {
		if metadata.Timestamp.Start != testCase.ExpectedStartMS || metadata.Timestamp.End != testCase.ExpectedEndMS || metadata.Stats.TurnCount != testCase.ExpectedMetadataTurns || metadata.Stats.ToolCallCount != testCase.ExpectedMetadataTools || metadata.Stats.TokensIn != testCase.ExpectedTokensIn || metadata.Stats.TokensOut != testCase.ExpectedTokensOut || string(metadata.Model) != testCase.ExpectedModel || metadata.CWD != "/synthetic/parity" {
			t.Errorf("semantic metadata diverged: timestamp=%+v stats=%+v model=%q cwd=%q", metadata.Timestamp, metadata.Stats, metadata.Model, metadata.CWD)
		}
	}
}

func canonicalSemanticEntries(entries []schema.SessionEntry) []schema.SessionEntry {
	canonical := append([]schema.SessionEntry(nil), entries...)
	for index := range canonical {
		canonical[index].RawByteLength = nil
	}
	// Structural current messages have no upstream nested identity. Their parent
	// row identity remains stable; nested structural EntryID is source-specific.
	for index := range canonical {
		if canonical[index].PartType != nil && (*canonical[index].PartType == "compaction" || *canonical[index].PartType == "agent" || *canonical[index].PartType == "subtask") {
			canonical[index].EntryID = nil
		}
	}
	return canonical
}

func assertSemanticEntries(t testing.TB, testCase openCodeSemanticCase, entries []schema.SessionEntry) {
	t.Helper()
	if len(entries) != len(testCase.ExpectedEntryIDs) {
		t.Fatalf("entries=%d want %d", len(entries), len(testCase.ExpectedEntryIDs))
	}
	for index, entry := range entries {
		entryID := ""
		if entry.EntryID != nil {
			entryID = *entry.EntryID
		}
		parent := -1
		if entry.ParentIndex != nil {
			parent = *entry.ParentIndex
		}
		if entryID != testCase.ExpectedEntryIDs[index] || string(entry.Role) != testCase.ExpectedRoles[index] || string(entry.EntryType) != testCase.ExpectedTypes[index] || entry.Depth != testCase.ExpectedDepths[index] || parent != testCase.ExpectedParentIndexes[index] {
			t.Errorf("entry %d identity/shape = id=%q role=%q type=%q depth=%d parent=%d", index, entryID, entry.Role, entry.EntryType, entry.Depth, parent)
		}
		if entry.TimestampMs == nil || *entry.TimestampMs != testCase.ExpectedTimestamps[index] {
			t.Errorf("entry %d timestamp=%v want %d", index, entry.TimestampMs, testCase.ExpectedTimestamps[index])
		}
	}
	if entries[1].TokensIn == nil || *entries[1].TokensIn != testCase.ExpectedTokensIn || entries[1].TokensOut == nil || *entries[1].TokensOut != testCase.ExpectedTokensOut {
		t.Errorf("assistant token accounting=%v/%v", entries[1].TokensIn, entries[1].TokensOut)
	}
	if entries[2].ToolCallID == nil || *entries[2].ToolCallID != testCase.ExpectedToolCallID {
		t.Errorf("tool call/result pairing was not preserved")
	}
	if entries[1].Extra == nil || !strings.Contains(*entries[1].Extra, testCase.ExpectedModel) {
		t.Errorf("assistant model observation missing: %v", entries[1].Extra)
	}
	if entries[6].Extra == nil || !strings.Contains(*entries[6].Extra, "/fixture-skill") {
		t.Errorf("skill semantic marker missing: %v", entries[6].Extra)
	}
}

type semanticNegativeSource struct{ rows []OpenCodeCurrentMessageRow }

var _ OpenCodeSQLiteSource = semanticNegativeSource{}

func (source semanticNegativeSource) Catalog(context.Context) (OpenCodeSchemaEvidence, error) {
	return OpenCodeSchemaEvidence{}, nil
}
func (source semanticNegativeSource) CurrentSessionIDs(context.Context, OpenCodeCurrentSessionPageRequest) (OpenCodeCurrentSessionPage, error) {
	return OpenCodeCurrentSessionPage{}, nil
}
func (source semanticNegativeSource) LegacySessionIDs(context.Context, OpenCodeLegacySessionPageRequest) (OpenCodeLegacySessionPage, error) {
	return OpenCodeLegacySessionPage{}, nil
}
func (source semanticNegativeSource) CurrentFreshnessBySession(context.Context) (map[string]time.Time, error) {
	return nil, nil
}
func (source semanticNegativeSource) LegacyFreshnessBySession(context.Context) (map[string]time.Time, error) {
	return nil, nil
}
func (source semanticNegativeSource) LegacyMessages(context.Context, OpenCodeLegacyMessagePageRequest) (OpenCodeLegacyMessagePage, error) {
	return OpenCodeLegacyMessagePage{}, nil
}
func (source semanticNegativeSource) LegacySessionParts(context.Context, OpenCodeLegacySessionPartPageRequest) (OpenCodeLegacyPartPage, error) {
	return OpenCodeLegacyPartPage{}, nil
}
func (source semanticNegativeSource) SessionRecords(context.Context, OpenCodeSessionRecordPageRequest) (OpenCodeSessionRecordPage, error) {
	return OpenCodeSessionRecordPage{}, nil
}
func (source semanticNegativeSource) CurrentMessages(context.Context, OpenCodeCurrentPageRequest) (OpenCodeCurrentPage, error) {
	return OpenCodeCurrentPage{Messages: source.rows}, nil
}
func (source semanticNegativeSource) Close(context.Context) error { return nil }

func TestOpenCodeCurrentNormalizationRejectsStrictNegativeCases(t *testing.T) {
	fixture, err := loadOpenCodeSemanticFixture(openCodeSemanticFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, _ := NewOpenCodeCurrentSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
	pageSize, _ := NewOpenCodeCurrentPageSize(8)
	for _, testCase := range fixture.NegativeCases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			_, err := readOpenCodeCurrentProjection(t.Context(), semanticNegativeSource{rows: semanticCurrentRows(t, testCase.Rows)}, sessionID, pageSize)
			if err == nil || !strings.Contains(err.Error(), testCase.ErrorContains) {
				t.Fatalf("error=%v want substring %q", err, testCase.ErrorContains)
			}
		})
	}
}

func semanticCurrentRows(t testing.TB, fixtures []openCodeSemanticCurrentRow) []OpenCodeCurrentMessageRow {
	t.Helper()
	sessionID, _ := NewOpenCodeCurrentSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
	rows := make([]OpenCodeCurrentMessageRow, 0, len(fixtures))
	for index, fixture := range fixtures {
		id, err := NewOpenCodeCurrentMessageID(fixture.ID)
		if err != nil {
			t.Fatal(err)
		}
		rowType, err := NewOpenCodeCurrentMessageType(fixture.RowType)
		if err != nil {
			t.Fatal(err)
		}
		seq, _ := NewOpenCodeCurrentSeq(int64(index + 1))
		rows = append(rows, OpenCodeCurrentMessageRow{ID: id, SessionID: sessionID, Type: rowType, Data: fixture.Data, Seq: seq, TimeCreated: int64(2000 + index), TimeUpdated: int64(2000 + index)})
	}
	return rows
}

func TestOpenCodeSemanticFixtureLoaderRejectsMutations(t *testing.T) {
	fixture, err := loadOpenCodeSemanticFixture(openCodeSemanticFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range fixture.LoaderMutations {
		mutated := append([]byte(nil), openCodeSemanticFixtureYAML...)
		switch mutation.Kind {
		case "unknown_field":
			mutated = bytes.Replace(mutated, []byte("legacy_fixture:"), []byte("unexpected:"), 1)
		case "trailing_document":
			mutated = append(mutated, []byte("\n---\nextra: true\n")...)
		case "declared_count":
			mutated = bytes.Replace(mutated, []byte("declared_cases: 1"), []byte("declared_cases: 2"), 1)
		case "duplicate_name":
			mutated = bytes.Replace(mutated, []byte("reject-unknown-row-type"), []byte(fixture.Cases[0].Name), 1)
		default:
			t.Fatalf("unknown fixture mutation %q", mutation.Kind)
		}
		if _, err := loadOpenCodeSemanticFixture(mutated); err == nil || !strings.Contains(err.Error(), mutation.ErrorContains) {
			t.Errorf("mutation %q error=%v", mutation.Name, err)
		}
	}
}
