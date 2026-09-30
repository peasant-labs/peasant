package push_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/share-steps.yaml
var shareStepsYAML []byte

type shareStepsFixture struct {
	RequiredNames []string                        `yaml:"requiredNames"`
	Collectives   map[string]shareStepsCollective `yaml:"collectives"`
	Cases         []shareStepsCase                `yaml:"cases"`
}

type shareStepsCollective struct {
	ID         schema.VillageUUID                `yaml:"id"`
	Acceptance schema.VillageGroupAcceptanceMode `yaml:"acceptance"`
	Member     bool                              `yaml:"member"`
}

type shareStepsCase struct {
	Name    string   `yaml:"name"`
	Session string   `yaml:"session"`
	Before  []string `yaml:"before"`
	Request struct {
		Add    []string `yaml:"add"`
		Remove []string `yaml:"remove"`
	} `yaml:"request"`
	Village struct {
		Unreachable          bool     `yaml:"unreachable"`
		RequiresNewerPeasant bool     `yaml:"requiresNewerPeasant"`
		FailShare            []string `yaml:"failShare"`
		FailShareRead        bool     `yaml:"failShareRead"`
		WaitingPullRequest   bool     `yaml:"waitingPullRequest"`
	} `yaml:"village"`
	Expect struct {
		Status schema.SyncPushSessionStatus `yaml:"status"`
		Counts struct {
			New     int `yaml:"new"`
			Updated int `yaml:"updated"`
			Skipped int `yaml:"skipped"`
			Errors  int `yaml:"errors"`
		} `yaml:"counts"`
		Steps []struct {
			Step       schema.SyncPushStep        `yaml:"step"`
			Collective string                     `yaml:"collective"`
			Outcome    schema.SyncPushStepOutcome `yaml:"outcome"`
			Reason     bool                       `yaml:"reason"`
		} `yaml:"steps"`
		ErrorContains       []string                             `yaml:"errorContains"`
		TranscriptURL       bool                                 `yaml:"transcriptUrl"`
		WaitingPullRequests int                                  `yaml:"waitingPullRequests"`
		Publishes           *int                                 `yaml:"publishes"`
		Audience            map[string]schema.VillageShareStatus `yaml:"audience"`
	} `yaml:"expect"`
}

// Session shapes a case publishes.
const (
	shareSessionReady        = "ready"
	shareSessionMissingModel = "missing-model"
)

func loadShareStepsFixture(t *testing.T) shareStepsFixture {
	t.Helper()
	var fixture shareStepsFixture
	if err := testutil.DecodeNamedFixtureYAML(shareStepsYAML, &fixture); err != nil {
		t.Fatalf("testdata/share-steps.yaml: %v", err)
	}
	for _, c := range fixture.Cases {
		if c.Session != shareSessionReady && c.Session != shareSessionMissingModel {
			t.Fatalf("case %q names session %q; use %s or %s", c.Name, c.Session, shareSessionReady, shareSessionMissingModel)
		}
		if c.Expect.Status == "" || len(c.Expect.Steps) == 0 || c.Expect.Publishes == nil || c.Expect.Audience == nil {
			t.Fatalf("case %q must state its status, its steps, its publishes, and its audience ({} for none); a missing value would assert nothing", c.Name)
		}
		if (c.Expect.Status == schema.SyncPushSessionError) != (len(c.Expect.ErrorContains) > 0) {
			t.Fatalf("case %q must name what its error says exactly when its status is error", c.Name)
		}
		aliases := append(append(append([]string{}, c.Before...), c.Request.Add...), c.Request.Remove...)
		aliases = append(aliases, c.Village.FailShare...)
		for _, step := range c.Expect.Steps {
			if step.Collective != "" {
				aliases = append(aliases, step.Collective)
			}
		}
		for alias := range c.Expect.Audience {
			aliases = append(aliases, alias)
		}
		for _, alias := range aliases {
			if _, ok := fixture.Collectives[alias]; !ok {
				t.Fatalf("case %q names collective %q, which the fixture does not declare", c.Name, alias)
			}
		}
	}
	return fixture
}

// TestSharePublishSteps runs each case of testdata/share-steps.yaml through the
// real pipeline, the real Village client, and the real store, against a Village
// double, and checks the typed result and what Village holds afterwards.
func TestSharePublishSteps(t *testing.T) {
	t.Parallel()
	fixture := loadShareStepsFixture(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runShareStepsCase(t, fixture, c)
		})
	}
}

func runShareStepsCase(t *testing.T, fixture shareStepsFixture, c shareStepsCase) {
	ctx := context.Background()
	db := seedShareSession(t, c.Session == shareSessionMissingModel)
	sessionID := testutil.TestSessionUUID

	remote := newShareVillage(t, fixture.Collectives)
	for _, alias := range c.Village.FailShare {
		remote.failShare[fixture.Collectives[alias].ID] = true
	}
	remote.failShareRead = c.Village.FailShareRead
	if c.Village.WaitingPullRequest {
		remote.promptRequests = []schema.VillagePromptRequest{
			{Owner: "user", Name: "repo", Number: 7, State: schema.VillagePullRequestAttachmentWaiting, Remote: "user/repo", HeadRemote: "user/repo", RequestedAt: time.Unix(1700000000, 0).UTC()},
			{Owner: "someone", Name: "other", Number: 8, State: schema.VillagePullRequestAttachmentWaiting, Remote: "someone/other", HeadRemote: "someone/other", RequestedAt: time.Unix(1700000000, 0).UTC()},
		}
	}

	creds := &auth.Credentials{APIKey: "test-key", KeyID: "key-1", UserID: "user-1", Username: "tester", VillageURL: remote.server.URL}
	cfg := config.BaseConfig()
	cfg.Output.BasePath = filepath.Join(t.TempDir(), "peasant-sync")
	cfg.Push.Visibility = schema.VisibilityPublic
	cfg.Push.License = schema.License("CC-BY-4.0")
	redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatal(err)
	}
	publish := func(changes push.CollectiveChanges) (push.SharePublishResult, error) {
		client := village.NewVillageClient(creds.VillageURL, creds.APIKey, nil)
		pipeline, err := push.NewSharePipeline(db, client, creds, cfg, redactor, []string{sessionID})
		if err != nil {
			t.Fatal(err)
		}
		return push.SharePublish{Pipeline: pipeline, Store: db, Village: client, Creds: creds}.Run(ctx, []string{sessionID}, changes)
	}
	ids := func(aliases []string) []schema.VillageUUID {
		out := make([]schema.VillageUUID, 0, len(aliases))
		for _, alias := range aliases {
			out = append(out, fixture.Collectives[alias].ID)
		}
		return out
	}

	if len(c.Before) > 0 {
		before, err := publish(push.CollectiveChanges{Add: ids(c.Before)})
		if err != nil || before.Response.New != 1 || before.Response.Errors != 0 {
			t.Fatalf("the earlier publish that shares with %v: %+v, %v", c.Before, before.Response, err)
		}
	}
	if c.Village.Unreachable {
		remote.server.Close()
	}
	remote.setRequiresNewerPeasant(c.Village.RequiresNewerPeasant)
	publishesBefore := remote.publishCount()
	published, err := publish(push.CollectiveChanges{Add: ids(c.Request.Add), Remove: ids(c.Request.Remove)})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	response := published.Response
	assertShareResponseDecodes(t, response)

	if got := [4]int{response.New, response.Updated, response.Skipped, response.Errors}; got != [4]int{c.Expect.Counts.New, c.Expect.Counts.Updated, c.Expect.Counts.Skipped, c.Expect.Counts.Errors} {
		t.Errorf("counts new/updated/skipped/errors = %v, want %+v", got, c.Expect.Counts)
	}
	if len(response.Sessions) != 1 {
		t.Fatalf("sessions = %+v, want one result", response.Sessions)
	}
	result := response.Sessions[0]
	if result.SessionID != sessionID || result.Status != c.Expect.Status {
		t.Errorf("session %s status = %q, want %q (error %q)", result.SessionID, result.Status, c.Expect.Status, result.Error)
	}
	if len(result.Steps) != len(c.Expect.Steps) {
		t.Fatalf("steps = %+v, want %d steps", result.Steps, len(c.Expect.Steps))
	}
	for i, want := range c.Expect.Steps {
		got := result.Steps[i]
		wantCollective := ""
		if want.Collective != "" {
			wantCollective = fixture.Collectives[want.Collective].ID.String()
		}
		gotCollective := ""
		if got.CollectiveID != nil {
			gotCollective = got.CollectiveID.String()
		}
		if got.Step != want.Step || gotCollective != wantCollective || got.Outcome != want.Outcome || (got.Reason != "") != want.Reason {
			t.Errorf("steps[%d] = %s %s %s (reason %q), want %s %s %s (reason given: %v)", i, got.Step, gotCollective, got.Outcome, got.Reason, want.Step, wantCollective, want.Outcome, want.Reason)
		}
	}
	for _, needle := range c.Expect.ErrorContains {
		if !strings.Contains(result.Error, needle) {
			t.Errorf("error %q does not say %q", result.Error, needle)
		}
	}
	if (result.TranscriptURL != "") != c.Expect.TranscriptURL {
		t.Errorf("transcriptUrl = %q, want present: %v", result.TranscriptURL, c.Expect.TranscriptURL)
	}
	if len(result.WaitingPullRequests) != c.Expect.WaitingPullRequests {
		t.Errorf("waitingPullRequests = %+v, want %d for the session's repository", result.WaitingPullRequests, c.Expect.WaitingPullRequests)
	}
	if sent := remote.publishCount() - publishesBefore; sent != *c.Expect.Publishes {
		t.Errorf("Village received %d publish requests, want %d", sent, *c.Expect.Publishes)
	}
	remote.assertPrivateWithoutLicense(t)

	audience := remote.audience()
	want := map[schema.VillageUUID]schema.VillageShareStatus{}
	for alias, status := range c.Expect.Audience {
		want[fixture.Collectives[alias].ID] = status
	}
	if len(audience) != len(want) {
		t.Errorf("Village audience = %v, want %v", audience, want)
	}
	for id, status := range want {
		if audience[id] != status {
			t.Errorf("Village audience = %v, want %v", audience, want)
			break
		}
	}

	if c.Session == shareSessionMissingModel {
		// The consent scan refuses this capture with the same check the push
		// refused it with, so a scan that failed never leads to a publish.
		input, _, err := push.LoadPublicationInput(ctx, db, sessionID)
		if err == nil {
			err = push.ValidatePublicationInput(input)
		}
		if err == nil {
			t.Error("the capture this case publishes passes the scan's check, so the case does not show a scan failure blocking the publish")
		}
	}
}

// assertShareResponseDecodes decodes the response the way a client does: with
// the schema module's type, refusing unknown fields, and its validation.
func assertShareResponseDecodes(t *testing.T, response schema.SyncPushResponse) {
	t.Helper()
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var decoded schema.SyncPushResponse
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("the response does not decode as schema.SyncPushResponse: %v; body %s", err, raw)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("the response breaks the contract: %v; body %s", err, raw)
	}
}

// seedShareSession stores one publishable session of a GitHub repository, or
// one whose capture lacks its model, which the consent scan and the push both
// refuse.
func seedShareSession(t *testing.T, missingModel bool) *store.Store {
	t.Helper()
	path := storetest.CopyGoldenDB(t)
	db, err := store.Open(path, store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = testutil.TestSessionUUID
	meta.HostSlug = testutil.TestHostSlug
	meta.ModelHarness = defaults.HarnessClaudeCode
	meta.Model = testutil.TestModel
	if missingModel {
		meta.Model = ""
	}
	meta.Source = ingest.SourceInfo{FilePath: "/nonexistent/source.jsonl", Format: ingest.SourceFormatJSONL}
	meta.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "synthetic-project"}
	remote, branch := "git@github.com:user/repo.git", "main"
	meta.Git = ingest.GitContext{Remote: &remote, Branch: &branch}
	ingested := int64(1700000120000)
	meta.Timestamp = ingest.TimestampInfo{Start: 1700000000000, End: 1700000060000, Ingested: &ingested}
	meta.Stats = ingest.StatsInfo{TurnCount: 1, DurationMs: 60000}
	content := "a synthetic turn to publish"
	testutil.SeedReadyPublication(t, db, &meta, []schema.SessionEntry{{SessionID: meta.SessionID, EntryIndex: 1, Harness: meta.ModelHarness, Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &content}})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path, store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// shareVillage keeps one transcript and its collectives the way Village does.
type shareVillage struct {
	server      *httptest.Server
	collectives map[schema.VillageUUID]shareStepsCollective
	names       map[schema.VillageUUID]string

	mu             sync.Mutex
	exists         bool
	shares         map[schema.VillageUUID]schema.VillageShareStatus
	publishes      []schema.AuthoritativePublishRequest
	ownerUpdates   int
	failShare      map[schema.VillageUUID]bool
	failShareRead  bool
	promptRequests []schema.VillagePromptRequest
	// requiresNewerPeasant makes Village advertise a push contract window that
	// starts above this build's, so the push stops before it sends anything.
	requiresNewerPeasant bool
}

func (v *shareVillage) setRequiresNewerPeasant(required bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.requiresNewerPeasant = required
}

func newShareVillage(t *testing.T, declared map[string]shareStepsCollective) *shareVillage {
	t.Helper()
	v := &shareVillage{
		collectives: map[schema.VillageUUID]shareStepsCollective{},
		names:       map[schema.VillageUUID]string{},
		shares:      map[schema.VillageUUID]schema.VillageShareStatus{},
		failShare:   map[schema.VillageUUID]bool{},
	}
	for alias, collective := range declared {
		v.collectives[collective.ID] = collective
		v.names[collective.ID] = alias
	}
	transcriptPrefix := "/api/v1/transcripts/" + testutil.TestSessionUUID
	v.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/schema/version":
			window := schema.SchemaVersionResponse{MinPushContractVersion: "0.1.0", PushContractVersion: defaults.PublishSchemaVersion, ContentCapabilities: []schema.ContentCapability{schema.ContentCapabilityObservedModelV1}}
			v.mu.Lock()
			if v.requiresNewerPeasant {
				window.MinPushContractVersion, window.PushContractVersion = "99.0.0", "99.0.0"
			}
			v.mu.Unlock()
			_ = json.NewEncoder(w).Encode(window)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/transcripts/publish":
			v.publish(t, w, r)
		case r.Method == http.MethodPatch && r.URL.Path == transcriptPrefix:
			v.mu.Lock()
			v.ownerUpdates++
			v.mu.Unlock()
			t.Errorf("a publish from the local web sent an owner update; it must keep the audience and license")
			http.Error(w, `{"error":"unexpected owner update"}`, http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == transcriptPrefix+"/share":
			v.share(t, w, r)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, transcriptPrefix+"/share/"):
			v.mu.Lock()
			delete(v.shares, schema.VillageUUID(strings.TrimPrefix(r.URL.Path, transcriptPrefix+"/share/")))
			v.mu.Unlock()
			_ = json.NewEncoder(w).Encode(schema.VillageStatusResponse{Status: "unshared"})
		case r.Method == http.MethodGet && r.URL.Path == transcriptPrefix:
			v.readShares(w)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/users/me/prompt-requests":
			v.mu.Lock()
			requests := append([]schema.VillagePromptRequest{}, v.promptRequests...)
			v.mu.Unlock()
			_ = json.NewEncoder(w).Encode(schema.VillagePromptRequestsResponse{Requests: requests})
		default:
			t.Errorf("unexpected Village request %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":"unexpected"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(v.server.Close)
	return v
}

func (v *shareVillage) publish(t *testing.T, w http.ResponseWriter, r *http.Request) {
	metadata := multipartField(t, r, "metadata")
	request, err := schema.DecodeAuthoritativePublishRequest([]byte(metadata))
	if err != nil {
		t.Errorf("decode publish request: %v", err)
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	created := !v.exists
	v.exists = true
	v.publishes = append(v.publishes, request)
	raw, err := testutil.AuthoritativePublishReceiptFromRequest(request, created)
	var receipt schema.AuthoritativePublishResponse
	if err == nil {
		err = json.Unmarshal(raw, &receipt)
	}
	if err != nil {
		t.Errorf("build receipt: %v", err)
		http.Error(w, `{"error":"receipt"}`, http.StatusInternalServerError)
		return
	}
	// Content lands private; a transcript shared with a collective keeps that
	// audience when it is updated.
	if len(v.shares) > 0 {
		receipt.Visibility = schema.VisibilityGroup
		receipt.Applied.NormalizedValues.Visibility = schema.VisibilityGroup
	}
	if created {
		w.WriteHeader(http.StatusCreated)
	}
	_ = json.NewEncoder(w).Encode(receipt)
}

func (v *shareVillage) share(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var request schema.VillageShareTranscriptRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.GroupIDs) != 1 {
		t.Errorf("share request %+v, %v: the client shares with one collective per request", request, err)
		http.Error(w, `{"error":"Invalid request body"}`, http.StatusBadRequest)
		return
	}
	id := request.GroupIDs[0]
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.failShare[id] {
		http.Error(w, `{"error":"Could not record the submission of this transcript"}`, http.StatusInternalServerError)
		return
	}
	if status, live := v.shares[id]; live && (status == schema.VillageShareStatusPending || status == schema.VillageShareStatusApproved) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(schema.VillageErrorResponse{Error: "This transcript is already submitted to 1 collective"})
		return
	}
	// Village skips a collective the caller is not a member of without an error.
	if collective, known := v.collectives[id]; known && collective.Member {
		status := schema.VillageShareStatusApproved
		if collective.Acceptance == schema.VillageGroupAcceptanceCurated {
			status = schema.VillageShareStatusPending
		}
		v.shares[id] = status
	}
	shares := make([]schema.VillageTranscriptShare, 0, len(v.shares))
	for shared := range v.shares {
		shares = append(shares, schema.VillageTranscriptShare{GroupID: shared, GroupName: v.names[shared], SharedAt: time.Unix(1700000000, 0).UTC()})
	}
	_ = json.NewEncoder(w).Encode(shares)
}

func (v *shareVillage) readShares(w http.ResponseWriter) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.failShareRead {
		http.Error(w, `{"error":"Failed to read the transcript"}`, http.StatusInternalServerError)
		return
	}
	shares := make([]schema.VillageEnrichedTranscriptShare, 0, len(v.shares))
	for id, status := range v.shares {
		shares = append(shares, schema.VillageEnrichedTranscriptShare{TranscriptID: testutil.TestSessionUUID, GroupID: id, GroupName: v.names[id], AcceptanceMode: v.collectives[id].Acceptance, Status: status, SharedAt: time.Unix(1700000000, 0).UTC()})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"enriched_shares": shares})
}

func (v *shareVillage) publishCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.publishes)
}

func (v *shareVillage) audience() map[schema.VillageUUID]schema.VillageShareStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make(map[schema.VillageUUID]schema.VillageShareStatus, len(v.shares))
	for id, status := range v.shares {
		out[id] = status
	}
	return out
}

// assertPrivateWithoutLicense checks that no publish carried a license, although
// the configuration names a default one, and that no owner update moved the
// transcript to the configured public visibility.
func (v *shareVillage) assertPrivateWithoutLicense(t *testing.T) {
	t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	for i, request := range v.publishes {
		if request.License != "" {
			t.Errorf("publish %d carried license %q; a publish from the local web sends none", i, request.License)
		}
	}
	if v.ownerUpdates != 0 {
		t.Errorf("Village received %d owner updates; a publish from the local web opens private and changes no visibility", v.ownerUpdates)
	}
}

func multipartField(t *testing.T, r *http.Request, name string) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		t.Errorf("publish content type: %v", err)
		return ""
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := reader.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() == name {
			data, _ := io.ReadAll(part)
			return string(data)
		}
	}
}
