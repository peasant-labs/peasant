package push_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"strings"
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
	// ChangeContent changes the session's content after the earlier publish,
	// so the case sends it again.
	ChangeContent bool `yaml:"changeContent"`
	Request       struct {
		Add    []string `yaml:"add"`
		Remove []string `yaml:"remove"`
	} `yaml:"request"`
	Village struct {
		Unreachable          bool     `yaml:"unreachable"`
		RequiresNewerPeasant bool     `yaml:"requiresNewerPeasant"`
		FailPublish          bool     `yaml:"failPublish"`
		ConflictShare        []string `yaml:"conflictShare"`
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
		aliases = append(append(aliases, c.Village.FailShare...), c.Village.ConflictShare...)
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

	declared := make([]testutil.VillageCollective, 0, len(fixture.Collectives))
	for alias, collective := range fixture.Collectives {
		declared = append(declared, testutil.VillageCollective{ID: collective.ID, Name: alias, Acceptance: collective.Acceptance, Member: collective.Member})
	}
	remote := testutil.NewCollectiveVillage(t, declared...)
	for _, alias := range c.Village.FailShare {
		remote.FailShare(fixture.Collectives[alias].ID)
	}
	for _, alias := range c.Village.ConflictShare {
		remote.ConflictShare(fixture.Collectives[alias].ID)
	}
	remote.FailShareRead(c.Village.FailShareRead)
	if c.Village.WaitingPullRequest {
		remote.SetPromptRequests(
			schema.VillagePromptRequest{Owner: "user", Name: "repo", Number: 7, State: schema.VillagePullRequestAttachmentWaiting, Remote: "user/repo", HeadRemote: "user/repo", RequestedAt: time.Unix(1700000000, 0).UTC()},
			schema.VillagePromptRequest{Owner: "someone", Name: "other", Number: 8, State: schema.VillagePullRequestAttachmentWaiting, Remote: "someone/other", HeadRemote: "someone/other", RequestedAt: time.Unix(1700000000, 0).UTC()},
		)
	}

	creds := &auth.Credentials{APIKey: "test-key", KeyID: "key-1", UserID: "user-1", Username: "tester", VillageURL: remote.URL()}
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
		remote.Close()
	}
	if c.ChangeContent {
		seedShareContent(t, db, false, "the second draft of this session")
	}
	remote.RequireNewerPeasant(c.Village.RequiresNewerPeasant)
	remote.FailPublish(c.Village.FailPublish)
	publishesBefore := len(remote.Publishes())
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
	if sent := len(remote.Publishes()) - publishesBefore; sent != *c.Expect.Publishes {
		t.Errorf("Village received %d publish requests, want %d", sent, *c.Expect.Publishes)
	}
	// The configuration names a default license and a public visibility; a
	// publish from the local web sends neither.
	for i, request := range remote.Publishes() {
		if request.License != "" {
			t.Errorf("publish %d carried license %q; a publish from the local web sends none", i, request.License)
		}
	}
	if updates := remote.OwnerUpdates(); updates != 0 {
		t.Errorf("Village received %d owner updates; a publish from the local web opens private and changes no visibility", updates)
	}

	audience := remote.Audience(testutil.TestSessionUUID)
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
	seedShareContent(t, db, missingModel, "a synthetic turn to publish")
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

// seedShareContent stores the session's capture with one user turn carrying
// the text. Storing it again with other text changes the session.
func seedShareContent(t *testing.T, db *store.Store, missingModel bool, text string) {
	t.Helper()
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
	testutil.SeedReadyPublication(t, db, &meta, []schema.SessionEntry{{SessionID: meta.SessionID, EntryIndex: 1, Harness: meta.ModelHarness, Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &text}})
}
