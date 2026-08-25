package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/web_discovery_http.yaml
var webDiscoveryHTTPYAML []byte

type discoveryHTTPFixture struct {
	ExpectedRowCount int                    `yaml:"expectedRowCount"`
	SearchQuery      string                 `yaml:"searchQuery"`
	Sessions         []discoveryHTTPSession `yaml:"sessions"`
	LegacySessions   []discoveryHTTPSession `yaml:"legacySessions"`
	Failures         []discoveryHTTPFailure `yaml:"failures"`
	Forbidden        []string               `yaml:"forbidden"`
}

type discoveryHTTPSession struct {
	ID, Project, ProjectHash, Worktree, Remote, Branch, Cohort, SearchText, Status, Label string
	RemovedWorktree                                                                       bool
}

type discoveryHTTPFailure struct {
	Name, Kind, Code string
	Status           int
}

func loadDiscoveryHTTPFixture(t *testing.T) discoveryHTTPFixture {
	t.Helper()
	var fixture discoveryHTTPFixture
	decoder := yaml.NewDecoder(bytes.NewReader(webDiscoveryHTTPYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode mounted API fixture: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("mounted API fixture must contain exactly one YAML document: %v", err)
	}
	if fixture.ExpectedRowCount < 4 || len(fixture.Sessions) != fixture.ExpectedRowCount {
		t.Fatalf("mounted API fixture row guard = %d, rows = %d; want at least four exact rows", fixture.ExpectedRowCount, len(fixture.Sessions))
	}
	seen := map[string]bool{}
	for _, row := range fixture.Sessions {
		if row.ID == "" || seen[row.ID] || row.Worktree == "" || row.Cohort == "" || (row.Status != "selected" && row.Status != "unselected") {
			t.Fatalf("invalid mounted API fixture row: %+v", row)
		}
		seen[row.ID] = true
	}
	if len(fixture.Failures) < 3 || len(fixture.Forbidden) < 4 {
		t.Fatal("mounted API fixture must cover topology failures and hostile evidence")
	}
	if len(fixture.LegacySessions) != 4 || fixture.LegacySessions[0].ID == "" || fixture.LegacySessions[0].Worktree != "" || !fixture.LegacySessions[2].RemovedWorktree {
		t.Fatal("mounted API fixture must cover multiple unresolved sessions in each of two projects, including missing and removed stored worktrees")
	}
	return fixture
}

type fixtureRepositoryResolver struct {
	identities map[ingest.ClonePath]ingest.RepositoryIdentity
	failPath   string
}

func (r fixtureRepositoryResolver) ResolveRepositoryIdentity(_ context.Context, path ingest.ClonePath) (ingest.RepositoryIdentity, error) {
	if strings.Contains(path.String(), r.failPath) && r.failPath != "" {
		return ingest.RepositoryIdentity{}, fmt.Errorf("fixture topology failure")
	}
	identity, ok := r.identities[path]
	if !ok {
		return ingest.RepositoryIdentity{}, nil
	}
	return identity, nil
}

var _ ingest.RepositoryIdentityResolver = fixtureRepositoryResolver{}

func TestMountedHTTPContractsWithStoredSelectionData(t *testing.T) {
	fixture := loadDiscoveryHTTPFixture(t)
	db := storetest.Open(t)
	paths, resolver := seedDiscoveryHTTPStore(t, db, fixture)
	selection := config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{
		defaults.HarnessClaudeCode.String(): {Projects: []config.ProjectSelection{{GitRemote: fixture.Sessions[0].Remote, ClonePaths: []string{paths[fixture.Sessions[0].Worktree]}, Branches: []string{fixture.Sessions[0].Branch}}}},
	}}
	policy, err := sessionvisibility.New(selection)
	if err != nil {
		t.Fatalf("create mounted API selection: %v", err)
	}
	provider := NewStoreDataProvider(db, policy)
	base := startDiscoveryHTTPServer(t, ServerConfig{Port: 0, Store: db, Config: &config.Config{Selection: selection}, Provider: provider, RepositoryIdentityResolver: resolver})

	discovery := getMountedJSON(t, base+"/api/v1/web/discovery", http.StatusOK)
	assertExactKeys(t, discovery, "items")
	items := objectSlice(t, discovery["items"])
	if len(items) != fixture.ExpectedRowCount {
		t.Fatalf("discovery rows = %d, want %d", len(items), fixture.ExpectedRowCount)
	}
	byID := map[string]map[string]any{}
	for _, item := range items {
		assertExactKeys(t, item, "branch", "locationLabel", "repositoryLocationId", "selectionStatus", "sessionId")
		id := stringField(t, item, "sessionId")
		byID[id] = item
		locationID, label := stringField(t, item, "repositoryLocationId"), stringField(t, item, "locationLabel")
		if !strings.HasPrefix(locationID, "rl_") || locationID == label {
			t.Fatalf("session %s location identity is not opaque and distinct: %q / %q", id, locationID, label)
		}
	}
	for _, expected := range fixture.Sessions {
		item := byID[expected.ID]
		if item == nil || stringField(t, item, "branch") != expected.Branch || stringField(t, item, "selectionStatus") != expected.Status || stringField(t, item, "locationLabel") != expected.Label {
			t.Fatalf("discovery item %q = %+v, want branch=%q status=%q", expected.ID, item, expected.Branch, expected.Status)
		}
	}
	if stringField(t, byID[fixture.Sessions[0].ID], "repositoryLocationId") != stringField(t, byID[fixture.Sessions[1].ID], "repositoryLocationId") {
		t.Fatal("linked worktrees did not share one repository-location cohort")
	}
	if stringField(t, byID[fixture.Sessions[0].ID], "repositoryLocationId") == stringField(t, byID[fixture.Sessions[2].ID], "repositoryLocationId") {
		t.Fatal("independent same-remote clone collapsed into selected cohort")
	}
	assertNoHostileEvidence(t, discovery, fixture.Forbidden)

	search := getMountedJSON(t, base+"/api/v1/search?q="+fixture.SearchQuery, http.StatusOK)
	assertExactKeys(t, search, "query", "results")
	results := objectSlice(t, search["results"])
	if len(results) != fixture.ExpectedRowCount {
		t.Fatalf("search rows = %d, want all stored selected and excluded history (%d)", len(results), fixture.ExpectedRowCount)
	}
	searchIDs := make([]string, 0, len(results))
	for _, result := range results {
		assertExactKeys(t, result, "entryIndex", "project", "projectHash", "role", "score", "sessionId", "snippet")
		searchIDs = append(searchIDs, stringField(t, result, "sessionId"))
	}
	sort.Strings(searchIDs)
	if !reflect.DeepEqual(searchIDs, sortedFixtureIDs(fixture)) {
		t.Fatalf("search IDs = %v, want %v", searchIDs, sortedFixtureIDs(fixture))
	}

	sessions := getMountedJSON(t, base+"/api/v1/sessions", http.StatusOK)
	assertExactKeys(t, sessions, "sessions")
	sessionRows := objectSlice(t, sessions["sessions"])
	if len(sessionRows) != 1 || stringField(t, sessionRows[0], "id") != fixture.Sessions[0].ID {
		t.Fatalf("sessions visibility changed: %+v", sessionRows)
	}
	// sessionOrigin is the producer's declaration of who drove the session. It
	// is always present on a stored row, because sessions.session_origin is NOT
	// NULL over the three-value menu.
	assertExactKeys(t, sessionRows[0], "durationMins", "harness", "id", "preview", "project", "projectHash", "sessionOrigin", "startTime", "toolCallCount", "totalTokens", "turnCount")
	assertNoHostileEvidence(t, sessions, fixture.Forbidden)
}

func TestMountedDiscoveryFailsClosed(t *testing.T) {
	fixture := loadDiscoveryHTTPFixture(t)
	for _, failure := range fixture.Failures {
		t.Run(failure.Name, func(t *testing.T) {
			db := storetest.Open(t)
			paths, resolver := seedDiscoveryHTTPStore(t, db, fixture)
			cfg := &config.Config{Selection: config.SelectionConfig{Mode: config.SelectionModeAll}}
			path := "/api/v1/web/discovery"
			switch failure.Kind {
			case "missing-topology":
				resolver.identities[ingest.ClonePath(paths[fixture.Sessions[0].Worktree])] = ingest.RepositoryIdentity{}
			case "ambiguous-topology":
				resolver.failPath = fixture.Sessions[0].Worktree
			case "invalid-selection":
				cfg.Selection.Mode = config.SelectionMode("hostile-invalid")
			case "unsupported-query":
				path += "?physical=/private/credential"
			default:
				t.Fatalf("unknown failure kind %q", failure.Kind)
			}
			base := startDiscoveryHTTPServer(t, ServerConfig{Port: 0, Store: db, Config: cfg, RepositoryIdentityResolver: resolver})
			body := getMountedJSON(t, base+path, failure.Status)
			assertExactKeys(t, body, "code", "error")
			if stringField(t, body, "code") != failure.Code || stringField(t, body, "error") == "" {
				t.Fatalf("failure body = %+v, want code %q and actionable error", body, failure.Code)
			}
			assertNoHostileEvidence(t, body, fixture.Forbidden)
		})
	}
}

func TestMountedDiscoveryKeepsLegacySessionWithoutStoredWorktree(t *testing.T) {
	fixture := loadDiscoveryHTTPFixture(t)
	db := storetest.Open(t)
	paths, resolver := seedDiscoveryHTTPStore(t, db, fixture)
	root := filepath.Dir(filepath.Dir(paths[fixture.Sessions[0].Worktree]))
	selection := config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{
		defaults.HarnessClaudeCode.String(): {Projects: []config.ProjectSelection{
			{GitRemote: fixture.LegacySessions[0].Remote, Branches: []string{fixture.LegacySessions[1].Branch}},
			{GitRemote: fixture.LegacySessions[2].Remote, Branches: []string{fixture.LegacySessions[2].Branch}},
		}},
	}}
	base := startDiscoveryHTTPServer(t, ServerConfig{Port: 0, Store: db, Config: &config.Config{Selection: selection}, RepositoryIdentityResolver: resolver})
	baselineItems := objectSlice(t, getMountedJSON(t, base+"/api/v1/web/discovery", http.StatusOK)["items"])
	baselineLabels := make(map[string]string, len(baselineItems))
	for _, item := range baselineItems {
		baselineLabels[stringField(t, item, "sessionId")] = stringField(t, item, "locationLabel")
	}
	seedDiscoveryHTTPSessions(t, db, fixture.LegacySessions, root, paths, resolver, len(fixture.Sessions))

	body := getMountedJSON(t, base+"/api/v1/web/discovery", http.StatusOK)
	items := objectSlice(t, body["items"])
	wantRows := len(fixture.Sessions) + len(fixture.LegacySessions)
	if len(items) != wantRows {
		t.Fatalf("discovery rows = %d, want all %d resolved and legacy rows", len(items), wantRows)
	}
	byID := make(map[string]map[string]any, len(items))
	for _, item := range items {
		assertExactKeys(t, item, "branch", "locationLabel", "repositoryLocationId", "selectionStatus", "sessionId")
		byID[stringField(t, item, "sessionId")] = item
	}
	for _, resolved := range fixture.Sessions {
		item := byID[resolved.ID]
		if item == nil || stringField(t, item, "locationLabel") != baselineLabels[resolved.ID] {
			t.Fatalf("resolved discovery item %q = %+v, want preserved baseline label %q", resolved.ID, item, baselineLabels[resolved.ID])
		}
	}
	legacyLocationIDsByProject := make(map[string]string, 2)
	for _, legacy := range fixture.LegacySessions {
		item := byID[legacy.ID]
		if item == nil {
			t.Fatalf("legacy session %q missing from discovery response", legacy.ID)
		}
		locationID := stringField(t, item, "repositoryLocationId")
		if stringField(t, item, "locationLabel") != legacy.Label || !strings.HasPrefix(locationID, "rl_") || stringField(t, item, "branch") != "" || stringField(t, item, "selectionStatus") != legacy.Status {
			t.Fatalf("legacy discovery item = %+v, want opaque unavailable repository location with status %q", item, legacy.Status)
		}
		if existing := legacyLocationIDsByProject[legacy.ProjectHash]; existing != "" && existing != locationID {
			t.Fatalf("unresolved project %q location IDs differ: %q and %q; sessions in one project must share its unavailable repository group", legacy.ProjectHash, existing, locationID)
		}
		legacyLocationIDsByProject[legacy.ProjectHash] = locationID
	}
	// Unresolved location identity is project-scoped rather than session-scoped:
	// this keeps every row while presenting one unavailable location per project.
	if len(legacyLocationIDsByProject) != 2 {
		t.Fatalf("unresolved repository locations = %v, want two project-scoped identities", legacyLocationIDsByProject)
	}
	if legacyLocationIDsByProject[fixture.LegacySessions[0].ProjectHash] == legacyLocationIDsByProject[fixture.LegacySessions[2].ProjectHash] {
		t.Fatalf("different unresolved projects shared repository location ID %q", legacyLocationIDsByProject[fixture.LegacySessions[0].ProjectHash])
	}
}

func TestUnavailableWebDiscoveryIdentityFallsBackToSessionIdentity(t *testing.T) {
	first := unavailableWebDiscoveryIdentity(store.SessionRow{SessionID: "legacy-session-a"})
	second := unavailableWebDiscoveryIdentity(store.SessionRow{SessionID: "legacy-session-b"})
	if first.locationID == second.locationID {
		t.Fatalf("unresolved rows without project hashes shared location ID %q; per-session fallback must prevent conflation", first.locationID)
	}
}

func seedDiscoveryHTTPStore(t *testing.T, db *store.Store, fixture discoveryHTTPFixture) (map[string]string, fixtureRepositoryResolver) {
	t.Helper()
	root := t.TempDir()
	paths := map[string]string{}
	resolver := fixtureRepositoryResolver{identities: map[ingest.ClonePath]ingest.RepositoryIdentity{}}
	seedDiscoveryHTTPSessions(t, db, fixture.Sessions, root, paths, resolver, 0)
	return paths, resolver
}

func seedDiscoveryHTTPSessions(t *testing.T, db *store.Store, sessions []discoveryHTTPSession, root string, paths map[string]string, resolver fixtureRepositoryResolver, offset int) {
	t.Helper()
	entries := make([]ingest.StoreEntry, 0, len(sessions))
	for i, row := range sessions {
		worktree := ""
		if row.Worktree != "" {
			path := filepath.Join(root, "repositories", row.Worktree)
			if row.RemovedWorktree {
				worktree = path
			} else {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatalf("create fixture worktree: %v", err)
				}
				physical, err := ingest.NewPhysicalPathResolver().Resolve(path)
				if err != nil {
					t.Fatalf("resolve fixture worktree: %v", err)
				}
				paths[row.Worktree] = physical.String()
				resolver.identities[physical] = ingest.RepositoryIdentity{CohortKey: ingest.RepositoryCohortKey(row.Cohort), GitDirectory: ingest.RepositoryPath(filepath.Join(root, ".git-"+row.Cohort))}
				worktree = physical.String()
			}
		}
		remote, branch := row.Remote, row.Branch
		start := int64(1700000000000 + (offset+i)*1000)
		ingested := start + 501
		entries = append(entries, ingest.StoreEntry{Metadata: &schema.UnifiedMetadata{SchemaVersion: ingest.CurrentSchemaVersion, SessionID: schema.SessionID(row.ID), ModelHarness: defaults.HarnessClaudeCode, Model: "claude-opus-4-6", HostSlug: "fixture-host", Project: schema.ProjectContext{Hash: schema.ProjectHash(row.ProjectHash), Name: row.Project, FilePath: worktree}, Git: schema.GitContext{Remote: &remote, Branch: &branch, Worktree: &worktree}, Timestamp: schema.TimestampInfo{Start: start, End: start + 500, Ingested: &ingested}, Source: schema.SourceInfo{FilePath: "/safe/session.jsonl", Format: schema.SourceFormatJSONL}, Stats: schema.SessionStats{TurnCount: 1, TokensIn: 2, TokensOut: 3}}})
	}
	if err := db.InsertSessions(t.Context(), entries); err != nil {
		t.Fatalf("seed mounted API sessions: %v", err)
	}
	for i, row := range sessions {
		ms := int64(1700000000000 + (offset+i)*1000)
		preview := row.SearchText
		if err := db.IndexSessionEntries(t.Context(), schema.SessionID(row.ID), []schema.SessionEntry{{SessionID: schema.SessionID(row.ID), EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleUser, TimestampMs: &ms, ContentPreview: &preview}}); err != nil {
			t.Fatalf("index mounted search row %s: %v", row.ID, err)
		}
	}
}

func startDiscoveryHTTPServer(t *testing.T, cfg ServerConfig) string {
	t.Helper()
	server := NewServer(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("listen mounted API server: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop mounted API server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("mounted API server did not stop")
		}
	})
	return "http://" + server.Addr().String()
}

func getMountedJSON(t *testing.T, url string, status int) map[string]any {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if response.StatusCode != status {
		t.Fatalf("GET %s status = %d, want %d; body=%s", url, response.StatusCode, status, raw)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode %s: %v; body=%s", url, err, raw)
	}
	return body
}

func assertExactKeys(t *testing.T, object map[string]any, keys ...string) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(keys)
	if !reflect.DeepEqual(got, keys) {
		t.Fatalf("JSON keys = %v, want exactly %v; object=%+v", got, keys, object)
	}
}
func objectSlice(t *testing.T, value any) []map[string]any {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("JSON value = %T, want array", value)
	}
	out := make([]map[string]any, len(raw))
	for i := range raw {
		var ok bool
		out[i], ok = raw[i].(map[string]any)
		if !ok {
			t.Fatalf("JSON row %d = %T, want object", i, raw[i])
		}
	}
	return out
}
func stringField(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	value, ok := object[key].(string)
	if !ok {
		t.Fatalf("field %s = %T, want string", key, object[key])
	}
	return value
}
func sortedFixtureIDs(f discoveryHTTPFixture) []string {
	ids := make([]string, len(f.Sessions))
	for i := range f.Sessions {
		ids[i] = f.Sessions[i].ID
	}
	sort.Strings(ids)
	return ids
}
func assertNoHostileEvidence(t *testing.T, body any, forbidden []string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	for _, fragment := range forbidden {
		if strings.Contains(string(raw), fragment) {
			t.Fatalf("mounted API body leaked forbidden evidence %q: %s", fragment, raw)
		}
	}
}
