package metrics_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/classifier_turn_count.yaml
var classifierTurnCountYAML []byte

type classifierTurnCountCase struct {
	Name             string `yaml:"name"`
	ToolCalls        int    `yaml:"toolCalls"`
	MetricsTurnCount int    `yaml:"metricsTurnCount"`
	Entries          []struct {
		Index   int    `yaml:"index"`
		Role    string `yaml:"role"`
		Content string `yaml:"content"`
		// EntryType defaults to text when a row omits it.
		EntryType schema.EntryType `yaml:"entryType"`
	} `yaml:"entries"`
	// ConversationTurns is the conversation turn count the entries fold to,
	// stated as a literal for the rows that set it.
	ConversationTurns *int `yaml:"conversationTurns"`
	WantAbandoned     bool `yaml:"wantAbandoned"`
}

func loadClassifierTurnCountCases(t *testing.T) []classifierTurnCountCase {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(classifierTurnCountYAML))
	decoder.KnownFields(true)
	var fixture struct {
		ExpectedCaseCount int                       `yaml:"expectedCaseCount"`
		RequiredNames     []string                  `yaml:"requiredNames"`
		Cases             []classifierTurnCountCase `yaml:"cases"`
	}
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("classifier turn count fixture must contain exactly one document: %v", err)
	}
	if fixture.ExpectedCaseCount != len(fixture.RequiredNames) || fixture.ExpectedCaseCount != len(fixture.Cases) {
		t.Fatalf("classifier turn count fixture declares %d required names, %d cases, %d rows", len(fixture.RequiredNames), fixture.ExpectedCaseCount, len(fixture.Cases))
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		seen[testCase.Name] = true
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("classifier turn count fixture is missing required case %q", name)
		}
	}
	return fixture.Cases
}

// TestClassifierAbandonedUsesConversationTurns pins the restated heuristic:
// zero tool calls and at most one conversation turn. The retired metrics turn
// count is set to a divergent value in every case, so a classifier that reads
// it instead of folding the entries fails here.
func TestClassifierAbandonedUsesConversationTurns(t *testing.T) {
	t.Parallel()
	for _, testCase := range loadClassifierTurnCountCases(t) {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			sid := mustSessionID(t, testutil.TestSessionUUID)
			entries := make([]schema.SessionEntry, len(testCase.Entries))
			for i, source := range testCase.Entries {
				content := source.Content
				entryType := source.EntryType
				if entryType == "" {
					entryType = schema.EntryTypeText
				}
				entries[i] = schema.SessionEntry{
					SessionID:      sid,
					Harness:        schema.HarnessClaudeCode,
					EntryIndex:     source.Index,
					Role:           schema.Role(source.Role),
					EntryType:      entryType,
					ContentPreview: &content,
				}
			}
			if want := testCase.ConversationTurns; want != nil {
				if got := ingest.ConversationTurnCount(ingest.EntriesToTurns(entries)); got != *want {
					t.Fatalf("entries fold to %d conversation turns, want %d (entries: %s)", got, *want, describeEntryRoles(entries))
				}
			}
			turnCount := testCase.MetricsTurnCount
			metricsValue := &ingest.SessionMetrics{SessionID: sid}
			metricsValue.ToolCalls = &testCase.ToolCalls
			metricsValue.TurnCount = &turnCount
			results := metrics.NewClassifierEngine().Run(context.Background(), sid, entries, metricsValue)
			result := findResult(results, testutil.TestTypeIDSessionOutcome)
			abandoned := result != nil && result.Value == "abandoned"
			if abandoned != testCase.WantAbandoned {
				t.Fatalf("abandoned = %t, want %t (result: %+v, entries: %s)", abandoned, testCase.WantAbandoned, result, describeEntryRoles(entries))
			}
		})
	}
}

func describeEntryRoles(entries []schema.SessionEntry) string {
	roles := make([]string, len(entries))
	for i, entry := range entries {
		roles[i] = fmt.Sprintf("%d:%s", entry.EntryIndex, entry.Role)
	}
	return fmt.Sprintf("%v", roles)
}
