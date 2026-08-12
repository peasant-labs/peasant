package api_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/direct_link_selection.yaml
var directLinkSelectionYAML []byte

const directLinkSelectionRowCount = 2

type directLinkRole string

const (
	directLinkListedSelection directLinkRole = "listed-selection"
	directLinkHiddenHistory   directLinkRole = "hidden-direct-link"
)

var allDirectLinkRoles = []directLinkRole{
	directLinkListedSelection,
	directLinkHiddenHistory,
}

type directLinkSelectionFixture struct {
	DeclaredRows            int                      `yaml:"declared_rows"`
	Harness                 string                   `yaml:"harness"`
	ExpectedVisibleSessions int                      `yaml:"expected_visible_sessions"`
	ExpectedVisibleProjects int                      `yaml:"expected_visible_projects"`
	Rows                    []directLinkSelectionRow `yaml:"rows"`
}

type directLinkSelectionRow struct {
	Role           directLinkRole `yaml:"role"`
	SessionID      string         `yaml:"session_id"`
	ProjectHash    string         `yaml:"project_hash"`
	ProjectName    string         `yaml:"project_name"`
	ProjectDisplay string         `yaml:"project_display"`
	Clone          string         `yaml:"clone"`
	GitBranch      string         `yaml:"git_branch"`
	HostSlug       string         `yaml:"host_slug"`
	StartMs        int64          `yaml:"start_ms"`
	TokensIn       int            `yaml:"tokens_in"`
	TokensOut      int            `yaml:"tokens_out"`
	TurnCount      int            `yaml:"turn_count"`
	ToolCallCount  int            `yaml:"tool_call_count"`
	DurationMs     int64          `yaml:"duration_ms"`
}

func decodeDirectLinkSelection(source []byte) (directLinkSelectionFixture, error) {
	var fixture directLinkSelectionFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode direct-link selection fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixture, fmt.Errorf("direct-link selection fixture must contain exactly one YAML document: %v", err)
	}
	if fixture.DeclaredRows != directLinkSelectionRowCount || len(fixture.Rows) != directLinkSelectionRowCount {
		return fixture, fmt.Errorf("direct-link selection fixture row count mismatch: declared=%d actual=%d required=%d", fixture.DeclaredRows, len(fixture.Rows), directLinkSelectionRowCount)
	}
	if fixture.Harness != defaults.HarnessClaudeCode.String() {
		return fixture, fmt.Errorf("direct-link selection fixture harness = %q, want %q", fixture.Harness, defaults.HarnessClaudeCode)
	}
	if fixture.ExpectedVisibleSessions != 1 || fixture.ExpectedVisibleProjects != 1 {
		return fixture, fmt.Errorf("direct-link selection fixture must keep exactly one discovery session and project visible")
	}
	allowedRoles := make(map[directLinkRole]bool, len(allDirectLinkRoles))
	for _, role := range allDirectLinkRoles {
		allowedRoles[role] = true
	}
	seenRoles := make(map[directLinkRole]bool, len(allDirectLinkRoles))
	seenSessions := make(map[string]bool, len(fixture.Rows))
	seenProjects := make(map[string]bool, len(fixture.Rows))
	for index, row := range fixture.Rows {
		if !allowedRoles[row.Role] || seenRoles[row.Role] {
			return fixture, fmt.Errorf("direct-link selection fixture rows[%d] has unknown or duplicate role %q", index, row.Role)
		}
		seenRoles[row.Role] = true
		if row.SessionID == "" || seenSessions[row.SessionID] {
			return fixture, fmt.Errorf("direct-link selection fixture rows[%d] has an empty or duplicate session ID %q", index, row.SessionID)
		}
		seenSessions[row.SessionID] = true
		if _, err := ingest.NewSessionID(row.SessionID); err != nil {
			return fixture, fmt.Errorf("direct-link selection fixture rows[%d] has invalid session ID %q: %w", index, row.SessionID, err)
		}
		if _, err := schema.NewProjectHash(row.ProjectHash); err != nil {
			return fixture, fmt.Errorf("direct-link selection fixture rows[%d] has invalid project hash %q: %w", index, row.ProjectHash, err)
		}
		if seenProjects[row.ProjectHash] {
			return fixture, fmt.Errorf("direct-link selection fixture rows[%d] repeats project hash %q; the hidden deep link must belong to another project", index, row.ProjectHash)
		}
		seenProjects[row.ProjectHash] = true
		if row.ProjectName == "" || row.ProjectDisplay == "" || row.Clone == "" || row.GitBranch == "" || row.HostSlug == "" || row.StartMs <= 0 || row.TokensIn <= 0 || row.TokensOut <= 0 || row.TurnCount <= 0 || row.DurationMs <= 0 {
			return fixture, fmt.Errorf("direct-link selection fixture rows[%d] is incomplete", index)
		}
	}
	for _, role := range allDirectLinkRoles {
		if !seenRoles[role] {
			return fixture, fmt.Errorf("direct-link selection fixture does not cover role %q", role)
		}
	}
	return fixture, nil
}

func loadDirectLinkSelection(t *testing.T) directLinkSelectionFixture {
	t.Helper()
	fixture, err := decodeDirectLinkSelection(directLinkSelectionYAML)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f directLinkSelectionFixture) row(role directLinkRole) directLinkSelectionRow {
	for _, row := range f.Rows {
		if row.Role == role {
			return row
		}
	}
	return directLinkSelectionRow{}
}

func TestDirectLinkSelectionFixtureRejectsSemanticMutation(t *testing.T) {
	mutated := bytes.Replace(directLinkSelectionYAML, []byte("role: hidden-direct-link"), []byte("role: renamed-hidden-role"), 1)
	if _, err := decodeDirectLinkSelection(mutated); err == nil {
		t.Fatal("a count-preserving direct-link role mutation unexpectedly validated")
	}
}

func startDirectLinkSelectionServer(t *testing.T, db *store.Store, provider api.DataProvider) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hub := api.NewHub(provider)
	server := api.NewServer(api.ServerConfig{Port: 0, Provider: provider, Hub: hub, Store: db})
	if err := server.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("listen for mounted direct-link evidence: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop mounted direct-link server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("mounted direct-link server did not stop within 5 seconds")
		}
	})
	return "http://" + server.Addr().String()
}

func TestMountedDirectLinksResolveHistoryHiddenFromDiscovery(t *testing.T) {
	t.Parallel()
	fixture := loadDirectLinkSelection(t)
	listed := fixture.row(directLinkListedSelection)
	hidden := fixture.row(directLinkHiddenHistory)

	db := openTestStore(t)
	entries := make([]ingest.StoreEntry, 0, len(fixture.Rows))
	clonePaths := make(map[directLinkRole]string, len(fixture.Rows))
	for _, row := range fixture.Rows {
		clonePath := filepath.Join(t.TempDir(), row.Clone)
		if err := os.MkdirAll(clonePath, 0o755); err != nil {
			t.Fatalf("create direct-link fixture clone %q: %v", row.Clone, err)
		}
		clonePaths[row.Role] = clonePath
		entry := makeStoreEntry(
			t,
			row.SessionID,
			row.ProjectHash,
			row.HostSlug,
			defaults.HarnessClaudeCode,
			row.StartMs,
			row.TokensIn,
			row.TokensOut,
			row.ProjectName,
			row.TurnCount,
			row.ToolCallCount,
			row.DurationMs,
		)
		entry.Metadata.Git.Worktree = &clonePath
		branch := row.GitBranch
		entry.Metadata.Git.Branch = &branch
		entries = append(entries, entry)
	}
	if err := db.InsertSessions(t.Context(), entries); err != nil {
		t.Fatalf("seed mounted direct-link sessions: %v", err)
	}
	policy, err := sessionvisibility.New(config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			fixture.Harness: {
				Projects: []config.ProjectSelection{
					{ClonePaths: []string{clonePaths[directLinkListedSelection]}},
					{ClonePaths: []string{clonePaths[directLinkHiddenHistory]}},
				},
				Exclusions: config.SelectionExclusions{Sessions: []string{hidden.SessionID}},
			},
		},
	})
	if err != nil {
		t.Fatalf("build mounted direct-link selection policy: %v", err)
	}
	provider := api.NewStoreDataProvider(db, policy)
	baseURL := startDirectLinkSelectionServer(t, db, provider)

	status, _, body := getJSON(t, baseURL+defaults.RouteSessions.String())
	if status != 200 {
		t.Fatalf("mounted session discovery status = %d, want 200; body=%s", status, body)
	}
	var sessionEnvelope struct {
		Sessions []api.SessionSummary `json:"sessions"`
	}
	if err := json.Unmarshal(body, &sessionEnvelope); err != nil {
		t.Fatalf("decode mounted session discovery: %v", err)
	}
	if len(sessionEnvelope.Sessions) != fixture.ExpectedVisibleSessions || sessionEnvelope.Sessions[0].ID != listed.SessionID {
		t.Fatalf("mounted session discovery = %+v, want only %s", sessionEnvelope.Sessions, listed.SessionID)
	}

	status, _, body = getJSON(t, baseURL+defaults.RouteProjectsSummary.String())
	if status != 200 {
		t.Fatalf("mounted project discovery status = %d, want 200; body=%s", status, body)
	}
	var projects codemap.ProjectSummariesResult
	if err := json.Unmarshal(body, &projects); err != nil {
		t.Fatalf("decode mounted project discovery: %v", err)
	}
	if len(projects.Projects) != fixture.ExpectedVisibleProjects || projects.Projects[0].ProjectHash.String() != listed.ProjectHash {
		t.Fatalf("mounted project discovery = %+v, want only project %s", projects.Projects, listed.ProjectHash)
	}
	for _, project := range projects.Projects {
		if project.ProjectHash.String() == hidden.ProjectHash {
			t.Fatalf("hidden project %s unexpectedly appeared in discovery", hidden.ProjectHash)
		}
	}

	resolveURL := baseURL + defaults.RouteProjectResolve.String() + "?name=" + url.QueryEscape(hidden.ProjectHash)
	status, _, body = getJSON(t, resolveURL)
	if status != 200 {
		t.Fatalf("hidden canonical project direct link status = %d, want 200; body=%s", status, body)
	}
	var resolution schema.ProjectResolutionPayload
	if err := json.Unmarshal(body, &resolution); err != nil {
		t.Fatalf("decode hidden canonical project direct link: %v", err)
	}
	if resolution.ProjectHash.String() != hidden.ProjectHash || resolution.Project != hidden.ProjectDisplay {
		t.Fatalf("hidden canonical project direct link = %+v, want project=%q hash=%s", resolution, hidden.ProjectDisplay, hidden.ProjectHash)
	}

	wsContext, cancelWS := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelWS()
	wsURL := "ws://" + strings.TrimPrefix(baseURL, "http://") + defaults.RouteWS.String()
	conn, _, err := websocket.Dial(wsContext, wsURL, nil)
	if err != nil {
		t.Fatalf("dial mounted hidden-session direct link: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	_, connectedBytes, err := conn.Read(wsContext)
	if err != nil {
		t.Fatalf("read mounted websocket connection message: %v", err)
	}
	var connected struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(connectedBytes, &connected); err != nil || connected.Type != string(api.MsgConnected) {
		t.Fatalf("mounted websocket connection message = %s, error=%v, want type %q", connectedBytes, err, api.MsgConnected)
	}
	subscribe := api.ClientMessage{
		Type: api.MsgSubscribe,
		Channels: []api.ChannelSubscription{{
			Topic: api.TopicSessionDetail,
			ID:    hidden.SessionID,
		}},
	}
	encodedSubscribe, err := json.Marshal(subscribe)
	if err != nil {
		t.Fatalf("encode mounted hidden-session subscription: %v", err)
	}
	if err := conn.Write(wsContext, websocket.MessageText, encodedSubscribe); err != nil {
		t.Fatalf("write mounted hidden-session subscription: %v", err)
	}
	_, detailBytes, err := conn.Read(wsContext)
	if err != nil {
		t.Fatalf("read mounted hidden-session detail: %v", err)
	}
	var detailMessage struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(detailBytes, &detailMessage); err != nil {
		t.Fatalf("decode mounted hidden-session detail envelope: %v", err)
	}
	if detailMessage.Type != string(api.MsgSessionDetail) {
		t.Fatalf("mounted hidden-session detail message type = %q, want %q; message=%s", detailMessage.Type, api.MsgSessionDetail, detailBytes)
	}
	var detail struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(detailMessage.Data, &detail); err != nil {
		t.Fatalf("decode mounted hidden-session detail payload: %v", err)
	}
	if detail.ID != hidden.SessionID {
		t.Fatalf("mounted hidden-session direct link returned %q, want %q", detail.ID, hidden.SessionID)
	}
}
