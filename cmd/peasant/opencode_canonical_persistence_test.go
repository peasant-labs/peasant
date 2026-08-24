package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// expectedCanonicalPersistenceSessions is the scenario's own canonical session
// count, cross-checked against per-case data; it is the contract, not a row tally.
const expectedCanonicalPersistenceSessions = 7

type canonicalPersistenceCombination string

const (
	canonicalPersistenceAllThree      canonicalPersistenceCombination = "current_legacy_json"
	canonicalPersistenceCurrentLegacy canonicalPersistenceCombination = "current_legacy"
	canonicalPersistenceCurrentJSON   canonicalPersistenceCombination = "current_json"
	canonicalPersistenceLegacyJSON    canonicalPersistenceCombination = "legacy_json"
	canonicalPersistenceCurrentOnly   canonicalPersistenceCombination = "current_only"
	canonicalPersistenceLegacyOnly    canonicalPersistenceCombination = "legacy_only"
	canonicalPersistenceJSONOnly      canonicalPersistenceCombination = "json_only"
)

type canonicalPersistenceFixture struct {
	RequiredCases           []string                             `yaml:"required_cases"`
	Cases                   []canonicalPersistenceCase           `yaml:"cases"`
	RequiredLoaderMutations []string                             `yaml:"required_loader_mutations"`
	LoaderMutations         []canonicalPersistenceLoaderMutation `yaml:"loader_mutations"`
}

type canonicalPersistenceCase struct {
	Name                  string                        `yaml:"name"`
	SourceFixture         string                        `yaml:"source_fixture"`
	ExpectedSessions      int                           `yaml:"expected_sessions"`
	JSONSessions          []canonicalPersistenceJSON    `yaml:"json_sessions"`
	CanonicalSessions     []canonicalPersistenceSession `yaml:"canonical_sessions"`
	GraphSession          string                        `yaml:"graph_session"`
	ParentEntry           string                        `yaml:"parent_entry"`
	ChildEntry            string                        `yaml:"child_entry"`
	MissingParentEntry    string                        `yaml:"missing_parent_entry"`
	ToolCallID            string                        `yaml:"tool_call_id"`
	ToolResultContains    string                        `yaml:"tool_result_contains"`
	OrphanSession         string                        `yaml:"orphan_session"`
	OrphanEntry           string                        `yaml:"orphan_entry"`
	OrphanContentContains string                        `yaml:"orphan_content_contains"`
	ExpectedMetrics       canonicalPersistenceMetrics   `yaml:"expected_metrics"`
}

type canonicalPersistenceSession struct {
	Name            string                          `yaml:"name"`
	Combination     canonicalPersistenceCombination `yaml:"combination"`
	SessionID       string                          `yaml:"session_id"`
	WinningMarker   string                          `yaml:"winning_marker"`
	LosingMarkers   []string                        `yaml:"losing_markers"`
	OrderedEntryIDs []string                        `yaml:"ordered_entry_ids"`
	ExpectedMetrics canonicalPersistenceMetrics     `yaml:"expected_metrics"`
}

type canonicalPersistenceMetrics struct {
	TurnCount      int `yaml:"turn_count"`
	ToolCalls      int `yaml:"tool_calls"`
	ComputeVersion int `yaml:"compute_version"`
}

type canonicalPersistenceJSON struct {
	SessionID string `yaml:"session_id"`
	Marker    string `yaml:"marker"`
}

// canonicalPersistenceMutationKind is the closed set of loader mutations the
// persistence fixture proves non-vacuous.
type canonicalPersistenceMutationKind string

const (
	persistenceMutationUnknownField         canonicalPersistenceMutationKind = "unknown_field"
	persistenceMutationWrongCount           canonicalPersistenceMutationKind = "wrong_count"
	persistenceMutationDuplicateName        canonicalPersistenceMutationKind = "duplicate_name"
	persistenceMutationTrailingDocument     canonicalPersistenceMutationKind = "trailing_document"
	persistenceMutationWrongSessionCount    canonicalPersistenceMutationKind = "wrong_session_count"
	persistenceMutationDuplicateSessionID   canonicalPersistenceMutationKind = "duplicate_session_id"
	persistenceMutationUnknownCombination   canonicalPersistenceMutationKind = "unknown_combination"
	persistenceMutationOrphanJSONReference  canonicalPersistenceMutationKind = "orphan_json_reference"
	persistenceMutationMissingWinningMarker canonicalPersistenceMutationKind = "missing_winning_marker"
	persistenceMutationMissingOrderedEntry  canonicalPersistenceMutationKind = "missing_ordered_entry"
	persistenceMutationInvalidMetrics       canonicalPersistenceMutationKind = "invalid_metrics"
)

func (kind canonicalPersistenceMutationKind) validate() error {
	switch kind {
	case persistenceMutationUnknownField, persistenceMutationWrongCount, persistenceMutationDuplicateName, persistenceMutationTrailingDocument, persistenceMutationWrongSessionCount, persistenceMutationDuplicateSessionID, persistenceMutationUnknownCombination, persistenceMutationOrphanJSONReference, persistenceMutationMissingWinningMarker, persistenceMutationMissingOrderedEntry, persistenceMutationInvalidMetrics:
		return nil
	default:
		return fmt.Errorf("canonical persistence fixture has unknown mutation %q", kind)
	}
}

type canonicalPersistenceLoaderMutation struct {
	Name string                           `yaml:"name"`
	Kind canonicalPersistenceMutationKind `yaml:"kind"`
}

//go:embed testdata/opencode_canonical_persistence.yaml
var canonicalPersistenceYAML []byte

func loadCanonicalPersistenceFixture(data []byte) (canonicalPersistenceFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture canonicalPersistenceFixture
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode canonical OpenCode persistence fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixture, errors.New("canonical OpenCode persistence fixture must contain exactly one YAML document")
	}
	if len(fixture.RequiredCases) == 0 || len(fixture.RequiredLoaderMutations) == 0 {
		return fixture, errors.New("canonical OpenCode persistence fixture declares an empty required manifest")
	}
	seen := make(map[string]bool)
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || seen[testCase.Name] || testCase.SourceFixture == "" || testCase.ExpectedSessions != expectedCanonicalPersistenceSessions+2 || len(testCase.JSONSessions) == 0 || len(testCase.CanonicalSessions) != expectedCanonicalPersistenceSessions || testCase.GraphSession == "" || testCase.ParentEntry == "" || testCase.ChildEntry == "" || testCase.MissingParentEntry == "" || testCase.ToolCallID == "" || testCase.ToolResultContains == "" || testCase.OrphanSession == "" || testCase.OrphanEntry == "" || testCase.OrphanContentContains == "" || testCase.ExpectedMetrics.TurnCount <= 0 || testCase.ExpectedMetrics.ToolCalls <= 0 || testCase.ExpectedMetrics.ComputeVersion <= 0 {
			return fixture, fmt.Errorf("canonical OpenCode persistence fixture contains incomplete or duplicate case %+v", testCase)
		}
		seen[testCase.Name] = true
		wantedCombinations := map[canonicalPersistenceCombination]bool{
			canonicalPersistenceAllThree: false, canonicalPersistenceCurrentLegacy: false,
			canonicalPersistenceCurrentJSON: false, canonicalPersistenceLegacyJSON: false,
			canonicalPersistenceCurrentOnly: false, canonicalPersistenceLegacyOnly: false,
			canonicalPersistenceJSONOnly: false,
		}
		jsonMarkers := make(map[string]string, len(testCase.JSONSessions))
		for _, session := range testCase.JSONSessions {
			if session.SessionID == "" || session.Marker == "" || jsonMarkers[session.SessionID] != "" {
				return fixture, fmt.Errorf("canonical persistence case %q has incomplete or duplicate JSON session %+v", testCase.Name, session)
			}
			jsonMarkers[session.SessionID] = session.Marker
		}
		sessionIDs := make(map[string]bool, len(testCase.CanonicalSessions))
		markers := make(map[string]bool)
		for _, session := range testCase.CanonicalSessions {
			_, knownCombination := wantedCombinations[session.Combination]
			jsonMarker := jsonMarkers[session.SessionID]
			if session.Name == "" || seen[session.Name] || !knownCombination || wantedCombinations[session.Combination] || session.SessionID == "" || sessionIDs[session.SessionID] || session.WinningMarker == "" || len(session.LosingMarkers) != session.Combination.expectedLosingMarkers() || (jsonMarker != "") != session.Combination.includesJSON() || jsonMarker != "" && session.WinningMarker != jsonMarker && !slices.Contains(session.LosingMarkers, jsonMarker) || len(session.OrderedEntryIDs) == 0 || !session.ExpectedMetrics.valid() {
				return fixture, fmt.Errorf("canonical persistence case %q has incomplete, duplicate, or unknown session %+v", testCase.Name, session)
			}
			seen[session.Name] = true
			sessionIDs[session.SessionID] = true
			wantedCombinations[session.Combination] = true
			for _, marker := range append([]string{session.WinningMarker}, session.LosingMarkers...) {
				if marker == "" || markers[marker] {
					return fixture, fmt.Errorf("canonical persistence case %q has empty or duplicate marker %q", testCase.Name, marker)
				}
				markers[marker] = true
			}
			entryIDs := make(map[string]bool, len(session.OrderedEntryIDs))
			for _, entryID := range session.OrderedEntryIDs {
				if entryID == "" || entryIDs[entryID] {
					return fixture, fmt.Errorf("canonical persistence session %q has empty or duplicate ordered entry ID %q", session.Name, entryID)
				}
				entryIDs[entryID] = true
			}
		}
		for sessionID := range jsonMarkers {
			if !sessionIDs[sessionID] {
				return fixture, fmt.Errorf("canonical persistence JSON session %q lacks a canonical-session expectation", sessionID)
			}
		}
	}
	for _, mutation := range fixture.LoaderMutations {
		if mutation.Name == "" || seen[mutation.Name] {
			return fixture, fmt.Errorf("canonical persistence fixture has incomplete or duplicate loader mutation %q", mutation.Name)
		}
		seen[mutation.Name] = true
		if err := mutation.Kind.validate(); err != nil {
			return fixture, err
		}
	}
	for _, name := range fixture.RequiredCases {
		if !seen[name] {
			return fixture, fmt.Errorf("canonical OpenCode persistence fixture is missing required case %q", name)
		}
	}
	for _, name := range fixture.RequiredLoaderMutations {
		if !seen[name] {
			return fixture, fmt.Errorf("canonical OpenCode persistence fixture is missing required loader mutation %q", name)
		}
	}
	return fixture, nil
}

func (metrics canonicalPersistenceMetrics) valid() bool {
	return metrics.TurnCount > 0 && metrics.ToolCalls >= 0 && metrics.ComputeVersion > 0
}

func (combination canonicalPersistenceCombination) expectedLosingMarkers() int {
	switch combination {
	case canonicalPersistenceAllThree:
		return 2
	case canonicalPersistenceCurrentLegacy, canonicalPersistenceCurrentJSON, canonicalPersistenceLegacyJSON:
		return 1
	case canonicalPersistenceCurrentOnly, canonicalPersistenceLegacyOnly, canonicalPersistenceJSONOnly:
		return 0
	default:
		return -1
	}
}

func (combination canonicalPersistenceCombination) includesJSON() bool {
	switch combination {
	case canonicalPersistenceAllThree, canonicalPersistenceCurrentJSON, canonicalPersistenceLegacyJSON, canonicalPersistenceJSONOnly:
		return true
	default:
		return false
	}
}

func TestCanonicalOpenCodeRealStoreDetailAndAnalytics(t *testing.T) {
	fixture, err := loadCanonicalPersistenceFixture(canonicalPersistenceYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, testCase.SourceFixture)
			root := filepath.Dir(materialized.Path)
			writeCanonicalPersistenceJSON(t, root, testCase.JSONSessions)
			commandRoot := t.TempDir()
			outputRoot := filepath.Join(commandRoot, "managed")
			output, err := executeHarvestCmd(t, commandRoot, []string{"--source-provider=" + defaults.HarnessOpenCode.String(), "--source-path=" + root, "--output=" + outputRoot, "--force", "--include-active"})
			if err != nil {
				t.Fatalf("mounted canonical harvest: %v\n%s", err, output)
			}
			database, err := store.Open(defaults.ResolveDBFilePathWith(commandRoot).String(), store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			sessions, err := database.ListSessionsFiltered(t.Context(), store.SessionListFilter{})
			if err != nil || len(sessions) != testCase.ExpectedSessions {
				t.Fatalf("real canonical store sessions=%d error=%v, want %d", len(sessions), err, testCase.ExpectedSessions)
			}
			assertCanonicalPersistenceSessions(t, database, testCase.CanonicalSessions)
			graphID := mustCanonicalPersistenceSessionID(t, testCase.GraphSession)
			entries, err := database.ListEntries(t.Context(), graphID)
			if err != nil {
				t.Fatal(err)
			}
			parentIndex, childIndex, missingIndex := assertPersistedCanonicalGraph(t, entries, testCase)
			turns := api.EntriesToTurns(entries)
			assertCanonicalDetailGraph(t, turns, testCase, parentIndex, childIndex, missingIndex)
			detail := api.SessionToDetail(&ingest.Session{ID: graphID, Harness: ingest.HarnessOpenCode, Turns: turns, Model: "synthetic-model"})
			detailJSON, marshalErr := json.Marshal(detail)
			if marshalErr != nil || detail == nil || !bytes.Contains(detailJSON, []byte(testCase.ToolResultContains)) {
				t.Fatalf("production session detail omitted paired tool output %q: error=%v detail=%s", testCase.ToolResultContains, marshalErr, detailJSON)
			}
			metrics, err := database.GetMetrics(t.Context(), graphID)
			if err != nil || metrics == nil || canonicalMetricInt(metrics, func(value *ingest.SessionMetrics) *int { return value.TurnCount }) != testCase.ExpectedMetrics.TurnCount || canonicalMetricInt(metrics, func(value *ingest.SessionMetrics) *int { return value.ToolCalls }) != testCase.ExpectedMetrics.ToolCalls || canonicalMetricInt(metrics, func(value *ingest.SessionMetrics) *int { return value.ComputeVersion }) != testCase.ExpectedMetrics.ComputeVersion {
				t.Fatalf("real canonical analytics are incomplete: turns=%d tools=%d compute=%d error=%v", canonicalMetricInt(metrics, func(value *ingest.SessionMetrics) *int { return value.TurnCount }), canonicalMetricInt(metrics, func(value *ingest.SessionMetrics) *int { return value.ToolCalls }), canonicalMetricInt(metrics, func(value *ingest.SessionMetrics) *int { return value.ComputeVersion }), err)
			}
			orphanID := mustCanonicalPersistenceSessionID(t, testCase.OrphanSession)
			orphanEntries, err := database.ListEntries(t.Context(), orphanID)
			if err != nil {
				t.Fatal(err)
			}
			assertPersistedCanonicalOrphan(t, orphanEntries, testCase)
		})
	}
}

func assertCanonicalPersistenceSessions(t testing.TB, database *store.Store, sessions []canonicalPersistenceSession) {
	t.Helper()
	for _, expected := range sessions {
		entries, err := database.ListEntries(t.Context(), mustCanonicalPersistenceSessionID(t, expected.SessionID))
		if err != nil {
			t.Fatalf("list persisted canonical session %q: %v", expected.Name, err)
		}
		entryIDs := make([]string, len(entries))
		encoded, err := json.Marshal(entries)
		if err != nil {
			t.Fatalf("marshal persisted canonical session %q: %v", expected.Name, err)
		}
		for index, entry := range entries {
			if entry.EntryID == nil {
				t.Fatalf("persisted canonical session %q entry %d has no stable ID", expected.Name, index)
			}
			entryIDs[index] = *entry.EntryID
		}
		if !slices.Equal(entryIDs, expected.OrderedEntryIDs) || !bytes.Contains(encoded, []byte(expected.WinningMarker)) {
			t.Fatalf("persisted canonical session %q entries=%v payload=%s, want ordered IDs %v and marker %q", expected.Name, entryIDs, encoded, expected.OrderedEntryIDs, expected.WinningMarker)
		}
		for _, marker := range expected.LosingMarkers {
			if bytes.Contains(encoded, []byte(marker)) {
				t.Fatalf("persisted canonical session %q leaked losing marker %q: %s", expected.Name, marker, encoded)
			}
		}
		metrics, err := database.GetMetrics(t.Context(), mustCanonicalPersistenceSessionID(t, expected.SessionID))
		if err != nil || metrics == nil || canonicalMetricInt(metrics, func(value *ingest.SessionMetrics) *int { return value.TurnCount }) != expected.ExpectedMetrics.TurnCount || canonicalMetricInt(metrics, func(value *ingest.SessionMetrics) *int { return value.ToolCalls }) != expected.ExpectedMetrics.ToolCalls || canonicalMetricInt(metrics, func(value *ingest.SessionMetrics) *int { return value.ComputeVersion }) != expected.ExpectedMetrics.ComputeVersion {
			t.Fatalf("persisted canonical session %q metrics turns=%d tools=%d compute=%d error=%v, want %+v", expected.Name, canonicalMetricInt(metrics, func(value *ingest.SessionMetrics) *int { return value.TurnCount }), canonicalMetricInt(metrics, func(value *ingest.SessionMetrics) *int { return value.ToolCalls }), canonicalMetricInt(metrics, func(value *ingest.SessionMetrics) *int { return value.ComputeVersion }), err, expected.ExpectedMetrics)
		}
	}
}

func canonicalMetricInt(metrics *ingest.SessionMetrics, field func(*ingest.SessionMetrics) *int) int {
	if metrics == nil || field(metrics) == nil {
		return -1
	}
	return *field(metrics)
}

func assertPersistedCanonicalGraph(t testing.TB, entries []schema.SessionEntry, testCase canonicalPersistenceCase) (int, int, int) {
	t.Helper()
	indexes := make(map[string]int)
	toolPaired := false
	for index, entry := range entries {
		if entry.EntryID != nil {
			indexes[*entry.EntryID] = index
		}
		if entry.EntryID != nil && *entry.EntryID == testCase.ToolCallID && entry.ToolInput != nil && entry.ToolOutput != nil && strings.Contains(*entry.ToolOutput, testCase.ToolResultContains) {
			toolPaired = true
		}
	}
	parent, parentOK := indexes[testCase.ParentEntry]
	child, childOK := indexes[testCase.ChildEntry]
	missing, missingOK := indexes[testCase.MissingParentEntry]
	// Messages stay at Depth 0 so consumers keep Depth 0/1 as the message and
	// part discriminator. The depth-0 parent link lives in ParentEntryID, which
	// names the parent message by its native id, and ParentIndex stays nil at
	// Depth 0, matching the Claude and Cursor indexers.
	if !parentOK || !childOK || entries[parent].EntryID == nil ||
		entries[child].ParentEntryID == nil || *entries[child].ParentEntryID != *entries[parent].EntryID ||
		entries[child].ParentIndex != nil || entries[parent].ParentIndex != nil ||
		entries[child].Depth != 0 || entries[parent].Depth != 0 {
		t.Fatalf("persisted canonical parent graph is incorrect: parent=%d/%t child=%d/%t entries=%+v", parent, parentOK, child, childOK, entries)
	}
	if !missingOK || entries[missing].ParentEntryID != nil || entries[missing].ParentIndex != nil || entries[missing].Depth != 0 || !toolPaired {
		t.Fatalf("persisted canonical root/tool state is incorrect: missing=%d/%t tool_paired=%t entries=%+v", missing, missingOK, toolPaired, entries)
	}
	return entries[parent].EntryIndex, entries[child].EntryIndex, entries[missing].EntryIndex
}

func assertCanonicalDetailGraph(t testing.TB, turns []ingest.Turn, testCase canonicalPersistenceCase, parentIndex, childIndex, missingIndex int) {
	t.Helper()
	indexes := make(map[int]ingest.Turn, len(turns))
	toolPaired := false
	for _, turn := range turns {
		indexes[turn.Index] = turn
		for _, call := range turn.ToolCalls {
			if call.ID == testCase.ToolCallID && strings.Contains(call.Result, testCase.ToolResultContains) {
				toolPaired = true
			}
		}
	}
	// The depth-0 message graph lives on the entry ParentEntryID, not on the
	// wire turn ParentIndex, so a linked depth-0 child folds as its own
	// top-level turn with a nil ParentIndex, exactly like the parent and the
	// root. The wire keeps ParentIndex nil at Depth 0.
	child, childTopLevel := indexes[childIndex]
	parent, parentTopLevel := indexes[parentIndex]
	root, rootTopLevel := indexes[missingIndex]
	childTopLevel = childTopLevel && child.Depth == 0 && child.ParentIndex == nil
	parentTopLevel = parentTopLevel && parent.Depth == 0 && parent.ParentIndex == nil
	rootTopLevel = rootTopLevel && root.Depth == 0 && root.ParentIndex == nil
	if !childTopLevel || !parentTopLevel || !rootTopLevel || !toolPaired {
		t.Fatalf("production detail conversion lost the depth-0 turn contract or tool pairing: child=%t parent=%t root=%t tool=%t turns=%+v", childTopLevel, parentTopLevel, rootTopLevel, toolPaired, turns)
	}
}

func assertPersistedCanonicalOrphan(t testing.TB, entries []schema.SessionEntry, testCase canonicalPersistenceCase) {
	t.Helper()
	for _, entry := range entries {
		if entry.EntryID != nil && *entry.EntryID == testCase.OrphanEntry {
			if entry.ParentIndex != nil || entry.Depth != 0 || entry.ContentPreview == nil || !strings.Contains(*entry.ContentPreview, testCase.OrphanContentContains) {
				t.Fatalf("persisted orphan entry is not root-attached with content: %+v", entry)
			}
			return
		}
	}
	t.Fatalf("real canonical store dropped orphan entry %q", testCase.OrphanEntry)
}

func writeCanonicalPersistenceJSON(t testing.TB, root string, sessions []canonicalPersistenceJSON) {
	t.Helper()
	for index, session := range sessions {
		messageID := "msg_json_" + session.SessionID
		partID := "part_json_" + session.SessionID
		sessionDir := filepath.Join(root, "storage", "session", "synthetic")
		messageDir := filepath.Join(root, "storage", "message", session.SessionID)
		partDir := filepath.Join(root, "storage", "part", messageID)
		for _, directory := range []string{sessionDir, messageDir, partDir} {
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		sessionData := fmt.Sprintf(`{"id":%q,"version":"synthetic","directory":"/synthetic/selection","title":%q,"time":{"created":%d,"updated":%d}}`, session.SessionID, session.SessionID, 3000+index, 3010+index)
		messageData := fmt.Sprintf(`{"id":%q,"sessionID":%q,"role":"user","path":{"cwd":"/synthetic/selection"},"time":{"created":%d},"content":%q}`, messageID, session.SessionID, 3000+index, session.Marker)
		partData := fmt.Sprintf(`{"id":%q,"messageID":%q,"type":"text","text":%q,"time":{"created":%d}}`, partID, messageID, session.Marker, 3001+index)
		if err := os.WriteFile(filepath.Join(sessionDir, session.SessionID+".json"), []byte(sessionData), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(messageDir, messageID+".json"), []byte(messageData), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(partDir, partID+".json"), []byte(partData), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func mustCanonicalPersistenceSessionID(t testing.TB, raw string) ingest.SessionID {
	t.Helper()
	id, err := ingest.NewSessionID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCanonicalOpenCodePersistenceFixtureRejectsMutations(t *testing.T) {
	fixture, err := loadCanonicalPersistenceFixture(canonicalPersistenceYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range fixture.LoaderMutations {
		mutated := append([]byte(nil), canonicalPersistenceYAML...)
		switch mutation.Kind {
		case persistenceMutationUnknownField:
			mutated = bytes.Replace(mutated, []byte("source_fixture:"), []byte("unexpected:"), 1)
		case persistenceMutationWrongCount:
			mutated = bytes.Replace(mutated, []byte("\n  - mixed-canonical-sessions-persist-through-detail-and-analytics\n"), []byte("\n  - mixed-canonical-sessions-renamed-away\n"), 1)
		case persistenceMutationDuplicateName:
			mutated = bytes.Replace(mutated, []byte("name: trailing-document"), []byte("name: unknown-field"), 1)
		case persistenceMutationTrailingDocument:
			mutated = append(mutated, []byte("\n---\nextra: true\n")...)
		case persistenceMutationWrongSessionCount:
			mutated = bytes.Replace(mutated, []byte("expected_sessions: 9"), []byte("expected_sessions: 8"), 1)
		case persistenceMutationDuplicateSessionID:
			mutated = bytes.Replace(mutated, []byte("ses_3cd91f52effeXd3QAJ54jOyzv6"), []byte("ses_3cd91f52effeXd3QAJ54jOyzv5"), 1)
		case persistenceMutationUnknownCombination:
			mutated = bytes.Replace(mutated, []byte("combination: current_legacy_json"), []byte("combination: event_history"), 1)
		case persistenceMutationOrphanJSONReference:
			mutated = bytes.Replace(mutated, []byte("session_id: ses_3cd91f52effeXd3QAJ54jOyzvB, marker: JSON_ONLY"), []byte("session_id: ses_unknown_json, marker: JSON_ONLY"), 1)
		case persistenceMutationMissingWinningMarker:
			mutated = bytes.Replace(mutated, []byte("winning_marker: CURRENT_ALL"), []byte("winning_marker: \"\""), 1)
		case persistenceMutationMissingOrderedEntry:
			mutated = bytes.Replace(mutated, []byte("ordered_entry_ids: [msg_current_all]"), []byte("ordered_entry_ids: []"), 1)
		case persistenceMutationInvalidMetrics:
			mutated = bytes.Replace(mutated, []byte("expected_metrics: {turn_count: 1, tool_calls: 0, compute_version: 7}"), []byte("expected_metrics: {turn_count: 0, tool_calls: 0, compute_version: 6}"), 1)
		}
		if _, err := loadCanonicalPersistenceFixture(mutated); err == nil || strings.TrimSpace(mutation.Name) == "" {
			t.Errorf("canonical persistence loader mutation %q was accepted", mutation.Name)
		}
	}
}
