package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
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
	ExpectedIgnoredControlTypes []string                    `yaml:"expected_ignored_control_types"`
	ExpectedContentOmitted      bool                        `yaml:"expected_content_omitted"`
	ExpectedSkippedControlTypes []skippedControlExpectation `yaml:"expected_skipped_control_types"`
}

type currentControlRowsDocument struct {
	RequiredCases []string                 `yaml:"required_cases"`
	Cases         []currentControlRowsCase `yaml:"cases"`
}

//go:embed testdata/opencode_current_control_rows.yaml
var currentControlRowsYAML []byte

func loadCurrentControlRowsDocument(data []byte) (currentControlRowsDocument, error) {
	var document currentControlRowsDocument
	if err := testutil.DecodeFixtureYAML(data, &document); err != nil {
		return document, err
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
		if testCase.Name == "" || testCase.SourceFixture == "" || testCase.SessionID == "" {
			return document, errors.New("current control rows fixture has an incomplete required row")
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			return document, errors.New("current control rows fixture has a duplicate case name")
		}
		seen[testCase.Name] = struct{}{}
	}
	return document, nil
}

// TestOpenCodeCurrentControlRowsTolerated proves that every control row this
// build interprets is accounted for without failing the session: a model switch
// lands as a model observation on the following assistant turn, an agent switch
// renders as inert context, a clean or interrupted idle marker is skipped with
// no diagnostic, a failed idle marker keeps its failure text, an id-less
// unknown control row is skipped and named once per type, and the user,
// assistant and tool rows still materialize.
//
// Mutation proof: removing an arm from the current-row dispatch sends that kind
// down the unknown-control path, so its case goes red by name. Dropping the
// failed-idle arm into the skip path removes the failure text, and the case that
// requires it goes red.
func TestOpenCodeCurrentControlRowsTolerated(t *testing.T) {
	document, err := loadCurrentControlRowsDocument(currentControlRowsYAML)
	if err != nil {
		t.Fatalf("load current control rows expectations: %v", err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			runCurrentControlRowsCase(t, testCase)
		})
	}
}

func runCurrentControlRowsCase(t *testing.T, testCase currentControlRowsCase) {
	t.Helper()
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

	captured, err := adapter.MaterializeTranscript(context.Background(), session)
	metadata, data := captured.Metadata, captured.Data
	if err != nil {
		t.Fatalf("materialize current session with control rows: %v", err)
	}

	var projection struct {
		ContentOmitted bool `json:"content_omitted"`
	}
	if err := json.Unmarshal(data, &projection); err != nil {
		t.Fatalf("decode materialized projection: %v", err)
	}
	if projection.ContentOmitted != testCase.ExpectedContentOmitted {
		t.Errorf("projection content_omitted = %v, want %v", projection.ContentOmitted, testCase.ExpectedContentOmitted)
	}

	byType := make(map[string]int)
	for _, warning := range metadata.Diagnostics.Warnings {
		if warning.ErrorType != string(ingest.ContentCaptureUnknownDataRetained) {
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
	ignoredNamed := make(map[string]int)
	for _, warning := range metadata.Diagnostics.Warnings {
		for _, ignored := range testCase.ExpectedIgnoredControlTypes {
			if strings.Contains(warning.Message, "\""+ignored+"\"") {
				ignoredNamed[ignored]++
			}
		}
	}
	for _, ignored := range testCase.ExpectedIgnoredControlTypes {
		if ignoredNamed[ignored] != 0 {
			t.Errorf("interpreted control type %q was reported as uninterpreted retained evidence", ignored)
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
	if testCase.ExpectedToolName != "" && !toolSeen {
		t.Errorf("rendered turns omit the assistant tool call %q", testCase.ExpectedToolName)
	}
	if testCase.ExpectedObservedModel != "" && !observedOnAssistant {
		t.Errorf("model switch did not land as observation %q on the following assistant turn", testCase.ExpectedObservedModel)
	}
}

// TestOpenCodeCurrentIdleMarkerTolerated proves that an interpreted idle
// marker is skipped without being reported as uninterpreted retained evidence,
// while the surrounding conversation still materializes.
//
// Mutation proof: removing the "idle" arm from the current-row dispatch sends
// the marker down the unknown-control path, which reports it as retained
// evidence and makes this case go red by name.
func TestOpenCodeCurrentIdleMarkerTolerated(t *testing.T) {
	document, err := loadCurrentControlRowsDocument(currentControlRowsYAML)
	if err != nil {
		t.Fatalf("load current control rows expectations: %v", err)
	}
	for _, testCase := range document.Cases {
		if testCase.Name != "idle-marker-tolerated-without-omission" {
			continue
		}
		runCurrentControlRowsCase(t, testCase)
		return
	}
	t.Fatal("fixture has no idle-marker case")
}
