package store_test

import (
	_ "embed"
	"errors"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/collective_domain_cas.yaml
var domainCASYAML []byte

func TestCollectiveDomainSourceCAS(t *testing.T) {
	var fixture struct {
		RequiredNames  []string                             `yaml:"requiredNames"`
		CatalogSources []schema.CollectiveObservationSource `yaml:"catalogSources"`
		Cases          []struct {
			Name      string `yaml:"name"`
			Mode      string `yaml:"mode"`
			WantName  string `yaml:"wantName"`
			WantError bool   `yaml:"wantError"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(domainCASYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			db := storetest.Open(t)
			home := t.TempDir()
			creds := auth.Credentials{APIKey: "fixture", KeyID: "fixture-key", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture", VillageURL: "https://village.invalid/installation"}
			if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			snapshot, err := auth.CaptureCredentials(t.Context(), home)
			if err != nil {
				t.Fatal(err)
			}
			old, err := db.BeginCollectiveObservation(t.Context(), snapshot)
			if err != nil {
				t.Fatal(err)
			}
			newer, err := db.BeginCollectiveObservation(t.Context(), snapshot)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
			const collective schema.VillageUUID = "33333333-3333-4333-8333-333333333333"
			const transcript schema.TranscriptID = "22222222-2222-4222-8222-222222222222"
			sources := []schema.CollectiveFetchSourceResult{}
			for _, source := range fixture.CatalogSources {
				sources = append(sources, schema.CollectiveFetchSourceResult{Source: source, Outcome: schema.CollectiveSourceDisclosed, Complete: true, ObservedAt: &at})
			}
			historySources := []schema.CollectiveFetchSourceResult{{Source: schema.CollectiveObservationSourceContributionDiscovery, Outcome: schema.CollectiveSourceDisclosed, Complete: true, ObservedAt: &at}}
			row := schema.CollectiveObservation{ID: collective, Name: "New", Reasons: []schema.CollectiveObservationReason{schema.CollectiveObservationReasonExplicit}, State: schema.CollectiveObservationStateObserved, ObservedAt: at}
			pending := schema.VillageShareEvent{EventNum: 1, Status: schema.VillageShareStatusPending, RecordedAt: at}
			pairs := []store.ContributionAttemptSource{{CollectiveID: collective, TranscriptID: transcript, Events: []schema.VillageShareEvent{pending}, ObservedAt: at}}
			switch c.Mode {
			case "independent":
				if _, err := db.CommitCollectiveCatalog(t.Context(), newer, []schema.CollectiveObservation{row}, sources, at, at); err != nil {
					t.Fatal(err)
				}
				_, err = db.CommitCollectiveHistory(t.Context(), old, pairs, historySources, schema.CollectiveEvidenceCoverageAvailable, at, at)
			case "catalog":
				if _, err := db.CommitCollectiveCatalog(t.Context(), newer, []schema.CollectiveObservation{row}, sources, at, at); err != nil {
					t.Fatal(err)
				}
				row.Name = "Old"
				_, err = db.CommitCollectiveCatalog(t.Context(), old, []schema.CollectiveObservation{row}, sources, at, at)
			case "tracking":
				if err := db.TrackCollectiveObservation(t.Context(), newer, row); err != nil {
					t.Fatal(err)
				}
				row.Name = "Old"
				_, err = db.CommitCollectiveCatalog(t.Context(), old, []schema.CollectiveObservation{row}, sources, at, at)
			case "attempt":
				approved := pending
				approved.Status = schema.VillageShareStatusApproved
				approved.DecidedByActor = schema.VillageShareEventActorModerator
				approved.DecidedAt = &at
				if _, err := db.InstallContributionAttemptSources(t.Context(), newer, []store.ContributionAttemptSource{{CollectiveID: collective, TranscriptID: transcript, Events: []schema.VillageShareEvent{approved}, ObservedAt: at}}); err != nil {
					t.Fatal(err)
				}
				_, err = db.CommitCollectiveHistory(t.Context(), old, pairs, historySources, schema.CollectiveEvidenceCoverageAvailable, at, at)
			case "failed-then-complete":
				var latest store.CollectiveObservationInvocation
				latest, err = db.BeginCollectiveObservation(t.Context(), snapshot)
				if err != nil {
					t.Fatal(err)
				}
				issue := schema.OperationIssue{Code: schema.OperationIssueRemote, Operation: schema.CollectiveOperationFetch, Message: "Fixture complete source failed before installation.", Retry: "Retry the required metadata source explicitly."}
				failure := schema.CollectiveFetchDomainResult{Domain: schema.CollectiveFetchDomainHistory, Requested: true, State: schema.CollectiveFetchStateFailed, Commit: schema.CollectiveFetchCommitRetained, StartedAt: &at, LastAttemptAt: &at, Coverage: schema.CollectiveEvidenceCoverageUnknown, SourceOutcomes: []schema.CollectiveFetchSourceResult{}, Issue: &issue}
				if _, err := db.RecordCollectiveDomainFailure(t.Context(), latest, failure); err != nil {
					t.Fatal(err)
				}
				if _, err := db.CommitCollectiveHistory(t.Context(), newer, []store.ContributionAttemptSource{}, historySources, schema.CollectiveEvidenceCoverageAvailable, at, at); !errors.Is(err, store.ErrObservationSuperseded) {
					t.Fatalf("newer failure did not fence intermediate older empty completion: %v", err)
				}
				_, err = db.CommitCollectiveHistory(t.Context(), old, pairs, historySources, schema.CollectiveEvidenceCoverageAvailable, at, at)
			default:
				t.Fatal("unknown domain CAS mode")
			}
			if c.WantError {
				if !errors.Is(err, store.ErrObservationSuperseded) {
					t.Fatalf("older overlapping observation was not superseded: %v", err)
				}
			} else if err != nil {
				t.Fatalf("unrelated domain blocked valid earlier history: %v", err)
			}
			catalog, err := db.ReadCollectiveObservations(t.Context(), snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if c.WantName != "" && (len(catalog.Collectives) != 1 || catalog.Collectives[0].Name != c.WantName) {
				t.Fatalf("newer catalog lost: %#v", catalog.Collectives)
			}
			if c.Mode == "failed-then-complete" {
				summary := catalog.Fetch.History
				retainedFailure := summary.State == schema.CollectiveFetchStateFailed && summary.Issue != nil
				noFabricatedCompletion := summary.LastCompletedAt == nil && summary.LastInstalledRevision == nil && summary.Coverage == schema.CollectiveEvidenceCoverageUnknown
				if !retainedFailure || !noFabricatedCompletion {
					t.Fatalf("newer failure or independently completed evidence was lost: %#v", summary)
				}
			}
			attempt, err := db.ReadContributionAttempt(t.Context(), snapshot, collective, transcript, 1)
			if err != nil {
				t.Fatal(err)
			}
			if c.Mode == "attempt" && (attempt == nil || attempt.Attempt.Status != schema.VillageShareStatusApproved) {
				t.Fatalf("confirmed terminal state lost: %#v", attempt)
			}
			if c.Mode == "failed-then-complete" && attempt != nil {
				t.Fatalf("older invocation installed after later complete set: %#v", attempt)
			}
		})
	}
}
