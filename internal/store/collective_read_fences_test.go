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

//go:embed testdata/collective_read_fences.yaml
var readFencesYAML []byte

func TestCollectiveLatestIssuedReadFences(t *testing.T) {
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name        string `yaml:"name"`
			Subject     string `yaml:"subject"`
			Newer       string `yaml:"newer"`
			Fail        bool   `yaml:"fail"`
			Blocked     bool   `yaml:"blocked"`
			Reopen      bool   `yaml:"reopen"`
			Invalidated bool   `yaml:"invalidated"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(readFencesYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			path := storetest.CopyGoldenDB(t)
			db, err := store.Open(path, store.WithSkipMigrations(), store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			home := t.TempDir()
			creds := auth.Credentials{APIKey: "fixture", KeyID: "fixture", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture", VillageURL: "https://village.invalid"}
			if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			snapshot, err := auth.CaptureCredentials(t.Context(), home)
			if err != nil {
				t.Fatal(err)
			}
			const collective schema.VillageUUID = "33333333-3333-4333-8333-333333333333"
			const other schema.VillageUUID = "44444444-4444-4444-8444-444444444444"
			const transcript schema.TranscriptID = "22222222-2222-4222-8222-222222222222"
			at := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
			begin := func(db *store.Store) store.CollectiveObservationInvocation {
				t.Helper()
				inv, err := db.BeginCollectiveObservation(t.Context(), snapshot)
				if err != nil {
					t.Fatal(err)
				}
				return inv
			}
			sources := []schema.CollectiveFetchSourceResult{}
			for _, source := range []schema.CollectiveObservationSource{schema.CollectiveObservationSourceMembership, schema.CollectiveObservationSourceContributionDiscovery, schema.CollectiveObservationSourceOrganization, schema.CollectiveObservationSourceExplicit} {
				sources = append(sources, schema.CollectiveFetchSourceResult{Source: source, Outcome: schema.CollectiveSourceDisclosed, Complete: true, ObservedAt: &at})
			}
			historySources := []schema.CollectiveFetchSourceResult{sources[1]}
			row := schema.CollectiveObservation{ID: collective, Name: "PriorGood", Reasons: []schema.CollectiveObservationReason{schema.CollectiveObservationReasonMembership}, State: schema.CollectiveObservationStateObserved, ObservedAt: at}
			prior := begin(db)
			if _, err := db.CommitCollectiveCatalog(t.Context(), prior, []schema.CollectiveObservation{row}, sources, at, at); err != nil {
				t.Fatal(err)
			}
			pairs := []store.ContributionAttemptSource{{CollectiveID: collective, TranscriptID: transcript, ObservedAt: at, Events: []schema.VillageShareEvent{{EventNum: 1, Status: schema.VillageShareStatusPending, RecordedAt: at}}}}
			if _, err := db.CommitCollectiveHistory(t.Context(), prior, pairs, historySources, schema.CollectiveEvidenceCoverageAvailable, at, at); err != nil {
				t.Fatal(err)
			}
			assoc := []schema.VillageTranscriptCollective{{ID: collective, Name: "PriorGood", SharedAt: at}}
			if err := db.InstallTranscriptAssociations(t.Context(), prior, transcript, assoc, at, false); err != nil {
				t.Fatal(err)
			}
			q := schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceRemoteAttempts, Page: 1, Limit: 50}
			before, err := db.ReadContributionHistory(t.Context(), snapshot, q)
			if err != nil {
				t.Fatal(err)
			}
			plan := func(subject string) store.CollectiveReadPlan {
				switch subject {
				case "catalog":
					return store.CollectiveReadPlan{Domains: []schema.CollectiveFetchDomain{schema.CollectiveFetchDomainCatalog}}
				case "history":
					return store.CollectiveReadPlan{Domains: []schema.CollectiveFetchDomain{schema.CollectiveFetchDomainHistory}}
				case "target":
					return store.CollectiveReadPlan{Sources: []store.CollectiveReadSource{{Source: schema.CollectiveObservationSourceExplicit, ID: collective.String()}}}
				case "other-target":
					return store.CollectiveReadPlan{Sources: []store.CollectiveReadSource{{Source: schema.CollectiveObservationSourceExplicit, ID: other.String()}}}
				case "pair":
					return store.CollectiveReadPlan{Sources: []store.CollectiveReadSource{store.ContributionAttemptReadSource(collective, transcript)}}
				case "other-pair":
					return store.CollectiveReadPlan{Sources: []store.CollectiveReadSource{store.ContributionAttemptReadSource(other, transcript)}}
				case "association":
					return store.CollectiveReadPlan{Transcripts: []schema.TranscriptID{transcript}}
				case "other-transcript":
					return store.CollectiveReadPlan{Transcripts: []schema.TranscriptID{schema.TranscriptID(other.String())}}
				default:
					t.Fatal("unknown read-fence subject")
					return store.CollectiveReadPlan{}
				}
			}
			old := begin(db)
			if err := db.IssueCollectiveReads(t.Context(), old, plan(c.Subject), at); err != nil {
				t.Fatal(err)
			}
			writer := db
			if c.Reopen {
				writer, err = store.Open(path, store.WithSkipMigrations(), store.WithPoolSize(1))
				if err != nil {
					t.Fatal(err)
				}
				// Retain ownership on fatal exits before the explicit durability close below.
				defer writer.Close()
			}
			newer := begin(writer)
			if err := writer.IssueCollectiveReads(t.Context(), newer, plan(c.Newer), at); err != nil {
				t.Fatal(err)
			}
			if c.Fail {
				issue := schema.OperationIssue{Code: schema.OperationIssueRemote, Operation: schema.CollectiveOperationFetch, Message: "Newer complete source failed.", Retry: "Fetch metadata explicitly."}
				if c.Newer == "association" {
					issue.Operation = schema.CollectiveOperationPull
					err = writer.RecordTranscriptAssociationFailure(t.Context(), newer, transcript, at, issue)
				} else {
					domain := schema.CollectiveFetchDomainCatalog
					if c.Newer == "history" {
						domain = schema.CollectiveFetchDomainHistory
					}
					_, err = writer.RecordCollectiveDomainFailure(t.Context(), newer, schema.CollectiveFetchDomainResult{Domain: domain, Requested: true, State: schema.CollectiveFetchStateFailed, Commit: schema.CollectiveFetchCommitRetained, LastAttemptAt: &at, Coverage: schema.CollectiveEvidenceCoverageUnknown, SourceOutcomes: []schema.CollectiveFetchSourceResult{}, Issue: &issue})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if c.Reopen {
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			}
			afterIssue, err := db.ReadContributionHistory(t.Context(), snapshot, q)
			if err != nil {
				t.Fatal(err)
			}
			if afterIssue.TraversalRevision != before.TraversalRevision {
				t.Fatal("issuance/failure invalidated unchanged business evidence")
			}
			row.Name = "StaleOlderResponse"
			switch c.Subject {
			case "catalog":
				_, err = db.CommitCollectiveCatalog(t.Context(), old, []schema.CollectiveObservation{row}, sources, at, at)
			case "target":
				err = db.TrackCollectiveObservation(t.Context(), old, row)
			case "history":
				_, err = db.CommitCollectiveHistory(t.Context(), old, []store.ContributionAttemptSource{}, historySources, schema.CollectiveEvidenceCoverageAvailable, at, at)
			case "pair":
				_, err = db.InstallContributionAttemptSources(t.Context(), old, pairs)
			case "association":
				rows := []schema.VillageTranscriptCollective{}
				if !c.Invalidated {
					rows = append(rows, schema.VillageTranscriptCollective{ID: other, Name: "StaleOlderResponse", SharedAt: at})
				}
				err = db.InstallTranscriptAssociations(t.Context(), old, transcript, rows, at, c.Invalidated)
			}
			if c.Blocked {
				if !errors.Is(err, store.ErrObservationSuperseded) {
					t.Fatalf("obsolete overlapping response installed: %v", err)
				}
			} else if err != nil {
				t.Fatalf("unrelated read fenced independent work: %v", err)
			}
			if c.Blocked {
				saved, err := db.ReadCollectiveObservations(t.Context(), snapshot)
				if err != nil || len(saved.Collectives) != 1 || saved.Collectives[0].Name != "PriorGood" {
					t.Fatalf("prior catalog replaced: %#v %v", saved, err)
				}
				rows, err := db.ReadTranscriptAssociations(t.Context(), snapshot, []schema.TranscriptID{transcript})
				if err != nil || len(rows[transcript].Collectives) != 1 || rows[transcript].Collectives[0].Name != "PriorGood" {
					t.Fatalf("prior disclosure replaced: %#v %v", rows, err)
				}
				catalogFailed := c.Fail && c.Newer == "catalog"
				missingFailure := saved.Fetch.Catalog.Issue == nil || saved.Fetch.Catalog.State != schema.CollectiveFetchStateFailed
				if catalogFailed && missingFailure {
					t.Fatal("newer failure reporting lost")
				}
			}
		})
	}
}
