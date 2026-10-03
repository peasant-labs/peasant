package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// Credentials planted in a user turn and in a tool call's arguments. The first
// appears twice, so its one preview item must name the earlier turn.
const (
	entryIndexTurnSecret = "sk-ant-api03-ENTRYINDEXTURNKEY0000000000x"
	entryIndexToolSecret = "sk-ant-api03-ENTRYINDEXTOOLKEY0000000000x"
	entryIndexToolCallID = "toolu_entry_index_1"
)

// TestSyncRedactionsNameTheTurnAndToolCall checks that each item of the
// redaction preview names the turn that shows its match, in the index space
// the transcript viewer opens a turn by, and the tool call when the match lies
// in one. The expected turns are read from the session detail route the viewer
// reads, not from the preview.
func TestSyncRedactionsNameTheTurnAndToolCall(t *testing.T) {
	t.Parallel()
	world := newPublishingWorld(t)
	const sessionID = "44445555-6666-4777-8888-9999aaaabbbb"
	seedToolCallSession(t, world.db, sessionID)

	status, body := world.request(t, http.MethodGet, "/api/v1/sessions/"+sessionID, nil)
	if status != http.StatusOK {
		t.Fatalf("session detail = %d %s", status, body)
	}
	var detail schema.SessionDetailPayload
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	turnIndex, toolTurnIndex := -1, -1
	for _, turn := range detail.Turns {
		if turnIndex < 0 && strings.Contains(turn.Content, entryIndexTurnSecret) {
			turnIndex = turn.Index
		}
		for _, call := range turn.ToolCalls {
			if call.ID == entryIndexToolCallID && strings.Contains(call.Arguments, entryIndexToolSecret) {
				toolTurnIndex = turn.Index
			}
		}
	}
	if turnIndex < 0 || toolTurnIndex < 0 || turnIndex == toolTurnIndex {
		t.Fatalf("the viewer's detail does not show the planted turns apart: turn %d, tool call turn %d; turns %+v", turnIndex, toolTurnIndex, detail.Turns)
	}

	var preview schema.SyncRedactionsResponse
	world.decode(t, http.MethodGet, defaults.RouteSyncRedactions.String()+"?session_id="+sessionID, &preview)
	items := map[string]schema.SyncRedactionItem{}
	for _, category := range preview.Categories {
		for _, rule := range category.Rules {
			for _, item := range rule.Items {
				items[item.OriginalText] = item
				if item.EntryIndex != nil && !detailHasTurn(detail, *item.EntryIndex) {
					t.Errorf("item %q names turn %d, which the viewer does not show", item.OriginalText, *item.EntryIndex)
				}
			}
		}
	}
	inTurn, ok := items[entryIndexTurnSecret]
	if !ok || inTurn.EntryIndex == nil || *inTurn.EntryIndex != turnIndex || inTurn.ToolCallID != "" {
		t.Errorf("the item of the turn's credential = %+v; want entryIndex %d and no tool call", inTurn, turnIndex)
	}
	inTool, ok := items[entryIndexToolSecret]
	if !ok || inTool.EntryIndex == nil || *inTool.EntryIndex != toolTurnIndex || inTool.ToolCallID != entryIndexToolCallID {
		t.Errorf("the item of the tool call's credential = %+v; want entryIndex %d and tool call %s", inTool, toolTurnIndex, entryIndexToolCallID)
	}
}

func detailHasTurn(detail schema.SessionDetailPayload, index int) bool {
	for _, turn := range detail.Turns {
		if turn.Index == index {
			return true
		}
	}
	return false
}

// seedToolCallSession stores a session in the listed acme/tools project whose
// user turn carries one credential twice, around an assistant turn whose tool
// call carries another.
func seedToolCallSession(t *testing.T, db *store.Store, sessionID string) {
	t.Helper()
	const startMs int64 = 1700000000000
	ingested := startMs + 120000
	remote, branch := "https://github.com/acme/tools.git", "main"
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = schema.SessionID(sessionID)
	meta.HostSlug = schema.HostSlug("github.com-synthetic")
	meta.ModelHarness = defaults.HarnessClaudeCode
	meta.Model = testutil.TestModel
	meta.Timestamp = ingest.TimestampInfo{Start: startMs, End: startMs + 60000, Ingested: &ingested}
	meta.Source = ingest.SourceInfo{FilePath: "/synthetic/" + sessionID + ".jsonl", Format: ingest.SourceFormatJSONL}
	meta.Project = ingest.ProjectInfo{Hash: insideProject, Name: "synthetic"}
	meta.Stats = ingest.StatsInfo{TurnCount: 3, ToolCallCount: 1, DurationMs: 60000}
	meta.Git = ingest.GitContext{Remote: &remote, Branch: &branch}
	harness := schema.Harness(defaults.HarnessClaudeCode)
	ask := "use the key " + entryIndexTurnSecret + " to deploy"
	answer := "I will run the deploy."
	input := `{"command":"deploy --key ` + entryIndexToolSecret + `"}`
	output := "deployed"
	toolName := "Bash"
	callID := entryIndexToolCallID
	again := "the key was " + entryIndexTurnSecret
	at := func(offset int64) *int64 { v := startMs + offset; return &v }
	parent := 1
	testutil.SeedReadyPublication(t, db, &meta, []schema.SessionEntry{
		{SessionID: meta.SessionID, EntryIndex: 0, Harness: harness, Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &ask, TimestampMs: at(0)},
		{SessionID: meta.SessionID, EntryIndex: 1, Harness: harness, Role: schema.RoleAssistant, EntryType: schema.EntryTypeText, ContentPreview: &answer, HasToolUse: true, TimestampMs: at(100)},
		{SessionID: meta.SessionID, EntryIndex: 2, Harness: harness, Role: schema.RoleAssistant, EntryType: schema.EntryTypeToolUse, HasToolUse: true, ToolNamesCSV: &toolName, ToolCallID: &callID, ToolInput: &input, ToolOutput: &output, Depth: 1, ParentIndex: &parent, TimestampMs: at(200)},
		{SessionID: meta.SessionID, EntryIndex: 3, Harness: harness, Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &again, TimestampMs: at(300)},
	})
}
