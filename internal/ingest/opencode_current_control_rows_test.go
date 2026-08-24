package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/transcript"
	"gopkg.in/yaml.v3"
)

type skippedControlExpectation struct {
	Type  string `yaml:"type"`
	Count int    `yaml:"count"`
}

type currentControlRowsCase struct {
	Name                        string                      `yaml:"name"`
	SourceFixture               string                      `yaml:"source_fixture"`
	SessionID                   string                      `yaml:"session_id"`
	ExpectedObservedModel       string                      `yaml:"expected_observed_model"`
	ExpectedToolName            string                      `yaml:"expected_tool_name"`
	ExpectedKnownMarkers        []string                    `yaml:"expected_known_markers"`
	ExpectedSkippedControlTypes []skippedControlExpectation `yaml:"expected_skipped_control_types"`
}

type currentControlRowsDocument struct {
	RequiredCases []string                 `yaml:"required_cases"`
	Cases         []currentControlRowsCase `yaml:"cases"`
}

//go:embed testdata/opencode_current_control_rows.yaml
var currentControlRowsYAML []byte

func loadCurrentControlRowsDocument(data []byte) (currentControlRowsDocument, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var document currentControlRowsDocument
	if err := decoder.Decode(&document); err != nil {
		return document, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return document, errors.New("expected exactly one YAML document")
	}
	present := make(map[string]struct{}, len(document.Cases))
	for _, testCase := range document.Cases {
		present[testCase.Name] = struct{}{}
	}
	if len(document.RequiredCases) == 0 {
		return document, errors.New("current control rows fixture declares no required cases")
	}
	for _, name := range document.RequiredCases {
		if _, ok := present[name]; !ok {
			return document, fmt.Errorf("current control rows fixture is missing required case %q", name)
		}
	}
	seen := make(map[string]struct{}, len(document.Cases))
	for _, testCase := range document.Cases {
		if testCase.Name == "" || testCase.SourceFixture == "" || testCase.SessionID == "" || testCase.ExpectedObservedModel == "" {
			return document, errors.New("current control rows fixture has an incomplete required row")
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			return document, errors.New("current control rows fixture has a duplicate case name")
		}
		seen[testCase.Name] = struct{}{}
	}
	return document, nil
}

// TestOpenCodeCurrentControlRowsTolerated proves that a current session whose
// session_message rows include control records no longer fails with "upstream
// message id is required". The user, assistant, and tool rows materialize; the
// model switch lands as a model observation on the following assistant turn,
// which had no model of its own; and an id-less unknown control row is skipped
// and named by one diagnostic per type.
//
// Mutation proof: restoring the upstream-id requirement for control rows
// (calling validateIdentity, or requiring the "id" field) makes
// MaterializeTranscript fail with "upstream message id is required", so this
// case can no longer materialize and it goes red.
func TestOpenCodeCurrentControlRowsTolerated(t *testing.T) {
	document, err := loadCurrentControlRowsDocument(currentControlRowsYAML)
	if err != nil {
		t.Fatalf("load current control rows expectations: %v", err)
	}
	testCase := document.Cases[0]
	materialized := testfixture.MaterializeByName(t, testCase.SourceFixture)
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic root: %v", err)
	}
	adapter := newUnknownVocabularyAdapter(t)
	discovered, err := adapter.Discover(context.Background(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover synthetic control-row source: %v", err)
	}
	var session ingest.DiscoveredSession
	found := false
	for _, candidate := range discovered {
		if string(candidate.SessionID) == testCase.SessionID {
			session = candidate
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("discovery omitted session %q; discovered %d sessions", testCase.SessionID, len(discovered))
	}
	if session.TranscriptOrigin != ingest.TranscriptOriginOpenCodeCurrentSQLite {
		t.Fatalf("session %q origin = %v, want current SQLite", testCase.SessionID, session.TranscriptOrigin)
	}

	metadata, data, err := adapter.MaterializeTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("materialize current session with control rows: %v", err)
	}

	byType := make(map[string]int)
	for _, warning := range metadata.Diagnostics.Warnings {
		if warning.ErrorType != string(ingest.OpenCodeUnknownPartType) {
			continue
		}
		for _, expected := range testCase.ExpectedSkippedControlTypes {
			if strings.Contains(warning.Message, "\""+expected.Type+"\"") {
				byType[expected.Type]++
				if !strings.Contains(warning.Message, itoa(expected.Count)) {
					t.Errorf("diagnostic for %q does not name count %d: %q", expected.Type, expected.Count, warning.Message)
				}
			}
		}
	}
	for _, expected := range testCase.ExpectedSkippedControlTypes {
		if byType[expected.Type] != 1 {
			t.Errorf("expected exactly one diagnostic naming skipped control type %q, got %d", expected.Type, byType[expected.Type])
		}
	}

	indexer := ingest.NewIndexerRegistry(&ingest.OSFileSystem{}, ingest.IndexerRegistryOptions{FullContent: true})[ingest.HarnessOpenCode]
	entries, err := indexer.IndexTranscriptBytes(context.Background(), session, data)
	if err != nil {
		t.Fatalf("index materialized control-row projection: %v", err)
	}
	turns := transcript.EntriesToTurns(entries)
	if len(turns) == 0 {
		t.Fatalf("current session with control rows returned no turns")
	}

	observedOnAssistant := false
	toolSeen := false
	var rendered strings.Builder
	for _, turn := range turns {
		rendered.WriteString(turn.Content)
		rendered.WriteString("\n")
		for _, call := range turn.ToolCalls {
			if call.Name == testCase.ExpectedToolName {
				toolSeen = true
			}
		}
		if turn.Role == ingest.RoleAssistant && turn.ObservedModel.String() == testCase.ExpectedObservedModel {
			observedOnAssistant = true
		}
	}
	for _, marker := range testCase.ExpectedKnownMarkers {
		if !strings.Contains(rendered.String(), marker) {
			t.Errorf("rendered turns omit known marker %q", marker)
		}
	}
	if !toolSeen {
		t.Errorf("rendered turns omit the assistant tool call %q", testCase.ExpectedToolName)
	}
	if !observedOnAssistant {
		t.Errorf("model switch did not land as observation %q on the following assistant turn", testCase.ExpectedObservedModel)
	}
}
