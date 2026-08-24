package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"gopkg.in/yaml.v3"
)

const expectedUnknownPartVocabularyCases = 1

type unknownTypeExpectation struct {
	Type  string `yaml:"type"`
	Count int    `yaml:"count"`
}

type unknownPartVocabularyCase struct {
	Name                 string                   `yaml:"name"`
	SourceFixture        string                   `yaml:"source_fixture"`
	SessionID            string                   `yaml:"session_id"`
	ExpectedKnownMarkers []string                 `yaml:"expected_known_markers"`
	ExpectedToolName     string                   `yaml:"expected_tool_name"`
	ExpectedToolCalls    int                      `yaml:"expected_tool_calls"`
	ExpectedInertNote    string                   `yaml:"expected_inert_note"`
	ForbiddenMarkers     []string                 `yaml:"forbidden_markers"`
	ExpectedUnknownTypes []unknownTypeExpectation `yaml:"expected_unknown_types"`
}

type unknownPartVocabularyDocument struct {
	DeclaredCases int                         `yaml:"declared_cases"`
	Cases         []unknownPartVocabularyCase `yaml:"cases"`
}

//go:embed testdata/opencode_unknown_part_vocabulary.yaml
var unknownPartVocabularyYAML []byte

func loadUnknownPartVocabularyDocument(data []byte) (unknownPartVocabularyDocument, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var document unknownPartVocabularyDocument
	if err := decoder.Decode(&document); err != nil {
		return document, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return document, errors.New("expected exactly one YAML document")
	}
	if document.DeclaredCases != expectedUnknownPartVocabularyCases || len(document.Cases) != expectedUnknownPartVocabularyCases {
		return document, errors.New("unknown-part vocabulary fixture count guard failed")
	}
	seen := make(map[string]struct{}, len(document.Cases))
	for _, testCase := range document.Cases {
		if testCase.Name == "" || testCase.SourceFixture == "" || testCase.SessionID == "" || len(testCase.ExpectedUnknownTypes) == 0 {
			return document, errors.New("unknown-part vocabulary fixture has an incomplete required row")
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			return document, errors.New("unknown-part vocabulary fixture has a duplicate case name")
		}
		seen[testCase.Name] = struct{}{}
	}
	return document, nil
}

func newUnknownVocabularyAdapter(t *testing.T) *ingest.OpenCodeAdapter {
	t.Helper()
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{}, "latest", fixedOpenCodeEnvironment{}, &ingest.OSFileSystem{}, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct candidate-capable adapter: %v", err)
	}
	return adapter
}

// TestOpenCodeLegacyUnknownPartVocabularyTolerated proves that a well-formed
// legacy part whose declared type is outside the known transcript set no longer
// fails the session. The known text, reasoning, and tool parts keep their exact
// rendering; a text-bearing unknown part becomes an inert system note; the
// text-free step-start and file parts are dropped; and one diagnostic per
// distinct unknown type names the type and its row count.
//
// Mutation proof: restoring the closed-set failure in
// parseManagedOpenCodeSemanticMessages (return an error when a part type is not
// known) makes MaterializeTranscript fail with "outside the supported closed
// set", so this case can no longer materialize and it goes red.
func TestOpenCodeLegacyUnknownPartVocabularyTolerated(t *testing.T) {
	document, err := loadUnknownPartVocabularyDocument(unknownPartVocabularyYAML)
	if err != nil {
		t.Fatalf("load unknown-part vocabulary expectations: %v", err)
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
		t.Fatalf("discover synthetic unknown-vocabulary source: %v", err)
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
	if session.TranscriptOrigin != ingest.TranscriptOriginOpenCodeLegacySQLite {
		t.Fatalf("session %q origin = %v, want legacy SQLite", testCase.SessionID, session.TranscriptOrigin)
	}

	metadata, data, err := adapter.MaterializeTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("materialize session with unknown part vocabulary: %v", err)
	}

	byType := make(map[string]int)
	for _, warning := range metadata.Diagnostics.Warnings {
		if warning.ErrorType != string(ingest.OpenCodeUnknownPartType) {
			continue
		}
		for _, expected := range testCase.ExpectedUnknownTypes {
			if strings.Contains(warning.Message, "\""+expected.Type+"\"") {
				byType[expected.Type]++
				if !strings.Contains(warning.Message, itoa(expected.Count)) {
					t.Errorf("diagnostic for %q does not name count %d: %q", expected.Type, expected.Count, warning.Message)
				}
			}
		}
	}
	for _, expected := range testCase.ExpectedUnknownTypes {
		if byType[expected.Type] != 1 {
			t.Errorf("expected exactly one diagnostic naming unknown type %q, got %d", expected.Type, byType[expected.Type])
		}
	}

	indexer := ingest.NewIndexerRegistry(&ingest.OSFileSystem{}, ingest.IndexerRegistryOptions{FullContent: true})[ingest.HarnessOpenCode]
	entries, err := indexer.IndexTranscriptBytes(context.Background(), session, data)
	if err != nil {
		t.Fatalf("index materialized unknown-vocabulary projection: %v", err)
	}
	turns := transcript.EntriesToTurns(entries)
	if len(turns) == 0 {
		t.Fatalf("session with tolerated unknown parts returned no turns")
	}

	var joined strings.Builder
	toolCalls := 0
	toolNameSeen := false
	for _, turn := range turns {
		joined.WriteString(turn.Content)
		joined.WriteString("\n")
		toolCalls += len(turn.ToolCalls)
		for _, call := range turn.ToolCalls {
			if call.Name == testCase.ExpectedToolName {
				toolNameSeen = true
			}
		}
	}
	rendered := joined.String()
	for _, marker := range testCase.ExpectedKnownMarkers {
		if !strings.Contains(rendered, marker) {
			t.Errorf("rendered turns omit known marker %q", marker)
		}
	}
	if !strings.Contains(rendered, testCase.ExpectedInertNote) {
		t.Errorf("rendered turns omit the inert note %q from the text-bearing unknown part", testCase.ExpectedInertNote)
	}
	for _, forbidden := range testCase.ForbiddenMarkers {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("rendered turns leak text-free unknown part marker %q that should have been dropped", forbidden)
		}
	}
	if !toolNameSeen {
		t.Errorf("rendered turns omit the known tool call %q", testCase.ExpectedToolName)
	}
	if toolCalls != testCase.ExpectedToolCalls {
		t.Errorf("tool call count = %d, want %d; an unknown part must not inflate the tool count", toolCalls, testCase.ExpectedToolCalls)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	negative := value < 0
	if negative {
		value = -value
	}
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	if negative {
		return "-" + digits
	}
	return digits
}
