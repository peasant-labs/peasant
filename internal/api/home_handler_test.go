package api

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/sync_pages.yaml
var syncPagesYAML []byte

//go:embed testdata/home_ancestry.yaml
var homeAncestryYAML []byte

//go:embed testdata/home_publications.yaml
var homePublicationsYAML []byte

//go:embed testdata/home_selection.yaml
var homeSelectionYAML []byte

//go:embed testdata/sync_project_options.yaml
var syncProjectOptionsYAML []byte

//go:embed testdata/home_repository_evidence.yaml
var homeRepositoryEvidenceYAML []byte

type boundedHomeReader struct {
	*store.Store
	allowed    map[string]bool
	reads      map[string][]string
	windows    []int
	cohortRows int
	parentRows int
}

var _ homeReader = (*boundedHomeReader)(nil)

func (r *boundedHomeReader) record(kind string, ids []string) error {
	for _, id := range ids {
		if !r.allowed[id] {
			return fmt.Errorf("off-page %s read of %s", kind, id)
		}
	}
	r.reads[kind] = append(r.reads[kind], ids...)
	return nil
}
func (r *boundedHomeReader) HomeIdentityCohort(ctx context.Context) ([]store.HomeCandidate, error) {
	rows, err := r.Store.HomeIdentityCohort(ctx)
	r.cohortRows += len(rows)
	return rows, err
}
func (r *boundedHomeReader) SyncParentIdentities(ctx context.Context, rows []ingest.PushSessionRow) ([]store.ParentIdentityRow, error) {
	r.parentRows += len(rows)
	return r.Store.SyncParentIdentities(ctx, rows)
}
func (r *boundedHomeReader) SyncHomeWindow(ctx context.Context, b *store.SyncPageBoundary, n int, f store.SyncWindowFilter) (store.SyncCandidateWindow, error) {
	w, err := r.Store.SyncHomeWindow(ctx, b, n, f)
	if len(w.Candidates) > n {
		return w, fmt.Errorf("candidate window exceeded %d", n)
	}
	r.windows = append(r.windows, len(w.Candidates))
	return w, err
}
func (r *boundedHomeReader) HomePageRows(ctx context.Context, ids []string) ([]store.HomePageRow, error) {
	if err := r.record("sync/title", ids); err != nil {
		return nil, err
	}
	return r.Store.HomePageRows(ctx, ids)
}
func (r *boundedHomeReader) LoadPublicationMetadata(ctx context.Context, ids []ingest.SessionID) (map[ingest.SessionID]ingest.PublicationMetadata, error) {
	raw := make([]string, len(ids))
	for i, id := range ids {
		raw[i] = id.String()
	}
	if err := r.record("readiness", raw); err != nil {
		return nil, err
	}
	return r.Store.LoadPublicationMetadata(ctx, ids)
}
func (r *boundedHomeReader) FirstUserMessageBulk(ctx context.Context, ids []string) (map[string]string, error) {
	if err := r.record("preview", ids); err != nil {
		return nil, err
	}
	return r.Store.FirstUserMessageBulk(ctx, ids)
}
func (r *boundedHomeReader) GroupingEvidenceForSessions(ctx context.Context, ids []string) (map[string]store.GroupingEvidenceRow, error) {
	if err := r.record("grouping", ids); err != nil {
		return nil, err
	}
	return r.Store.GroupingEvidenceForSessions(ctx, ids)
}
func (r *boundedHomeReader) ClaudeTeammates(ctx context.Context, ids []string) (map[string]schema.SessionTeammate, error) {
	if err := r.record("teammate", ids); err != nil {
		return nil, err
	}
	return r.Store.ClaudeTeammates(ctx, ids)
}

func homeExec(t *testing.T, db *store.Store, query string, args ...any) {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatal(err)
	}
}
func seedHomeSessions(t *testing.T, db *store.Store, ids []string) {
	t.Helper()
	entries := make([]ingest.StoreEntry, 0, len(ids))
	ingested := int64(1700000060001)
	for _, id := range ids {
		entries = append(entries, ingest.StoreEntry{Metadata: &schema.UnifiedMetadata{SchemaVersion: ingest.CurrentSchemaVersion, SessionID: schema.SessionID(id), ModelHarness: defaults.HarnessOpenCode, Model: "fixture-model", HostSlug: "fixture-host", Project: schema.ProjectContext{Hash: schema.ProjectHash(strings.Repeat("a", 64)), Name: "fixture-project"}, Timestamp: schema.TimestampInfo{Start: 1700000000000, End: 1700000060000, Ingested: &ingested}, Source: schema.SourceInfo{FilePath: "/fixture/session.jsonl", Format: schema.SourceFormatJSONL}, Stats: schema.SessionStats{TurnCount: 1}}})
	}
	if err := db.InsertSessions(t.Context(), entries); err != nil {
		t.Fatal(err)
	}
	homeExec(t, db, `UPDATE session_metrics SET title='stored title'`)
}
func homeTestHandler(t *testing.T, reader homeReader, db *store.Store, cfg *config.Config) http.Handler {
	t.Helper()
	sh := &syncHandler{config: newLiveConfig(cfg)}
	h := &homeHandler{reader: reader, sync: sh, publishing: &publishingHandler{store: db, configHome: t.TempDir()}, resolver: fixtureRepositoryResolver{}}
	sh.home = h
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+defaults.RouteSyncSessions.String(), sh.handleSyncSessions)
	return mux
}
func syncPageRequest(t *testing.T, h http.Handler, query string, status int) schema.LocalSyncSessionPage {
	t.Helper()
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, defaults.RouteSyncSessions.String()+"?"+query, nil))
	if response.Code != status {
		t.Fatalf("status %d want %d: %s", response.Code, status, response.Body.String())
	}
	var page schema.LocalSyncSessionPage
	if status == http.StatusOK {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &keys); err != nil {
			t.Fatal(err)
		}
		if len(keys) != 2 || keys["items"] == nil || keys["next"] == nil {
			t.Fatalf("unexpected page wire keys: %v", keys)
		}
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if err := page.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	return page
}
func rootIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("sess_root%03d", i)
	}
	return ids
}
func assertHydrated(t *testing.T, reader *boundedHomeReader, want []string) {
	t.Helper()
	expected := slices.Clone(want)
	slices.Sort(expected)
	for kind, ids := range reader.reads {
		actual := slices.Clone(ids)
		slices.Sort(actual)
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("%s hydrated %v want %v", kind, ids, want)
		}
	}
	if len(want) > 0 && len(reader.reads) != 5 {
		t.Fatalf("hydration exits missing: %v", reader.reads)
	}
}

type syncTraversalCase struct {
	Name                  string
	Roots                 int
	Parent                string
	Selected              []string
	Maxima, Counts, First []int
	Next                  []bool
	DeleteBoundary        bool `yaml:"deleteBoundary"`
}
type syncScopeCase struct {
	Name, Query, Origin, Code string
	Selection, Owner, Missing bool
	OK                        bool `yaml:"ok"`
	Version                   int
	InvalidID                 bool `yaml:"invalidID"`
	Duplicate                 bool
}
type syncPagesFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Roots         int
	NewestIndex   int `yaml:"newestIndex"`
	Cases         []struct {
		Name, Query   string
		Status, Count int
		Next          bool
		HugeToken     bool `yaml:"hugeToken"`
	}
	Traversals         []syncTraversalCase
	RequiredTraversals []string        `yaml:"requiredTraversals"`
	ScopeCases         []syncScopeCase `yaml:"scopeCases"`
	RequiredScopes     []string        `yaml:"requiredScopes"`
}

func loadSyncPages(t *testing.T) syncPagesFixture {
	t.Helper()
	var f syncPagesFixture
	if err := testutil.DecodeNamedFixtureYAML(syncPagesYAML, &f); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range f.Traversals {
		names[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("sync traversal", "name", f.RequiredTraversals, names); err != nil {
		t.Fatal(err)
	}
	names = map[string]bool{}
	for _, c := range f.ScopeCases {
		names[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("sync scopes", "name", f.RequiredScopes, names); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSyncHomePageBounds(t *testing.T) {
	f := loadSyncPages(t)
	db := storetest.Open(t)
	ids := rootIDs(f.Roots)
	seedHomeSessions(t, db, ids)
	// A newer row sorts before equal-time IDs. The remaining IDs retain ASC ties.
	newest := ids[f.NewestIndex]
	homeExec(t, db, `UPDATE sessions SET start_ms=start_ms+1 WHERE session_id=?`, newest)
	ids = append([]string{newest}, append(ids[:f.NewestIndex], ids[f.NewestIndex+1:]...)...)
	for _, tc := range f.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			allowed := map[string]bool{}
			want := ids[:tc.Count]
			for _, id := range want {
				allowed[id] = true
			}
			r := &boundedHomeReader{Store: db, allowed: allowed, reads: map[string][]string{}}
			query := tc.Query
			if tc.HugeToken {
				query += "&pageToken=" + strings.Repeat("x", 4097)
			}
			p := syncPageRequest(t, homeTestHandler(t, r, db, config.BaseConfig()), query, tc.Status)
			if tc.Status != http.StatusOK {
				if len(r.reads) != 0 {
					t.Fatal("rejected query hydrated rows")
				}
				return
			}
			got := []string{}
			for _, row := range p.Items {
				got = append(got, row.Sync.ID)
				if row.Title != "stored title" {
					t.Fatal(row)
				}
			}
			if !reflect.DeepEqual(got, want) || (p.Next != "") != tc.Next {
				t.Fatalf("got %v next=%q want %v next=%v", got, p.Next, want, tc.Next)
			}
			assertHydrated(t, r, want)
			if r.cohortRows != f.Roots || r.parentRows != f.Roots {
				t.Fatalf("cohort work must be explicit: identity=%d ancestry=%d", r.cohortRows, r.parentRows)
			}
		})
	}
	base := startDiscoveryHTTPServer(t, ServerConfig{Store: db, Config: config.BaseConfig(), Provider: NewStoreDataProvider(db, sessionvisibility.Browsing()), ConfigHome: t.TempDir()})
	resp, err := http.Get(base + defaults.RouteSyncSessions.String() + "?view=home")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var p schema.LocalSyncSessionPage
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || len(p.Items) != 25 {
		t.Fatalf("mounted page %d %+v", resp.StatusCode, p)
	}
}

func TestSyncHomeKeysetTraversal(t *testing.T) {
	for _, tc := range loadSyncPages(t).Traversals {
		t.Run(tc.Name, func(t *testing.T) {
			db := storetest.Open(t)
			ids := rootIDs(tc.Roots)
			seedHomeSessions(t, db, ids)
			cfg := config.BaseConfig()
			if tc.Parent != "" {
				seedHomeSessions(t, db, []string{tc.Parent})
				homeExec(t, db, `UPDATE sessions SET parent_id=? WHERE session_id<>?`, tc.Parent, tc.Parent)
				homeExec(t, db, `UPDATE sessions SET start_ms=start_ms+1 WHERE session_id=?`, tc.Parent)
			}
			if tc.Selected != nil {
				cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{defaults.HarnessOpenCode.String(): {Sessions: tc.Selected}}}
			}
			var next schema.PageToken
			for i, max := range tc.Maxima {
				want := make([]string, 0, tc.Counts[i])
				allowed := map[string]bool{}
				for j := 0; j < tc.Counts[i]; j++ {
					id := ids[tc.First[i]+j]
					want = append(want, id)
					allowed[id] = true
				}
				r := &boundedHomeReader{Store: db, allowed: allowed, reads: map[string][]string{}}
				q := url.Values{"view": {"home"}, "maxItems": {fmt.Sprint(max)}, "pageToken": {string(next)}}
				if tc.Parent != "" {
					q.Set("parentSessionId", tc.Parent)
				}
				p := syncPageRequest(t, homeTestHandler(t, r, db, cfg), q.Encode(), 200)
				got := []string{}
				for _, row := range p.Items {
					got = append(got, row.Sync.ID)
				}
				if !reflect.DeepEqual(got, want) || (p.Next != "") != tc.Next[i] {
					t.Fatalf("page%d got=%v next=%q want=%v next=%v", i, got, p.Next, want, tc.Next[i])
				}
				assertHydrated(t, r, want)
				if len(r.windows) != 1 || r.windows[0] > 4*max {
					t.Fatalf("window: %v maximum=%d", r.windows, 4*max)
				}
				if p.Next != "" && p.Next == next {
					t.Fatal("continuation did not advance")
				}
				next = p.Next
				if i == 0 && tc.DeleteBoundary {
					homeExec(t, db, `DELETE FROM session_metrics WHERE session_id=?`, got[len(got)-1])
					homeExec(t, db, `DELETE FROM sessions WHERE session_id=?`, got[len(got)-1])
				}
			}
		})
	}
}

func TestSyncHomeTokenScope(t *testing.T) {
	for _, tc := range loadSyncPages(t).ScopeCases {
		t.Run(tc.Name, func(t *testing.T) {
			db := storetest.Open(t)
			seedHomeSessions(t, db, rootIDs(5))
			cfg := config.BaseConfig()
			hs := newTestXDGHomes(t)
			sh := &syncHandler{store: db, config: newLiveConfig(cfg)}
			h := &homeHandler{reader: db, sync: sh, publishing: &publishingHandler{store: db, configHome: hs.Config}, resolver: fixtureRepositoryResolver{}}
			sh.home = h
			if tc.Owner {
				writeSyncDoorCredentials(t, hs.Config, "https://owner.example")
			}
			p := syncPageRequest(t, http.HandlerFunc(sh.handleSyncSessions), "view=home&maxItems=1", 200)
			if tc.Selection {
				cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected}
			}
			if tc.Origin != "" {
				writeSyncDoorCredentials(t, hs.Config, tc.Origin)
			}
			if tc.Owner {
				raw, err := auth.LoadCredentialsFrom(hs.Config)
				if err != nil {
					t.Fatal(err)
				}
				raw.UserID = "user-2"
				if err := auth.SaveCredentialsFrom(raw, hs.Config); err != nil {
					t.Fatal(err)
				}
			}
			token := p.Next
			if tc.Version != 0 || tc.InvalidID {
				raw, _ := base64.RawURLEncoding.DecodeString(string(token))
				var v syncPageToken
				_ = json.Unmarshal(raw, &v)
				if tc.Version != 0 {
					v.Version = tc.Version
				}
				if tc.InvalidID {
					v.ID = "\ninvalid"
				}
				raw, _ = json.Marshal(v)
				token = schema.PageToken(base64.RawURLEncoding.EncodeToString(raw))
			}
			if tc.Missing {
				token = schema.PageToken(base64.RawURLEncoding.EncodeToString([]byte(`{"v":1}`)))
			}
			if tc.Duplicate {
				raw, _ := base64.RawURLEncoding.DecodeString(string(token))
				raw = []byte(strings.Replace(string(raw), `{"v":1,`, `{"v":1,"v":1,`, 1))
				token = schema.PageToken(base64.RawURLEncoding.EncodeToString(raw))
			}
			response := httptest.NewRecorder()
			sh.handleSyncSessions(response, httptest.NewRequest(http.MethodGet, defaults.RouteSyncSessions.String()+"?view=home&pageToken="+url.QueryEscape(string(token))+"&"+tc.Query, nil))
			if tc.OK {
				// Changing the saved ingest selection must not invalidate an
				// unrelated browsing continuation.
				if response.Code != http.StatusOK {
					t.Fatalf("unchanged-scope replay status = %d, want 200; body=%s", response.Code, response.Body.String())
				}
				return
			}
			var refusal errorEnvelope
			_ = json.Unmarshal(response.Body.Bytes(), &refusal)
			if response.Code != 400 || refusal.Code != tc.Code || !strings.Contains(refusal.Error, "restart") {
				t.Fatalf("scope refusal: %d %+v", response.Code, refusal)
			}
		})
	}
}

type homeAncestrySession struct {
	ID       string
	Cache    string
	Snapshot string
	Kind     string
	State    string
	Target   string
	Latest   bool
	Inactive bool
}

type homeAncestryCase struct {
	Name             string
	Query            string
	IDs              []string
	Children         []int
	Selected         []string
	Status           int
	InvalidSelection bool `yaml:"invalidSelection"`
}

type homeAncestryDocument struct {
	RequiredNames []string `yaml:"requiredNames"`
	Sessions      []homeAncestrySession
	Cases         []homeAncestryCase
}

func loadHomeAncestry(t *testing.T) homeAncestryDocument {
	t.Helper()
	var document homeAncestryDocument
	if err := testutil.DecodeNamedFixtureYAML(homeAncestryYAML, &document); err != nil {
		t.Fatalf("decode home ancestry fixture: %v", err)
	}
	return document
}

func TestSyncHomeAncestryIgnoresSavedSelection(t *testing.T) {
	f := loadHomeAncestry(t)
	db := storetest.Open(t)
	ids := make([]string, len(f.Sessions))
	for i, s := range f.Sessions {
		ids[i] = s.ID
	}
	seedHomeSessions(t, db, ids)
	for _, s := range f.Sessions {
		if s.Cache != "" {
			homeExec(t, db, `UPDATE sessions SET parent_id=? WHERE session_id=?`, s.Cache, s.ID)
		}
		if s.Snapshot != "" {
			raw, _ := json.Marshal(map[string]string{"parentUuid": s.Snapshot})
			hash := strings.Repeat("a", 64)
			homeExec(t, db, `INSERT INTO session_publication_metadata(session_id,capture_revision,schema_version,metadata_json,metadata_hash,content_hash,captured_at) VALUES(?,1,1,?,?,?,1)`, s.ID, string(raw), hash, hash)
		}
		if s.State != "" || s.Inactive {
			generation, column := "active", "active_generation_id"
			if s.Latest {
				column = "latest_attempt_generation_id"
			}
			homeExec(t, db, `UPDATE sessions SET `+column+`=? WHERE session_id=?`, generation, s.ID)
			kind, state, target := s.Kind, s.State, s.Target
			if kind == "" {
				kind = "started_by"
			}
			if s.Inactive {
				generation = "old"
				state = "explicit_none"
			}
			homeExec(t, db, `INSERT INTO session_relationship_evidence(session_id,generation_id,ordinal,kind,target_state,target_local_id,evidence) VALUES(?,?,0,?,?,NULLIF(?,''),'existing_adapter')`, s.ID, generation, kind, state, target)
		}
	}
	for _, tc := range f.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			cfg := config.BaseConfig()
			if tc.Selected != nil {
				cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{defaults.HarnessOpenCode.String(): {Sessions: tc.Selected}}}
			}
			if tc.InvalidSelection {
				cfg.Selection.Mode = "invalid"
			}
			status := tc.Status
			if status == 0 {
				status = 200
			}
			p := syncPageRequest(t, homeTestHandler(t, db, db, cfg), "view=home&"+tc.Query, status)
			if status != 200 {
				return
			}
			ids, counts := []string{}, []int{}
			for _, row := range p.Items {
				ids = append(ids, row.Sync.ID)
				counts = append(counts, row.ChildCount)
			}
			if !reflect.DeepEqual(ids, tc.IDs) || !reflect.DeepEqual(counts, tc.Children) {
				t.Fatalf("ids=%v children=%v; want %v/%v", ids, counts, tc.IDs, tc.Children)
			}
		})
	}
}

// TestSyncHomeInvalidSelectionFailsMetadataButNotStoredBrowsing proves the two
// questions stay separate. Home publication metadata cannot be reported
// truthfully from an unusable saved selector, so the page fails closed with no
// partial body. Plain stored browsing has no such dependency and still lists
// the same rows.
func TestSyncHomeInvalidSelectionFailsMetadataButNotStoredBrowsing(t *testing.T) {
	fixture := loadHomeAncestry(t)
	if len(fixture.Sessions) == 0 {
		t.Fatal("home ancestry fixture has no sessions")
	}
	db := storetest.Open(t)
	ids := make([]string, len(fixture.Sessions))
	for i, session := range fixture.Sessions {
		ids[i] = session.ID
	}
	seedHomeSessions(t, db, ids)

	cfg := config.BaseConfig()
	cfg.Selection.Mode = config.SelectionMode("hostile-invalid")

	response := httptest.NewRecorder()
	homeTestHandler(t, db, db, cfg).ServeHTTP(response, httptest.NewRequest(http.MethodGet, defaults.RouteSyncSessions.String()+"?view=home", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("Home status with an unusable selector = %d, want 500 (body %s)", response.Code, response.Body.String())
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode Home failure body: %v (body %s)", err, response.Body.String())
	}
	if keys["items"] != nil || keys["next"] != nil {
		t.Fatalf("Home failure returned a partial page: %s", response.Body.String())
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode Home error envelope: %v", err)
	}
	if !strings.Contains(envelope.Error, "selection") || !strings.Contains(envelope.Error, "kickstart") {
		t.Fatalf("Home failure message = %q, want the saved-selection cause and its remedy", envelope.Error)
	}

	provider := NewStoreDataProvider(db, sessionvisibility.New(cfg.Selection))
	base := startDiscoveryHTTPServer(t, ServerConfig{Store: db, Config: cfg, Provider: provider, ConfigHome: t.TempDir()})
	assertPublicationMetadataFailure(t, &publishingWorld{db: db, baseURL: base}, ids, "selection", "kickstart")
	sessions, err := provider.Sessions(context.Background())
	if err != nil {
		t.Fatalf("plain Sessions: %v", err)
	}
	if len(sessions) != len(ids) {
		t.Fatalf("plain Sessions = %d rows, want all %d stored rows with the same unusable selector", len(sessions), len(ids))
	}
	projects, err := provider.ProjectSummaries(context.Background())
	if err != nil {
		t.Fatalf("plain ProjectSummaries: %v", err)
	}
	if len(projects.Projects) == 0 {
		t.Fatal("plain ProjectSummaries returned no stored project for an unusable selector")
	}
}

// assertProjectFilteredConflictMetadata proves a Home page filtered to a
// healthy project never evaluates ingest rules for an unrelated stored project.
// The full cohort still supplies identity evidence, but only requested rows are
// matched. Requesting the conflicting project's metadata fails truthfully.
func assertProjectFilteredConflictMetadata(t *testing.T, tc homeSelectionCase, db *store.Store, source discoveryHTTPFixture, resolver fixtureRepositoryResolver) {
	t.Helper()
	healthy, conflicting := source.Sessions[0], source.Sessions[3]
	selection := config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{
		defaults.HarnessClaudeCode.String(): {
			Sessions: []string{healthy.ID},
			Projects: []config.ProjectSelection{
				{GitRemote: conflicting.Remote, Branches: []string{"release"}},
				{GitRemote: conflicting.Remote, Branches: []string{"main"}},
			},
		},
	}}
	cfg := config.BaseConfig()
	cfg.Selection = selection
	sh := &syncHandler{store: db, config: newLiveConfig(cfg)}
	h := &homeHandler{reader: db, sync: sh, publishing: &publishingHandler{store: db, configHome: t.TempDir()}, resolver: resolver}
	base := startDiscoveryHTTPServer(t, ServerConfig{Store: db, Config: cfg, Provider: NewStoreDataProvider(db, sessionvisibility.New(cfg.Selection)), ConfigHome: t.TempDir(), RepositoryIdentityResolver: resolver})
	world := &publishingWorld{db: db, baseURL: base}

	var healthyPage schema.LocalSyncSessionPage
	world.decode(t, http.MethodGet, defaults.RouteSyncSessions.String()+"?view=home&projectHash="+healthy.ProjectHash, &healthyPage)
	assertHomePublicationParity(t, world, healthyPage.Items)
	gotIDs := make([]string, 0, len(healthyPage.Items))
	for _, item := range healthyPage.Items {
		gotIDs = append(gotIDs, item.Sync.ID)
		wantOutside := false
		for _, id := range tc.OutsideSelection {
			if id == item.Sync.ID {
				wantOutside = true
				break
			}
		}
		if item.Publication.OutsideSelection != wantOutside {
			t.Fatalf("healthy project session %q outsideSelection=%t, want %t", item.Sync.ID, item.Publication.OutsideSelection, wantOutside)
		}
	}
	if !reflect.DeepEqual(gotIDs, tc.IDs) {
		t.Fatalf("project-filtered Home IDs = %v, want %v", gotIDs, tc.IDs)
	}

	response := httptest.NewRecorder()
	h.handleSessions(response, httptest.NewRequest(http.MethodGet, defaults.RouteSyncSessions.String()+"?view=home&projectHash="+conflicting.ProjectHash, nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("conflicting-project Home status = %d, want 500 (body %s)", response.Code, response.Body.String())
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode conflicting-project failure: %v (body %s)", err, response.Body.String())
	}
	if keys["items"] != nil || keys["next"] != nil {
		t.Fatalf("conflicting-project failure returned a partial page: %s", response.Body.String())
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode conflicting-project error: %v", err)
	}
	if !strings.Contains(envelope.Error, "conflicting") {
		t.Fatalf("conflicting-project failure = %q, want the conflicting-rule cause", envelope.Error)
	}
	assertPublicationMetadataFailure(t, world, []string{healthy.ID, conflicting.ID}, "conflicting", "kickstart")
}

func assertPublicationMetadataFailure(t *testing.T, world *publishingWorld, ids []string, cause, remedy string) {
	t.Helper()
	status, body := world.request(t, http.MethodGet, defaults.RoutePublications.String()+"?sessionIds="+strings.Join(ids, ","), nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("publication metadata failure status = %d, want 500; body %s", status, body)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatal(err)
	}
	if keys["publications"] != nil || keys["items"] != nil || keys["next"] != nil {
		t.Fatalf("publication metadata failure returned partial rows: %s", body)
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(envelope.Error, cause) || !strings.Contains(envelope.Error, remedy) {
		t.Fatalf("publication metadata failure = %q, want cause %q and remedy", envelope.Error, cause)
	}
	// An entirely stale request has no membership to compute, even when the
	// stored selector or another row's identity cannot be evaluated.
	var missing schema.LocalPublicationsResponse
	world.decode(t, http.MethodGet, defaults.RoutePublications.String()+"?sessionIds=missing-stored-session", &missing)
	if len(missing.Publications) != 0 {
		t.Fatalf("missing-only publication read invented rows: %+v", missing.Publications)
	}
}

type homeSelectionCase struct {
	Name, Branch, Explicit string
	Clone                  bool
	DropIndependentMetrics bool     `yaml:"dropIndependentMetrics"`
	ConflictUnrelated      bool     `yaml:"conflictUnrelated"`
	InstalledBinding       bool     `yaml:"installedBinding"`
	AutoPublish            []string `yaml:"autoPublish"`
	IDs                    []string `yaml:"ids"`
	OutsideSelection       []string `yaml:"outsideSelection"`
}

func TestSyncHomeListsEveryPushableSessionRegardlessOfSelection(t *testing.T) {
	var f struct {
		RequiredNames []string            `yaml:"requiredNames"`
		Cases         []homeSelectionCase `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(homeSelectionYAML, &f); err != nil {
		t.Fatal(err)
	}
	for _, tc := range f.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			db := storetest.Open(t)
			source := loadDiscoveryHTTPFixture(t)
			paths, resolver := seedDiscoveryHTTPStore(t, db, source)
			if tc.ConflictUnrelated {
				assertProjectFilteredConflictMetadata(t, tc, db, source, resolver)
				return
			}
			selection := config.SelectionHarnessConfig{}
			if tc.Explicit != "" {
				selection.Sessions = []string{tc.Explicit}
			} else {
				project := config.ProjectSelection{GitRemote: source.Sessions[0].Remote, Branches: []string{tc.Branch}}
				if tc.Clone {
					project.ClonePaths = []string{paths[source.Sessions[0].Worktree]}
				}
				selection.Projects = []config.ProjectSelection{project}
			}
			if tc.DropIndependentMetrics {
				helperGroupDropMetrics(t, db, source.Sessions[2].ID)
			}
			cfg := config.BaseConfig()
			cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{defaults.HarnessClaudeCode.String(): selection}}
			var topology ingest.RepositoryIdentityResolver = resolver
			if tc.InstalledBinding {
				main := paths[source.Sessions[0].Worktree]
				runGit(t, main, "init", "-b", "main")
				runGit(t, main, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "fixture")
				runGit(t, main, "worktree", "add", "-b", "fixture-feature", paths[source.Sessions[1].Worktree])
				runGit(t, paths[source.Sessions[2].Worktree], "init", "-b", "main")
				runGit(t, paths[source.Sessions[3].Worktree], "init", "-b", "main")
				topology = ingest.NewGitRepositoryIdentityResolver()
			}
			base := startDiscoveryHTTPServer(t, ServerConfig{Store: db, Config: cfg, Provider: NewStoreDataProvider(db, sessionvisibility.New(cfg.Selection)), ConfigHome: t.TempDir(), RepositoryIdentityResolver: topology})
			world := &publishingWorld{db: db, baseURL: base}
			if tc.InstalledBinding {
				var saved schema.AutoPublishRule
				status, body := world.request(t, http.MethodPut, ruleRoute("metadata-parity"), schema.AutoPublishRuleRequest{Kind: schema.AutoPublishRuleFolder, Match: filepath.Dir(paths[source.Sessions[0].Worktree]) + "/*", Events: []schema.AutoPublishEvent{schema.AutoPublishPrePush}, Collectives: []schema.VillageUUID{publishingCollectives["platform"].ID}})
				decodeContract(t, status, body, &saved)
				var installed schema.AutoPublishRepository
				status, body = world.request(t, http.MethodPost, installRoute("metadata-parity"), schema.AutoPublishInstallRequest{Path: paths[source.Sessions[0].Worktree]})
				decodeContract(t, status, body, &installed)
				if installed.Hooks[0].Status != schema.AutoPublishHookInstalled {
					t.Fatalf("binding hook not installed: %+v", installed)
				}
			}
			var p schema.LocalSyncSessionPage
			world.decode(t, http.MethodGet, defaults.RouteSyncSessions.String()+"?view=home", &p)
			assertHomePublicationParity(t, world, p.Items)
			ids := []string{}
			for _, row := range p.Items {
				ids = append(ids, row.Sync.ID)
				wantOutside := false
				for _, id := range tc.OutsideSelection {
					if id == row.Sync.ID {
						wantOutside = true
						break
					}
				}
				if row.Publication.OutsideSelection != wantOutside {
					t.Fatalf("session %q outsideSelection=%t, want %t", row.Sync.ID, row.Publication.OutsideSelection, wantOutside)
				}
				if wantOutside && row.Publication.AutoPublish {
					t.Fatalf("session %q reports autoPublish=true while outsideSelection=true", row.Sync.ID)
				}
				if row.Publication.AutoPublish != slices.Contains(tc.AutoPublish, row.Sync.ID) {
					t.Fatalf("session %q autoPublish=%t, want IDs %v", row.Sync.ID, row.Publication.AutoPublish, tc.AutoPublish)
				}
			}
			if !reflect.DeepEqual(ids, tc.IDs) {
				t.Fatalf("ids=%v want=%v", ids, tc.IDs)
			}
			if tc.InstalledBinding {
				var automatic schema.LocalSyncSessionPage
				world.decode(t, http.MethodGet, defaults.RouteSyncSessions.String()+"?view=home&publication=auto", &automatic)
				autoIDs := make([]string, 0, len(automatic.Items))
				for _, row := range automatic.Items {
					autoIDs = append(autoIDs, row.Sync.ID)
				}
				if !slices.Equal(autoIDs, tc.AutoPublish) {
					t.Fatalf("Home auto view IDs=%v, want=%v", autoIDs, tc.AutoPublish)
				}
				assertHomePublicationParity(t, world, automatic.Items)
			}
		})
	}
}

func assertHomePublicationParity(t *testing.T, world *publishingWorld, items []schema.LocalSyncSessionRow) {
	t.Helper()
	ids := make([]string, 0, len(items)+1)
	for _, item := range items {
		ids = append(ids, item.Sync.ID)
	}
	// A stale identifier must stay omitted, not acquire invented membership.
	ids = append(ids, "missing-stored-session")
	var publications schema.LocalPublicationsResponse
	world.decode(t, http.MethodGet, defaults.RoutePublications.String()+"?sessionIds="+strings.Join(ids, ","), &publications)
	if len(publications.Publications) != len(items) {
		t.Fatalf("by-ID publications = %+v, want exactly %d stored rows", publications.Publications, len(items))
	}
	for i, item := range items {
		if !reflect.DeepEqual(publications.Publications[i], item.Publication) {
			t.Errorf("session %q Home publication %+v differs from by-ID %+v", item.Sync.ID, item.Publication, publications.Publications[i])
		}
	}
}

func TestSyncHomeAccountPublications(t *testing.T) {
	var f struct {
		RequiredNames []string `yaml:"requiredNames"`
		Sessions      []string
		Receipts      []struct {
			Session, Origin, Owner, Project, Transcript string
			Updated                                     int64
			Poison                                      bool
		}
		Cases []struct {
			Name, Origin, Query, Transcript string
			IDs, States                     []string
			Next, Second                    bool
			Status                          int
		}
	}
	if err := testutil.DecodeNamedFixtureYAML(homePublicationsYAML, &f); err != nil {
		t.Fatal(err)
	}
	db := storetest.Open(t)
	seedHomeSessions(t, db, f.Sessions)
	homeExec(t, db, `UPDATE sessions SET parent_id='sess_a' WHERE session_id='sess_child'`)
	homeExec(t, db, `UPDATE sessions SET pushed_at=1000`)
	for _, receipt := range f.Receipts {
		response := schema.AuthoritativePublishResponse{TranscriptID: schema.TranscriptID(receipt.Transcript), TranscriptURL: "https://village.example/transcripts/" + receipt.Transcript, UpdatedAt: receipt.Updated, Visibility: schema.VisibilityPrivate, ContentHash: schema.TranscriptContentHash(strings.Repeat("e", 64)), RequestOperationFingerprint: schema.PublishRequestFingerprint(strings.Repeat("f", 64)), BlobKey: "transcripts/fixture", BlobSizeBytes: 1, PublishedAt: 1, Applied: schema.PublishAppliedState{Associations: []schema.PublishedAssociation{}, NormalizedValues: schema.PublishNormalizedValues{RootHarness: schema.HarnessOpenCode, EntryHarnesses: []schema.Harness{}, Visibility: schema.VisibilityPrivate, SchemaVersion: fmt.Sprint(ingest.CurrentSchemaVersion)}}}
		if err := response.Validate(); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Poison {
			raw = []byte(`"invalid receipt object"`)
		}
		homeExec(t, db, `INSERT INTO session_publications(village_origin,owner_user_id,session_id,remote_transcript_id,transcript_url,project_hash,operation_fingerprint,content_hash,visibility,published_at,remote_updated_at,receipt_json) VALUES(?,?,?,?,?,?,?,?,'private',1,?,?)`, receipt.Origin, receipt.Owner, receipt.Session, receipt.Transcript, "https://village.example/transcripts/"+receipt.Transcript, strings.Repeat(receipt.Project, 64), strings.Repeat("f", 64), strings.Repeat("e", 64), receipt.Updated, string(raw))
	}
	for _, tc := range f.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			hs := newTestXDGHomes(t)
			if tc.Origin != "" {
				writeSyncDoorCredentials(t, hs.Config, tc.Origin)
			}
			base := startDiscoveryHTTPServer(t, hs.config(ServerConfig{Store: db, Config: config.BaseConfig(), Provider: NewStoreDataProvider(db, sessionvisibility.Browsing())}))
			query := "view=home&" + tc.Query
			read := func(query string, status int) schema.LocalSyncSessionPage {
				t.Helper()
				resp, err := http.Get(base + defaults.RouteSyncSessions.String() + "?" + query)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != status {
					var e errorEnvelope
					_ = json.NewDecoder(resp.Body).Decode(&e)
					t.Fatalf("status=%d want=%d error=%+v", resp.StatusCode, status, e)
				}
				var p schema.LocalSyncSessionPage
				if status == 200 {
					if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
						t.Fatal(err)
					}
					if err := p.Validate(); err != nil {
						t.Fatal(err)
					}
				}
				return p
			}
			if tc.Second {
				first := read(query, 200)
				query += "&pageToken=" + url.QueryEscape(string(first.Next))
			}
			status := tc.Status
			if status == 0 {
				status = 200
			}
			p := read(query, status)
			if status != 200 {
				return
			}
			ids, states := []string{}, []string{}
			for _, row := range p.Items {
				ids = append(ids, row.Sync.ID)
				states = append(states, row.Publication.State.String())
				if tc.Transcript != "" && (row.Publication.TranscriptID == nil || row.Publication.TranscriptID.String() != tc.Transcript) {
					t.Fatal(row.Publication)
				}
			}
			if !reflect.DeepEqual(ids, tc.IDs) || !reflect.DeepEqual(states, tc.States) || (p.Next != "") != tc.Next {
				t.Fatalf("got ids=%v states=%v next=%q; want ids=%v states=%v next=%v", ids, states, p.Next, tc.IDs, tc.States, tc.Next)
			}
		})
	}
}

func TestSyncHomeProjectOptions(t *testing.T) {
	var f struct {
		RequiredNames []string `yaml:"requiredNames"`
		Sessions      []struct {
			ID, Project, Name, Parent string
			Unresolved                bool
		}
		Cases []struct {
			Name               string
			Selected, Projects []string
			Narrow             bool
			InvalidSelection   bool `yaml:"invalidSelection"`
			Conflicting        bool `yaml:"conflictingSelection"`
		}
	}
	if err := testutil.DecodeNamedFixtureYAML(syncProjectOptionsYAML, &f); err != nil {
		t.Fatal(err)
	}
	db := storetest.Open(t)
	ids := []string{}
	for _, s := range f.Sessions {
		ids = append(ids, s.ID)
	}
	seedHomeSessions(t, db, ids)
	for _, s := range f.Sessions {
		hash := strings.Repeat(s.Project, 64)
		homeExec(t, db, `INSERT INTO projects(project_hash,canonical_cwd) VALUES(?,?) ON CONFLICT(project_hash) DO UPDATE SET canonical_cwd=excluded.canonical_cwd`, hash, "/fixture/"+s.Name)
		homeExec(t, db, `UPDATE sessions SET project_hash=?,parent_id=NULLIF(?,'') WHERE session_id=?`, hash, s.Parent, s.ID)
		if s.Unresolved {
			homeExec(t, db, `UPDATE sessions SET latest_attempt_generation_id='attempt' WHERE session_id=?`, s.ID)
			homeExec(t, db, `INSERT INTO session_relationship_evidence(session_id,generation_id,ordinal,kind,target_state,evidence) VALUES(?,'attempt',0,'started_by','unknown','existing_adapter')`, s.ID)
		}
	}
	for _, tc := range f.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			cfg := config.BaseConfig()
			if tc.Narrow {
				cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{defaults.HarnessOpenCode.String(): {Sessions: tc.Selected}}}
			}
			if tc.InvalidSelection {
				cfg.Selection = config.SelectionConfig{Mode: config.SelectionMode("hostile-invalid")}
			}
			if tc.Conflicting {
				cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{defaults.HarnessOpenCode.String(): {Projects: []config.ProjectSelection{
					{GitRemote: "example.test/org/repo", Branches: []string{"main"}},
					{Name: "/repo", Branches: []string{"dev"}},
				}}}}
			}
			r := &boundedHomeReader{Store: db, allowed: map[string]bool{}, reads: map[string][]string{}}
			response := httptest.NewRecorder()
			homeTestHandler(t, r, db, cfg).ServeHTTP(response, httptest.NewRequest(http.MethodGet, defaults.RouteSyncSessions.String()+"?view=project-options", nil))
			if response.Code != 200 {
				t.Fatal(response.Body.String())
			}
			var options schema.LocalSyncProjectOptions
			if err := json.Unmarshal(response.Body.Bytes(), &options); err != nil {
				t.Fatal(err)
			}
			if err := options.Validate(); err != nil {
				t.Fatal(err)
			}
			got := []string{}
			for _, p := range options.Projects {
				got = append(got, p.ProjectHash.String()[:1])
			}
			if !reflect.DeepEqual(got, tc.Projects) {
				t.Fatalf("projects=%v want=%v", got, tc.Projects)
			}
			if len(r.reads) != 0 || len(r.windows) != 0 {
				t.Fatalf("project options performed page enrichment/window reads: %+v", r)
			}
			if r.cohortRows != len(f.Sessions) {
				t.Fatalf("incomplete identity cohort: %d", r.cohortRows)
			}
		})
	}
}

func TestSyncHomeCancellationAndUnavailable(t *testing.T) {
	db := storetest.Open(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := db.HomeIdentityCohort(ctx); err == nil {
		t.Fatal("cancelled identity read succeeded")
	}
	syncPageRequest(t, homeTestHandler(t, nil, db, config.BaseConfig()), "view=home", 503)
}

// TestSyncHomeRepositoryIdentityEvidence pins Home's publication-metadata
// identity boundary: browsing no longer filters by the saved selector, but an
// ambiguous captured worktree still prevents outsideSelection from being
// reported truthfully, while a project recorded outside Git stays ordinary.
func TestSyncHomeRepositoryIdentityEvidence(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name           string   `yaml:"name"`
			StoredWorktree bool     `yaml:"storedWorktree"`
			Status         int      `yaml:"status"`
			IDs            []string `yaml:"ids"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(homeRepositoryEvidenceYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	const sessionID = "session-recorded-directory"
	projectHash := strings.Repeat("a", 64)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			db := storetest.Open(t)
			seedHomeSessions(t, db, []string{sessionID})
			// A real directory that is not a Git worktree: the production
			// resolver must report that the directory is outside Git.
			physical, err := ingest.NewPhysicalPathResolver().Resolve(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			homeExec(t, db, `INSERT INTO projects(project_hash,canonical_cwd) VALUES(?,?) ON CONFLICT(project_hash) DO UPDATE SET canonical_cwd=excluded.canonical_cwd`, projectHash, physical.String())
			if tc.StoredWorktree {
				homeExec(t, db, `UPDATE sessions SET git_worktree=? WHERE session_id=?`, physical.String(), sessionID)
			}
			cfg := config.BaseConfig()
			cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{
				// A path-bound project: only exact clone-path evidence selects
				// this session, so an offered row proves the recorded directory
				// still reaches the canonical matcher.
				defaults.HarnessOpenCode.String(): {Projects: []config.ProjectSelection{{Name: "unrelated-name", ClonePaths: []string{physical.String()}}}},
			}}
			base := startDiscoveryHTTPServer(t, ServerConfig{Store: db, Config: cfg, Provider: NewStoreDataProvider(db, sessionvisibility.New(cfg.Selection)), ConfigHome: t.TempDir()})
			world := &publishingWorld{db: db, baseURL: base}
			sh := &syncHandler{store: db, config: newLiveConfig(cfg)}
			sh.home = &homeHandler{reader: db, sync: sh, publishing: &publishingHandler{store: db, configHome: t.TempDir()}, resolver: ingest.NewGitRepositoryIdentityResolver()}
			response := httptest.NewRecorder()
			http.HandlerFunc(sh.handleSyncSessions).ServeHTTP(response, httptest.NewRequest(http.MethodGet, defaults.RouteSyncSessions.String()+"?view=home", nil))
			if response.Code != tc.Status {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tc.Status, response.Body.String())
			}
			if tc.Status == http.StatusOK {
				var page schema.LocalSyncSessionPage
				if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				if err := page.Validate(); err != nil {
					t.Fatal(err)
				}
				ids := make([]string, 0, len(page.Items))
				for _, item := range page.Items {
					ids = append(ids, item.Sync.ID)
				}
				if !slices.Equal(ids, tc.IDs) {
					t.Fatalf("ids = %v, want %v", ids, tc.IDs)
				}
				assertHomePublicationParity(t, world, page.Items)
				return
			}
			var body errorEnvelope
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(body.Error, sessionID) || !strings.Contains(body.Error, "ambiguous") {
				t.Fatalf("failure body = %q, want the fail-closed topology error naming %q", body.Error, sessionID)
			}
			assertPublicationMetadataFailure(t, world, []string{sessionID}, "ambiguous", "Restore")
		})
	}
}
