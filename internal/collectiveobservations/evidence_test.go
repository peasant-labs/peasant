package collectiveobservations

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

func TestContributionEvidenceSavedService(t *testing.T) {
	f := testutil.LoadCollectiveEvidenceFixtures(t)
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx := t.Context()
			home := t.TempDir()
			db := storetest.Open(t)
			creds := auth.Credentials{APIKey: "fixture-key", KeyID: "fixture-key-id", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture", VillageURL: "https://village.example/install"}
			if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			snapshot, err := auth.CaptureCredentials(ctx, home)
			if err != nil {
				t.Fatal(err)
			}
			testutil.SeedCollectiveEvidence(t, ctx, db, snapshot, f)
			newService := func(local *store.Store) *Service {
				return New(Dependencies{Store: local, ConfigHome: home, NewIdentityReader: func(auth.Credentials) IdentityReader { t.Fatal("saved history contacted Village"); return nil }})
			}
			service := newService(db)
			q := schema.LocalContributionHistoryQuery{Kind: c.Kind, Page: 1, Limit: c.Limit, CollectiveID: c.CollectiveID}
			first, err := service.History(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			initialTokens := map[schema.LocalContributionEvidenceKind]schema.ContributionTraversalRevision{}
			for _, kind := range schema.AllLocalContributionEvidenceKinds {
				r, err := service.History(ctx, schema.LocalContributionHistoryQuery{Kind: kind, Page: 1, Limit: 1})
				if err != nil {
					t.Fatal(err)
				}
				initialTokens[kind] = r.TraversalRevision
			}
			appendDiagnostic := func(page int) {
				t.Helper()
				id := schema.VillageUUID(fmt.Sprintf("90000000-0000-4000-8000-%012d", page))
				if err := service.RecordDiagnostic(ctx, snapshot, testutil.FixtureCollectiveDiagnostic(f, id)); err != nil {
					t.Fatal(err)
				}
			}
			if c.Mode == "traverse" || c.Mode == "diagnostic-between-pages" {
				gotIDs := []string{}
				page := first
				for index := 1; ; index++ {
					if page.TraversalRevision != first.TraversalRevision || page.Total != c.WantTotal || page.Coverage != first.Coverage {
						t.Fatalf("selected traversal changed at page %d", index)
					}
					pageIDs := testutil.CollectiveEvidenceIDs(page)
					if len(pageIDs) == 0 && int64(len(gotIDs)) < page.Total {
						t.Fatal("premature empty page before declared total")
					}
					gotIDs = append(gotIDs, pageIDs...)
					data, err := json.Marshal(page)
					if err != nil {
						t.Fatal(err)
					}
					var object map[string]json.RawMessage
					if err := json.Unmarshal(data, &object); err != nil {
						t.Fatal(err)
					}
					selected := "remoteAttempts"
					if c.Kind == schema.LocalContributionEvidenceReceipts {
						selected = "receipts"
					}
					if c.Kind == schema.LocalContributionEvidenceOperationDiagnostics {
						selected = "operationDiagnostics"
					}
					for key := range object {
						if key == "cacheRevision" || key == "fetch" {
							t.Fatalf("unrelated live report leaked into selected traversal: %s", key)
						}
						if key == "remoteAttempts" || key == "receipts" || key == "operationDiagnostics" {
							if key != selected {
								t.Fatalf("nonselected array %s", key)
							}
						}
					}
					if int64(len(gotIDs)) >= page.Total {
						break
					}
					if c.Mode == "diagnostic-between-pages" {
						appendDiagnostic(index)
					}
					q.Page = index + 1
					token := first.TraversalRevision
					q.Revision = &token
					page, err = service.History(ctx, q)
					if err != nil {
						t.Fatalf("page %d failed after independent diagnostic: %v", q.Page, err)
					}
				}
				if !reflect.DeepEqual(gotIDs, c.WantIDs) {
					t.Fatalf("complete traversal IDs=%v want=%v", gotIDs, c.WantIDs)
				}
				return
			}
			if c.Mode == "selected-change" || c.Mode == "equivalent-poll" {
				switch c.Kind {
				case schema.LocalContributionEvidenceRemoteAttempts:
					events := []schema.VillageShareEvent{}
					for _, ordinal := range f.Ordinals {
						event := schema.VillageShareEvent{EventNum: ordinal, Status: schema.VillageShareStatusPending, RecordedAt: f.RecordedAt}
						if ordinal == f.Ordinals[0] && c.Mode == "selected-change" {
							decision := f.ObservedAt
							event.Status, event.DecidedAt, event.DecidedByActor = schema.VillageShareStatusApproved, &decision, schema.VillageShareEventActorModerator
						}
						events = append(events, event)
					}
					invocation, err := db.BeginCollectiveObservation(ctx, snapshot)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := db.InstallContributionAttemptSources(ctx, invocation, []store.ContributionAttemptSource{{CollectiveID: f.CollectiveID, TranscriptID: f.TranscriptID, Events: events, ObservedAt: f.ObservedAt.Add(time.Hour)}}); err != nil {
						t.Fatal(err)
					}
				case schema.LocalContributionEvidenceReceipts:
					if err := service.RecordReceipt(ctx, snapshot, schema.LocalConfirmedContributionReceipt{ReceiptID: "r-new", Operation: schema.CollectiveOperationPublish, TranscriptID: f.TranscriptID, ConfirmedAt: f.ObservedAt, Outcome: schema.LocalConfirmedContributionPublished}); err != nil {
						t.Fatal(err)
					}
				case schema.LocalContributionEvidenceOperationDiagnostics:
					appendDiagnostic(100)
				}
				for _, kind := range schema.AllLocalContributionEvidenceKinds {
					r, err := service.History(ctx, schema.LocalContributionHistoryQuery{Kind: kind, Page: 1, Limit: 1})
					if err != nil {
						t.Fatal(err)
					}
					if kind != c.Kind && r.TraversalRevision != initialTokens[kind] {
						t.Fatalf("selected %s change invalidated independent %s", c.Kind, kind)
					}
				}
			}
			if c.Mode == "coverage-change" {
				if err := db.SetContributionCoverage(ctx, snapshot, c.Kind, schema.CollectiveEvidenceCoverageAvailable); err != nil {
					t.Fatal(err)
				}
			}
			if c.Mode == "catalog-rename" {
				row := schema.CollectiveObservation{ID: f.CollectiveID, Name: "Original", Reasons: []schema.CollectiveObservationReason{schema.CollectiveObservationReasonExplicit}, ObservedAt: f.ObservedAt, State: schema.CollectiveObservationStateObserved}
				invocation, err := db.BeginCollectiveObservation(ctx, snapshot)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.TrackCollectiveObservation(ctx, invocation, row); err != nil {
					t.Fatal(err)
				}
				row.Name = "Renamed"
				row.ObservedAt = row.ObservedAt.Add(time.Hour)
				invocation, err = db.BeginCollectiveObservation(ctx, snapshot)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.TrackCollectiveObservation(ctx, invocation, row); err != nil {
					t.Fatal(err)
				}
			}
			q.Page = c.Page
			token := first.TraversalRevision
			q.Revision = &token
			switch c.Token {
			case "missing":
				q.Revision = nil
			case "malformed":
				token = "not-a-token"
			case "other-kind":
				token = initialTokens[schema.LocalContributionEvidenceRemoteAttempts]
			case "other-scope":
				identity, err := schema.ParseContributionTraversalRevision(token)
				if err != nil {
					t.Fatal(err)
				}
				identity.Scope.UserID = f.AlternateCollectiveID
				token, err = schema.EncodeContributionTraversalRevision(identity)
				if err != nil {
					t.Fatal(err)
				}
			case "other-incarnation":
				fresh := storetest.Open(t)
				testutil.SeedCollectiveEvidence(t, ctx, fresh, snapshot, f)
				service = newService(fresh)
			}
			response, err := service.History(ctx, q)
			status := 200
			if err != nil {
				var queryErr *store.ContributionHistoryError
				if !errors.As(err, &queryErr) {
					t.Fatal(err)
				}
				status = queryErr.Status
				if status == 409 {
					if queryErr.Conflict == nil || queryErr.Conflict.CurrentTraversalRevision == token {
						t.Fatal("conflict omitted new selected token")
					}
					if response.RemoteAttempts != nil || response.Receipts != nil || response.OperationDiagnostics != nil {
						t.Fatal("conflict leaked rows")
					}
				}
			}
			if status != c.WantStatus {
				t.Fatalf("history status=%d want=%d err=%v", status, c.WantStatus, err)
			}
			if status == 200 {
				if response.Total != c.WantTotal || !reflect.DeepEqual(testutil.CollectiveEvidenceIDs(response), c.WantIDs) {
					t.Fatalf("history total=%d IDs=%v want=%d %v", response.Total, testutil.CollectiveEvidenceIDs(response), c.WantTotal, c.WantIDs)
				}
			}
		})
	}
}
