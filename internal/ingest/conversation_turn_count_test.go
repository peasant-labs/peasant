package ingest_test

import (
	_ "embed"
	"fmt"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/conversation_turn_count.yaml
var conversationTurnCountYAML []byte

//go:embed testdata/conversation_turn_count.manifest.yaml
var conversationTurnCountManifestYAML []byte

type conversationTurnCountCase struct {
	Name    string `yaml:"name"`
	Entries []struct {
		Index       int              `yaml:"index"`
		Role        schema.Role      `yaml:"role"`
		EntryType   schema.EntryType `yaml:"entryType"`
		Depth       int              `yaml:"depth"`
		ParentIndex *int             `yaml:"parentIndex"`
		ToolCallID  *string          `yaml:"toolCallId"`
		ToolName    *string          `yaml:"toolName"`
		HasToolUse  bool             `yaml:"hasToolUse"`
		PartType    *string          `yaml:"partType"`
		Content     *string          `yaml:"content"`
	} `yaml:"entries"`
	Expected       int  `yaml:"expected"`
	ExpectedServed *int `yaml:"expectedServed"`
}

func loadConversationTurnCountCases(t *testing.T) []conversationTurnCountCase {
	t.Helper()
	var fixture struct {
		Cases []conversationTurnCountCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(conversationTurnCountYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeSemanticManifest(conversationTurnCountManifestYAML, "conversation turn count")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for i, testCase := range fixture.Cases {
		names[i] = testCase.Name
	}
	if err := testutil.ValidateSemanticNames(manifest, names, "conversation turn count"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// TestConversationTurnCount pins the one ingest definition every count surface
// reports: top-level folded turns on the contract's allowed list of role and
// entry type. The fixture also proves system, tool and thinking rows stay
// served while leaving the count.
func TestConversationTurnCount(t *testing.T) {
	t.Parallel()
	for _, testCase := range loadConversationTurnCountCases(t) {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			entries := make([]schema.SessionEntry, len(testCase.Entries))
			for i, source := range testCase.Entries {
				entries[i] = schema.SessionEntry{
					SessionID:      testutil.TestSessionUUID,
					Harness:        schema.HarnessClaudeCode,
					EntryIndex:     source.Index,
					Role:           source.Role,
					EntryType:      source.EntryType,
					Depth:          source.Depth,
					ParentIndex:    source.ParentIndex,
					ToolCallID:     source.ToolCallID,
					ToolNamesCSV:   source.ToolName,
					HasToolUse:     source.HasToolUse,
					PartType:       source.PartType,
					ContentPreview: source.Content,
				}
			}
			turns := ingest.EntriesToTurns(entries)
			if got := ingest.ConversationTurnCount(turns); got != testCase.Expected {
				t.Fatalf("conversation turn count = %d, want %d (turns: %s)", got, testCase.Expected, describeTurns(turns))
			}
			if testCase.ExpectedServed != nil && len(turns) != *testCase.ExpectedServed {
				t.Fatalf("served turns = %d, want %d (turns: %s)", len(turns), *testCase.ExpectedServed, describeTurns(turns))
			}
		})
	}
}

func describeTurns(turns []ingest.Turn) string {
	out := make([]string, len(turns))
	for i, turn := range turns {
		out[i] = fmt.Sprintf("%d:%s", turn.Index, turn.Role)
	}
	return fmt.Sprintf("%v", out)
}
