package push_test

import (
	"context"
	_ "embed"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/share_existing.yaml
var shareExistingYAML []byte

const shareExistingTranscript = schema.TranscriptID("99d59925-36bc-424c-a789-8be54d9702ba")

// shareExistingIDs maps the fixture's collective aliases to identifiers.
var shareExistingIDs = map[string]schema.VillageUUID{
	"one":   "aaaaaaaa-0000-4000-8000-000000000001",
	"two":   "aaaaaaaa-0000-4000-8000-000000000002",
	"three": "aaaaaaaa-0000-4000-8000-000000000003",
}

type shareExistingStep struct {
	Collective     string                     `yaml:"collective"`
	Outcome        schema.SyncPushStepOutcome `yaml:"outcome"`
	Reason         string                     `yaml:"reason"`
	ReasonContains string                     `yaml:"reasonContains"`
}

type shareExistingCase struct {
	Name         string                               `yaml:"name"`
	Collectives  []string                             `yaml:"collectives"`
	Before       map[string]schema.VillageShareStatus `yaml:"before"`
	After        map[string]schema.VillageShareStatus `yaml:"after"`
	SendStatus   int                                  `yaml:"sendStatus"`
	DryRun       bool                                 `yaml:"dryRun"`
	ReadFails    bool                                 `yaml:"readFails"`
	RuleStatuses []schema.VillageShareStatus          `yaml:"ruleStatuses"`
	Sends        [][]string                           `yaml:"sends"`
	Steps        []shareExistingStep                  `yaml:"steps"`
	WantError    string                               `yaml:"wantError"`
}

// fakeSharer answers from the fixture and records every send. Its single-target
// and take-back calls fail the test: the command sends one request and takes
// nothing back.
type fakeSharer struct {
	t      *testing.T
	c      shareExistingCase
	status map[schema.VillageUUID]schema.VillageShareStatus
	sends  [][]string
}

func (f *fakeSharer) ShareTranscript(context.Context, schema.TranscriptID, schema.VillageUUID) error {
	f.t.Error("a single-target share lost the all-target request")
	return nil
}

func (f *fakeSharer) UnshareTranscript(context.Context, schema.TranscriptID, schema.VillageUUID) error {
	f.t.Error("sharing must never take a transcript back")
	return nil
}

func (f *fakeSharer) LatestShareStatus(_ context.Context, _ schema.TranscriptID, id schema.VillageUUID) (schema.VillageShareStatus, error) {
	if f.c.ReadFails {
		return "", errors.New("history unavailable")
	}
	return f.status[id], nil
}

func (f *fakeSharer) ShareTranscriptCollectives(_ context.Context, _ schema.TranscriptID, ids []schema.VillageUUID) error {
	aliases := make([]string, 0, len(ids))
	for _, id := range ids {
		for alias, known := range shareExistingIDs {
			if known == id {
				aliases = append(aliases, alias)
			}
		}
	}
	f.sends = append(f.sends, aliases)
	for alias, status := range f.c.After {
		f.status[shareExistingIDs[alias]] = status
	}
	if f.c.SendStatus != 0 {
		message := map[int]string{http.StatusForbidden: "You do not own this transcript", http.StatusNotFound: "Transcript not found", http.StatusConflict: "already submitted"}[f.c.SendStatus]
		return &village.StatusError{Operation: "share", StatusCode: f.c.SendStatus, Message: message}
	}
	return nil
}

func TestShareExisting(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string            `yaml:"requiredNames"`
		Cases         []shareExistingCase `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(shareExistingYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeSharer{t: t, c: c, status: map[schema.VillageUUID]schema.VillageShareStatus{}}
			for alias, status := range c.Before {
				fake.status[shareExistingIDs[alias]] = status
			}
			ids := make([]schema.VillageUUID, 0, len(c.Collectives))
			for _, alias := range c.Collectives {
				ids = append(ids, shareExistingIDs[alias])
			}
			steps, err := push.ShareExisting(t.Context(), fake, shareExistingTranscript, ids, c.DryRun)
			if c.WantError != "" {
				if err == nil || !strings.Contains(err.Error(), c.WantError) {
					t.Fatalf("error = %v, want %q", err, c.WantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got, want := fake.sends, c.Sends; len(got)+len(want) > 0 && !reflect.DeepEqual(got, want) {
				t.Fatalf("sends = %v, want %v", got, want)
			}
			if len(steps) != len(c.Steps) {
				t.Fatalf("steps = %+v, want %+v", steps, c.Steps)
			}
			for i, want := range c.Steps {
				assertShareExistingStep(t, steps[i], want)
			}
			for _, status := range c.RuleStatuses {
				assertRuleSkips(t, status, c.Steps[0].Reason)
			}
		})
	}
}

func assertShareExistingStep(t *testing.T, got schema.SyncPushStepResult, want shareExistingStep) {
	t.Helper()
	if got.Step != schema.SyncPushStepAddCollective || got.CollectiveID == nil || *got.CollectiveID != shareExistingIDs[want.Collective] {
		t.Fatalf("step = %+v, want an add_collective step for %s", got, want.Collective)
	}
	if got.Outcome != want.Outcome {
		t.Fatalf("outcome = %s (%s), want %s", got.Outcome, got.Reason, want.Outcome)
	}
	if want.ReasonContains != "" && !strings.Contains(got.Reason, want.ReasonContains) {
		t.Fatalf("reason = %q, want it to contain %q", got.Reason, want.ReasonContains)
	}
	if want.ReasonContains == "" && got.Reason != want.Reason {
		t.Fatalf("reason = %q, want %q", got.Reason, want.Reason)
	}
}

// assertRuleSkips runs an update through the real rule path against a transcript
// a collective already decided, and checks it says what the command says.
func assertRuleSkips(t *testing.T, status schema.VillageShareStatus, heldReason string) {
	t.Helper()
	db := seedShareSession(t, false)
	collective := shareExistingIDs["one"]
	remote := testutil.NewCollectiveVillage(t, testutil.VillageCollective{ID: collective, Name: "team", Acceptance: schema.VillageGroupAcceptanceOpen, Member: true})
	creds := &auth.Credentials{APIKey: "test-key", KeyID: "key-1", UserID: "user-1", VillageURL: remote.URL()}
	client := village.NewVillageClient(remote.URL(), creds.APIKey, nil)
	redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := push.NewSharePipeline(db, client, creds, config.BaseConfig(), redactor, []string{testutil.TestSessionUUID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	remote.Share(schema.TranscriptID(testutil.TestSessionUUID), collective, status)
	updated := &push.PushResult{Sessions: []push.SessionPushResult{{SessionID: testutil.TestSessionUUID, Status: push.PushStatusUpdated}}}
	results, err := (push.RuleShare{Store: db, Village: client, Creds: creds, Retry: "peasant village push --force"}).Run(t.Context(), updated, map[string][]schema.VillageUUID{testutil.TestSessionUUID: {collective}})
	if err != nil || len(results) != 1 {
		t.Fatalf("rule share: %+v, %v", results, err)
	}
	ask, skipped, err := push.UndecidedCollectives(t.Context(), client, schema.TranscriptID(testutil.TestSessionUUID), []schema.VillageUUID{collective})
	if err != nil || len(ask) != 0 || len(skipped) != 1 {
		t.Fatalf("undecided: ask %v, skipped %v, %v", ask, skipped, err)
	}
	steps := results[0].Steps
	last := steps[len(steps)-1]
	if last.Outcome != schema.SyncPushStepSkipped || last.Reason != skipped[0].Reason {
		t.Fatalf("rule step = %+v, want the skip %+v", last, skipped[0])
	}
	if (status == schema.VillageShareStatusApproved || status == schema.VillageShareStatusPending) && last.Reason != heldReason {
		t.Fatalf("rule reason = %q, want %q", last.Reason, heldReason)
	}
	for _, request := range remote.Requests() {
		if strings.HasSuffix(request, "/share") {
			t.Fatalf("a decided collective was asked again: %v", remote.Requests())
		}
	}
}

// TestObserveSharerRefusesWithoutItsService proves the wrapper stops before it
// hands back anything that could send.
func TestObserveSharerRefusesWithoutItsService(t *testing.T) {
	t.Parallel()
	sharer, err := push.ObserveSharer(t.Context(), nil, nil, &auth.Credentials{}, &fakeSharer{t: t})
	if err == nil || sharer != nil {
		t.Fatalf("sharer = %v, err = %v, want a refusal", sharer, err)
	}
}
