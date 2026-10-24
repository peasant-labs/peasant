package collectiveobservations

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/fetch.yaml
var fetchFixtureYAML []byte

type fetchCase struct {
	Name                 string                               `yaml:"name"`
	MembershipEmpty      bool                                 `yaml:"membershipEmpty"`
	ContributionEmpty    bool                                 `yaml:"contributionEmpty"`
	Org                  bool                                 `yaml:"org"`
	Track                bool                                 `yaml:"track"`
	FailPath             string                               `yaml:"failPath"`
	FailStatus           int                                  `yaml:"failStatus"`
	Cancel               bool                                 `yaml:"cancel"`
	DiscoveryBody        string                               `yaml:"discoveryBody"`
	AssociationFailure   bool                                 `yaml:"associationFailure"`
	RotateDuringHistory  bool                                 `yaml:"rotateDuringHistory"`
	RotateBeforeAuth     bool                                 `yaml:"rotateBeforeAuth"`
	HistoryOnly          bool                                 `yaml:"historyOnly"`
	HistoryStatus        schema.VillageShareStatus            `yaml:"historyStatus"`
	WantReasons          []schema.CollectiveObservationReason `yaml:"wantReasons"`
	WantCoverage         schema.CollectiveEvidenceCoverage    `yaml:"wantCoverage"`
	AwaitHistory         bool                                 `yaml:"awaitHistory"`
	WantStatus           int                                  `yaml:"wantStatus"`
	WantName             string                               `yaml:"wantName"`
	WantEvent            schema.VillageShareStatus            `yaml:"wantEvent"`
	RetainedDomain       schema.CollectiveFetchDomain         `yaml:"retainedDomain"`
	WantObservationState schema.CollectiveObservationState    `yaml:"wantObservationState"`
}

func TestFetchIndependentCompleteDomains(t *testing.T) {
	var fixtures struct {
		RequiredNames []string    `yaml:"requiredNames"`
		Cases         []fetchCase `yaml:"cases"`
	}
	d := yaml.NewDecoder(strings.NewReader(string(fetchFixtureYAML)))
	d.KnownFields(true)
	if err := d.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range fixtures.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatal("missing/duplicate fetch case")
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required fetch case %s absent", name)
		}
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			db := storetest.Open(t)
			home := t.TempDir()
			var phase atomic.Int32
			operation, stop := context.WithCancel(t.Context())
			defer stop()
			const collective = "33333333-3333-4333-8333-333333333333"
			const transcript = "22222222-2222-4222-8222-222222222222"
			creds := auth.Credentials{APIKey: "fixture-key", KeyID: "fixture-key-id", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture"}
			var rotated atomic.Bool
			rotate := func() {
				if rotated.Swap(true) {
					return
				}
				other := creds
				other.APIKey, other.KeyID, other.UserID = "other-key", "other-id", "77777777-7777-4777-8777-777777777777"
				if err := auth.SaveCredentialsFrom(&other, home); err != nil {
					t.Error(err)
				}
			}
			// These cases deliberately cancel AFTER actual earlier domain commits.
			// Observe real Store rows with a two-second deadline; no production
			// hook or SQL replacement can manufacture the committed effect.
			awaitCommitted := func(request context.Context, catalogToo, historyToo bool) bool {
				ctx, cancel := context.WithTimeout(request, 2*time.Second)
				defer cancel()
				snapshot, err := auth.CaptureCredentials(ctx, home)
				if err != nil {
					t.Error(err)
					return false
				}
				ticks := time.NewTicker(5 * time.Millisecond)
				defer ticks.Stop()
				for {
					ready := true
					if catalogToo {
						catalog, err := db.ReadCollectiveObservations(ctx, snapshot)
						ready = err == nil && len(catalog.Collectives) == 1 && catalog.Collectives[0].Name == "New"
					}
					if ready && historyToo {
						attempt, err := db.ReadContributionAttempt(ctx, snapshot, collective, transcript, 1)
						ready = err == nil && attempt != nil && attempt.Attempt.Status == schema.VillageShareStatusApproved
					}
					if ready {
						return true
					}
					select {
					case <-ticks.C:
					case <-ctx.Done():
						t.Errorf("earlier domain did not commit before cancellation: %v", ctx.Err())
						return false
					}
				}
			}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Errorf("non-metadata or wrong-credential request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(400)
					return
				}
				current := phase.Load() > 0
				name := "Old"
				if current {
					name = "New"
				}
				if current && c.FailPath == r.URL.Path {
					if c.Cancel {
						if !awaitCommitted(r.Context(), !c.AwaitHistory, c.AwaitHistory) {
							w.WriteHeader(500)
							return
						}
						stop()
						<-r.Context().Done()
						return
					}
					w.WriteHeader(c.FailStatus)
					_, _ = w.Write([]byte(`{"error":"fixture unavailable"}`))
					return
				}
				var value any
				switch r.URL.Path {
				case "/api/v1/auth/me":
					value = schema.VillageUser{ID: schema.VillageUUID(creds.UserID)}
				case "/api/v1/transcripts/" + transcript + "/collectives":
					value = schema.VillageTranscriptCollectivesResponse{Collectives: []schema.VillageTranscriptCollective{}}
				case "/api/v1/transcripts/44444444-4444-4444-8444-444444444444/collectives":
					if current && c.AssociationFailure {
						if !awaitCommitted(r.Context(), true, true) {
							w.WriteHeader(500)
							return
						}
						stop()
						<-r.Context().Done()
						return
					}
					value = schema.VillageTranscriptCollectivesResponse{Collectives: []schema.VillageTranscriptCollective{}}
				case "/api/v1/collectives":
					rows := []schema.VillageUserGroup{}
					if !current || !c.MembershipEmpty {
						rows = append(rows, schema.VillageUserGroup{Creator: schema.AnonymousVillageOwner(), ID: collective, Name: name, Role: "member", AcceptanceMode: "open", DataAccess: "public", MemberSince: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
					}
					value = rows
				case "/api/v1/users/me/collectives/contributions":
					if current && c.DiscoveryBody != "" {
						_, _ = w.Write([]byte(c.DiscoveryBody))
						return
					}
					rows := []schema.VillageContributedCollective{}
					if !current || !c.ContributionEmpty {
						rows = append(rows, schema.VillageContributedCollective{ID: collective, Name: name, PendingCount: 1})
					}
					if current && c.HistoryOnly {
						rows[0].PendingCount = 0
						rows[0].RejectedAttemptCount = 1
					}
					value = schema.VillageContributedCollectivesResponse{Collectives: rows}
				case "/api/v1/auth/orgs":
					rows := []schema.VillageMetadataUserOrganization{}
					if c.Org {
						rows = append(rows, schema.VillageMetadataUserOrganization{OrgID: 1, OrgLogin: "example", FetchedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
					}
					value = rows
				case "/api/v1/orgs/example":
					value = schema.VillageOrganizationCollectivesResponse{Collectives: []schema.VillageOrganizationCollective{{ID: collective, Name: name}}}
				case "/api/v1/collectives/" + collective:
					value = schema.VillageGroupDetailResponse{Group: schema.VillageGroupDetailRecord{Creator: schema.AnonymousVillageOwner(), ID: collective, Name: name, AcceptanceMode: "open", DataAccess: "public"}}
				case "/api/v1/users/me/collectives/" + collective + "/submissions":
					value = []schema.VillageCollectiveSubmission{{TranscriptID: transcript, GroupID: collective, EventNum: 1, Status: "pending", RecordedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
				case "/api/v1/users/me/collectives/" + collective + "/transcripts/" + transcript + "/events":
					if current && c.RotateDuringHistory {
						if !awaitCommitted(r.Context(), true, false) {
							w.WriteHeader(500)
							return
						}
						rotate()
					}
					event := schema.VillageShareEvent{EventNum: 1, Status: "pending", RecordedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
					if current {
						at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
						event.Status = "approved"
						event.DecidedAt = &at
						event.DecidedByActor = "moderator"
						if c.HistoryStatus != "" {
							event.Status = c.HistoryStatus
							actor, err := schema.VillageContributionAttemptActor(event.Status)
							if err != nil {
								t.Error(err)
							}
							event.DecidedByActor = actor
						}
					}
					value = []schema.VillageShareEvent{event}
				default:
					t.Errorf("unexpected non-narrow metadata route: %s", r.URL.String())
					w.WriteHeader(404)
					return
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer remote.Close()
			creds.VillageURL = remote.URL
			if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
			service := New(Dependencies{Store: db, ConfigHome: home, Now: func() time.Time {
				if phase.Load() > 0 && c.RotateBeforeAuth {
					rotate()
				}
				return at
			}, LocallyHeldTranscriptIDs: func(context.Context, schema.CollectiveObservationScope) ([]schema.TranscriptID, error) {
				if c.AssociationFailure {
					return []schema.TranscriptID{transcript, "44444444-4444-4444-8444-444444444444"}, nil
				}
				return []schema.TranscriptID{}, nil
			}})
			if c.Track {
				if _, err := service.Track(t.Context(), collective); err != nil {
					t.Fatal(err)
				}
			}
			before, err := service.Fetch(t.Context())
			if err != nil {
				t.Fatalf("initial full fetch: %v %#v", err, before)
			}
			previousCatalog, err := service.List(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			previousHistory, err := service.History(t.Context(), schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceRemoteAttempts, Page: 1, Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			phase.Store(1)
			at = at.Add(time.Hour)
			report, err := service.Fetch(operation)
			if (err != nil) != (c.WantStatus != 200) {
				t.Fatalf("report error=%v status=%d domains=%#v", err, schema.CollectiveFetchHTTPStatus(report), report.Domains)
			}
			if validation := report.Validate(); validation != nil {
				t.Fatalf("invalid typed report: %v %#v", validation, report)
			}
			if c.AssociationFailure {
				domain := report.Domains[2]
				counts := domain.Associations.Completed == 1 && domain.Associations.Failed == 1
				scopes := domain.Associations.Scopes[0].State == schema.CollectiveFetchStateComplete && domain.Associations.Scopes[1].State != schema.CollectiveFetchStateComplete
				if domain.Commit != schema.CollectiveFetchCommitUpdated || !counts || !scopes {
					t.Fatalf("per-set effects were lost: %#v", domain)
				}
			}
			if status := schema.CollectiveFetchHTTPStatus(report); status != c.WantStatus {
				t.Fatalf("status=%d want=%d report=%#v", status, c.WantStatus, report)
			}
			if c.RotateBeforeAuth || c.RotateDuringHistory {
				if report.Scope.UserID.String() != creds.UserID {
					t.Fatal("partial report silently reselected a different account")
				}
				other, err := service.List(t.Context())
				if err != nil || len(other.Collectives) != 0 {
					t.Fatalf("old catalog leaked into new account: %#v %v", other, err)
				}
				otherHistory, err := service.History(t.Context(), schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceRemoteAttempts, Page: 1, Limit: 50})
				if err != nil || otherHistory.Total != 0 {
					t.Fatalf("old history leaked into new account: %#v %v", otherHistory, err)
				}
				if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
					t.Fatal(err)
				}
			}
			catalog, err := service.List(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if c.WantName == "" && len(catalog.Collectives) != 0 {
				t.Fatalf("history-only evidence conferred catalog relevance: %#v", catalog.Collectives)
			}
			if c.WantName != "" && (len(catalog.Collectives) != 1 || catalog.Collectives[0].Name != c.WantName) {
				t.Fatalf("catalog=%#v want=%s", catalog.Collectives, c.WantName)
			}
			if c.WantName != "" && c.WantReasons != nil && !reflect.DeepEqual(catalog.Collectives[0].Reasons, c.WantReasons) {
				t.Fatalf("catalog reasons=%v want=%v", catalog.Collectives[0].Reasons, c.WantReasons)
			}
			history, err := service.History(t.Context(), schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceRemoteAttempts, Page: 1, Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			if len(history.RemoteAttempts) != 1 || history.RemoteAttempts[0].Attempt.Status != c.WantEvent {
				t.Fatalf("history=%#v want=%s", history.RemoteAttempts, c.WantEvent)
			}
			if c.WantCoverage != "" && history.Coverage != c.WantCoverage {
				t.Fatalf("unavailable history range was called empty/available: %#v", history)
			}
			if c.WantObservationState != "" && catalog.Collectives[0].State != c.WantObservationState {
				t.Fatalf("catalog observation state=%s want=%s", catalog.Collectives[0].State, c.WantObservationState)
			}
			if c.RetainedDomain != "" {
				retained, completed := report.Domains[0], report.Domains[1]
				oldDomain, savedDomain := previousCatalog.Fetch.Catalog, catalog.Fetch.Catalog
				if c.RetainedDomain == schema.CollectiveFetchDomainHistory {
					retained, completed = completed, retained
					oldDomain, savedDomain = previousCatalog.Fetch.History, catalog.Fetch.History
					if !reflect.DeepEqual(history, previousHistory) {
						t.Fatalf("history lookup failure changed saved payload, coverage or category token: before=%#v after=%#v", previousHistory, history)
					}
				} else if !reflect.DeepEqual(catalog.Collectives, previousCatalog.Collectives) {
					t.Fatalf("detail lookup failure changed saved catalog payload or availability: before=%#v after=%#v", previousCatalog.Collectives, catalog.Collectives)
				}
				if retained.Domain != c.RetainedDomain || retained.Commit != schema.CollectiveFetchCommitRetained || retained.State != schema.CollectiveFetchStateFailed || retained.CompletedAt != nil {
					t.Fatalf("failed lookup was reported as a completed availability decision: %#v", retained)
				}
				if retained.Issue == nil || retained.Issue.Code != schema.OperationIssueRemote {
					t.Fatalf("lookup failure lost its remote issue: %#v", retained)
				}
				datesRetained := reflect.DeepEqual(retained.LastCompletedAt, oldDomain.LastCompletedAt) && reflect.DeepEqual(savedDomain.LastCompletedAt, oldDomain.LastCompletedAt)
				revisionRetained := reflect.DeepEqual(savedDomain.LastInstalledRevision, oldDomain.LastInstalledRevision)
				if !datesRetained || savedDomain.Coverage != oldDomain.Coverage || !revisionRetained {
					t.Fatalf("lookup failure changed last-good domain evidence: before=%#v report=%#v saved=%#v", oldDomain, retained, savedDomain)
				}
				if completed.State != schema.CollectiveFetchStateComplete || completed.Commit != schema.CollectiveFetchCommitUpdated || completed.LastCompletedAt == nil || !completed.LastCompletedAt.Equal(at) {
					t.Fatalf("independent complete domain did not commit at the injected clock: %#v", completed)
				}
				if !reflect.DeepEqual(catalog.Fetch.LastCompletedFetchAt, previousCatalog.Fetch.LastCompletedFetchAt) || report.Outcome != schema.CollectiveFetchOutcomePartial {
					t.Fatalf("lookup failure falsely advanced global completion: %#v", catalog.Fetch)
				}
			}
			if c.WantStatus != 200 && catalog.Fetch.LastCompletedFetchAt != nil && !catalog.Fetch.LastCompletedFetchAt.Equal(before.Domains[0].CompletedAt.UTC()) {
				t.Fatal("partial fetch advanced all-complete timestamp")
			}
		})
	}
}
