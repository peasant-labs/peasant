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

const (
	expectedCanonicalPersistenceCases     = 1
	expectedCanonicalPersistenceMutations = 4
)

type canonicalPersistenceFixture struct {
	DeclaredCases           int                                  `yaml:"declared_cases"`
	Cases                   []canonicalPersistenceCase           `yaml:"cases"`
	DeclaredLoaderMutations int                                  `yaml:"declared_loader_mutations"`
	LoaderMutations         []canonicalPersistenceLoaderMutation `yaml:"loader_mutations"`
}

type canonicalPersistenceCase struct {
	Name                  string                     `yaml:"name"`
	SourceFixture         string                     `yaml:"source_fixture"`
	ExpectedSessions      int                        `yaml:"expected_sessions"`
	JSONSessions          []canonicalPersistenceJSON `yaml:"json_sessions"`
	GraphSession          string                     `yaml:"graph_session"`
	ParentEntry           string                     `yaml:"parent_entry"`
	ChildEntry            string                     `yaml:"child_entry"`
	MissingParentEntry    string                     `yaml:"missing_parent_entry"`
	ToolCallID            string                     `yaml:"tool_call_id"`
	ToolResultContains    string                     `yaml:"tool_result_contains"`
	OrphanSession         string                     `yaml:"orphan_session"`
	OrphanEntry           string                     `yaml:"orphan_entry"`
	OrphanContentContains string                     `yaml:"orphan_content_contains"`
	MinimumTurns          int                        `yaml:"minimum_turns"`
	MinimumToolCalls      int                        `yaml:"minimum_tool_calls"`
}

type canonicalPersistenceJSON struct {
	SessionID string `yaml:"session_id"`
	Marker    string `yaml:"marker"`
}

type canonicalPersistenceLoaderMutation struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"`
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
	if fixture.DeclaredCases != expectedCanonicalPersistenceCases || len(fixture.Cases) != expectedCanonicalPersistenceCases || fixture.DeclaredLoaderMutations != expectedCanonicalPersistenceMutations || len(fixture.LoaderMutations) != expectedCanonicalPersistenceMutations {
		return fixture, errors.New("canonical OpenCode persistence fixture count guard failed")
	}
	seen := make(map[string]bool)
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || seen[testCase.Name] || testCase.SourceFixture == "" || testCase.ExpectedSessions <= 0 || len(testCase.JSONSessions) == 0 || testCase.GraphSession == "" || testCase.ParentEntry == "" || testCase.ChildEntry == "" || testCase.MissingParentEntry == "" || testCase.ToolCallID == "" || testCase.ToolResultContains == "" || testCase.OrphanSession == "" || testCase.OrphanEntry == "" || testCase.OrphanContentContains == "" || testCase.MinimumTurns <= 0 || testCase.MinimumToolCalls <= 0 {
			return fixture, fmt.Errorf("canonical OpenCode persistence fixture contains incomplete or duplicate case %+v", testCase)
		}
		seen[testCase.Name] = true
		jsonIDs := make(map[string]bool, len(testCase.JSONSessions))
		for _, session := range testCase.JSONSessions {
			if session.SessionID == "" || session.Marker == "" || jsonIDs[session.SessionID] {
				return fixture, fmt.Errorf("canonical persistence case %q has incomplete or duplicate JSON session %+v", testCase.Name, session)
			}
			jsonIDs[session.SessionID] = true
		}
	}
	for _, mutation := range fixture.LoaderMutations {
		if mutation.Name == "" || seen[mutation.Name] {
			return fixture, fmt.Errorf("canonical persistence fixture has incomplete or duplicate loader mutation %q", mutation.Name)
		}
		seen[mutation.Name] = true
		switch mutation.Kind {
		case "unknown_field", "wrong_count", "duplicate_name", "trailing_document":
		default:
			return fixture, fmt.Errorf("canonical persistence fixture has unknown mutation %q", mutation.Kind)
		}
	}
	return fixture, nil
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
			if err != nil || metrics == nil || metrics.TurnCount == nil || *metrics.TurnCount < testCase.MinimumTurns || metrics.ToolCalls == nil || *metrics.ToolCalls < testCase.MinimumToolCalls || metrics.ComputeVersion == nil || *metrics.ComputeVersion <= 0 {
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
	if !parentOK || !childOK || entries[child].ParentIndex == nil || *entries[child].ParentIndex != entries[parent].EntryIndex || entries[child].Depth != entries[parent].Depth+1 {
		t.Fatalf("persisted canonical parent graph is incorrect: parent=%d/%t child=%d/%t entries=%+v", parent, parentOK, child, childOK, entries)
	}
	if !missingOK || entries[missing].ParentIndex != nil || entries[missing].Depth != 0 || !toolPaired {
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
	linked := false
	rooted := false
	for _, turn := range turns {
		if turn.Index == childIndex && turn.ParentIndex != nil && *turn.ParentIndex == parentIndex {
			if parent, ok := indexes[parentIndex]; ok && turn.Depth == parent.Depth+1 {
				linked = true
			}
		}
		if turn.Index == missingIndex && turn.ParentIndex == nil && turn.Depth == 0 {
			rooted = true
		}
	}
	if !linked || !rooted || !toolPaired {
		t.Fatalf("production detail conversion lost graph or tool pairing: linked=%t rooted=%t tool=%t turns=%+v", linked, rooted, toolPaired, turns)
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
		case "unknown_field":
			mutated = bytes.Replace(mutated, []byte("source_fixture:"), []byte("unexpected:"), 1)
		case "wrong_count":
			mutated = bytes.Replace(mutated, []byte("declared_cases: 1"), []byte("declared_cases: 2"), 1)
		case "duplicate_name":
			mutated = bytes.Replace(mutated, []byte("name: trailing-document"), []byte("name: unknown-field"), 1)
		case "trailing_document":
			mutated = append(mutated, []byte("\n---\nextra: true\n")...)
		}
		if _, err := loadCanonicalPersistenceFixture(mutated); err == nil || strings.TrimSpace(mutation.Name) == "" {
			t.Errorf("canonical persistence loader mutation %q was accepted", mutation.Name)
		}
	}
}
