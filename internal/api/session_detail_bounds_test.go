package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestHub_SessionDetail_BoundsAnOversizedToolResult mounts the real session_detail
// channel over a real store holding a tool result far larger than the wire
// contract lets a served document be. Before the read-path bound, the channel
// answered such a subscription with a topic error and the session could not be
// viewed at all. The stored entry keeps every byte; only the served projection is
// shortened, and it says so.
func TestHub_SessionDetail_BoundsAnOversizedToolResult(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	ctx := context.Background()
	sessionID := "77777777-7777-7777-7777-777777777777"

	entry := makeStoreEntry(t, sessionID, hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-bounded", 5, 2, 60000)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	sid := ingest.SessionID(sessionID)
	toolID := "toolu_oversized_1"
	toolInput := `{"command":"cat huge.log"}`
	// 9 MiB: over the contract's 8 MiB served-document policy, and well under the
	// per-record cap ingest accepts, so this is a record the store really holds.
	toolOutput := strings.Repeat("x", 9<<20)
	preview := "I will read the whole log."

	entries := []schema.SessionEntry{
		{
			SessionID:      sid,
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(day1Ms),
			ContentPreview: &preview,
			HasToolUse:     true,
			Depth:          0,
		},
		{
			SessionID:    sid,
			EntryIndex:   1,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(day1Ms + 100),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("Bash"),
			ToolCallID:   &toolID,
			ToolInput:    &toolInput,
			ToolOutput:   &toolOutput,
			Depth:        1,
			ParentIndex:  intPtr(0),
		},
	}
	if err := testutil.WriteFullEntries(ctx, s, sid, entries); err != nil {
		t.Fatalf("WriteFullEntries: %v", err)
	}

	storeProv := api.NewStoreDataProvider(s, sessionvisibility.All())
	cfg := &config.Config{Sources: config.SourcesConfig{Mock: config.MockConfig{Enabled: false}}}
	prov := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, nil, storeProv)

	hub := api.NewHub(prov)
	hubCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(hubCtx)

	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	conn, _, err := websocket.Dial(hubCtx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(int64(defaults.SessionDetailDocumentCapBytes))

	if _, _, err = conn.Read(hubCtx); err != nil {
		t.Fatalf("websocket.Read (connected): %v", err)
	}

	subMsg := api.ClientMessage{
		Type:     api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{Topic: api.TopicSessionDetail, ID: sessionID}},
	}
	if err := conn.Write(hubCtx, websocket.MessageText, mustMarshalJSON(subMsg)); err != nil {
		t.Fatalf("websocket.Write (subscribe): %v", err)
	}

	_, data, err := conn.Read(hubCtx)
	if err != nil {
		t.Fatalf("websocket.Read (session_detail): %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if msg["type"] != string(api.MsgSessionDetail) {
		t.Fatalf("message type = %q, want %q (the oversized record must not turn the subscription into an error)", msg["type"], api.MsgSessionDetail)
	}
	if len(data) > defaults.SessionDetailDocumentCapBytes {
		t.Errorf("served frame is %d bytes, over the contract cap %d", len(data), defaults.SessionDetailDocumentCapBytes)
	}

	dataMap, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatalf("msg.data = %T, want map", msg["data"])
	}
	detailRaw, err := json.Marshal(dataMap)
	if err != nil {
		t.Fatalf("re-marshal served detail: %v", err)
	}
	if _, err := schema.DecodeSessionDetailPayloadRaw(detailRaw); err != nil {
		t.Fatalf("served detail does not decode through the contract: %v", err)
	}

	turnsRaw, ok := dataMap["turns"].([]any)
	if !ok || len(turnsRaw) != 1 {
		t.Fatalf("turns = %v, want 1 turn", dataMap["turns"])
	}
	toolCallsRaw, ok := turnsRaw[0].(map[string]any)["toolCalls"].([]any)
	if !ok || len(toolCallsRaw) != 1 {
		t.Fatalf("toolCalls = %v, want 1 entry", turnsRaw[0])
	}
	result, ok := toolCallsRaw[0].(map[string]any)["result"].(string)
	if !ok {
		t.Fatalf("toolCalls[0].result = %v, want a string", toolCallsRaw[0])
	}
	note := strings.Index(result, "\n[")
	if note < 0 {
		t.Fatalf("served tool result carries no bound note")
	}
	if note != defaults.ServedTextFieldBudgetBytes {
		t.Errorf("served tool result shows %d bytes, want the per-field budget %d", note, defaults.ServedTextFieldBudgetBytes)
	}
	if !strings.HasPrefix(toolOutput, result[:note]) {
		t.Errorf("served tool result is not the leading bytes of the stored record")
	}
	for _, want := range []string{"tool result bounded for display", "showing 1 MiB of 9 MiB", "kept by the peasant store that recorded this session"} {
		if !strings.Contains(result[note:], want) {
			t.Errorf("bound note %q does not contain %q", result[note:], want)
		}
	}

	// The store still holds every byte: the bound is a read-path projection.
	page, err := s.ReadSessionEntries(ctx, sid, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent, Limit: 10, SoftMaxBytes: int64(32 << 20)})
	if err != nil {
		t.Fatalf("ReadSessionEntries: %v", err)
	}
	stored := ""
	for _, storedEntry := range page.Entries {
		if storedEntry.ToolOutput != nil && len(*storedEntry.ToolOutput) > len(stored) {
			stored = *storedEntry.ToolOutput
		}
	}
	if len(stored) != len(toolOutput) {
		t.Errorf("stored tool output is %d bytes, want the whole %d-byte record", len(stored), len(toolOutput))
	}
}
