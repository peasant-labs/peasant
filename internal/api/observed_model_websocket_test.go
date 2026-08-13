package api_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/observed_model_websocket.yaml
var observedModelWebSocketFixtureYAML []byte

type observedModelWebSocketTurn struct {
	Name          string `yaml:"name"`
	Index         int    `yaml:"index"`
	Role          string `yaml:"role"`
	Depth         int    `yaml:"depth"`
	Content       string `yaml:"content"`
	ObservedModel string `yaml:"observedModel"`
}

type observedModelWebSocketFixture struct {
	SessionID              string                       `yaml:"sessionId"`
	StoredModel            string                       `yaml:"storedModel"`
	Turns                  []observedModelWebSocketTurn `yaml:"turns"`
	ExpectedSeed           string                       `yaml:"expectedSeed"`
	InvalidRole            string                       `yaml:"invalidRole"`
	InvalidObservedModel   string                       `yaml:"invalidObservedModel"`
	ExpectedObservedModels []string                     `yaml:"expectedObservedModels"`
	ExpectedCaseCount      int                          `yaml:"expectedCaseCount"`
	RequiredNames          []string                     `yaml:"requiredNames"`
}

func loadObservedModelWebSocketFixture(t *testing.T) observedModelWebSocketFixture {
	t.Helper()
	var fixture observedModelWebSocketFixture
	if err := yaml.Unmarshal(observedModelWebSocketFixtureYAML, &fixture); err != nil {
		t.Fatalf("decode observed model websocket fixture: %v", err)
	}
	if fixture.SessionID == "" || fixture.StoredModel == "" || fixture.ExpectedSeed == "" || fixture.InvalidRole == "" || fixture.InvalidObservedModel == "" || fixture.ExpectedCaseCount != 2 || len(fixture.Turns) != fixture.ExpectedCaseCount || len(fixture.RequiredNames) != fixture.ExpectedCaseCount || len(fixture.ExpectedObservedModels) != len(fixture.Turns) {
		t.Fatalf("observed model websocket fixture inventory is incomplete: %+v", fixture)
	}
	seen := map[string]bool{}
	for _, turn := range fixture.Turns {
		if turn.Name == "" || seen[turn.Name] {
			t.Fatalf("observed model websocket fixture has empty or duplicate name %q", turn.Name)
		}
		seen[turn.Name] = true
	}
	for _, required := range fixture.RequiredNames {
		if !seen[required] {
			t.Fatalf("observed model websocket fixture is missing required name %q", required)
		}
	}
	return fixture
}

func TestHubSessionDetailRejectsPersistedNonAssistantEvidence(t *testing.T) {
	fixture := loadObservedModelWebSocketFixture(t)
	db := openTestStore(t)
	stored := makeStoreEntry(t, fixture.SessionID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "fixture-host", defaults.HarnessClaudeCode, 1700000000000, 1, 1, "fixture-project", 1, 0, 1000)
	if err := db.InsertSessions(context.Background(), []ingest.StoreEntry{stored}); err != nil {
		t.Fatal(err)
	}
	extraBytes, _ := json.Marshal(map[string]string{"model_id": fixture.InvalidObservedModel})
	extra, content := string(extraBytes), "invalid attribution"
	entry := schema.SessionEntry{SessionID: schema.SessionID(fixture.SessionID), EntryIndex: 1, Role: schema.Role(fixture.InvalidRole), Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, ContentPreview: &content, Extra: &extra}
	if err := db.IndexSessionEntries(context.Background(), ingest.SessionID(fixture.SessionID), []schema.SessionEntry{entry}); err != nil {
		t.Fatal(err)
	}
	hub := api.NewHub(api.NewStoreDataProvider(db, sessionvisibility.All()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	connection, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "")
	_, _, _ = connection.Read(ctx)
	subscription, _ := json.Marshal(api.ClientMessage{Type: api.MsgSubscribe, Channels: []api.ChannelSubscription{{Topic: api.TopicSessionDetail, ID: fixture.SessionID}}})
	if err := connection.Write(ctx, websocket.MessageText, subscription); err != nil {
		t.Fatal(err)
	}
	_, data, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var message struct {
		Type    api.MessageType `json:"type"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	if message.Type != api.MsgError {
		t.Fatalf("message type=%q, want %q; body=%s", message.Type, api.MsgError, data)
	}
	for _, fragment := range []string{"observed model evidence is invalid", "what:", "why:", "where:", "when:", "meaning:", "fix:"} {
		if !strings.Contains(message.Message, fragment) {
			t.Errorf("mounted error missing %q: %s", fragment, message.Message)
		}
	}
}

func TestHubSessionDetailEmitsObservedModelEvidence(t *testing.T) {
	fixture := loadObservedModelWebSocketFixture(t)
	db := openTestStore(t)
	stored := makeStoreEntry(t, fixture.SessionID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "fixture-host", defaults.HarnessClaudeCode, 1700000000000, 1, 1, "fixture-project", len(fixture.Turns), 0, 1000)
	model, err := ingest.NewModelID(fixture.StoredModel)
	if err != nil {
		t.Fatalf("stored model: %v", err)
	}
	stored.Metadata.Model = model
	if err := db.InsertSessions(context.Background(), []ingest.StoreEntry{stored}); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	entries := make([]schema.SessionEntry, 0, len(fixture.Turns))
	for _, source := range fixture.Turns {
		extraBytes, _ := json.Marshal(map[string]string{"model_id": source.ObservedModel})
		extra := string(extraBytes)
		content := source.Content
		entries = append(entries, schema.SessionEntry{SessionID: schema.SessionID(fixture.SessionID), EntryIndex: source.Index, Role: schema.Role(source.Role), Depth: source.Depth, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, ContentPreview: &content, Extra: &extra})
	}
	if err := db.IndexSessionEntries(context.Background(), ingest.SessionID(fixture.SessionID), entries); err != nil {
		t.Fatalf("persist entries: %v", err)
	}
	provider := api.NewStoreDataProvider(db, sessionvisibility.All())
	hub := api.NewHub(provider)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	connection, _, err := websocket.Dial(ctx, "ws://"+server.Listener.Addr().String(), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "")
	if _, _, err := connection.Read(ctx); err != nil {
		t.Fatalf("read connected: %v", err)
	}
	subscription, _ := json.Marshal(api.ClientMessage{Type: api.MsgSubscribe, Channels: []api.ChannelSubscription{{Topic: api.TopicSessionDetail, ID: fixture.SessionID}}})
	if err := connection.Write(ctx, websocket.MessageText, subscription); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	_, data, err := connection.Read(ctx)
	if err != nil {
		t.Fatalf("read session detail: %v", err)
	}
	var message struct {
		Type api.MessageType             `json:"type"`
		Data schema.SessionDetailPayload `json:"data"`
	}
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatalf("decode session detail: %v", err)
	}
	if message.Type != api.MsgSessionDetail || message.Data.Model != fixture.ExpectedSeed || len(message.Data.Turns) != len(fixture.ExpectedObservedModels) {
		t.Fatalf("session detail mismatch: %+v", message)
	}
	for index, expected := range fixture.ExpectedObservedModels {
		if got := message.Data.Turns[index].ObservedModel.String(); got != expected {
			t.Fatalf("turn %d observedModel=%q, want %q", index, got, expected)
		}
	}
}
