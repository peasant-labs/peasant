package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// The publishing world: two stored sessions, a saved selection that lists only
// one of them, and a Village double that knows four collectives.
const (
	outsideSessionID = "11112222-3333-4444-8555-666677778888"
	insideSessionID  = "22223333-4444-4555-8666-777788889999"
	unknownSessionID = "33334444-5555-4666-8777-88889999aaaa"
)

var (
	outsideProject = schema.ProjectHash(strings.Repeat("1", 64))
	insideProject  = schema.ProjectHash(strings.Repeat("2", 64))
	acmeOrg        = "acme"
)

// publishingCollectives are the collectives the Village double knows, by alias.
var publishingCollectives = map[string]testutil.VillageCollective{
	"platform":  {ID: "11111111-1111-4111-8111-111111111111", Name: "platform", Acceptance: schema.VillageGroupAcceptanceOpen, Member: true, Repositories: []string{"acme/tools"}},
	"research":  {ID: "22222222-2222-4222-8222-222222222222", Name: "research", Acceptance: schema.VillageGroupAcceptanceOpen, Member: true, LinkedGithubOrg: &acmeOrg},
	"review":    {ID: "33333333-3333-4333-8333-333333333333", Name: "review", Acceptance: schema.VillageGroupAcceptanceCurated, Member: true},
	"outsiders": {ID: "44444444-4444-4444-8444-444444444444", Name: "outsiders", Acceptance: schema.VillageGroupAcceptanceOpen, Member: false},
}

// publishingSessions maps a fixture alias to its session ID.
var publishingSessions = map[string]string{"outside": outsideSessionID, "inside": insideSessionID, "unknown": unknownSessionID}

type publishingWorld struct {
	hs      testXDGHomes
	db      *store.Store
	village *testutil.CollectiveVillage
	baseURL string
}

// newPublishingWorld seeds the two sessions, signs this computer in to the
// Village double, and serves the local API with the selection that lists only
// the acme/tools session.
func newPublishingWorld(t *testing.T) *publishingWorld {
	t.Helper()
	hs := newTestXDGHomes(t)
	if err := os.MkdirAll(filepath.Dir(hs.dbPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	storetest.CopyGoldenTo(t, hs.dbPath())
	db, err := store.Open(hs.dbPath(), store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	seedPublishingSession(t, db, outsideSessionID, outsideProject, "git@github.com:user/repo.git")
	seedPublishingSession(t, db, insideSessionID, insideProject, "https://github.com/acme/tools.git")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(hs.dbPath(), store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	collectives := make([]testutil.VillageCollective, 0, len(publishingCollectives))
	for _, alias := range []string{"platform", "research", "review", "outsiders"} {
		collectives = append(collectives, publishingCollectives[alias])
	}
	remote := testutil.NewCollectiveVillage(t, collectives...)
	writeSyncDoorCredentials(t, hs.Config, remote.URL())

	cfg := config.BaseConfig()
	cfg.Output.BasePath = filepath.Join(hs.Data, "peasant-sync")
	cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{
		defaults.HarnessClaudeCode.String(): {Projects: []config.ProjectSelection{{GitRemote: "git@github.com:acme/tools.git"}}},
	}}
	policy, err := sessionvisibility.New(cfg.Selection)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := NewServer(hs.config(ServerConfig{Port: 0, Store: db, Config: cfg, Provider: NewStoreDataProvider(db, policy)}))
	if err := server.Listen(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return &publishingWorld{hs: hs, db: db, village: remote, baseURL: "http://" + server.Addr().String()}
}

func seedPublishingSession(t *testing.T, db *store.Store, sessionID string, project schema.ProjectHash, remote string) {
	t.Helper()
	const startMs int64 = 1700000000000
	ingested := startMs + 120000
	branch := "main"
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = schema.SessionID(sessionID)
	meta.HostSlug = schema.HostSlug("github.com-synthetic")
	meta.ModelHarness = defaults.HarnessClaudeCode
	meta.Model = testutil.TestModel
	meta.Timestamp = ingest.TimestampInfo{Start: startMs, End: startMs + 60000, Ingested: &ingested}
	meta.Source = ingest.SourceInfo{FilePath: "/synthetic/" + sessionID + ".jsonl", Format: ingest.SourceFormatJSONL}
	meta.Project = ingest.ProjectInfo{Hash: project, Name: "synthetic"}
	meta.Stats = ingest.StatsInfo{TurnCount: 1, DurationMs: 60000}
	meta.Git = ingest.GitContext{Remote: &remote, Branch: &branch}
	text := "a synthetic turn"
	testutil.SeedReadyPublication(t, db, &meta, []schema.SessionEntry{{SessionID: meta.SessionID, EntryIndex: 1, Role: schema.RoleUser, Harness: schema.Harness(defaults.HarnessClaudeCode), EntryType: schema.EntryTypeText, ContentPreview: &text}})
}

// request sends one request to the local API and returns its status and body.
func (w *publishingWorld) request(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, w.baseURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get(defaults.HeaderContentType); !strings.HasPrefix(got, defaults.ContentJSON.String()) {
		t.Errorf("%s %s answered Content-Type %q; every publishing route answers JSON", method, path, got)
	}
	return resp.StatusCode, raw
}

// decode sends one request and decodes its 200 answer with the schema type.
func (w *publishingWorld) decode(t *testing.T, method, path string, into contractValidator) {
	t.Helper()
	status, body := w.request(t, method, path, nil)
	decodeContract(t, status, body, into)
}

// publish publishes one session from the local web and shares it with the
// collectives, failing the test unless every step applied.
func (w *publishingWorld) publish(t *testing.T, sessionID string, add ...string) schema.SyncPushResponse {
	t.Helper()
	request := schema.SyncPushRequest{SessionIDs: []string{sessionID}}
	if len(add) > 0 {
		request.Collectives = &schema.SyncPushCollectives{}
		for _, alias := range add {
			request.Collectives.Add = append(request.Collectives.Add, publishingCollectives[alias].ID)
		}
	}
	status, body := w.request(t, http.MethodPost, defaults.RouteSyncPush.String(), request)
	var response schema.SyncPushResponse
	decodeContract(t, status, body, &response)
	if response.Errors != 0 {
		t.Fatalf("publish %s: %s", sessionID, body)
	}
	return response
}

// contractValidator is a schema type that checks its own invariants.
type contractValidator interface{ Validate() error }

// decodeContract decodes a 200 answer the way a client does: with the schema
// module's type, refusing unknown fields, and its validation.
func decodeContract(t *testing.T, status int, body []byte, into contractValidator) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("the answer does not decode as %T: %v; body %s", into, err, body)
	}
	if err := into.Validate(); err != nil {
		t.Fatalf("the answer breaks the contract: %v; body %s", err, body)
	}
}

// decodeRefusal checks a refusal's status and error code.
func decodeRefusal(t *testing.T, status int, body []byte, wantStatus int, wantCode string) errorEnvelope {
	t.Helper()
	var refusal errorEnvelope
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("the refusal is not a JSON error envelope: %v; body %s", err, body)
	}
	if status != wantStatus || refusal.Code != wantCode || refusal.Error == "" {
		t.Fatalf("refusal = %d %+v, want %d with code %q and a reason", status, refusal, wantStatus, wantCode)
	}
	return refusal
}

//go:embed testdata/publications.yaml
var publicationsYAML []byte

type publicationsCase struct {
	Name          string                               `yaml:"name"`
	Published     []string                             `yaml:"published"`
	VillageShares map[string]schema.VillageShareStatus `yaml:"villageShares"`
	FailedAttempt []string                             `yaml:"failedAttempt"`
	SignedOut     bool                                 `yaml:"signedOut"`
	VillageDown   bool                                 `yaml:"villageDown"`
	Query         struct {
		SessionIDs string `yaml:"sessionIds"`
		Include    string `yaml:"include"`
	} `yaml:"query"`
	Expect struct {
		Status int    `yaml:"status"`
		Code   string `yaml:"code"`
		Rows   []struct {
			Session          string                               `yaml:"session"`
			State            schema.LocalPublicationState         `yaml:"state"`
			OutsideSelection bool                                 `yaml:"outsideSelection"`
			Transcript       bool                                 `yaml:"transcript"`
			LastAttempt      bool                                 `yaml:"lastAttempt"`
			Audience         map[string]schema.VillageShareStatus `yaml:"audience"`
		} `yaml:"rows"`
	} `yaml:"expect"`
}

// TestPublicationsReadIgnoresTheSavedSelection runs testdata/publications.yaml
// against the mounted route. A session the saved selection leaves out of the
// lists is still returned, marked outsideSelection, and every answer decodes
// with the schema module's types.
func TestPublicationsReadIgnoresTheSavedSelection(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string           `yaml:"requiredNames"`
		Cases         []publicationsCase `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(publicationsYAML, &fixture); err != nil {
		t.Fatalf("testdata/publications.yaml: %v", err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runPublicationsCase(t, c)
		})
	}
}

func runPublicationsCase(t *testing.T, c publicationsCase) {
	world := newPublishingWorld(t)
	for _, alias := range c.Published {
		world.publish(t, publishingSessions[alias], "platform")
	}
	for alias, status := range c.VillageShares {
		world.village.Share(schema.TranscriptID(insideSessionID), publishingCollectives[alias].ID, status)
	}
	for _, alias := range c.FailedAttempt {
		project := outsideProject
		if alias == "inside" {
			project = insideProject
		}
		if err := world.db.RecordPublicationAttempt(t.Context(), store.PublicationAttemptDiagnostic{VillageOrigin: world.village.URL(), OwnerUserID: "user-1", SessionID: publishingSessions[alias], ProjectHash: project, AttemptedAt: 1800000000000, Stage: store.PublicationAttemptStagePublish, Message: "Village rejected the request"}); err != nil {
			t.Fatal(err)
		}
	}
	if c.SignedOut {
		if err := os.Remove(filepath.Join(string(defaults.ResolveConfigDirPathWith(world.hs.Config)), string(defaults.CredentialsFile))); err != nil {
			t.Fatal(err)
		}
	}
	if c.VillageDown {
		world.village.Close()
	}

	ids := strings.Split(c.Query.SessionIDs, ",")
	for i, alias := range ids {
		if id, ok := publishingSessions[strings.TrimSpace(alias)]; ok {
			ids[i] = id
		}
	}
	query := url.Values{"sessionIds": {strings.Join(ids, ",")}}
	if c.Query.Include != "" {
		query.Set("include", c.Query.Include)
	}
	status, body := world.request(t, http.MethodGet, defaults.RoutePublications.String()+"?"+query.Encode(), nil)
	if c.Expect.Status != http.StatusOK {
		decodeRefusal(t, status, body, c.Expect.Status, c.Expect.Code)
		return
	}
	var response schema.LocalPublicationsResponse
	decodeContract(t, status, body, &response)
	if len(response.Publications) != len(c.Expect.Rows) {
		t.Fatalf("rows = %s, want %d rows", body, len(c.Expect.Rows))
	}
	for i, want := range c.Expect.Rows {
		got := response.Publications[i]
		if got.SessionID != publishingSessions[want.Session] || got.State != want.State || got.OutsideSelection != want.OutsideSelection || (got.TranscriptURL != "") != want.Transcript || (got.LastAttempt != nil) != want.LastAttempt || got.AutoPublish {
			t.Errorf("rows[%d] = %+v, want %+v", i, got, want)
		}
		if want.Audience == nil {
			if got.Audience != nil {
				t.Errorf("rows[%d] audience = %+v, want none: it was not requested or the row is unpublished", i, *got.Audience)
			}
			continue
		}
		if got.Audience == nil || len(*got.Audience) != len(want.Audience) {
			t.Fatalf("rows[%d] audience = %v, want %v", i, got.Audience, want.Audience)
		}
		for _, member := range *got.Audience {
			wantStatus, ok := want.Audience[member.Name]
			if !ok || member.Status != wantStatus || member.CollectiveID != publishingCollectives[member.Name].ID {
				t.Errorf("rows[%d] audience member %+v, want %v", i, member, want.Audience)
			}
		}
	}
}
