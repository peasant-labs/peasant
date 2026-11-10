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
	// 9 MiB exceeds the per-field display budget, but not the document cap or
	// the per-record cap ingest accepts; the store really holds the whole record.
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

	storeProv := api.NewStoreDataProvider(s, sessionvisibility.Browsing())
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
	if !ok || len(turnsRaw) != 0 {
		t.Fatalf("frame turns = %v, want none; an oversized tool result must not ride the session_detail frame", turnsRaw)
	}
	if strings.Contains(string(data), toolOutput[:64]) {
		t.Fatal("the session_detail frame still carries the oversized tool result")
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
