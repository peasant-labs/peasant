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
	sh := &syncHandler{store: db, config: newLiveConfig(cfg)}
	sh.home = &homeHandler{reader: reader, sync: sh, publishing: &publishingHandler{store: db, configHome: t.TempDir()}, resolver: fixtureRepositoryResolver{}}
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
	if len(want) > 0 && len(reader.reads) != 4 {
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
	base := startDiscoveryHTTPServer(t, ServerConfig{Store: db, Config: config.BaseConfig(), Provider: NewStoreDataProvider(db, sessionvisibility.All()), ConfigHome: t.TempDir()})
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
			var refusal errorEnvelope
			_ = json.Unmarshal(response.Body.Bytes(), &refusal)
			if response.Code != 400 || refusal.Code != tc.Code || !strings.Contains(refusal.Error, "restart") {
				t.Fatalf("scope refusal: %d %+v", response.Code, refusal)
			}
		})
	}
}

func TestSyncHomeAncestryAndSelection(t *testing.T) {
	var f struct {
		RequiredNames []string `yaml:"requiredNames"`
		Sessions      []struct {
			ID, Cache, Snapshot, Kind, State, Target string
			Latest, Inactive                         bool
		}
		Cases []struct {
			Name, Query      string
			IDs              []string
			Children         []int
			Selected         []string
			Status           int
			InvalidSelection bool `yaml:"invalidSelection"`
		}
	}
	if err := testutil.DecodeNamedFixtureYAML(homeAncestryYAML, &f); err != nil {
		t.Fatal(err)
	}
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

func TestSyncHomeCompleteCloneSelectionCohort(t *testing.T) {
	var f struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name, Branch, Explicit string
			Clone                  bool
			DropIndependentMetrics bool `yaml:"dropIndependentMetrics"`
			IDs                    []string
		}
	}
	if err := testutil.DecodeNamedFixtureYAML(homeSelectionYAML, &f); err != nil {
		t.Fatal(err)
	}
	for _, tc := range f.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			db := storetest.Open(t)
			source := loadDiscoveryHTTPFixture(t)
			paths, resolver := seedDiscoveryHTTPStore(t, db, source)
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
			sh := &syncHandler{store: db, config: newLiveConfig(cfg)}
			sh.home = &homeHandler{reader: db, sync: sh, publishing: &publishingHandler{store: db, configHome: t.TempDir()}, resolver: resolver}
			p := syncPageRequest(t, http.HandlerFunc(sh.handleSyncSessions), "view=home", 200)
			ids := []string{}
			for _, row := range p.Items {
				ids = append(ids, row.Sync.ID)
			}
			if !reflect.DeepEqual(ids, tc.IDs) {
				t.Fatalf("ids=%v want=%v", ids, tc.IDs)
			}
		})
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
			base := startDiscoveryHTTPServer(t, hs.config(ServerConfig{Store: db, Config: config.BaseConfig(), Provider: NewStoreDataProvider(db, sessionvisibility.All())}))
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
