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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/session_summaries_by_id.yaml
var sessionSummariesByIDYAML []byte

// requiredSessionSummariesByIDRoles is the deletion guard: the roles the
// fixture must keep, by NAME. Each hidden role is paired with the control that
// makes its absence meaningful, so removing either half fails here rather than
// quietly turning an assertion vacuous.
var requiredSessionSummariesByIDRoles = []string{
	"listed-user-root",
	"hidden-agent-root",
	"hidden-unselected-user-root",
	"listed-user-subagent-row",
	"hidden-agent-subagent-row",
}

type summariesByIDFixture struct {
	Harness               string             `yaml:"harness"`
	UnresolvableSessionID string             `yaml:"unresolvable_session_id"`
	Rows                  []summariesByIDRow `yaml:"rows"`
}

type summariesByIDRow struct {
	Role          string `yaml:"role"`
	SessionID     string `yaml:"session_id"`
	ProjectHash   string `yaml:"project_hash"`
	ProjectName   string `yaml:"project_name"`
	Clone         string `yaml:"clone"`
	GitBranch     string `yaml:"git_branch"`
	HostSlug      string `yaml:"host_slug"`
	StartMs       int64  `yaml:"start_ms"`
	TokensIn      int    `yaml:"tokens_in"`
	TokensOut     int    `yaml:"tokens_out"`
	TurnCount     int    `yaml:"turn_count"`
	ToolCallCount int    `yaml:"tool_call_count"`
	DurationMs    int64  `yaml:"duration_ms"`
	Origin        string `yaml:"origin"`
	Selected      bool   `yaml:"selected"`
	Listed        bool   `yaml:"listed"`
	ParentRole    string `yaml:"parent_role"`
}

func decodeSummariesByID(source []byte) (summariesByIDFixture, error) {
	var fixture summariesByIDFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode session-summaries-by-id fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixture, fmt.Errorf("session-summaries-by-id fixture must contain exactly one YAML document: %v", err)
	}
	if fixture.Harness != defaults.HarnessClaudeCode.String() {
		return fixture, fmt.Errorf("session-summaries-by-id fixture harness = %q, want %q", fixture.Harness, defaults.HarnessClaudeCode)
	}
	if _, err := ingest.NewSessionID(fixture.UnresolvableSessionID); err != nil {
		return fixture, fmt.Errorf("session-summaries-by-id fixture unresolvable_session_id %q must still be a well-formed identifier: %w", fixture.UnresolvableSessionID, err)
	}
	present := make(map[string]bool, len(fixture.Rows))
	sessions := make(map[string]bool, len(fixture.Rows))
	for index, row := range fixture.Rows {
		if row.Role == "" || present[row.Role] {
			return fixture, fmt.Errorf("session-summaries-by-id fixture rows[%d] has an empty or duplicate role %q", index, row.Role)
		}
		present[row.Role] = true
		if _, err := ingest.NewSessionID(row.SessionID); err != nil {
			return fixture, fmt.Errorf("session-summaries-by-id fixture rows[%d] has invalid session ID %q: %w", index, row.SessionID, err)
		}
		if sessions[row.SessionID] {
			return fixture, fmt.Errorf("session-summaries-by-id fixture rows[%d] repeats session ID %q", index, row.SessionID)
		}
		sessions[row.SessionID] = true
		if row.SessionID == fixture.UnresolvableSessionID {
			return fixture, fmt.Errorf("session-summaries-by-id fixture rows[%d] uses the unresolvable identifier; it must name no stored session", index)
		}
		if _, err := ingest.NewProjectHash(row.ProjectHash); err != nil {
			return fixture, fmt.Errorf("session-summaries-by-id fixture rows[%d] has invalid project hash %q: %w", index, row.ProjectHash, err)
		}
		if err := sessionorigin.Origin(row.Origin).Validate(); err != nil {
			return fixture, fmt.Errorf("session-summaries-by-id fixture rows[%d] origin: %w", index, err)
		}
		if row.Listed && !row.Selected {
			return fixture, fmt.Errorf("session-summaries-by-id fixture rows[%d] claims to be listed while outside the selection", index)
		}
		if row.Listed && sessionorigin.Origin(row.Origin) == sessionorigin.Agent {
			return fixture, fmt.Errorf("session-summaries-by-id fixture rows[%d] claims an agent-driven row is listed", index)
		}
		if row.ProjectName == "" || row.Clone == "" || row.GitBranch == "" || row.HostSlug == "" || row.StartMs <= 0 || row.TokensIn <= 0 || row.TokensOut <= 0 || row.TurnCount <= 0 || row.DurationMs <= 0 {
			return fixture, fmt.Errorf("session-summaries-by-id fixture rows[%d] is incomplete", index)
		}
	}
	// The required-name manifest runs BEFORE the parent-role check, so renaming
	// a required role reports the deletion it is rather than the dangling
	// reference it also causes.
	if err := testutil.RequireFixtureNames("session-summaries-by-id fixture", "role", requiredSessionSummariesByIDRoles, present); err != nil {
		return fixture, err
	}
	for index, row := range fixture.Rows {
		if row.ParentRole != "" && !present[row.ParentRole] {
			return fixture, fmt.Errorf("session-summaries-by-id fixture rows[%d] names parent role %q, which the fixture does not define", index, row.ParentRole)
		}
	}
	return fixture, nil
}

func loadSummariesByID(t *testing.T) summariesByIDFixture {
	t.Helper()
	fixture, err := decodeSummariesByID(sessionSummariesByIDYAML)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f summariesByIDFixture) row(t *testing.T, role string) summariesByIDRow {
	t.Helper()
	for _, row := range f.Rows {
		if row.Role == role {
			return row
		}
	}
	t.Fatalf("session-summaries-by-id fixture has no role %q", role)
	return summariesByIDRow{}
}

func TestSessionSummariesByIDFixtureRejectsRoleDeletion(t *testing.T) {
	t.Parallel()
	for _, role := range requiredSessionSummariesByIDRoles {
		t.Run(role, func(t *testing.T) {
			mutated := bytes.Replace(sessionSummariesByIDYAML, []byte("role: "+role+"\n"), []byte("role: removed-"+role+"\n"), 1)
			if bytes.Equal(mutated, sessionSummariesByIDYAML) {
				t.Fatalf("role %q was not present to rename; the deletion guard cannot be trusted", role)
			}
			if _, err := decodeSummariesByID(mutated); err == nil || !strings.Contains(err.Error(), "missing required role") {
				t.Fatalf("renamed-role fixture error = %v, want a missing-required-role rejection", err)
			}
		})
	}
}

// TestMountedLinkedSessionsResolveOutsideBothDiscoveryScopes is the mounted
// evidence for the two halves of the link-resolution contract:
//
//   - the discovery list applies BOTH scopes, so an agent-driven root and an
//     unselected project's root are absent from it;
//   - the by-id route applies NEITHER, so both of them resolve when a caller
//     names them, and an identifier naming no stored session is omitted rather
//     than failing the batch.
//
// The selection half rests on the rule this project already ratified for stored
// sessions: a narrowed selection hides a session from every list, and a direct
// link to it still resolves.
func TestMountedLinkedSessionsResolveOutsideBothDiscoveryScopes(t *testing.T) {
	t.Parallel()
	fixture := loadSummariesByID(t)

	db := openTestStore(t)
	clonePaths := make(map[string]string, len(fixture.Rows))
	sessionIDByRole := make(map[string]string, len(fixture.Rows))
	entries := make([]ingest.StoreEntry, 0, len(fixture.Rows))
	for _, row := range fixture.Rows {
		sessionIDByRole[row.Role] = row.SessionID
	}
	for _, row := range fixture.Rows {
		clonePath, ok := clonePaths[row.Clone]
		if !ok {
			clonePath = filepath.Join(t.TempDir(), row.Clone)
			if err := os.MkdirAll(clonePath, 0o755); err != nil {
				t.Fatalf("create fixture clone %q: %v", row.Clone, err)
			}
			clonePaths[row.Clone] = clonePath
		}
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
		worktree := clonePath
		entry.Metadata.Git.Worktree = &worktree
		branch := row.GitBranch
		entry.Metadata.Git.Branch = &branch
		entry.Session.Origin = sessionorigin.Origin(row.Origin)
		if row.ParentRole != "" {
			parentID, err := ingest.NewSessionID(sessionIDByRole[row.ParentRole])
			if err != nil {
				t.Fatalf("parse parent session id for role %q: %v", row.Role, err)
			}
			entry.Metadata.ParentUUID = &parentID
		}
		entries = append(entries, entry)
	}
	if err := db.InsertSessions(t.Context(), entries); err != nil {
		t.Fatalf("seed linked-session rows: %v", err)
	}
	api.MarkStoredSessionsIndexed(t, db)

	projects := make([]config.ProjectSelection, 0, len(clonePaths))
	seenClone := make(map[string]bool, len(clonePaths))
	for _, row := range fixture.Rows {
		if !row.Selected || seenClone[row.Clone] {
			continue
		}
		seenClone[row.Clone] = true
		projects = append(projects, config.ProjectSelection{ClonePaths: []string{clonePaths[row.Clone]}})
	}
	policy, err := sessionvisibility.New(config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			fixture.Harness: {Projects: projects},
		},
	})
	if err != nil {
		t.Fatalf("build linked-session selection policy: %v", err)
	}

	provider := api.NewStoreDataProvider(db, policy)
	baseURL := startLinkedSessionServer(t, db, provider)

	listed := decodeSessionsEnvelope(t, baseURL+defaults.RouteSessions.String())
	gotListed := summaryIDs(listed)
	wantListed := make([]string, 0, len(fixture.Rows))
	for _, row := range fixture.Rows {
		if row.Listed {
			wantListed = append(wantListed, row.SessionID)
		}
	}
	assertSameSessionSet(t, "discovery list", gotListed, wantListed)

	// Every stored row, plus one identifier that names nothing.
	requested := make([]string, 0, len(fixture.Rows)+1)
	for _, row := range fixture.Rows {
		requested = append(requested, row.SessionID)
	}
	requested = append(requested, fixture.UnresolvableSessionID)
	resolveURL := baseURL + defaults.RouteSessionSummaries.String() + "?ids=" + url.QueryEscape(strings.Join(requested, ","))
	resolved := decodeSessionsEnvelope(t, resolveURL)

	wantResolved := make([]string, 0, len(fixture.Rows))
	for _, row := range fixture.Rows {
		wantResolved = append(wantResolved, row.SessionID)
	}
	assertSameSessionSet(t, "by-id resolution", summaryIDs(resolved), wantResolved)

	// The two hidden roles are the load-bearing ones: they are absent above and
	// present here, which is the whole discovery-versus-access distinction.
	for _, role := range []string{"hidden-agent-root", "hidden-unselected-user-root", "hidden-agent-subagent-row"} {
		row := fixture.row(t, role)
		summary, ok := summaryByID(resolved, row.SessionID)
		if !ok {
			t.Fatalf("by-id resolution omitted %s (%s); a hidden session must still resolve from its link", role, row.SessionID)
		}
		if string(summary.SessionOrigin) != row.Origin {
			t.Fatalf("%s resolved with sessionOrigin %q, want %q", role, summary.SessionOrigin, row.Origin)
		}
	}

	// The two paths must differ only in WHICH rows they return. A row present in
	// both must be byte-identical, which is what the shared construction site
	// buys and what a future divergence would break.
	shared := fixture.row(t, "listed-user-root")
	fromList, okList := summaryByID(listed, shared.SessionID)
	fromLink, okLink := summaryByID(resolved, shared.SessionID)
	if !okList || !okLink {
		t.Fatalf("listed-user-root must appear on both paths; list=%v link=%v", okList, okLink)
	}
	if !reflect.DeepEqual(fromList, fromLink) {
		t.Fatalf("the two paths built different summaries for the same row:\n  list = %+v\n  link = %+v", fromList, fromLink)
	}
}

func TestMountedLinkedSessionsRejectAnEmptyIdentifierSet(t *testing.T) {
	t.Parallel()
	db := openTestStore(t)
	provider := api.NewStoreDataProvider(db, sessionvisibility.All())
	baseURL := startLinkedSessionServer(t, db, provider)
	status, _, body := getJSON(t, baseURL+defaults.RouteSessionSummaries.String()+"?ids=")
	if status != 400 {
		t.Fatalf("empty by-id request status = %d, want 400; body=%s", status, body)
	}
	for _, want := range []string{"named no session identifiers", defaults.RouteSessionSummaries.String()} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("empty by-id request body = %s, want it to mention %q", body, want)
		}
	}
}

func startLinkedSessionServer(t *testing.T, db *store.Store, provider api.DataProvider) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hub := api.NewHub(provider)
	server := api.NewServer(api.ServerConfig{Port: 0, Provider: provider, Hub: hub, Store: db})
	if err := server.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("listen for mounted linked-session evidence: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop mounted linked-session server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("mounted linked-session server did not stop within 5 seconds")
		}
	})
	return "http://" + server.Addr().String()
}

func decodeSessionsEnvelope(t *testing.T, requestURL string) []api.SessionSummary {
	t.Helper()
	status, _, body := getJSON(t, requestURL)
	if status != 200 {
		t.Fatalf("GET %s status = %d, want 200; body=%s", requestURL, status, body)
	}
	var envelope struct {
		Sessions []api.SessionSummary `json:"sessions"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode sessions envelope from %s: %v", requestURL, err)
	}
	return envelope.Sessions
}

func summaryIDs(summaries []api.SessionSummary) []string {
	ids := make([]string, len(summaries))
	for i := range summaries {
		ids[i] = summaries[i].ID
	}
	return ids
}

func summaryByID(summaries []api.SessionSummary, id string) (api.SessionSummary, bool) {
	for i := range summaries {
		if summaries[i].ID == id {
			return summaries[i], true
		}
	}
	return api.SessionSummary{}, false
}

func assertSameSessionSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	gotSet := make(map[string]bool, len(got))
	for _, id := range got {
		if gotSet[id] {
			t.Fatalf("%s repeated session %s", what, id)
		}
		gotSet[id] = true
	}
	for _, id := range want {
		if !gotSet[id] {
			t.Fatalf("%s omitted session %s; got %v, want %v", what, id, got, want)
		}
		delete(gotSet, id)
	}
	for id := range gotSet {
		t.Fatalf("%s unexpectedly returned session %s; got %v, want %v", what, id, got, want)
	}
}
