package push_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

func TestBuildTranscriptContent_UsesCanonicalHistoricalWrapperRoles(t *testing.T) {
	t.Parallel()
	fixture, err := testutil.LoadInjectedCommandTurnFixture()
	if err != nil {
		t.Fatal(err)
	}

	sessionID := schema.SessionID("45454545-4545-4545-4545-454545454547")
	entries := make([]schema.SessionEntry, len(fixture.Cases))
	for index, testCase := range fixture.Cases {
		content := testCase.StoredContent()
		entries[index] = schema.SessionEntry{
			SessionID:      sessionID,
			EntryIndex:     index,
			Harness:        testCase.Harness,
			EntryType:      schema.EntryTypeText,
			Role:           testCase.SourceRole,
			ContentPreview: &content,
		}
	}

	result := push.BuildTranscriptContent(
		&ingest.UnifiedMetadata{
			SessionID:    sessionID,
			ModelHarness: defaults.HarnessClaudeCode,
		},
		entries,
		defaults.PublishSchemaVersion,
		config.DefaultPushFieldVisibility(),
	)
	if result.SessionDetail == nil {
		t.Fatal("BuildTranscriptContent returned no session detail payload")
	}
	if len(result.SessionDetail.Turns) != len(fixture.Cases) {
		t.Fatalf("BuildTranscriptContent returned %d turns, want %d fixture rows", len(result.SessionDetail.Turns), len(fixture.Cases))
	}
	for index, testCase := range fixture.Cases {
		turn := result.SessionDetail.Turns[index]
		if turn.Index != index {
			t.Errorf("case %q projected turn index = %d, want %d", testCase.Name, turn.Index, index)
		}
		if turn.Role != testCase.ExpectedRole {
			t.Errorf("case %q projected push role = %q, want %q", testCase.Name, turn.Role, testCase.ExpectedRole)
		}
		if turn.Content != testCase.StoredContent() {
			t.Errorf("case %q projected push content changed", testCase.Name)
		}
	}
}
