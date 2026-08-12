package api_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"
	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/ingest"
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
	if fixture.SessionID == "" || fixture.StoredModel == "" || fixture.ExpectedSeed == "" || fixture.ExpectedCaseCount != 2 || len(fixture.Turns) != fixture.ExpectedCaseCount || len(fixture.RequiredNames) != fixture.ExpectedCaseCount || len(fixture.ExpectedObservedModels) != len(fixture.Turns) {
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

func TestHubSessionDetailEmitsObservedModelEvidence(t *testing.T) {
	fixture := loadObservedModelWebSocketFixture(t)
	session := ingest.Session{ID: ingest.SessionID(fixture.SessionID), Model: fixture.StoredModel}
	for _, source := range fixture.Turns {
		session.Turns = append(session.Turns, ingest.Turn{Index: source.Index, Role: schema.Role(source.Role), Depth: source.Depth, Content: source.Content, ObservedModel: ingest.ObservedModelID(source.ObservedModel)})
	}
	provider := &wsMockProvider{sessions: []ingest.Session{session}}
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

func observedModelFixtureEntries(t *testing.T, fixture observedModelWebSocketFixture) []schema.SessionEntry {
	t.Helper()
	entries := make([]schema.SessionEntry, len(fixture.Turns))
	for index, source := range fixture.Turns {
		extra, err := json.Marshal(map[string]string{"model_id": source.ObservedModel})
		if err != nil {
			t.Fatalf("encode observed model: %v", err)
		}
		extraString := string(extra)
		entries[index] = schema.SessionEntry{
			SessionID:      schema.SessionID(fixture.SessionID),
			EntryIndex:     source.Index,
			Role:           schema.Role(source.Role),
			EntryType:      schema.EntryTypeText,
			Depth:          source.Depth,
			ContentPreview: &source.Content,
			Extra:          &extraString,
		}
	}
	return entries
}
