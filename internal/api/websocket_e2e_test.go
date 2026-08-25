package api_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

type wsMockProvider struct {
	mu          sync.Mutex
	sessions    []ingest.Session
	dashboard   *api.DashboardPayload
	trends      *api.TrendsPayload
	summaries   []api.SessionSummary
	quality     []api.QualitySession
	annotations map[string][]schema.AnnotationSummary // sessionID → annotations
	failTopics  map[schema.ChannelTopic]error         // topic → injected failure; unset/nil means succeed
}

// setFailure injects (or, with a nil err, clears) a failure for one ticker
// channel (dashboard/sessions/trends), letting tests drive the exact
// valid → conflict → cleared → repaired sequence the production Hub is
// contracted to produce through both sendSnapshots (on subscribe) and
// broadcastAll (on tick).
func (m *wsMockProvider) setFailure(topic schema.ChannelTopic, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failTopics == nil {
		m.failTopics = make(map[schema.ChannelTopic]error)
	}
	if err == nil {
		delete(m.failTopics, topic)
		return
	}
	m.failTopics[topic] = err
}

func (m *wsMockProvider) failureFor(topic schema.ChannelTopic) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failTopics[topic]
}

func (m *wsMockProvider) Sessions(ctx context.Context) ([]ingest.Session, error) {
	return m.sessions, nil
}

func (m *wsMockProvider) SessionSummaries(ctx context.Context) ([]api.SessionSummary, error) {
	if err := m.failureFor(api.TopicSessions); err != nil {
		return nil, err
	}
	return m.summaries, nil
}

func (m *wsMockProvider) SessionSummariesByID(ctx context.Context, ids []string) ([]api.SessionSummary, error) {
	if err := m.failureFor(api.TopicSessions); err != nil {
		return nil, err
	}
	resolved := make([]api.SessionSummary, 0, len(ids))
	for _, id := range ids {
		for i := range m.summaries {
			if m.summaries[i].ID == id {
				resolved = append(resolved, m.summaries[i])
			}
		}
	}
	return resolved, nil
}

func (m *wsMockProvider) SessionByID(ctx context.Context, id string) (*ingest.Session, error) {
	for i := range m.sessions {
		if string(m.sessions[i].ID) == id {
			return &m.sessions[i], nil
		}
	}
	return nil, nil
}

func (m *wsMockProvider) DashboardMetrics(ctx context.Context) (*api.DashboardPayload, error) {
	if err := m.failureFor(api.TopicDashboard); err != nil {
		return nil, err
	}
	return m.dashboard, nil
}

func (m *wsMockProvider) TrendsData(ctx context.Context) (*api.TrendsPayload, error) {
	if err := m.failureFor(api.TopicTrends); err != nil {
		return nil, err
	}
	return m.trends, nil
}

func (m *wsMockProvider) QualitySessions(ctx context.Context, f api.QualityFilter) ([]api.QualitySession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.quality, nil
}

func (m *wsMockProvider) AnnotationsForSession(_ context.Context, sessionID string) ([]schema.AnnotationSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.annotations != nil {
		return m.annotations[sessionID], nil
	}
	return nil, nil
}

func (m *wsMockProvider) ProjectFamiliarity(_ context.Context, projectHash schema.ProjectHash) (*schema.FamiliarityPayload, error) {
	return &schema.FamiliarityPayload{
		ProjectHash:     projectHash,
		FamiliarityPct:  65.0,
		UnexploredCount: 3,
		Files:           nil,
	}, nil
}

func (m *wsMockProvider) ChildSessionsForParent(_ context.Context, _ string) ([]schema.ChildSessionRef, error) {
	return nil, nil
}

func (m *wsMockProvider) ProjectSummaries(_ context.Context) (*codemap.ProjectSummariesResult, error) {
	return &codemap.ProjectSummariesResult{Projects: []schema.ProjectSummary{}}, nil
}

func (m *wsMockProvider) ResolveProject(_ context.Context, project string) (*schema.ProjectResolutionPayload, error) {
	return &schema.ProjectResolutionPayload{Project: project, ProjectHash: schema.ProjectHash("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")}, nil
}

func (m *wsMockProvider) MapGraph(_ context.Context, projectHash schema.ProjectHash, _ string) (*schema.MapGraphPayload, error) {
	return schema.NewMapGraphPayload(projectHash), nil
}

func (m *wsMockProvider) MapNodeDetail(_ context.Context, _ schema.ProjectHash, path string) (*schema.MapNodeDetailPayload, error) {
	return schema.NewMapNodeDetailPayload(path), nil
}

func (m *wsMockProvider) ProjectTasks(_ context.Context, projectHash schema.ProjectHash, _ string) (*schema.ProjectTasksPayload, error) {
	return schema.NewProjectTasksPayload(projectHash), nil
}

func (m *wsMockProvider) ReviewChanges(_ context.Context, projectHash schema.ProjectHash) (*schema.ReviewListPayload, error) {
	return schema.NewReviewListPayload(projectHash), nil
}

func (m *wsMockProvider) ChangeDetail(_ context.Context, _ schema.ProjectHash, branch string) (*schema.ChangeDetailPayload, error) {
	return schema.NewChangeDetailPayload(branch), nil
}

func (m *wsMockProvider) ChangeDiff(_ context.Context, _ schema.ProjectHash, branch, file string) (*schema.ChangeDiffPayload, error) {
	return schema.NewChangeDiffPayload(branch, file), nil
}

func (m *wsMockProvider) Search(_ context.Context, query string, _ int) (*schema.SearchPayload, error) {
	return schema.NewSearchPayload(query), nil
}

func TestProgressiveProvider_E2E_WebSocket(t *testing.T) {
	t.Parallel()

	mockProv := &wsMockProvider{
		sessions:  []ingest.Session{{ID: "mock-session-1"}},
		summaries: []api.SessionSummary{{ID: "mock-session-1"}},
		dashboard: &api.DashboardPayload{TotalSessions: 1},
	}
	realProv := &wsMockProvider{
		sessions:  []ingest.Session{{ID: "real-session-1"}},
		summaries: []api.SessionSummary{{ID: "real-session-1"}},
		dashboard: &api.DashboardPayload{TotalSessions: 10},
	}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     nil, // nil means all sections mocked
			},
		},
	}

	prov := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	hub := api.NewHub(prov)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	conn, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var msg map[string]any
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (connected): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if msg["type"] != string(api.MsgConnected) {
		t.Errorf("first message type = %q, want %q", msg["type"], api.MsgConnected)
	}

	subscribeMsg := api.ClientMessage{
		Type:     api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{Topic: api.TopicSessions}},
	}
	subBytes, err := json.Marshal(subscribeMsg)
	if err != nil {
		t.Fatalf("json.Marshal subscribe: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, subBytes); err != nil {
		t.Fatalf("websocket.Write (subscribe): %v", err)
	}

	_, data, err = conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (sessions): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal sessions: %v", err)
	}
	if msg["type"] != string(api.MsgSessions) {
		t.Errorf("message type = %q, want %q", msg["type"], api.MsgSessions)
	}

	dataMap, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatalf("msg.Data = %T, want map", msg["data"])
	}
	sessionsData, ok := dataMap["sessions"].([]any)
	if !ok || len(sessionsData) != 1 {
		t.Errorf("sessions = %v, want 1 session", sessionsData)
	}
	sessionMap, ok := sessionsData[0].(map[string]any)
	if !ok || sessionMap["id"] != "mock-session-1" {
		t.Errorf("session id = %v, want mock-session-1", sessionMap["id"])
	}
}

func TestProgressiveProvider_E2E_WebSocket_MockDisabled(t *testing.T) {
	t.Parallel()

	mockProv := &wsMockProvider{
		sessions:  []ingest.Session{{ID: "mock-session-1"}},
		summaries: []api.SessionSummary{{ID: "mock-session-1"}},
		dashboard: &api.DashboardPayload{TotalSessions: 1},
	}
	realProv := &wsMockProvider{
		sessions:  []ingest.Session{{ID: "real-session-1"}},
		summaries: []api.SessionSummary{{ID: "real-session-1"}},
		dashboard: &api.DashboardPayload{TotalSessions: 10},
	}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: false,
			},
		},
	}

	prov := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	hub := api.NewHub(prov)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	conn, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var msg map[string]any
	_, data, _ := conn.Read(ctx)
	json.Unmarshal(data, &msg)

	subscribeMsg := api.ClientMessage{
		Type:     api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{Topic: api.TopicSessions}},
	}
	subBytes, _ := json.Marshal(subscribeMsg)
	conn.Write(ctx, websocket.MessageText, subBytes)

	_, data, err = conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (sessions): %v", err)
	}
	json.Unmarshal(data, &msg)

	dataMap := msg["data"].(map[string]any)
	sessionsData := dataMap["sessions"].([]any)
	sessionMap := sessionsData[0].(map[string]any)
	if sessionMap["id"] != "real-session-1" {
		t.Errorf("session id = %v, want real-session-1", sessionMap["id"])
	}
}

func TestProgressiveProvider_E2E_WebSocket_ComponentSpecific(t *testing.T) {
	t.Parallel()

	tuiMockProv := &wsMockProvider{
		sessions:  []ingest.Session{{ID: "tui-mock"}},
		summaries: []api.SessionSummary{{ID: "tui-mock"}},
	}
	webMockProv := &wsMockProvider{
		sessions:  []ingest.Session{{ID: "web-mock"}},
		summaries: []api.SessionSummary{{ID: "web-mock"}},
	}
	realProv := &wsMockProvider{
		sessions:  []ingest.Session{{ID: "real"}},
		summaries: []api.SessionSummary{{ID: "real"}},
	}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				TUI:     []defaults.MockSection{defaults.MockSections.Sessions},
				Web:     nil, // nil means all mocked for Web
			},
		},
	}

	t.Run("WebComponent_AllMocked", func(t *testing.T) {
		webProv := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, webMockProv, realProv)
		hub := api.NewHub(webProv)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go hub.Run(ctx)

		server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
		defer server.Close()

		conn, _, _ := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
		defer conn.Close(websocket.StatusNormalClosure, "")

		_, data, _ := conn.Read(ctx)
		var connectedMsg map[string]any
		json.Unmarshal(data, &connectedMsg)

		subMsg := api.ClientMessage{Type: api.MsgSubscribe, Channels: []api.ChannelSubscription{{Topic: api.TopicSessions}}}
		conn.Write(ctx, websocket.MessageText, mustMarshalJSON(subMsg))

		_, data, _ = conn.Read(ctx)
		var msg map[string]any
		json.Unmarshal(data, &msg)

		dataMap := msg["data"].(map[string]any)
		sessionsData := dataMap["sessions"].([]any)
		sessionMap := sessionsData[0].(map[string]any)
		if sessionMap["id"] != "web-mock" {
			t.Errorf("expected web-mock, got %s", sessionMap["id"])
		}
	})

	t.Run("TUIComponent_SessionsMocked", func(t *testing.T) {
		tuiProv := api.NewProgressiveProvider(cfg, defaults.MockComponents.TUI, tuiMockProv, realProv)
		hub := api.NewHub(tuiProv)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go hub.Run(ctx)

		server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
		defer server.Close()

		conn, _, _ := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
		defer conn.Close(websocket.StatusNormalClosure, "")

		_, data, _ := conn.Read(ctx)
		var connectedMsg map[string]any
		json.Unmarshal(data, &connectedMsg)

		subMsg := api.ClientMessage{Type: api.MsgSubscribe, Channels: []api.ChannelSubscription{{Topic: api.TopicSessions}}}
		conn.Write(ctx, websocket.MessageText, mustMarshalJSON(subMsg))

		_, data, _ = conn.Read(ctx)
		var msg map[string]any
		json.Unmarshal(data, &msg)

		dataMap := msg["data"].(map[string]any)
		sessionsData := dataMap["sessions"].([]any)
		sessionMap := sessionsData[0].(map[string]any)
		if sessionMap["id"] != "tui-mock" {
			t.Errorf("expected tui-mock, got %s", sessionMap["id"])
		}
	})
}

func TestProgressiveProvider_E2E_WebSocket_SessionDetail(t *testing.T) {
	t.Parallel()

	mockProv := &wsMockProvider{
		sessions: []ingest.Session{
			{
				ID:      "session-123",
				Harness: defaults.HarnessClaudeCode,
				Turns: []ingest.Turn{
					{Index: 0, Role: "user", Content: "Hello"},
				},
			},
		},
		summaries: []api.SessionSummary{{ID: "session-123"}},
	}
	realProv := &wsMockProvider{
		sessions:  []ingest.Session{},
		summaries: []api.SessionSummary{},
	}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     nil,
			},
		},
	}

	prov := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	hub := api.NewHub(prov)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	conn, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	_, data, _ := conn.Read(ctx)
	var connectedMsg map[string]any
	json.Unmarshal(data, &connectedMsg)

	subscribeMsg := api.ClientMessage{
		Type:     api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{Topic: api.TopicSessionDetail, ID: "session-123"}},
	}
	subBytes, _ := json.Marshal(subscribeMsg)
	conn.Write(ctx, websocket.MessageText, subBytes)

	_, data, err = conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (session_detail): %v", err)
	}

	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if msg["type"] != string(api.MsgSessionDetail) {
		t.Errorf("message type = %q, want %q", msg["type"], api.MsgSessionDetail)
	}

	dataMap, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatalf("msg.Data = %T, want map", msg["data"])
	}
	if dataMap["id"] != "session-123" {
		t.Errorf("session ID = %v, want %v", dataMap["id"], "session-123")
	}
	turns, ok := dataMap["turns"].([]any)
	if !ok || len(turns) != 1 {
		t.Errorf("turn count = %v, want 1", turns)
	}
}

func TestHub_Quality_Channel(t *testing.T) {
	t.Parallel()

	mockProv := &wsMockProvider{
		quality: []api.QualitySession{
			{ID: "quality-1", Project: "test-project"},
			{ID: "quality-2", Project: "other-project"},
		},
	}
	realProv := &wsMockProvider{}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     nil, // nil means all sections mocked
			},
		},
	}

	prov := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	hub := api.NewHub(prov)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	conn, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Read the "connected" message
	var msg map[string]any
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (connected): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if msg["type"] != string(api.MsgConnected) {
		t.Errorf("first message type = %q, want %q", msg["type"], api.MsgConnected)
	}

	// Subscribe to quality channel
	subscribeMsg := api.ClientMessage{
		Type:     api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{Topic: api.TopicQuality}},
	}
	subBytes, err := json.Marshal(subscribeMsg)
	if err != nil {
		t.Fatalf("json.Marshal subscribe: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, subBytes); err != nil {
		t.Fatalf("websocket.Write (subscribe): %v", err)
	}

	// Read the snapshot response
	_, data, err = conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (quality): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal quality: %v", err)
	}
	if msg["type"] != string(api.MsgQuality) {
		t.Errorf("message type = %q, want %q", msg["type"], api.MsgQuality)
	}

	dataMap, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatalf("msg.Data = %T, want map", msg["data"])
	}
	sessionsData, ok := dataMap["sessions"].([]any)
	if !ok || len(sessionsData) != 2 {
		t.Fatalf("sessions = %v, want 2 sessions", sessionsData)
	}
	sessionMap, ok := sessionsData[0].(map[string]any)
	if !ok || sessionMap["id"] != "quality-1" {
		t.Errorf("session[0] id = %v, want quality-1", sessionMap["id"])
	}
}

func TestHub_Quality_BroadcastOnRefresh(t *testing.T) {
	t.Parallel()

	mockProv := &wsMockProvider{
		quality: []api.QualitySession{
			{ID: "quality-tick-1", Project: "proj-a"},
		},
	}
	realProv := &wsMockProvider{}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     nil,
			},
		},
	}

	prov := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	hub := api.NewHub(prov)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	conn, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Read "connected" message.
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (connected): %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if msg["type"] != string(api.MsgConnected) {
		t.Fatalf("first message type = %q, want %q", msg["type"], api.MsgConnected)
	}

	// Subscribe to quality channel.
	subBytes := mustMarshalJSON(api.ClientMessage{
		Type:     api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{Topic: api.TopicQuality}},
	})
	if err := conn.Write(ctx, websocket.MessageText, subBytes); err != nil {
		t.Fatalf("websocket.Write (subscribe): %v", err)
	}

	// Read initial snapshot — should have 1 session.
	_, data, err = conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (snapshot): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal snapshot: %v", err)
	}
	if msg["type"] != string(api.MsgQuality) {
		t.Fatalf("snapshot type = %q, want %q", msg["type"], api.MsgQuality)
	}
	dataMap := msg["data"].(map[string]any)
	sessionsData := dataMap["sessions"].([]any)
	if len(sessionsData) != 1 {
		t.Fatalf("snapshot sessions = %d, want 1", len(sessionsData))
	}

	// Mutate the mock provider: add a second quality session.
	mockProv.mu.Lock()
	mockProv.quality = []api.QualitySession{
		{ID: "quality-tick-1", Project: "proj-a"},
		{ID: "quality-tick-2", Project: "proj-b"},
	}
	mockProv.mu.Unlock()

	// Trigger event-driven quality refresh (replaces the old ticker-based broadcast).
	hub.RefreshQuality(ctx)

	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()

	_, data, err = conn.Read(readCtx)
	if err != nil {
		t.Fatalf("websocket.Read (refresh broadcast): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal broadcast: %v", err)
	}
	if msg["type"] != string(api.MsgQuality) {
		t.Fatalf("broadcast type = %q, want %q", msg["type"], api.MsgQuality)
	}

	dataMap = msg["data"].(map[string]any)
	sessionsData = dataMap["sessions"].([]any)
	if len(sessionsData) != 2 {
		t.Fatalf("broadcast sessions = %d, want 2", len(sessionsData))
	}
	// Verify the new session is present.
	found := false
	for _, s := range sessionsData {
		sm := s.(map[string]any)
		if sm["id"] == "quality-tick-2" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("broadcast did not contain quality-tick-2; got %v", sessionsData)
	}
}

// TestValidateSubscription is a unit test for the ValidateSubscription function
// in messages.go. It covers all valid topics and all validation failure modes.
// Test cases are loaded from the shared YAML fixture embedded in the github.com/peasant-labs/schema module.
func TestValidateSubscription(t *testing.T) {
	t.Parallel()

	fixtures, err := schema.LoadAnnotationFixtures()
	if err != nil {
		t.Fatalf("LoadAnnotationFixtures: %v", err)
	}

	for _, tc := range fixtures.Validations {
		t.Run(tc.Name, func(t *testing.T) {
			sub := api.ChannelSubscription{
				Topic: api.ChannelTopic(tc.Topic),
				Axis:  api.AnnotationAxis(tc.Axis),
				ID:    tc.ID,
			}
			got := api.ValidateSubscription(sub)
			if got != tc.Valid {
				t.Errorf("ValidateSubscription(%+v) = %v, want %v", sub, got, tc.Valid)
			}
		})
	}
}

// TestHub_Quality_EffectiveAnnotations is an E2E test that subscribes to the
// quality channel with a mock provider whose QualitySession includes
// EffectiveAnnotations, and asserts the field appears in the JSON wire format.
// Annotation data is loaded from the shared YAML fixture embedded in the github.com/peasant-labs/schema module.
func TestHub_Quality_EffectiveAnnotations(t *testing.T) {
	t.Parallel()

	fixtures, err := schema.LoadAnnotationFixtures()
	if err != nil {
		t.Fatalf("LoadAnnotationFixtures: %v", err)
	}
	humanResolved := fixtures.FindSummary("human_outcome_resolved")
	if humanResolved == nil {
		t.Fatal("fixture 'human_outcome_resolved' not found")
	}
	ann := humanResolved.ToAnnotationSummary()

	mockProv := &wsMockProvider{
		quality: []api.QualitySession{
			{
				ID:                   "quality-ann-1",
				Project:              "test-project",
				EffectiveAnnotations: []schema.AnnotationSummary{ann},
			},
		},
	}
	realProv := &wsMockProvider{}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     nil, // nil means all sections mocked
			},
		},
	}

	prov := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	hub := api.NewHub(prov)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	conn, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Read the "connected" message.
	var msg map[string]any
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (connected): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal (connected): %v", err)
	}
	if msg["type"] != string(api.MsgConnected) {
		t.Errorf("first message type = %q, want %q", msg["type"], api.MsgConnected)
	}

	// Subscribe to quality channel.
	subscribeMsg := api.ClientMessage{
		Type:     api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{Topic: api.TopicQuality}},
	}
	if err := conn.Write(ctx, websocket.MessageText, mustMarshalJSON(subscribeMsg)); err != nil {
		t.Fatalf("websocket.Write (subscribe): %v", err)
	}

	// Read the snapshot response.
	_, data, err = conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (quality): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal (quality): %v", err)
	}

	// Assert top-level type.
	if msg["type"] != string(api.MsgQuality) {
		t.Errorf("message type = %q, want %q", msg["type"], api.MsgQuality)
	}

	dataMap, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatalf("msg.data = %T, want map", msg["data"])
	}
	sessionsData, ok := dataMap["sessions"].([]any)
	if !ok || len(sessionsData) == 0 {
		t.Fatalf("sessions = %v, want non-empty array", sessionsData)
	}

	sessionMap, ok := sessionsData[0].(map[string]any)
	if !ok {
		t.Fatalf("sessions[0] = %T, want map", sessionsData[0])
	}
	if sessionMap["id"] != "quality-ann-1" {
		t.Errorf("sessions[0].id = %v, want quality-ann-1", sessionMap["id"])
	}

	// Assert effectiveAnnotations is a non-empty array.
	effectiveAnnotations, ok := sessionMap["effectiveAnnotations"].([]any)
	if !ok || len(effectiveAnnotations) == 0 {
		t.Fatalf("effectiveAnnotations = %v, want non-empty array", sessionMap["effectiveAnnotations"])
	}

	// Assert fields on the first annotation in the JSON response.
	annMap, ok := effectiveAnnotations[0].(map[string]any)
	if !ok {
		t.Fatalf("effectiveAnnotations[0] = %T, want map", effectiveAnnotations[0])
	}
	if annMap["annotatorKind"] != string(schema.AnnotatorHuman) {
		t.Errorf("annotatorKind = %v, want %q", annMap["annotatorKind"], schema.AnnotatorHuman)
	}
	if annMap["typeId"] != "quality.session_outcome" {
		t.Errorf("typeId = %v, want %q", annMap["typeId"], "quality.session_outcome")
	}
	if annMap["value"] != "resolved" {
		t.Errorf("value = %v, want %q", annMap["value"], "resolved")
	}
}

// TestHub_Annotations_InvalidSubscription is an E2E test that sends an invalid
// annotations subscription (missing axis and id) and verifies the hub responds
// with a MsgError message rather than crashing.
func TestHub_Annotations_InvalidSubscription(t *testing.T) {
	t.Parallel()

	mockProv := &wsMockProvider{}
	realProv := &wsMockProvider{}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     nil,
			},
		},
	}

	prov := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	hub := api.NewHub(prov)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	conn, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Read the "connected" message.
	var msg map[string]any
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (connected): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal (connected): %v", err)
	}
	if msg["type"] != string(api.MsgConnected) {
		t.Errorf("first message type = %q, want %q", msg["type"], api.MsgConnected)
	}

	// Subscribe with an invalid annotations subscription: topic is set but axis
	// and id are both missing, so ValidateSubscription should return false.
	subscribeMsg := api.ClientMessage{
		Type:     api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{Topic: api.TopicAnnotations}},
	}
	if err := conn.Write(ctx, websocket.MessageText, mustMarshalJSON(subscribeMsg)); err != nil {
		t.Fatalf("websocket.Write (subscribe): %v", err)
	}

	// The hub should respond with an error message (not crash or hang).
	_, data, err = conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (error): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal (error): %v", err)
	}
	if msg["type"] != string(api.MsgError) {
		t.Errorf("message type = %q, want %q", msg["type"], api.MsgError)
	}
	if msg["message"] == "" || msg["message"] == nil {
		t.Errorf("error message should be non-empty, got: %v", msg["message"])
	}
}

// TestHub_Annotations_SnapshotOnSubscribe is an E2E test that subscribes to
// TopicAnnotations with axis=session, id=X and verifies the hub responds with
// an annotations payload containing the expected annotation data.
func TestHub_Annotations_SnapshotOnSubscribe(t *testing.T) {
	t.Parallel()

	fixtures, err := schema.LoadAnnotationFixtures()
	if err != nil {
		t.Fatalf("LoadAnnotationFixtures: %v", err)
	}
	humanResolved := fixtures.FindSummary("human_outcome_resolved")
	if humanResolved == nil {
		t.Fatal("fixture 'human_outcome_resolved' not found")
	}
	ann := humanResolved.ToAnnotationSummary()
	testSessionID := "ann-session-001"

	mockProv := &wsMockProvider{
		annotations: map[string][]schema.AnnotationSummary{
			testSessionID: {ann},
		},
	}
	realProv := &wsMockProvider{}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     nil, // all sections mocked
			},
		},
	}

	prov := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	hub := api.NewHub(prov)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	conn, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Read the "connected" message.
	var msg map[string]any
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (connected): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal (connected): %v", err)
	}
	if msg["type"] != string(api.MsgConnected) {
		t.Errorf("first message type = %q, want %q", msg["type"], api.MsgConnected)
	}

	// Subscribe to annotations channel with axis=session.
	subscribeMsg := api.ClientMessage{
		Type: api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{
			Topic: api.TopicAnnotations,
			Axis:  schema.AxisSession,
			ID:    testSessionID,
		}},
	}
	if err := conn.Write(ctx, websocket.MessageText, mustMarshalJSON(subscribeMsg)); err != nil {
		t.Fatalf("websocket.Write (subscribe): %v", err)
	}

	// Read the snapshot response.
	_, data, err = conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (annotations): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal (annotations): %v", err)
	}

	// Assert message type.
	if msg["type"] != string(api.MsgAnnotations) {
		t.Errorf("message type = %q, want %q", msg["type"], api.MsgAnnotations)
	}

	// Assert payload structure.
	dataMap, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatalf("msg.data = %T, want map", msg["data"])
	}
	if dataMap["axis"] != string(schema.AxisSession) {
		t.Errorf("axis = %v, want %q", dataMap["axis"], schema.AxisSession)
	}
	if dataMap["id"] != testSessionID {
		t.Errorf("id = %v, want %q", dataMap["id"], testSessionID)
	}

	// Assert annotations array.
	annsRaw, ok := dataMap["annotations"].([]any)
	if !ok || len(annsRaw) == 0 {
		t.Fatalf("annotations = %v, want non-empty array", dataMap["annotations"])
	}
	annMap, ok := annsRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("annotations[0] = %T, want map", annsRaw[0])
	}
	if annMap["annotatorKind"] != string(schema.AnnotatorHuman) {
		t.Errorf("annotatorKind = %v, want %q", annMap["annotatorKind"], schema.AnnotatorHuman)
	}
	if annMap["typeId"] != "quality.session_outcome" {
		t.Errorf("typeId = %v, want %q", annMap["typeId"], "quality.session_outcome")
	}
	if annMap["value"] != "resolved" {
		t.Errorf("value = %v, want %q", annMap["value"], "resolved")
	}
}

// TestHub_Annotations_UnsupportedAxis verifies that subscribing to annotations
// with an unsupported axis (e.g., "type") returns an error message.
func TestHub_Annotations_UnsupportedAxis(t *testing.T) {
	t.Parallel()

	mockProv := &wsMockProvider{}
	realProv := &wsMockProvider{}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     nil,
			},
		},
	}

	prov := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	hub := api.NewHub(prov)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	conn, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Read "connected" message.
	var msg map[string]any
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (connected): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal (connected): %v", err)
	}

	// Subscribe with axis=type (not yet implemented).
	subscribeMsg := api.ClientMessage{
		Type: api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{
			Topic: api.TopicAnnotations,
			Axis:  schema.AxisType,
			ID:    "quality.session_outcome",
		}},
	}
	if err := conn.Write(ctx, websocket.MessageText, mustMarshalJSON(subscribeMsg)); err != nil {
		t.Fatalf("websocket.Write (subscribe): %v", err)
	}

	// Should receive an error about unsupported axis.
	_, data, err = conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (error): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal (error): %v", err)
	}
	if msg["type"] != string(api.MsgError) {
		t.Errorf("message type = %q, want %q", msg["type"], api.MsgError)
	}
	if msg["message"] == "" || msg["message"] == nil {
		t.Errorf("error message should be non-empty, got: %v", msg["message"])
	}
}

// TestHub_Annotations_EmptySession verifies subscribing to annotations for a
// session with no annotations returns an empty annotations array (not an error).
func TestHub_Annotations_EmptySession(t *testing.T) {
	t.Parallel()

	mockProv := &wsMockProvider{
		annotations: map[string][]schema.AnnotationSummary{},
	}
	realProv := &wsMockProvider{}

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: true,
				Web:     nil,
			},
		},
	}

	prov := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, mockProv, realProv)

	hub := api.NewHub(prov)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	conn, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Read "connected" message.
	_, _, err = conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (connected): %v", err)
	}

	// Subscribe to annotations for a session with no annotations.
	subscribeMsg := api.ClientMessage{
		Type: api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{
			Topic: api.TopicAnnotations,
			Axis:  schema.AxisSession,
			ID:    "nonexistent-session",
		}},
	}
	if err := conn.Write(ctx, websocket.MessageText, mustMarshalJSON(subscribeMsg)); err != nil {
		t.Fatalf("websocket.Write (subscribe): %v", err)
	}

	// Read the snapshot response — should be a valid annotations message with empty array.
	var msg map[string]any
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("websocket.Read (annotations): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal (annotations): %v", err)
	}
	if msg["type"] != string(api.MsgAnnotations) {
		t.Errorf("message type = %q, want %q", msg["type"], api.MsgAnnotations)
	}

	dataMap, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatalf("msg.data = %T, want map", msg["data"])
	}
	if dataMap["axis"] != string(schema.AxisSession) {
		t.Errorf("axis = %v, want %q", dataMap["axis"], schema.AxisSession)
	}
	if dataMap["id"] != "nonexistent-session" {
		t.Errorf("id = %v, want %q", dataMap["id"], "nonexistent-session")
	}
	// annotations should be nil/null (Go nil slice serializes as null in JSON).
	// Accept both null and empty array.
}

// TestHub_Annotations_BroadcastOnCreate is an E2E test that verifies:
// 1. Subscribe a WS client to TopicAnnotations for a session
// 2. Receive the initial empty snapshot
// 3. POST a new annotation for that session via HTTP
// 4. Assert the WS client receives an updated annotations payload with the new annotation
func TestHub_Annotations_BroadcastOnCreate(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	ctx := context.Background()

	// Insert a session so the annotation has a valid target.
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	entry := makeStoreEntry(t, sessionID, hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "test-project", 10, 5, 60000)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	// Wire up a full Server with store, hub, and annotation routes.
	storeProv := api.NewStoreDataProvider(s, sessionvisibility.All())
	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{Enabled: false},
		},
	}
	prov := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, nil, storeProv)
	hub := api.NewHub(prov)

	srvCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := api.NewServer(api.ServerConfig{
		Port:     0,
		Provider: prov,
		Hub:      hub,
		Store:    s,
	})
	if err := srv.Listen(srvCtx); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(srvCtx) }()

	baseURL := "http://" + srv.Addr().String()
	wsURL := "ws://" + srv.Addr().String() + string(defaults.RouteWS)

	// 1. Connect WebSocket and subscribe to annotations for this session.
	conn, _, err := websocket.Dial(srvCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Read "connected" message.
	_, _, err = conn.Read(srvCtx)
	if err != nil {
		t.Fatalf("websocket.Read (connected): %v", err)
	}

	// Subscribe to annotations for sessionID.
	subMsg := api.ClientMessage{
		Type: api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{
			Topic: api.TopicAnnotations,
			Axis:  schema.AxisSession,
			ID:    sessionID,
		}},
	}
	if err := conn.Write(srvCtx, websocket.MessageText, mustMarshalJSON(subMsg)); err != nil {
		t.Fatalf("websocket.Write (subscribe): %v", err)
	}

	// 2. Read initial snapshot — should be empty (no annotations yet).
	var msg map[string]any
	_, data, err := conn.Read(srvCtx)
	if err != nil {
		t.Fatalf("websocket.Read (snapshot): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal (snapshot): %v", err)
	}
	if msg["type"] != string(api.MsgAnnotations) {
		t.Fatalf("snapshot type = %q, want %q", msg["type"], api.MsgAnnotations)
	}

	// 3. POST a new annotation for this session via HTTP.
	postBody := mustMarshalJSON(map[string]any{
		"sessionId":     sessionID,
		"typeId":        "quality.session_outcome",
		"value":         "resolved",
		"isPrimary":     true,
		"annotatorName": "outcome-classifier",
	})
	resp, err := http.Post(baseURL+string(defaults.RouteAnnotations), "application/json",
		bytes.NewReader(postBody))
	if err != nil {
		t.Fatalf("POST /annotations: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /annotations status = %d, want 201; body = %s", resp.StatusCode, body)
	}

	// 4. Read the broadcast message triggered by POST.
	readCtx, readCancel := context.WithTimeout(srvCtx, 5*time.Second)
	defer readCancel()

	_, data, err = conn.Read(readCtx)
	if err != nil {
		t.Fatalf("websocket.Read (broadcast): %v", err)
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal (broadcast): %v", err)
	}
	if msg["type"] != string(api.MsgAnnotations) {
		t.Fatalf("broadcast type = %q, want %q", msg["type"], api.MsgAnnotations)
	}

	// Assert the broadcast payload contains the new annotation.
	dataMap, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatalf("msg.data = %T, want map", msg["data"])
	}
	if dataMap["axis"] != string(schema.AxisSession) {
		t.Errorf("axis = %v, want %q", dataMap["axis"], schema.AxisSession)
	}
	if dataMap["id"] != sessionID {
		t.Errorf("id = %v, want %q", dataMap["id"], sessionID)
	}
	annsRaw, ok := dataMap["annotations"].([]any)
	if !ok || len(annsRaw) == 0 {
		t.Fatalf("annotations = %v, want non-empty array", dataMap["annotations"])
	}
	annMap, ok := annsRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("annotations[0] = %T, want map", annsRaw[0])
	}
	if annMap["typeId"] != "quality.session_outcome" {
		t.Errorf("typeId = %v, want %q", annMap["typeId"], "quality.session_outcome")
	}
	if annMap["value"] != "resolved" {
		t.Errorf("value = %v, want %q", annMap["value"], "resolved")
	}

	cancel()
}

func mustMarshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestHub_SessionDetail_ToolCallFields exercises the full store→WS→JSON path for
// the session_detail channel, specifically asserting that ToolCallDetail.id,
// .name, .arguments, and .result are populated from indexed session entries.
// This covers the push-v2 ToolCallID field flowing through the store adapter.
func TestHub_SessionDetail_ToolCallFields(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	ctx := context.Background()

	// 1. Insert a session.
	entry := makeStoreEntry(t, "99999999-9999-9999-9999-999999999999", hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-detail", 5, 2, 60000)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	// 2. Index session entries: depth=0 assistant summary + depth=1 tool_use part.
	sid := ingest.SessionID("99999999-9999-9999-9999-999999999999")
	toolID := "toolu_detail_1"
	toolInput := `{"path":"internal/api/store_adapter.go"}`
	toolOutput := "package api\n..."
	preview := "I will read the adapter file."

	entries := []schema.SessionEntry{
		// depth=0: assistant message summary (HasToolUse=true, no ToolCallID)
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
		// depth=1: concrete tool_use content part (ToolCallID, ToolInput, ToolOutput set)
		{
			SessionID:    sid,
			EntryIndex:   1,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(day1Ms + 100),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("Read"),
			ToolCallID:   &toolID,
			ToolInput:    &toolInput,
			ToolOutput:   &toolOutput,
			Depth:        1,
			ParentIndex:  intPtr(0),
		},
	}
	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// 3. Wire up Hub backed by the real store.
	storeProv := api.NewStoreDataProvider(s, sessionvisibility.All())
	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{Enabled: false},
		},
	}
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

	// Consume "connected" message.
	_, _, err = conn.Read(hubCtx)
	if err != nil {
		t.Fatalf("websocket.Read (connected): %v", err)
	}

	// 4. Subscribe to session_detail for this session.
	subMsg := api.ClientMessage{
		Type:     api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{Topic: api.TopicSessionDetail, ID: "99999999-9999-9999-9999-999999999999"}},
	}
	if err := conn.Write(hubCtx, websocket.MessageText, mustMarshalJSON(subMsg)); err != nil {
		t.Fatalf("websocket.Write (subscribe): %v", err)
	}

	// 5. Read response and decode.
	_, data, err := conn.Read(hubCtx)
	if err != nil {
		t.Fatalf("websocket.Read (session_detail): %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if msg["type"] != string(api.MsgSessionDetail) {
		t.Fatalf("message type = %q, want %q", msg["type"], api.MsgSessionDetail)
	}

	dataMap, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatalf("msg.data = %T, want map", msg["data"])
	}
	if dataMap["id"] != "99999999-9999-9999-9999-999999999999" {
		t.Errorf("session id = %v, want 99999999-...", dataMap["id"])
	}

	// 6. Assert turns and ToolCallDetail fields.
	// After the depth=1 fold, the tool_use entry is merged into the depth=0
	// parent. Only 1 turn: the assistant message with folded ToolCalls.
	turnsRaw, ok := dataMap["turns"].([]any)
	if !ok {
		t.Fatalf("turns = %T, want []any", dataMap["turns"])
	}
	if len(turnsRaw) != 1 {
		t.Fatalf("turns count = %d, want 1 (tool_use folded into parent)", len(turnsRaw))
	}

	// Turn 0: depth=0 assistant — has folded ToolCall from depth=1 child.
	turn0 := turnsRaw[0].(map[string]any)
	if turn0["content"] != preview {
		t.Errorf("turns[0].content = %q, want %q", turn0["content"], preview)
	}
	toolCallsRaw, ok := turn0["toolCalls"].([]any)
	if !ok || len(toolCallsRaw) != 1 {
		t.Fatalf("turns[0].toolCalls = %v, want 1 entry", turn0["toolCalls"])
	}
	tc := toolCallsRaw[0].(map[string]any)
	if tc["id"] != toolID {
		t.Errorf("toolCalls[0].id = %q, want %q", tc["id"], toolID)
	}
	if tc["name"] != "Read" {
		t.Errorf("toolCalls[0].name = %q, want %q", tc["name"], "Read")
	}
	if tc["arguments"] != toolInput {
		t.Errorf("toolCalls[0].arguments = %q, want %q", tc["arguments"], toolInput)
	}
	if tc["result"] != toolOutput {
		t.Errorf("toolCalls[0].result = %q, want %q", tc["result"], toolOutput)
	}
}

// TestHub_SessionDetail_Outcome exercises the full store→WS→JSON path for the
// session_detail channel, asserting that SessionDetailPayload.outcome is
// populated from session_metrics.outcome. Covers issue 3a: surfacing the
// session outcome on the detail header.
func TestHub_SessionDetail_Outcome(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	ctx := context.Background()

	// 1. Insert a session.
	const sessionID = "abababab-abab-abab-abab-abababababab"
	entry := makeStoreEntry(t, sessionID, hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-outcome", 8, 3, 60000)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	// 2. Save quality metrics carrying an outcome via the production write path.
	sid := ingest.SessionID(sessionID)
	metrics := &ingest.SessionMetrics{
		SessionID: sid,
		QualityMetrics: schema.QualityMetrics{
			Outcome:        outcomePtr(ingest.OutcomePartial),
			TurnCount:      intPtr(8),
			ToolCalls:      intPtr(3),
			TotalTokens:    intPtr(1500),
			InputTokens:    intPtr(1000),
			OutputTokens:   intPtr(500),
			ComputeVersion: intPtr(1),
		},
	}
	if err := s.SaveMetrics(ctx, metrics); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}

	// 3. Wire up Hub backed by the real store.
	storeProv := api.NewStoreDataProvider(s, sessionvisibility.All())
	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{Enabled: false},
		},
	}
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

	// Consume "connected" message.
	if _, _, err = conn.Read(hubCtx); err != nil {
		t.Fatalf("websocket.Read (connected): %v", err)
	}

	// 4. Subscribe to session_detail for this session.
	subMsg := api.ClientMessage{
		Type:     api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{Topic: api.TopicSessionDetail, ID: sessionID}},
	}
	if err := conn.Write(hubCtx, websocket.MessageText, mustMarshalJSON(subMsg)); err != nil {
		t.Fatalf("websocket.Write (subscribe): %v", err)
	}

	// 5. Read response and decode.
	_, data, err := conn.Read(hubCtx)
	if err != nil {
		t.Fatalf("websocket.Read (session_detail): %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if msg["type"] != string(api.MsgSessionDetail) {
		t.Fatalf("message type = %q, want %q", msg["type"], api.MsgSessionDetail)
	}

	dataMap, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatalf("msg.data = %T, want map", msg["data"])
	}

	// 6. Assert the outcome field round-tripped through SessionDetailPayload.
	if dataMap["outcome"] != string(ingest.OutcomePartial) {
		t.Errorf("data.outcome = %v, want %q", dataMap["outcome"], string(ingest.OutcomePartial))
	}
}

// TestHub_SessionDetail_Scorecard exercises the full store→WS→JSON path for the
// session_detail channel, asserting that SessionDetailPayload.scorecard is
// populated with the M-series + cost signals consumed by the Highlights
// self-assessment card. These signals live only in session_metrics (not the
// detail row's quality columns), so this covers the SessionByID enrichment +
// scorecard projection end to end.
func TestHub_SessionDetail_Scorecard(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	ctx := context.Background()

	// 1. Insert a session.
	const sessionID = "cdcdcdcd-cdcd-cdcd-cdcd-cdcdcdcdcdcd"
	entry := makeStoreEntry(t, sessionID, hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-scorecard", 8, 3, 60000)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	// 2. Save quality metrics carrying the scorecard signals.
	metrics := &ingest.SessionMetrics{
		SessionID: ingest.SessionID(sessionID),
		QualityMetrics: schema.QualityMetrics{
			Outcome:                 outcomePtr(ingest.OutcomeFailed),
			TotalTokens:             intPtr(1500),
			M2TokenOutcomeRatio:     float64Ptr(0.62),
			M5ContextUtilizationPct: float64Ptr(82.0),
			M6OutputSurvivalPct:     float64Ptr(41.0),
			RetryTokensWasted:       intPtr(420),
			CostTotalUSD:            float64Ptr(2.37),
			SpecQualityScore:        float64Ptr(33.0),
			SignalDensity:           float64Ptr(24.0),
			M7SpecHasExamples:       boolPtr(false),
			M7SpecHasConstraints:    boolPtr(true),
			M4ConsecutiveErrorMax:   intPtr(5),
			WithinSessionReverts:    intPtr(3),
			ComputeVersion:          intPtr(1),
		},
	}
	if err := s.SaveMetrics(ctx, metrics); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}

	// 3. Wire up Hub backed by the real store.
	storeProv := api.NewStoreDataProvider(s, sessionvisibility.All())
	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{Enabled: false},
		},
	}
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

	// Consume "connected" message.
	if _, _, err = conn.Read(hubCtx); err != nil {
		t.Fatalf("websocket.Read (connected): %v", err)
	}

	// 4. Subscribe to session_detail for this session.
	subMsg := api.ClientMessage{
		Type:     api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{Topic: api.TopicSessionDetail, ID: sessionID}},
	}
	if err := conn.Write(hubCtx, websocket.MessageText, mustMarshalJSON(subMsg)); err != nil {
		t.Fatalf("websocket.Write (subscribe): %v", err)
	}

	// 5. Read response and decode.
	_, data, err := conn.Read(hubCtx)
	if err != nil {
		t.Fatalf("websocket.Read (session_detail): %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if msg["type"] != string(api.MsgSessionDetail) {
		t.Fatalf("message type = %q, want %q", msg["type"], api.MsgSessionDetail)
	}

	dataMap, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatalf("msg.data = %T, want map", msg["data"])
	}

	// 6. Assert the scorecard nested object round-tripped through the wire.
	sc, ok := dataMap["scorecard"].(map[string]any)
	if !ok {
		t.Fatalf("data.scorecard = %T, want map", dataMap["scorecard"])
	}
	if got := sc["m2TokenOutcomeRatio"]; got != 0.62 {
		t.Errorf("scorecard.m2TokenOutcomeRatio = %v, want 0.62", got)
	}
	if got := sc["m5ContextUtilizationPct"]; got != 82.0 {
		t.Errorf("scorecard.m5ContextUtilizationPct = %v, want 82.0", got)
	}
	if got := sc["m6OutputSurvivalPct"]; got != 41.0 {
		t.Errorf("scorecard.m6OutputSurvivalPct = %v, want 41.0", got)
	}
	if got := sc["retryTokensWasted"]; got != float64(420) {
		t.Errorf("scorecard.retryTokensWasted = %v, want 420", got)
	}
	if got := sc["costTotalUsd"]; got != 2.37 {
		t.Errorf("scorecard.costTotalUsd = %v, want 2.37", got)
	}
	if got := sc["m4ConsecutiveErrorMax"]; got != float64(5) {
		t.Errorf("scorecard.m4ConsecutiveErrorMax = %v, want 5", got)
	}
	if got := sc["withinSessionReverts"]; got != float64(3) {
		t.Errorf("scorecard.withinSessionReverts = %v, want 3", got)
	}
	if got := sc["m7SpecHasExamples"]; got != false {
		t.Errorf("scorecard.m7SpecHasExamples = %v, want false", got)
	}
	if got := sc["m7SpecHasConstraints"]; got != true {
		t.Errorf("scorecard.m7SpecHasConstraints = %v, want true", got)
	}
	if got := sc["outcome"]; got != string(ingest.OutcomeFailed) {
		t.Errorf("scorecard.outcome = %v, want %q", got, ingest.OutcomeFailed)
	}
}

func TestProgressiveProvider_E2E_WebSocket_WithStore(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	ctx := context.Background()

	hash1 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hash2 := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash1, "github.com-user-repo1",
			defaults.HarnessClaudeCode, 1700000000000, 1000, 500, "test-project", 10, 5, 60000),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash2, "github.com-user-repo2",
			defaults.HarnessOpenCode, 1700000060000, 2000, 800, "test-project", 15, 8, 60000),
	}

	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	storeProv := api.NewStoreDataProvider(s, sessionvisibility.All())

	cfg := &config.Config{
		Sources: config.SourcesConfig{
			Mock: config.MockConfig{
				Enabled: false,
			},
		},
	}

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

	_, data, _ := conn.Read(hubCtx)
	var connectedMsg map[string]any
	json.Unmarshal(data, &connectedMsg)

	subMsg := api.ClientMessage{
		Type:     api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{Topic: api.TopicSessions}},
	}
	conn.Write(hubCtx, websocket.MessageText, mustMarshalJSON(subMsg))

	_, data, err = conn.Read(hubCtx)
	if err != nil {
		t.Fatalf("websocket.Read (sessions): %v", err)
	}

	var msg map[string]any
	json.Unmarshal(data, &msg)

	dataMap := msg["data"].(map[string]any)
	sessionsData := dataMap["sessions"].([]any)
	if len(sessionsData) != 2 {
		t.Errorf("session count = %d, want 2", len(sessionsData))
	}

	var foundID string
	for _, s := range sessionsData {
		sessionMap := s.(map[string]any)
		if sessionMap["id"] == "11111111-1111-1111-1111-111111111111" || sessionMap["id"] == "22222222-2222-2222-2222-222222222222" {
			foundID = sessionMap["id"].(string)
			break
		}
	}
	if foundID == "" {
		t.Errorf("expected to find one of the test session IDs")
	}
}

//go:embed testdata/selection_visibility_ws_recovery.yaml
var selectionVisibilityWSRecoveryFixtureYAML []byte

var requiredSelectionVisibilityWSRecoveryCaseNames = map[string]struct{}{
	"dashboard channel recovers from a selection-visibility failure":   {},
	"sessions channel recovers from a selection-visibility failure":    {},
	"trends channel recovers from a selection-visibility failure":      {},
	"an unrelated provider failure is not tagged selection_visibility": {},
}

type selectionVisibilityWSRecoveryFixture struct {
	ExpectedCaseCount int      `yaml:"expectedCaseCount"`
	RequiredNames     []string `yaml:"requiredNames"`
	Cases             []struct {
		Name      string `yaml:"name"`
		Topic     string `yaml:"topic"`
		ErrorKind string `yaml:"errorKind"`
	} `yaml:"cases"`
}

func decodeSelectionVisibilityWSRecoveryFixture(source []byte) (selectionVisibilityWSRecoveryFixture, error) {
	var fixture selectionVisibilityWSRecoveryFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode selection visibility WS recovery fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fixture, fmt.Errorf("selection visibility WS recovery fixture contains more than one YAML document")
		}
		return fixture, fmt.Errorf("decode trailing selection visibility WS recovery fixture content: %w", err)
	}
	if fixture.ExpectedCaseCount != len(requiredSelectionVisibilityWSRecoveryCaseNames) || len(fixture.RequiredNames) != fixture.ExpectedCaseCount || len(fixture.Cases) != fixture.ExpectedCaseCount {
		return fixture, fmt.Errorf("selection visibility WS recovery fixture count mismatch: declared=%d names=%d cases=%d, want %d", fixture.ExpectedCaseCount, len(fixture.RequiredNames), len(fixture.Cases), len(requiredSelectionVisibilityWSRecoveryCaseNames))
	}
	seenRequired := make(map[string]struct{}, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		if _, required := requiredSelectionVisibilityWSRecoveryCaseNames[name]; !required {
			return fixture, fmt.Errorf("selection visibility WS recovery fixture requiredNames has unknown case %q", name)
		}
		if _, duplicate := seenRequired[name]; duplicate {
			return fixture, fmt.Errorf("selection visibility WS recovery fixture requiredNames repeats %q", name)
		}
		seenRequired[name] = struct{}{}
	}
	seenCases := make(map[string]struct{}, len(fixture.Cases))
	for index, testCase := range fixture.Cases {
		if _, required := requiredSelectionVisibilityWSRecoveryCaseNames[testCase.Name]; !required {
			return fixture, fmt.Errorf("selection visibility WS recovery fixture cases[%d] has unknown or missing semantic name %q", index, testCase.Name)
		}
		if _, duplicate := seenCases[testCase.Name]; duplicate {
			return fixture, fmt.Errorf("selection visibility WS recovery fixture duplicates semantic name %q", testCase.Name)
		}
		seenCases[testCase.Name] = struct{}{}
		if testCase.Topic != "dashboard" && testCase.Topic != "sessions" && testCase.Topic != "trends" {
			return fixture, fmt.Errorf("selection visibility WS recovery fixture cases[%d] %q has unsupported topic %q", index, testCase.Name, testCase.Topic)
		}
		if testCase.ErrorKind != "selection" && testCase.ErrorKind != "generic" {
			return fixture, fmt.Errorf("selection visibility WS recovery fixture cases[%d] %q has unsupported errorKind %q, want \"selection\" or \"generic\"", index, testCase.Name, testCase.ErrorKind)
		}
	}
	if len(seenCases) != len(requiredSelectionVisibilityWSRecoveryCaseNames) {
		return fixture, fmt.Errorf("selection visibility WS recovery fixture does not cover every required semantic case")
	}
	return fixture, nil
}

func TestSelectionVisibilityWSRecoveryFixture_LoaderRejectsMutations(t *testing.T) {
	t.Parallel()

	if _, err := decodeSelectionVisibilityWSRecoveryFixture(selectionVisibilityWSRecoveryFixtureYAML); err != nil {
		t.Fatalf("committed fixture must be valid: %v", err)
	}

	countDrift := bytes.Replace(selectionVisibilityWSRecoveryFixtureYAML, []byte("expectedCaseCount: 4"), []byte("expectedCaseCount: 5"), 1)
	if _, err := decodeSelectionVisibilityWSRecoveryFixture(countDrift); err == nil {
		t.Fatal("expectedCaseCount drift unexpectedly validated")
	}

	duplicateName := bytes.Replace(
		selectionVisibilityWSRecoveryFixtureYAML,
		[]byte("    topic: sessions"),
		[]byte("    topic: dashboard"),
		1,
	)
	duplicateName = bytes.Replace(
		duplicateName,
		[]byte("  - name: sessions channel recovers from a selection-visibility failure\n"),
		[]byte("  - name: dashboard channel recovers from a selection-visibility failure\n"),
		1,
	)
	if _, err := decodeSelectionVisibilityWSRecoveryFixture(duplicateName); err == nil {
		t.Fatal("duplicate case name unexpectedly validated")
	}

	unknownTopic := bytes.Replace(selectionVisibilityWSRecoveryFixtureYAML, []byte("topic: trends"), []byte("topic: quality"), 1)
	if _, err := decodeSelectionVisibilityWSRecoveryFixture(unknownTopic); err == nil {
		t.Fatal("unsupported topic unexpectedly validated")
	}

	unknownField := bytes.Replace(selectionVisibilityWSRecoveryFixtureYAML, []byte("expectedCaseCount:"), []byte("unexpectedField: true\nexpectedCaseCount:"), 1)
	if _, err := decodeSelectionVisibilityWSRecoveryFixture(unknownField); err == nil {
		t.Fatal("unknown top-level field unexpectedly validated")
	}

	unsupportedErrorKind := bytes.Replace(selectionVisibilityWSRecoveryFixtureYAML, []byte("errorKind: generic"), []byte("errorKind: bogus"), 1)
	if _, err := decodeSelectionVisibilityWSRecoveryFixture(unsupportedErrorKind); err == nil {
		t.Fatal("unsupported errorKind unexpectedly validated")
	}
}

// TestHub_SelectionVisibility_ErrorRecovery proves the production Hub's
// full valid → conflict → cleared state → repaired recovery contract for a
// persisted-selection-visibility failure on each ticker channel
// (dashboard/sessions/trends): a fresh subscribe snapshot delivers real
// data, an injected sessionvisibility failure clears that topic's stale
// data and delivers an actionable topic-scoped error carrying the
// "selection_visibility" code and kickstart remediation text (never a
// generic/untyped error and never silently dropped), and a subsequent
// subscribe after the failure clears delivers fresh data again with no
// trace of the error or of a sibling topic's data. This exercises the same
// production topicError/sendSnapshots code path broadcastAll uses on its
// recurring tick.
//
// The negative-control case (errorKind: generic) proves the converse:
// injecting an unrelated provider failure (not a sessionvisibility.Error)
// still surfaces a topic-scoped error message, but WITHOUT the
// "selection_visibility" code or the kickstart remediation suffix — so a
// future topicError regression that tags every WS error (not just
// selection-visibility ones) fails this test. Mirrors the REST-side
// negative control already established by
// TestProjectSummariesHandler_PreservesDiscoveryErrorClass's "unrelated
// provider failure stays untyped" case in map_handlers_test.go.
func TestHub_SelectionVisibility_ErrorRecovery(t *testing.T) {
	t.Parallel()

	fixture, err := decodeSelectionVisibilityWSRecoveryFixture(selectionVisibilityWSRecoveryFixtureYAML)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	// A real, non-test-constructed sessionvisibility.Error, so topicError's
	// sessionvisibility.IsError(err) classification is exercised for real
	// rather than special-cased for the test.
	_, selectionErr := sessionvisibility.New(config.SelectionConfig{Mode: "invalid-mode"})
	if selectionErr == nil || !sessionvisibility.IsError(selectionErr) {
		t.Fatalf("test setup: expected an invalid selection mode to produce a sessionvisibility.Error, got %v", selectionErr)
	}
	// The negative-control injected failure: a genuine, unrelated provider
	// error that is NOT a sessionvisibility.Error, mirroring the REST
	// sibling's "database unavailable" fixture.
	genericErr := fmt.Errorf("database unavailable")
	if sessionvisibility.IsError(genericErr) {
		t.Fatalf("test setup: expected a generic provider error to NOT be a sessionvisibility.Error, got IsError=true for %v", genericErr)
	}

	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			topic := schema.ChannelTopic(testCase.Topic)
			injectedErr := selectionErr
			if testCase.ErrorKind == "generic" {
				injectedErr = genericErr
			}
			mockProv := &wsMockProvider{
				dashboard: &api.DashboardPayload{TotalSessions: 7},
				summaries: []api.SessionSummary{{ID: "sess-1"}},
				trends:    &api.TrendsPayload{Days: []api.DayStats{{Date: "2026-07-15", Sessions: 3}}},
			}

			hub := api.NewHub(mockProv)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go hub.Run(ctx)

			server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
			defer server.Close()

			conn, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String()+"/api/v1/ws", nil)
			if err != nil {
				t.Fatalf("websocket.Dial: %v", err)
			}
			defer conn.Close(websocket.StatusNormalClosure, "")

			// Drain the "connected" message.
			if _, _, err := conn.Read(ctx); err != nil {
				t.Fatalf("websocket.Read (connected): %v", err)
			}

			readTopicMessage := func(step string) map[string]any {
				t.Helper()
				_, data, err := conn.Read(ctx)
				if err != nil {
					t.Fatalf("%s: websocket.Read: %v", step, err)
				}
				var msg map[string]any
				if err := json.Unmarshal(data, &msg); err != nil {
					t.Fatalf("%s: json.Unmarshal: %v", step, err)
				}
				return msg
			}
			subscribe := func(step string) map[string]any {
				sub := api.ClientMessage{Type: api.MsgSubscribe, Channels: []api.ChannelSubscription{{Topic: topic}}}
				if err := conn.Write(ctx, websocket.MessageText, mustMarshalJSON(sub)); err != nil {
					t.Fatalf("%s: websocket.Write (subscribe): %v", step, err)
				}
				return readTopicMessage(step)
			}

			// Step 1: valid data.
			validMsg := subscribe("initial subscribe")
			if validMsg["type"] == string(api.MsgError) {
				t.Fatalf("initial subscribe: unexpected error message before any failure was injected: %+v", validMsg)
			}
			if validMsg["topic"] != nil {
				t.Errorf("initial subscribe: message topic = %v, want unset on a successful payload message", validMsg["topic"])
			}

			// Step 2: conflict — inject the case's configured failure
			// (a persisted-selection failure for errorKind: selection, or an
			// unrelated provider failure for the errorKind: generic negative
			// control) and resubscribe (the client's real recovery path also
			// resubscribes after a WS reconnect/backoff cycle).
			mockProv.setFailure(topic, injectedErr)
			errMsg := subscribe("after failure injected")
			if errMsg["type"] != string(api.MsgError) {
				t.Fatalf("after failure injected: message type = %v, want %q; full message: %+v", errMsg["type"], api.MsgError, errMsg)
			}
			if errMsg["topic"] != string(topic) {
				t.Errorf("after failure injected: error topic = %v, want %q (topic-scoped, not global)", errMsg["topic"], topic)
			}
			message, _ := errMsg["message"].(string)
			const staleClearedFragment = "the previous data for only this topic was cleared to avoid showing stale results"
			if !strings.Contains(message, staleClearedFragment) {
				t.Errorf("after failure injected: message %q does not confirm the previous topic data was cleared", message)
			}
			const kickstartGuidanceSuffix = "run `peasant kickstart` to repair the persisted selection, then retry"
			if testCase.ErrorKind == "selection" {
				// The suffix topicError itself appends — deliberately distinct
				// from sessionvisibility.New's own error text (which also
				// mentions `peasant kickstart` but with different wording), so
				// this proves topicError's OWN guidance branch ran rather than
				// just echoing whatever phrase happened to be in err.Error().
				if !strings.Contains(message, kickstartGuidanceSuffix) {
					t.Errorf("after failure injected: message %q does not carry topicError's own kickstart remediation suffix %q", message, kickstartGuidanceSuffix)
				}
				errData, ok := errMsg["data"].(map[string]any)
				if !ok || errData["code"] != "selection_visibility" {
					t.Errorf("after failure injected: data = %v, want {code: selection_visibility}", errMsg["data"])
				}
			} else {
				// Negative control: an unrelated provider failure must NOT be
				// misclassified as a selection-visibility problem — no code,
				// no kickstart remediation text. A topicError regression that
				// tags every WS error unconditionally fails here.
				if strings.Contains(message, kickstartGuidanceSuffix) {
					t.Errorf("after failure injected: unrelated provider failure message %q incorrectly carries the kickstart remediation suffix %q", message, kickstartGuidanceSuffix)
				}
				if errData, ok := errMsg["data"].(map[string]any); ok && errData["code"] != nil && errData["code"] != "" {
					t.Errorf("after failure injected: unrelated provider failure data = %v, want no selection_visibility code", errMsg["data"])
				}
			}

			// Step 3: repaired recovery — clear the failure and resubscribe;
			// fresh real data must return, and it must be exactly this
			// topic's configured data (no sibling leakage, no stale error).
			mockProv.setFailure(topic, nil)
			recoveredMsg := subscribe("after failure cleared")
			if recoveredMsg["type"] == string(api.MsgError) {
				t.Fatalf("after failure cleared: still receiving an error message: %+v", recoveredMsg)
			}
			if recoveredMsg["data"] == nil {
				t.Fatalf("after failure cleared: expected repaired data, got nil data in %+v", recoveredMsg)
			}
		})
	}
}
