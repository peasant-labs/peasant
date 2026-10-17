package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

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

//go:embed testdata/home_pages.yaml
var homePagesYAML []byte

//go:embed testdata/home_ancestry.yaml
var homeAncestryYAML []byte

//go:embed testdata/home_publications.yaml
var homePublicationsYAML []byte

//go:embed testdata/home_selection.yaml
var homeSelectionYAML []byte

type boundedHomeReader struct {
	*store.Store
	allowed map[string]bool
	reads   map[string][]string
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
func (r *boundedHomeReader) HomePageRows(ctx context.Context, ids []string) ([]store.HomePageRow, error) {
	if err := r.record("sync/title", ids); err != nil {
		return nil, err
	}
	return r.Store.HomePageRows(ctx, ids)
}
func (r *boundedHomeReader) LoadPublicationMetadata(ctx context.Context, ids []ingest.SessionID) (map[ingest.SessionID]ingest.PublicationMetadata, error) {
	strings := make([]string, len(ids))
	for i, id := range ids {
		strings[i] = id.String()
	}
	if err := r.record("readiness", strings); err != nil {
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
	h := &homeHandler{reader: reader, sync: &syncHandler{config: newLiveConfig(cfg)}, publishing: &publishingHandler{store: db, configHome: t.TempDir()}, resolver: fixtureRepositoryResolver{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+defaults.RouteHomeSessions.String(), h.handleSessions)
	return mux
}

func TestHomePageBounds(t *testing.T) {
	var f struct {
		RequiredNames []string `yaml:"requiredNames"`
		Roots         int
		NewestIndex   int `yaml:"newestIndex"`
		Cases         []struct {
			Name, Query          string
			Status, First, Count int
			Next                 *int
		}
	}
	if err := testutil.DecodeNamedFixtureYAML(homePagesYAML, &f); err != nil {
		t.Fatal(err)
	}
	db := storetest.Open(t)
	ids := make([]string, f.Roots)
	for i := range ids {
		ids[i] = fmt.Sprintf("sess_root%03d", i)
	}
	seedHomeSessions(t, db, ids)
	newest := ids[f.NewestIndex]
	homeExec(t, db, `UPDATE sessions SET start_ms=start_ms+1 WHERE session_id=?`, newest)
	ids = append([]string{newest}, append(ids[:f.NewestIndex], ids[f.NewestIndex+1:]...)...)
	for _, tc := range f.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			allowed := map[string]bool{}
			want := make([]string, 0, tc.Count)
			for i := 0; i < tc.Count; i++ {
				id := ids[tc.First+i]
				allowed[id] = true
				want = append(want, id)
			}
			reader := &boundedHomeReader{Store: db, allowed: allowed, reads: map[string][]string{}}
			response := httptest.NewRecorder()
			homeTestHandler(t, reader, db, config.BaseConfig()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, defaults.RouteHomeSessions.String()+"?"+tc.Query, nil))
			if response.Code != tc.Status {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			if tc.Status != http.StatusOK {
				if len(reader.reads) != 0 {
					t.Fatalf("rejected query hydrated %v", reader.reads)
				}
				return
			}
			var page schema.LocalHomeSessionsPage
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if err := page.Validate(); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(page.Rows))
			for _, row := range page.Rows {
				got = append(got, row.Sync.ID)
				if row.Title != "stored title" {
					t.Fatalf("title = %q", row.Title)
				}
			}
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(page.NextOffset, tc.Next) {
				t.Fatalf("got %v next %v; want %v next %v", got, page.NextOffset, want, tc.Next)
			}
			for kind, read := range reader.reads {
				actualSet, expectedSet := slices.Clone(read), slices.Clone(want)
				slices.Sort(actualSet)
				slices.Sort(expectedSet)
				if !reflect.DeepEqual(actualSet, expectedSet) {
					t.Fatalf("%s read %v, want only %v", kind, read, want)
				}
			}
			if len(want) > 0 && len(reader.reads) != 4 {
				t.Fatalf("hydration exits not observed: %v", reader.reads)
			}
		})
	}
	// The actual server route is mounted, not merely a directly invoked helper.
	base := startDiscoveryHTTPServer(t, ServerConfig{Store: db, Config: config.BaseConfig(), Provider: NewStoreDataProvider(db, sessionvisibility.All()), ConfigHome: t.TempDir()})
	var page schema.LocalHomeSessionsPage
	resp, err := http.Get(base + defaults.RouteHomeSessions.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(page.Rows) != 25 {
		t.Fatalf("mounted first page status=%d rows=%d", resp.StatusCode, len(page.Rows))
	}
	// The same page operation bounds general children, including their later
	// offsets; a parent is not a helper-group proxy.
	seedHomeSessions(t, db, []string{"sess_parent"})
	homeExec(t, db, `UPDATE sessions SET parent_id='sess_parent' WHERE session_id<>'sess_parent'`)
	for _, tc := range f.Cases {
		if tc.Status != http.StatusOK {
			continue
		}
		t.Run("children/"+tc.Name, func(t *testing.T) {
			allowed := map[string]bool{}
			for i := 0; i < tc.Count; i++ {
				allowed[ids[tc.First+i]] = true
			}
			reader := &boundedHomeReader{Store: db, allowed: allowed, reads: map[string][]string{}}
			response := httptest.NewRecorder()
			homeTestHandler(t, reader, db, config.BaseConfig()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, defaults.RouteHomeSessions.String()+"?parentSessionId=sess_parent&"+tc.Query, nil))
			if response.Code != http.StatusOK {
				t.Fatal(response.Body.String())
			}
			var page schema.LocalHomeSessionsPage
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Rows) != tc.Count || !reflect.DeepEqual(page.NextOffset, tc.Next) {
				t.Fatalf("children count=%d next=%v; want count=%d next=%v", len(page.Rows), page.NextOffset, tc.Count, tc.Next)
			}
			for i, row := range page.Rows {
				if row.Sync.ID != ids[tc.First+i] || row.Sync.ParentSessionID == nil || *row.Sync.ParentSessionID != "sess_parent" {
					t.Fatalf("wrong child row: %+v", row)
				}
			}
		})
	}
}

func TestHomeAncestryAndSelection(t *testing.T) {
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
			generation := "active"
			column := "active_generation_id"
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
			response := httptest.NewRecorder()
			homeTestHandler(t, db, db, cfg).ServeHTTP(response, httptest.NewRequest(http.MethodGet, defaults.RouteHomeSessions.String()+"?"+tc.Query, nil))
			status := tc.Status
			if status == 0 {
				status = http.StatusOK
			}
			if response.Code != status {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			if status != http.StatusOK {
				if tc.InvalidSelection && !strings.Contains(response.Body.String(), selectionVisibilityErrorCode) {
					t.Fatal(response.Body.String())
				}
				return
			}
			var page schema.LocalHomeSessionsPage
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0)
			counts := make([]int, 0)
			for _, row := range page.Rows {
				got = append(got, row.Sync.ID)
				counts = append(counts, row.ChildCount)
			}
			if !reflect.DeepEqual(got, tc.IDs) || !reflect.DeepEqual(counts, tc.Children) {
				t.Fatalf("rows %v children %v; want %v children %v", got, counts, tc.IDs, tc.Children)
			}
		})
	}
}

func TestHomeAccountPublications(t *testing.T) {
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
			Next                            *int
			Status                          int
		}
	}
	if err := testutil.DecodeNamedFixtureYAML(homePublicationsYAML, &f); err != nil {
		t.Fatal(err)
	}
	db := storetest.Open(t)
	seedHomeSessions(t, db, f.Sessions)
	homeExec(t, db, `UPDATE sessions SET parent_id='sess_a' WHERE session_id='sess_child'`)
	// A historical pushed_at is not an account-scoped publication fact.
	homeExec(t, db, `UPDATE sessions SET pushed_at=1000`)
	for _, receipt := range f.Receipts {
		response := schema.AuthoritativePublishResponse{TranscriptID: schema.TranscriptID(receipt.Transcript), TranscriptURL: "https://village.example/transcripts/" + receipt.Transcript, UpdatedAt: receipt.Updated,
			Visibility: schema.VisibilityPrivate, ContentHash: schema.TranscriptContentHash(strings.Repeat("e", 64)), RequestOperationFingerprint: schema.PublishRequestFingerprint(strings.Repeat("f", 64)), BlobKey: "transcripts/fixture", BlobSizeBytes: 1, PublishedAt: 1,
			Applied: schema.PublishAppliedState{Associations: []schema.PublishedAssociation{}, NormalizedValues: schema.PublishNormalizedValues{RootHarness: schema.HarnessOpenCode, EntryHarnesses: []schema.Harness{}, Visibility: schema.VisibilityPrivate, SchemaVersion: fmt.Sprint(ingest.CurrentSchemaVersion)}},
		}
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
		homeExec(t, db, `INSERT INTO session_publications(village_origin,owner_user_id,session_id,remote_transcript_id,transcript_url,project_hash,operation_fingerprint,content_hash,visibility,published_at,remote_updated_at,receipt_json) VALUES(?,?,?,?,?,?,?,?,'private',1,?,?)`, receipt.Origin, receipt.Owner, receipt.Session, receipt.Transcript, "https://village.example/t/"+receipt.Transcript, strings.Repeat(receipt.Project, 64), strings.Repeat("f", 64), strings.Repeat("e", 64), receipt.Updated, string(raw))
	}
	for _, tc := range f.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			hs := newTestXDGHomes(t)
			if tc.Origin != "" {
				writeSyncDoorCredentials(t, hs.Config, tc.Origin)
			}
			base := startDiscoveryHTTPServer(t, hs.config(ServerConfig{Store: db, Config: config.BaseConfig(), Provider: NewStoreDataProvider(db, sessionvisibility.All())}))
			resp, err := http.Get(base + defaults.RouteHomeSessions.String() + "?" + tc.Query)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			status := tc.Status
			if status == 0 {
				status = http.StatusOK
			}
			if resp.StatusCode != status {
				var refusal errorEnvelope
				_ = json.NewDecoder(resp.Body).Decode(&refusal)
				t.Fatalf("status=%d want=%d: %+v", resp.StatusCode, status, refusal)
			}
			if status != http.StatusOK {
				return
			}
			var page schema.LocalHomeSessionsPage
			if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
				t.Fatal(err)
			}
			if err := page.Validate(); err != nil {
				t.Fatal(err)
			}
			ids, states := make([]string, 0), make([]string, 0)
			for _, row := range page.Rows {
				ids = append(ids, row.Sync.ID)
				states = append(states, row.Publication.State.String())
				if tc.Transcript != "" && (row.Publication.TranscriptID == nil || row.Publication.TranscriptID.String() != tc.Transcript) {
					t.Fatalf("wrong newest receipt: %+v", row.Publication)
				}
			}
			if !reflect.DeepEqual(ids, tc.IDs) || !reflect.DeepEqual(states, tc.States) || !reflect.DeepEqual(page.NextOffset, tc.Next) {
				t.Fatalf("got ids=%v states=%v next=%v; want ids=%v states=%v next=%v", ids, states, page.NextOffset, tc.IDs, tc.States, tc.Next)
			}
		})
	}
}

func TestHomeCompleteCloneSelectionCohort(t *testing.T) {
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
			h := &homeHandler{reader: db, sync: &syncHandler{config: newLiveConfig(cfg)}, publishing: &publishingHandler{store: db, configHome: t.TempDir()}, resolver: resolver}
			response := httptest.NewRecorder()
			h.handleSessions(response, httptest.NewRequest(http.MethodGet, defaults.RouteHomeSessions.String(), nil))
			if response.Code != http.StatusOK {
				t.Fatal(response.Body.String())
			}
			var page schema.LocalHomeSessionsPage
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(page.Rows))
			for _, row := range page.Rows {
				ids = append(ids, row.Sync.ID)
			}
			if !reflect.DeepEqual(ids, tc.IDs) {
				t.Fatalf("ids=%v want=%v", ids, tc.IDs)
			}
		})
	}
}

func TestHomeCancellationAndUnavailableStore(t *testing.T) {
	// Cancellation reaches the real SQLite reader before hydration starts.
	db := storetest.Open(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := db.HomeCandidates(ctx, "", ""); err == nil {
		t.Fatal("cancelled metadata read succeeded")
	}
	response := httptest.NewRecorder()
	homeTestHandler(t, nil, db, config.BaseConfig()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, defaults.RouteHomeSessions.String(), nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable store: %d %s", response.Code, response.Body.String())
	}
}
