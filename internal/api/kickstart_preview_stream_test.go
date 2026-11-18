package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/kickstart_preview_stream.yaml
var streamFixture []byte

func TestKickstartPreviewStream(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name       string `yaml:"name"`
			Transcript string `yaml:"transcript"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(streamFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(tc.Transcript), 0600); err != nil {
				t.Fatal(err)
			}
			const sourceID = "11111111-2222-4333-8444-555555555555"
			st := newKickstartSessions(kickstartEnv{})
			st.byID["setup"] = &kickstartSession{
				forest:   onboarding.Forest{Nodes: []onboarding.Node{{ID: "row", Kind: onboarding.NodeKindSession, SessionID: sourceID}}},
				listings: []onboarding.SessionListing{{SessionID: sourceID, Harness: "claude-code", Source: onboarding.SessionSource{Path: path, Origin: onboarding.SessionSourceOriginFile}}},
			}
			hub := NewHub(&mockDataProvider{})
			hub.attachKickstart(st)
			go hub.Run(ctx)
			server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
			defer server.Close()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			_, _, err = conn.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			write := func(value any) {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
					t.Fatal(err)
				}
			}
			write(ClientMessage{Type: MsgSubscribe, Channels: []ChannelSubscription{{Topic: TopicKickstart}}})
			write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewOpen, RequestID: "request", SessionID: "setup", NodeID: "row"}})
			var state schema.LocalKickstartPreviewState
			for range 2 {
				_, data, err := conn.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var envelope struct {
					Type schema.MessageType `json:"type"`
					Data json.RawMessage    `json:"data"`
				}
				if err := json.Unmarshal(data, &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Type != schema.MsgKickstartPreview {
					t.Fatalf("unexpected message: %s", data)
				}
				var event schema.LocalKickstartPreviewEvent
				if err := json.Unmarshal(envelope.Data, &event); err != nil {
					t.Fatal(err)
				}
				state, err = schema.MergeLocalKickstartPreview(state, event)
				if err != nil {
					t.Fatalf("invalid frame %s: %v", data, err)
				}
			}
			if len(state.Segments) != 1 || !state.Directory.Complete {
				t.Fatalf("expected one completed segment, got %+v", state.Directory)
			}
		})
	}
}
