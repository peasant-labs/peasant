package api_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/thinking_level_websocket.yaml
var thinkingLevelWebSocketYAML []byte

type wsThinkingChange struct {
	Level schema.ThinkingLevel    `yaml:"level"`
	Raw   schema.ThinkingLevelRaw `yaml:"raw"`
}

func TestHubSessionDetailThinkingLevel(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name            string                  `yaml:"name"`
			StoredLevel     schema.ThinkingLevel    `yaml:"storedLevel"`
			StoredRaw       schema.ThinkingLevelRaw `yaml:"storedRaw"`
			StoredHistory   []wsThinkingChange      `yaml:"storedHistory"`
			ExpectedLevel   schema.ThinkingLevel    `yaml:"expectedLevel"`
			ExpectedRaw     schema.ThinkingLevelRaw `yaml:"expectedRaw"`
			ExpectedHistory []wsThinkingChange      `yaml:"expectedHistory"`
			Turns           []struct {
				Role  schema.Role             `yaml:"role"`
				Level schema.ThinkingLevel    `yaml:"level"`
				Raw   schema.ThinkingLevelRaw `yaml:"raw"`
			} `yaml:"turns"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(thinkingLevelWebSocketYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("thinking level websocket fixture declares no requiredNames inventory; name every case the fixture must retain")
	}
	names := make([]string, len(fixture.Cases))
	for i, tc := range fixture.Cases {
		names[i] = tc.Name
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: fixture.RequiredNames}, names, "thinking level websocket"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			db := openTestStore(t)
			sid := testutil.TestSessionUUID
			stored := makeStoreEntry(t, sid, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "fixture-host", defaults.HarnessClaudeCode, 1700000000000, 1, 1, "fixture-project", len(tc.Turns), 0, 1000)
			stored.Metadata.ThinkingLevel, stored.Metadata.ThinkingLevelRaw = tc.StoredLevel, tc.StoredRaw
			for _, change := range tc.StoredHistory {
				stored.Metadata.ThinkingLevelHistory = append(stored.Metadata.ThinkingLevelHistory, schema.ThinkingLevelChange{Level: change.Level, Raw: change.Raw})
			}
			if err := db.InsertSessions(ctx, []ingest.StoreEntry{stored}); err != nil {
				t.Fatal(err)
			}
			var entries []schema.SessionEntry
			for i, turn := range tc.Turns {
				extra := map[string]string{}
				if turn.Level != "" {
					extra["thinking_level"] = string(turn.Level)
				}
				if turn.Raw != "" {
					extra["thinking_level_raw"] = string(turn.Raw)
				}
				data, err := json.Marshal(extra)
				if err != nil {
					t.Fatal(err)
				}
				value, content := string(data), "fixture message"
				entries = append(entries, schema.SessionEntry{SessionID: schema.SessionID(sid), EntryIndex: i + 1, Role: turn.Role, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, ContentPreview: &content, Extra: &value})
			}
			if err := testutil.WriteFullEntries(ctx, db, schema.SessionID(sid), entries); err != nil {
				t.Fatal(err)
			}
			hub := api.NewHub(api.NewStoreDataProvider(db, sessionvisibility.All()))
			go hub.Run(ctx)
			server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
			defer server.Close()
			conn, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			if _, _, err = conn.Read(ctx); err != nil {
				t.Fatal(err)
			}
			subscription, err := json.Marshal(api.ClientMessage{Type: api.MsgSubscribe, Channels: []api.ChannelSubscription{{Topic: api.TopicSessionDetail, ID: sid}}})
			if err != nil {
				t.Fatal(err)
			}
			if err = conn.Write(ctx, websocket.MessageText, subscription); err != nil {
				t.Fatal(err)
			}
			_, data, err := conn.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var message struct {
				Type api.MessageType             `json:"type"`
				Data schema.SessionDetailPayload `json:"data"`
			}
			if err = json.Unmarshal(data, &message); err != nil {
				t.Fatal(err)
			}
			if message.Type != api.MsgSessionDetail || message.Data.ThinkingLevel != tc.ExpectedLevel || message.Data.ThinkingLevelRaw != tc.ExpectedRaw {
				t.Fatalf("unexpected detail: %s", data)
			}
			if len(message.Data.ThinkingLevelHistory) != len(tc.ExpectedHistory) {
				t.Fatalf("history mismatch: %s", data)
			}
			for i, want := range tc.ExpectedHistory {
				got := message.Data.ThinkingLevelHistory[i]
				if got.Level != want.Level || got.Raw != want.Raw {
					t.Fatalf("history %d=(%q,%q), want (%q,%q)", i, got.Level, got.Raw, want.Level, want.Raw)
				}
			}
			if len(message.Data.Turns) != len(tc.Turns) {
				t.Fatalf("turn count mismatch: %s", data)
			}
			for i, want := range tc.Turns {
				got := message.Data.Turns[i]
				if got.ThinkingLevel != want.Level || got.ThinkingLevelRaw != want.Raw {
					t.Fatalf("turn %d evidence=(%q,%q), want (%q,%q)", i, got.ThinkingLevel, got.ThinkingLevelRaw, want.Level, want.Raw)
				}
			}
		})
	}
}
