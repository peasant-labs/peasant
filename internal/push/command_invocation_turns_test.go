package push_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestBuildTranscriptContentValidated_CarriesCommandInvocation exercises the
// builder production pushes through, so an attribution failure surfaces as the
// error the pipeline would return rather than as an empty payload.
func TestBuildTranscriptContentValidated_CarriesCommandInvocation(t *testing.T) {
	t.Parallel()
	fixture, err := testutil.LoadCommandInvocationTurnFixture()
	if err != nil {
		t.Fatal(err)
	}

	sessionID := schema.SessionID("45454545-4545-4545-4545-45454545454b")
	entries := make([]schema.SessionEntry, len(fixture.Cases))
	for index, testCase := range fixture.Cases {
		timestamp := int64(1705276800000) + int64(index)
		extra := testCase.StoredExtra
		entries[index] = schema.SessionEntry{
			SessionID:   sessionID,
			EntryIndex:  index,
			Harness:     testCase.Harness,
			EntryType:   schema.EntryTypeText,
			Role:        testCase.SourceRole,
			TimestampMs: &timestamp,
			Extra:       &extra,
		}
		if testCase.Content != "" {
			content := testCase.Content
			entries[index].ContentPreview = &content
		}
	}

	result, err := push.BuildTranscriptContentValidated(
		&ingest.UnifiedMetadata{
			SessionID:    sessionID,
			ModelHarness: defaults.HarnessClaudeCode,
		},
		entries,
		defaults.PublishSchemaVersion,
		config.DefaultPushFieldVisibility(),
		sessionorigin.User,
	)
	if err != nil {
		t.Fatalf("BuildTranscriptContentValidated: %v", err)
	}
	if result.SessionDetail == nil {
		t.Fatal("BuildTranscriptContentValidated returned no session detail payload")
	}
	if len(result.SessionDetail.Turns) != len(fixture.Cases) {
		t.Fatalf("push body has %d turns, want %d fixture rows", len(result.SessionDetail.Turns), len(fixture.Cases))
	}
	for index, testCase := range fixture.Cases {
		turn := result.SessionDetail.Turns[index]
		if turn.Index != index {
			t.Errorf("case %q push turn index = %d, want %d", testCase.Name, turn.Index, index)
		}
		if turn.Role != testCase.ExpectedRole {
			t.Errorf("case %q push role = %q, want %q", testCase.Name, turn.Role, testCase.ExpectedRole)
		}
		if !testCase.ExpectedCommand {
			if turn.Command != nil {
				t.Errorf("case %q push body carries command %+v, want none", testCase.Name, *turn.Command)
			}
			continue
		}
		if turn.Command == nil {
			t.Errorf("case %q push body carries no command, want %q", testCase.Name, testCase.ExpectedName)
			continue
		}
		if turn.Command.Name != testCase.ExpectedName || turn.Command.Args != testCase.ExpectedArgs {
			t.Errorf("case %q push command = %+v, want name %q args %q", testCase.Name, *turn.Command, testCase.ExpectedName, testCase.ExpectedArgs)
		}
	}
}
