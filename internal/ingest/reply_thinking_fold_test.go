package ingest_test

import (
	_ "embed"
	"fmt"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/reply_thinking_fold.yaml
var replyThinkingFoldYAML []byte

//go:embed testdata/reply_thinking_fold.manifest.yaml
var replyThinkingFoldManifestYAML []byte

// TestReplyThinkingFoldByHarness folds a reply carrier with a tool call and a
// thinking child for every harness and asserts which harnesses serve the
// thinking as a turn under its reply.
func TestReplyThinkingFoldByHarness(t *testing.T) {
	t.Parallel()
	var document struct {
		Cases []struct {
			Name    string         `yaml:"name"`
			Harness schema.Harness `yaml:"harness"`
			Turns   []string       `yaml:"turns"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(replyThinkingFoldYAML, &document); err != nil {
		t.Fatalf("decode reply thinking fold fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(replyThinkingFoldManifestYAML, "reply thinking fold")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "reply thinking fold"); err != nil {
		t.Fatal(err)
	}
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			harness := row.Harness
			parent := 0
			prose, thought, input, callID, name := "Reading the file.", "the file is short", `{"path":"a.go"}`, "call-1", "Read"
			entries := []schema.SessionEntry{
				{SessionID: testutil.TestSessionUUID, EntryIndex: 0, Harness: harness, Role: schema.RoleAssistant, EntryType: schema.EntryTypeText, HasToolUse: true, HasThinking: true, ContentPreview: &prose},
				{SessionID: testutil.TestSessionUUID, EntryIndex: 1, Harness: harness, Role: schema.RoleAssistant, EntryType: schema.EntryTypeThinking, HasThinking: true, Depth: 1, ParentIndex: &parent, ContentPreview: &thought},
				{SessionID: testutil.TestSessionUUID, EntryIndex: 2, Harness: harness, Role: schema.RoleAssistant, EntryType: schema.EntryTypeToolUse, HasToolUse: true, Depth: 1, ParentIndex: &parent, ToolCallID: &callID, ToolNamesCSV: &name, ToolInput: &input},
			}
			var got []string
			for _, turn := range ingest.EntriesToTurns(entries) {
				got = append(got, fmt.Sprintf("%d:%d:%s", turn.Index, turn.Depth, turn.EntryType))
			}
			if !reflect.DeepEqual(got, row.Turns) {
				t.Fatalf("folded turns = %v, want %v", got, row.Turns)
			}
		})
	}
}
