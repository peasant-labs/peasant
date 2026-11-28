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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/sourcepreview"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/kickstart_preview_stream.yaml
var streamFixture []byte

type gatedPreviewFS struct {
	*ingest.OSFileSystem
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

var _ ingest.FileSystem = (*gatedPreviewFS)(nil)
var _ ingest.RangeReadableFileSystem = (*gatedPreviewFS)(nil)

func (fs *gatedPreviewFS) ReadFileRange(path string, offset, length int64) ([]byte, error) {
	if length > 4096 && fs.armed.CompareAndSwap(true, false) {
		close(fs.reached)
		<-fs.release
	}
	return fs.OSFileSystem.ReadFileRange(path, offset, length)
}

func (fs *gatedPreviewFS) unblock() { fs.once.Do(func() { close(fs.release) }) }

// lockedLog is a log sink server goroutines write while the test reads it.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// previewLogLine is one preview line of the JSON log: its message and reason,
// and every attribute raw so the test can check keys and values alike.
type previewLogLine struct {
	Msg    string `json:"msg"`
	Reason string `json:"reason"`
	attrs  map[string]json.RawMessage
}

// previewLogRecords returns the preview lines of a JSON log.
func previewLogRecords(t *testing.T, log string, messages []string) []previewLogLine {
	t.Helper()
	var records []previewLogLine
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if line == "" {
			continue
		}
		var record previewLogLine
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %v", err)
		}
		if !strings.HasPrefix(record.Msg, "kickstart preview:") {
			continue
		}
		if !slices.Contains(messages, record.Msg) {
			t.Fatalf("preview log line %q is not in the privacy fixture's closed set", record.Msg)
		}
		if err := json.Unmarshal([]byte(line), &record.attrs); err != nil {
			t.Fatalf("log line attributes are not a JSON object: %v", err)
		}
		records = append(records, record)
	}
	return records
}

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
			// Fraction is the share of the file a truncation keeps.
			Fraction float64 `yaml:"fraction"`
			// Logs names the preview log lines the case must write, in order.
			Logs []string `yaml:"logs"`
			// FreedReason is the reason the freed line must carry.
			FreedReason string `yaml:"freedReason"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(streamFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	var privacy previewPrivacyFixture
	if err := testutil.DecodeNamedFixtureYAML(kickstartPreviewPrivacyYAML, &privacy); err != nil {
		t.Fatal(err)
	}
	production := map[string]string{"opened": previewLogOpened, "sent": previewLogSent, "ended": previewLogEnded, "closed": previewLogClosed, "freed": previewLogFreed}
	if len(privacy.PreviewLog.Named) != len(production) {
		t.Fatalf("the privacy fixture names %d preview log lines, production writes %d", len(privacy.PreviewLog.Named), len(production))
	}
	for name, msg := range production {
		if privacy.PreviewLog.Named[name] != msg {
			t.Fatalf("the privacy fixture's preview log line %q is %q, production writes %q", name, privacy.PreviewLog.Named[name], msg)
		}
		privacy.PreviewLog.Messages = append(privacy.PreviewLog.Messages, msg)
	}
	for _, name := range privacy.SocketCases {
		found := false
		for _, tc := range fixture.Cases {
			found = found || tc.Name == name
		}
		if !found {
			t.Fatalf("privacy socket case %q is missing", name)
		}
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			logs := &lockedLog{}
			oldLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
			t.Cleanup(func() { slog.SetDefault(oldLogger) })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "session.jsonl")
			t.Cleanup(func() {
				if strings.Contains(logs.String(), path) || strings.Contains(logs.String(), privacy.Secrets.Transcript) {
					t.Error("socket preview log disclosed source content or path")
				}
			})
			// checkLogs runs after the case's last frame: the preview lines carry
			// only the fixture's keys, no identifier, path or text, and the
			// lines the case names, in order.
			checkLogs := func(captureID string) {
				t.Helper()
				records := previewLogRecords(t, logs.String(), privacy.PreviewLog.Messages)
				allowed := map[string]bool{"time": true, "level": true, "msg": true}
				for _, key := range privacy.PreviewLog.Keys {
					allowed[key] = true
				}
				forbidden := []string{path, filepath.Dir(path), privacy.Secrets.Transcript, "setup", "row", "11111111-2222-4333-8444-555555555555"}
				if captureID != "" {
					forbidden = append(forbidden, captureID)
				}
				next := 0
				freedCount := 0
				for _, record := range records {
					if record.Msg == privacy.PreviewLog.Named["freed"] {
						freedCount++
					}
					for key, value := range record.attrs {
						if !allowed[key] {
							t.Fatalf("preview log line %q carries attribute %q outside the privacy fixture's keys", record.Msg, key)
						}
						if key == "time" {
							continue
						}
						for _, secret := range forbidden {
							if secret != "" && strings.Contains(string(value), secret) {
								t.Fatalf("preview log line %q attribute %q discloses an identifier, path or text", record.Msg, key)
							}
						}
					}
					if next < len(tc.Logs) && record.Msg == privacy.PreviewLog.Named[tc.Logs[next]] {
						if tc.Logs[next] == "freed" && tc.FreedReason != "" && record.Reason != tc.FreedReason {
							t.Fatalf("the freed line says %q, want %s", record.Reason, tc.FreedReason)
						}
						next++
					}
				}
				if next < len(tc.Logs) {
					t.Fatalf("the preview log lacks %q (and what follows it) among %d preview lines", tc.Logs[next], len(records))
				}
				if slices.Contains(tc.Logs, "freed") && freedCount != 1 {
					t.Fatalf("preview log wrote %d freed lines, want exactly 1", freedCount)
				}
			}
			content := tc.Transcript
			content = strings.ReplaceAll(content, "{{PAD}}", strings.Repeat("x", 600))
			if tc.Operation == "bounded-turn" {
				content = fmt.Sprintf("{\"type\":\"user\",\"uuid\":\"u1\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"%s\"}}\n", strings.Repeat("x", fixture.Generated.OversizedBytes))
			}
			if tc.Long {
				var data strings.Builder
				character := "x"
				if tc.Operation == "finish-without-segment" {
					// JSON escaping expands ampersands in the served segments.
					character = "&"
				}
				for i := range fixture.Generated.Pairs {
					parent := ""
					if i > 0 {
						parent = fmt.Sprintf(`"parentUuid":"a%d",`, i-1)
					}
					fmt.Fprintf(&data, "{\"type\":\"user\",\"uuid\":\"u%d\",%s\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"question\"}}\n", i, parent)
					content := strings.Repeat(character, fixture.Generated.AnswerBytes)
					if i == 0 {
						content += privacy.Secrets.Transcript
					}
					fmt.Fprintf(&data, "{\"type\":\"assistant\",\"uuid\":\"a%d\",\"parentUuid\":\"u%d\",\"timestamp\":\"2026-01-01T00:00:01Z\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"%s\"}]}}\n", i, i, content)
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
			gated := strings.HasPrefix(tc.Operation, "gated-")
			rescan := tc.Operation == "rescan" || tc.Operation == "gated-rescan"
			var gatedFS *gatedPreviewFS
			if gated {
				gatedFS = &gatedPreviewFS{OSFileSystem: &ingest.OSFileSystem{}, reached: make(chan struct{}), release: make(chan struct{})}
				env.previewFS = gatedFS
				env.now = func() time.Time { return time.Unix(1, 0) }
				env.afterFunc = func(time.Duration, func()) func() bool { return func() bool { return true } }
				defer gatedFS.unblock()
			}
			if rescan {
				env.ctx = ctx
				env.configPath = filepath.Join(t.TempDir(), "config.yaml")
				env.newID = func() string { return "setup" }
				env.loadConfig = func(context.Context) (*config.Config, error) { return config.BaseConfig(), nil }
				env.scan = func(ctx context.Context, _ *config.Config, _ ingest.KnownSessionIndex, _ discovery.ScanProgress) (discovery.ProviderInventory, []onboarding.SessionListing, discovery.SubagentRelation) {
					<-ctx.Done()
					return nil, nil, nil
				}
			}
			if tc.Operation == "expire" {
				env.afterFunc = func(_ time.Duration, fn func()) func() bool {
					expire <- fn
					return func() bool { return true }
				}
			}
			st := newKickstartSessions(env)
			if rescan {
				if _, err := st.open(ctx, onboarding.ScopeFull); err != nil {
					t.Fatal(err)
				}
				defer st.shutdown()
			} else {
				st.byID["setup"] = &kickstartSession{id: "setup"}
			}
			st.mu.Lock()
			if rescan {
				delete(st.byScope, onboarding.ScopeFull)
			}
			st.byID["setup"].forest = onboarding.Forest{Nodes: []onboarding.Node{{ID: "row", Kind: onboarding.NodeKindSession, SessionID: sourceID}}}
			st.byID["setup"].listings = []onboarding.SessionListing{{SessionID: sourceID, Harness: "claude-code", Source: onboarding.SessionSource{Path: path, Origin: onboarding.SessionSourceOriginFile}}}
			st.mu.Unlock()
			hub := NewHub(&mockDataProvider{})
			hub.attachKickstart(st)
			switch tc.Operation {
			case "late-result":
				hub.previews.captureOptions = sourcepreview.CaptureOptions{FirstReadBytes: 1300, SliceReadBytes: 400}
			case "truncate-between-loads", "every-more-answered":
				// Small reads, so each load past the first reads the source again.
				hub.previews.captureOptions = sourcepreview.CaptureOptions{FirstReadBytes: 64 << 10, SliceReadBytes: 1 << 20, SourceCapBytes: defaults.KickstartWebPreviewSourceCapBytes}
			}
			idReached, idRelease := make(chan struct{}), make(chan struct{})
			if tc.Operation == "refuse-reopen" {
				var count atomic.Int32
				hub.previews.newID = func() (string, error) {
					if count.Add(1) == 2 {
						close(idReached)
						select {
						case <-idRelease:
						case <-ctx.Done():
							return "", ctx.Err()
						}
					}
					return newPreviewCaptureID()
				}
			}
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
			if tc.Operation == "busy-capacity" || tc.Operation == "capacity-race" {
				// Occupy the parse worker so both registered captures are busy.
				hub.previews.worker <- struct{}{}
				defer func() { <-hub.previews.worker }()
				send := func(socket *websocket.Conn, msg ClientMessage) {
					t.Helper()
					data, err := json.Marshal(msg)
					if err != nil {
						t.Fatal(err)
					}
					if err := socket.Write(ctx, websocket.MessageText, data); err != nil {
						t.Fatal(err)
					}
				}
				subscribe := ClientMessage{Type: MsgSubscribe, Channels: []ChannelSubscription{{Topic: TopicKickstart}}}
				open := ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewOpen, RequestID: "request", SessionID: "setup", NodeID: "row"}}
				barrier := ClientMessage{Type: MsgDashboard}
				openBusy := func(socket *websocket.Conn) {
					t.Helper()
					send(socket, subscribe)
					send(socket, open)
					send(socket, barrier)
					_, data, err := socket.Read(ctx)
					if err != nil {
						t.Fatal(err)
					}
					var message ServerMessage
					if err := json.Unmarshal(data, &message); err != nil || message.Type != MsgError {
						t.Fatal("capture registration barrier did not complete")
					}
				}
				openBusy(conn)
				connect := func() *websocket.Conn {
					t.Helper()
					socket, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
					if err != nil {
						t.Fatal(err)
					}
					if _, _, err := socket.Read(ctx); err != nil {
						t.Fatal(err)
					}
					return socket
				}
				second := connect()
				defer second.CloseNow()
				if tc.Operation == "capacity-race" {
					third := connect()
					defer third.CloseNow()
					start := make(chan struct{})
					results := make(chan []byte, 2)
					run := func(socket *websocket.Conn) {
						<-start
						for _, msg := range []ClientMessage{subscribe, open, barrier} {
							data, _ := json.Marshal(msg)
							if socket.Write(ctx, websocket.MessageText, data) != nil {
								results <- nil
								return
							}
						}
						_, data, err := socket.Read(ctx)
						if err != nil {
							results <- nil
							return
						}
						results <- data
					}
					go run(second)
					go run(third)
					close(start)
					successes, refusals := 0, 0
					for range 2 {
						var received struct {
							Type schema.MessageType                `json:"type"`
							Data schema.LocalKickstartPreviewEvent `json:"data"`
						}
						select {
						case data := <-results:
							if err := json.Unmarshal(data, &received); err != nil {
								t.Fatal(err)
							}
						case <-ctx.Done():
							t.Fatal("racing opens did not answer")
						}
						if received.Type == MsgError {
							successes++
						} else if received.Type == schema.MsgKickstartPreview && received.Data.Reason == schema.LocalKickstartPreviewCapacity {
							refusals++
						}
					}
					if successes != 1 || refusals != 1 {
						t.Fatal("racing opens did not yield exactly one reservation and one capacity refusal")
					}
					return
				}
				openBusy(second)
				third := connect()
				defer third.CloseNow()
				send(third, subscribe)
				send(third, open)
				_, data, err := third.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var message struct {
					Data schema.LocalKickstartPreviewEvent `json:"data"`
				}
				if err := json.Unmarshal(data, &message); err != nil || message.Data.Reason != schema.LocalKickstartPreviewCapacity || message.Data.Code != schema.LocalKickstartErrorPreviewCapacity {
					t.Fatal("third open was not refused for capacity")
				}
				return
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
				if event.Kind == schema.LocalKickstartPreviewClosed || event.Continuation == schema.LocalKickstartPreviewContinuationCut {
					if strings.Contains(string(envelope.Data), path) || strings.Contains(string(envelope.Data), privacy.Secrets.Transcript) {
						t.Fatal("preview error or notice disclosed source content or path")
					}
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
				if len(tc.Logs) > 0 {
					checkLogs("")
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
			if len(state.Segments) != 1 || state.Directory.Complete == (tc.Long || tc.Operation == "late-result") {
				t.Fatalf("unexpected first segment: %+v", state.Directory)
			}
			more := func() {
				t.Helper()
				write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewMore, RequestID: "next", SessionID: "setup", CaptureID: state.CaptureID, After: state.Seq}})
			}
			// barrier proves no preview frame is still owed or queued: the
			// socket answers a non-preview message with an error, in order.
			barrier := func() {
				t.Helper()
				write(ClientMessage{Type: MsgDashboard})
				_, data, err := conn.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var message ServerMessage
				if err := json.Unmarshal(data, &message); err != nil || message.Type != MsgError {
					t.Fatalf("a preview frame arrived where the barrier's reply was due: %.200s", data)
				}
			}
			defer func() {
				if !t.Failed() && len(tc.Logs) > 0 {
					checkLogs(state.CaptureID)
				}
			}()
			switch tc.Operation {
			case "":
			case "every-more-answered":
				// Every load until the capture ends gets exactly one frame.
				for loads := 0; !state.Directory.Complete; loads++ {
					if loads > 200 {
						t.Fatal("the capture did not end within 200 loads")
					}
					more()
					event := read()
					if event.Kind == schema.LocalKickstartPreviewClosed {
						t.Fatalf("load %d closed the capture with %s", loads+1, event.Reason)
					}
					next, err := schema.MergeLocalKickstartPreview(state, event)
					if err != nil {
						t.Fatalf("merge load %d: %v", loads+1, err)
					}
					state = next
					barrier()
				}
			case "truncate-between-loads":
				more()
				loaded, err := schema.MergeLocalKickstartPreview(state, read())
				if err != nil {
					t.Fatalf("merge the load before truncation: %v", err)
				}
				state = loaded
				barrier()
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(path, int64(float64(info.Size())*tc.Fraction)); err != nil {
					t.Fatal(err)
				}
				more()
				event := read()
				if event.Kind != schema.LocalKickstartPreviewClosed || event.Reason != schema.LocalKickstartPreviewSourceChanged || event.Code != schema.LocalKickstartErrorPreviewSourceChanged || event.Seq != state.Seq+1 {
					t.Fatalf("the load after truncation was answered with %s/%s, want one source_changed close in sequence", event.Kind, event.Reason)
				}
				barrier()
			case "short":
				if state.Segments[0].ByteLength >= schema.TranscriptSegmentBudgetBytes {
					t.Fatal("first source read did not send a short segment")
				}
			case "bounded-turn":
				if len(state.Segments[0].Turns) != 1 || !state.Segments[0].Turns[0].Truncated {
					t.Fatal("oversized turn was not sent alone and truncated")
				}
			case "load-next":
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
			case "close-preview":
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
			case "rescan":
				st.mu.Lock()
				session := st.byID["setup"]
				generation := session.scanGen
				st.mu.Unlock()
				st.finishScan(session, generation, kickstartScanResult{})
				closed := read()
				if closed.Kind != schema.LocalKickstartPreviewClosed || closed.Reason != schema.LocalKickstartPreviewRescanned || closed.CaptureID != state.CaptureID {
					t.Fatal("finished scan did not close its capture")
				}
			case "alter-source":
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
				write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewMore, RequestID: "next", SessionID: "setup", CaptureID: state.CaptureID, After: state.Seq}})
				event := read()
				if event.Kind != schema.LocalKickstartPreviewClosed || event.Reason != schema.LocalKickstartPreviewSourceChanged || event.Code != schema.LocalKickstartErrorPreviewSourceChanged {
					t.Fatal("changed source did not close with its reason")
				}
			case "late-result":
				// Load until the second answer is read; the capture must close
				// as a late result, never complete as if nothing happened.
				for loads := 0; ; loads++ {
					if loads > 20 {
						t.Fatal("the late result never closed the capture")
					}
					more()
					event := read()
					if event.Kind == schema.LocalKickstartPreviewClosed {
						if event.Reason != schema.LocalKickstartPreviewLateResult || event.Code != schema.LocalKickstartErrorPreviewLateResult || !strings.Contains(event.Message, "read it again") || strings.Contains(event.Message, "changed") {
							t.Fatalf("late result closed with %s %q, want a late result offering a fresh read without claiming a change", event.Reason, event.Message)
						}
						break
					}
					next, err := schema.MergeLocalKickstartPreview(state, event)
					if err != nil {
						t.Fatalf("merge load %d: %v", loads+1, err)
					}
					if next.Directory.Complete {
						t.Fatal("the capture completed although a shown call was answered again later")
					}
					state = next
				}
			case "refuse-reopen":
				write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewOpen, RequestID: "rejected-request", SessionID: "setup", NodeID: "row"}})
				select {
				case <-idReached:
				case <-ctx.Done():
					t.Fatal("replacement did not reach its admission gate")
				}
				st.mu.Lock()
				st.byID["setup"].scanGen++
				st.mu.Unlock()
				close(idRelease)
				refused := read()
				if refused.Kind != schema.LocalKickstartPreviewClosed || refused.Reason != schema.LocalKickstartPreviewRescanned || refused.RequestID != "rejected-request" {
					t.Fatal("changed setup did not refuse the replacement")
				}
				write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewMore, RequestID: "next", SessionID: "setup", CaptureID: state.CaptureID, After: state.Seq}})
				if next := read(); next.Kind != schema.LocalKickstartPreviewSegmentEvent || next.CaptureID != state.CaptureID {
					t.Fatal("refused replacement lost the existing capture")
				}
			case "finish-without-segment":
				for !state.Directory.Complete {
					previousRefs, err := json.Marshal(state.Directory.Segments)
					if err != nil {
						t.Fatal(err)
					}
					write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewMore, RequestID: "next", SessionID: "setup", CaptureID: state.CaptureID, After: state.Seq}})
					event := read()
					next, err := schema.MergeLocalKickstartPreview(state, event)
					if err != nil {
						t.Fatalf("merge continuation: %v", err)
					}
					if next.Directory.Complete {
						if event.Segment != nil {
							t.Fatal("expected the serialized cap to finish without a new segment")
						}
						finalRefs, err := json.Marshal(event.Directory.Segments)
						if err != nil || !bytes.Equal(previousRefs, finalRefs) {
							t.Fatal("final frame altered previously sent references")
						}
					}
					state = next
				}
			case "gated-close", "gated-reopen", "gated-rescan":
				hub.previews.mu.Lock()
				var held *webPreview
				for _, candidate := range hub.previews.captures {
					if candidate.id == state.CaptureID {
						held = candidate
					}
				}
				if held == nil {
					hub.previews.mu.Unlock()
					t.Fatal("opened capture is missing")
				}
				var releases atomic.Int32
				cancel := held.cancel
				held.cancel = func() { releases.Add(1); cancel() }
				hub.previews.mu.Unlock()
				gatedFS.armed.Store(true)
				write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewMore, RequestID: "next", SessionID: "setup", CaptureID: state.CaptureID, After: state.Seq}})
				select {
				case <-gatedFS.reached:
				case <-ctx.Done():
					t.Fatal("continuation did not reach the range read")
				}
				switch tc.Operation {
				case "gated-close":
					write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewClose, RequestID: "close", SessionID: "setup", CaptureID: state.CaptureID}})
				case "gated-reopen":
					write(ClientMessage{Type: schema.MsgKickstartPreview, Preview: &schema.LocalKickstartPreviewRequest{Op: schema.LocalKickstartPreviewOpen, RequestID: "replacement", SessionID: "setup", NodeID: "row"}})
				case "gated-rescan":
					st.mu.Lock()
					session := st.byID["setup"]
					generation := session.scanGen
					st.mu.Unlock()
					st.finishScan(session, generation, kickstartScanResult{})
				}
				closed := read()
				if closed.Kind != schema.LocalKickstartPreviewClosed || closed.CaptureID != state.CaptureID || closed.Seq != state.Seq+1 {
					t.Fatal("old capture did not close in sequence")
				}
				gatedFS.unblock()
				if tc.Operation == "gated-reopen" {
					opened := read()
					first := read()
					if opened.Kind != schema.LocalKickstartPreviewOpened || opened.CaptureID == state.CaptureID || first.Kind != schema.LocalKickstartPreviewSegmentEvent || first.CaptureID != opened.CaptureID {
						t.Fatal("replacement frames did not follow the old close")
					}
				}
				// Acquiring the parse slot is a barrier: the late read returned and
				// its worker has finished before the following socket barrier.
				select {
				case hub.previews.worker <- struct{}{}:
				case <-ctx.Done():
					t.Fatal("late read did not release the parse worker")
				}
				<-hub.previews.worker
				write(ClientMessage{Type: MsgDashboard})
				_, data, err := conn.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var barrier ServerMessage
				if err := json.Unmarshal(data, &barrier); err != nil || barrier.Type != MsgError {
					t.Fatal("a late preview frame was delivered after close")
				}
				if releases.Load() != 1 {
					t.Fatalf("capture released %d times, want once", releases.Load())
				}
				hub.previews.mu.Lock()
				current := hub.previews.captures[held.conn]
				hub.previews.mu.Unlock()
				if current == held {
					t.Fatal("closed capture still occupies its registry slot")
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
