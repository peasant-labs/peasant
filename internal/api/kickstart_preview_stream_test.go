package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/kickstart_preview_stream.yaml
var streamFixture []byte

func TestKickstartPreviewStream(t *testing.T) {
	var fixture struct {
		Generated struct {
			Pairs          int `yaml:"pairs"`
			AnswerBytes    int `yaml:"answerBytes"`
			OversizedBytes int `yaml:"oversizedBytes"`
		} `yaml:"generated"`
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name       string `yaml:"name"`
			Transcript string `yaml:"transcript"`
			Long       bool   `yaml:"long"`
			Operation  string `yaml:"operation"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(streamFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			var logs bytes.Buffer
			oldLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(oldLogger) })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "session.jsonl")
			content := tc.Transcript
			if tc.Operation == "bounded-turn" {
				content = fmt.Sprintf("{\"type\":\"user\",\"uuid\":\"u1\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"%s\"}}\n", strings.Repeat("x", fixture.Generated.OversizedBytes))
			}
			if tc.Long {
				var data strings.Builder
				for i := range fixture.Generated.Pairs {
					parent := ""
					if i > 0 {
						parent = fmt.Sprintf(`"parentUuid":"a%d",`, i-1)
					}
					fmt.Fprintf(&data, "{\"type\":\"user\",\"uuid\":\"u%d\",%s\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"question\"}}\n", i, parent)
					fmt.Fprintf(&data, "{\"type\":\"assistant\",\"uuid\":\"a%d\",\"parentUuid\":\"u%d\",\"timestamp\":\"2026-01-01T00:00:01Z\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"%s\"}]}}\n", i, i, strings.Repeat("x", fixture.Generated.AnswerBytes))
				}
				content = data.String()
			}
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.Operation == "missing-source" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			const sourceID = "11111111-2222-4333-8444-555555555555"
			expire := make(chan func(), 1)
			env := kickstartEnv{}
			if tc.Operation == "expire" {
				env.afterFunc = func(_ time.Duration, fn func()) func() bool {
					expire <- fn
					return func() bool { return true }
				}
			}
			st := newKickstartSessions(env)
			st.byID["setup"] = &kickstartSession{
				id:       "setup",
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
			conn.SetReadLimit(defaults.KickstartWebPreviewFrameCapBytes)
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
			var other *websocket.Conn
			if tc.Operation == "isolation" {
				other, _, err = websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer other.CloseNow()
				other.SetReadLimit(defaults.KickstartWebPreviewFrameCapBytes)
				if _, _, err := other.Read(ctx); err != nil {
					t.Fatal(err)
				}
				subscribe, err := json.Marshal(ClientMessage{Type: MsgSubscribe, Channels: []ChannelSubscription{{Topic: TopicKickstart}}})
				if err != nil {
					t.Fatal(err)
				}
				if err := other.Write(ctx, websocket.MessageText, subscribe); err != nil {
					t.Fatal(err)
				}
				// A refusal on the second socket is a barrier proving its subscription
				// was processed before the first socket opens a capture.
				barrier, err := json.Marshal(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewOpen, RequestID: "barrier", SessionID: "missing", NodeID: "row"}})
				if err != nil {
					t.Fatal(err)
				}
				if err := other.Write(ctx, websocket.MessageText, barrier); err != nil {
					t.Fatal(err)
				}
				if _, _, err := other.Read(ctx); err != nil {
					t.Fatal(err)
				}
			}
			write(ClientMessage{Type: MsgSubscribe, Channels: []ChannelSubscription{{Topic: TopicKickstart}}})
			write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewOpen, RequestID: "request", SessionID: "setup", NodeID: "row"}})
			read := func() schema.LocalKickstartPreviewEvent {
				t.Helper()
				_, data, err := conn.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(data) > defaults.KickstartWebPreviewFrameCapBytes {
					t.Fatal("server sent a frame over the cap")
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
				return event
			}
			if tc.Operation == "missing-source" {
				event := read()
				if event.Kind != schema.LocalKickstartPreviewClosed || event.Code != schema.LocalKickstartErrorPreviewUnreadable {
					t.Fatal("missing source did not return the fixed unreadable refusal")
				}
				if strings.Contains(event.Message, path) || strings.Contains(logs.String(), path) || strings.Contains(logs.String(), "synthetic preview content") {
					t.Fatal("preview error or log disclosed source content or path")
				}
				return
			}
			var state schema.LocalKickstartPreviewState
			for range 2 {
				event := read()
				state, err = schema.MergeLocalKickstartPreview(state, event)
				if err != nil {
					t.Fatalf("invalid frame: %v", err)
				}
			}
			if len(state.Segments) != 1 || state.Directory.Complete == tc.Long {
				t.Fatalf("unexpected first segment: %+v", state.Directory)
			}
			switch tc.Operation {
			case "":
			case "short":
				if state.Segments[0].ByteLength >= schema.TranscriptSegmentBudgetBytes {
					t.Fatal("first source read did not send a short segment")
				}
			case "bounded-turn":
				if len(state.Segments[0].Turns) != 1 || !state.Segments[0].Turns[0].Truncated {
					t.Fatal("oversized turn was not sent alone and truncated")
				}
			case "more":
				write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewMore, RequestID: "next", SessionID: "setup", CaptureID: state.CaptureID, After: state.Seq}})
				event := read()
				next, err := schema.MergeLocalKickstartPreview(state, event)
				if err != nil {
					t.Fatalf("merge continuation: %v", err)
				}
				if event.Seq != state.Seq+1 || event.Segment == nil || len(next.Segments) != len(state.Segments)+1 {
					t.Fatal("continuation did not append exactly one segment")
				}
			case "reopen":
				write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewOpen, RequestID: "replacement", SessionID: "setup", NodeID: "row"}})
				closed, opened := read(), read()
				if closed.Kind != schema.LocalKickstartPreviewClosed || closed.Reason != schema.LocalKickstartPreviewReplaced || closed.CaptureID != state.CaptureID || opened.Kind != schema.LocalKickstartPreviewOpened || opened.CaptureID == state.CaptureID {
					t.Fatal("replacement did not close the old capture before opening the new capture")
				}
			case "close":
				write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewClose, RequestID: "close", SessionID: "setup", CaptureID: state.CaptureID}})
				closed := read()
				if closed.Kind != schema.LocalKickstartPreviewClosed || closed.Reason != schema.LocalKickstartPreviewClosedByClient {
					t.Fatal("close did not close the capture")
				}
				write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewOpen, RequestID: "again", SessionID: "setup", NodeID: "row"}})
				if reopened := read(); reopened.Kind != schema.LocalKickstartPreviewOpened {
					t.Fatal("close did not allow another capture")
				}
			case "isolation":
				request := ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewMore, RequestID: "foreign", SessionID: "setup", CaptureID: state.CaptureID, After: state.Seq}}
				data, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				if err := other.Write(ctx, websocket.MessageText, data); err != nil {
					t.Fatal(err)
				}
				_, data, err = other.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var message struct {
					Data schema.LocalKickstartPreviewEvent `json:"data"`
				}
				if err := json.Unmarshal(data, &message); err != nil {
					t.Fatal(err)
				}
				if message.Data.Kind != schema.LocalKickstartPreviewClosed || message.Data.Code != schema.LocalKickstartErrorPreviewCaptureNotFound || message.Data.CaptureID != "" {
					t.Fatal("the second connection received a foreign frame or identifier")
				}
			case "expire":
				select {
				case fire := <-expire:
					fire()
				case <-ctx.Done():
					t.Fatal("idle timer was not installed")
				}
				closed := read()
				if closed.Kind != schema.LocalKickstartPreviewClosed || closed.Reason != schema.LocalKickstartPreviewIdle {
					t.Fatal("idle expiration did not close the capture")
				}
				write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewOpen, RequestID: "again", SessionID: "setup", NodeID: "row"}})
				if reopened := read(); reopened.Kind != schema.LocalKickstartPreviewOpened {
					t.Fatal("idle expiration did not free the capture")
				}
			case "end-setup":
				st.mu.Lock()
				st.closeLocked(st.byID["setup"], schema.LocalKickstartClosedExpired, nil)
				st.mu.Unlock()
				closed := read()
				if closed.Kind != schema.LocalKickstartPreviewClosed || closed.Reason != schema.LocalKickstartPreviewSessionClosed || closed.CaptureID != state.CaptureID {
					t.Fatal("ending setup did not close its capture")
				}
			case "disconnect":
				hub.previews.mu.Lock()
				var held *webPreview
				for _, candidate := range hub.previews.captures {
					if candidate.id == state.CaptureID {
						held = candidate
					}
				}
				hub.previews.mu.Unlock()
				if held == nil {
					t.Fatal("opened capture is missing")
				}
				if err := conn.CloseNow(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-held.ctx.Done():
				case <-ctx.Done():
					t.Fatal("disconnect did not cancel the capture")
				}
				hub.previews.mu.Lock()
				remaining := hub.previews.captures[held.conn]
				hub.previews.mu.Unlock()
				if remaining != nil {
					t.Fatal("disconnect did not free its registry slot")
				}
			default:
				t.Fatalf("unknown fixture operation %q", tc.Operation)
			}
		})
	}
}

func TestPreviewFrameCapRefusesBeforeQueueing(t *testing.T) {
	conn := newConn(nil, NewHub(&mockDataProvider{}))
	err := conn.SendWait(context.Background(), ServerMessage{
		Type: schema.MsgKickstartPreview,
		Data: strings.Repeat("x", defaults.KickstartWebPreviewFrameCapBytes),
	})
	if err == nil {
		t.Fatal("oversized encoded frame was accepted")
	}
	select {
	case <-conn.writeCh:
		t.Fatal("oversized frame entered the write queue")
	default:
	}
}
