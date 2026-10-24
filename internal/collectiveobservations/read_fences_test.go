package collectiveobservations

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/read_fences.yaml
var readFenceYAML []byte

func TestMetadataHTTPReadsFenceObsoleteResponses(t *testing.T) {
	var f struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name         string `yaml:"name"`
			Operation    string `yaml:"operation"`
			FailFirst    bool   `yaml:"failFirst"`
			Negative     bool   `yaml:"negative"`
			DenyIssuance bool   `yaml:"denyIssuance"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(readFenceYAML, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			db := storetest.OpenWith(t, store.WithPoolSize(1))
			home := t.TempDir()
			const collective schema.VillageUUID = "33333333-3333-4333-8333-333333333333"
			const transcript schema.TranscriptID = "22222222-2222-4222-8222-222222222222"
			creds := auth.Credentials{APIKey: "fixture", KeyID: "fixture", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture"}
			at := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
			oldEntered, newEntered := make(chan struct{}), make(chan struct{})
			releaseOld, releaseNew := make(chan struct{}), make(chan struct{})
			var oldOnce, newOnce sync.Once
			defer func() { oldOnce.Do(func() { close(releaseOld) }); newOnce.Do(func() { close(releaseNew) }) }()
			var phase, reads atomic.Int32
			gatePath := "/api/v1/collectives/" + collective.String()
			switch c.Operation {
			case "fetch":
				gatePath = "/api/v1/collectives"
			case "history", "pair":
				gatePath = "/api/v1/users/me/collectives/" + collective.String() + "/transcripts/" + transcript.String() + "/events"
			case "association":
				gatePath = "/api/v1/transcripts/" + transcript.String() + "/collectives"
			}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stale := false
				if r.URL.Path == gatePath && phase.Load() != 0 {
					n := reads.Add(1)
					if n == 1 {
						close(oldEntered)
						select {
						case <-releaseOld:
						case <-r.Context().Done():
							return
						}
						stale = true
						if c.Negative {
							w.WriteHeader(404)
							return
						}
					} else if n == 2 {
						close(newEntered)
						select {
						case <-releaseNew:
						case <-r.Context().Done():
							return
						}
						w.WriteHeader(503)
						return
					} else {
						t.Errorf("unexpected overlapping read %d", n)
						w.WriteHeader(500)
						return
					}
				}
				name := "PriorGood"
				if stale {
					name = "Obsolete"
				}
				var value any
				switch r.URL.Path {
				case "/api/v1/auth/me":
					value = schema.VillageUser{ID: schema.VillageUUID(creds.UserID)}
				case "/api/v1/collectives":
					value = []schema.VillageUserGroup{{ID: collective, Name: name, MemberSince: at, Role: "member", AcceptanceMode: "open", DataAccess: "public"}}
				case "/api/v1/collectives/" + collective.String():
					value = schema.VillageGroupDetailResponse{Group: schema.VillageGroupDetailRecord{ID: collective, Name: name, AcceptanceMode: "open", DataAccess: "public"}}
				case "/api/v1/auth/orgs":
					value = []schema.VillageMetadataUserOrganization{}
				case "/api/v1/users/me/collectives/contributions":
					rows := []schema.VillageContributedCollective{}
					if c.Operation == "history" {
						rows = append(rows, schema.VillageContributedCollective{ID: collective, Name: name, PendingCount: 1})
					}
					value = schema.VillageContributedCollectivesResponse{Collectives: rows}
				case "/api/v1/users/me/collectives/" + collective.String() + "/submissions":
					value = []schema.VillageCollectiveSubmission{{TranscriptID: transcript, GroupID: collective, EventNum: 1, Status: schema.VillageShareStatusPending, RecordedAt: at}}
				case "/api/v1/users/me/collectives/" + collective.String() + "/transcripts/" + transcript.String() + "/events":
					event := schema.VillageShareEvent{EventNum: 1, Status: schema.VillageShareStatusPending, RecordedAt: at}
					if stale {
						event.Status = schema.VillageShareStatusApproved
						event.DecidedByActor = schema.VillageShareEventActorModerator
						event.DecidedAt = &at
					}
					value = []schema.VillageShareEvent{event}
				case "/api/v1/transcripts/" + transcript.String() + "/collectives":
					value = schema.VillageTranscriptCollectivesResponse{Collectives: []schema.VillageTranscriptCollective{{ID: collective, Name: name, SharedAt: at}}}
				default:
					t.Errorf("unexpected source %s", r.URL.Path)
					w.WriteHeader(400)
					return
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer func() { cancel(); remote.Close() }()
			creds.VillageURL = remote.URL
			if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			service := New(Dependencies{ConfigHome: home, Store: db, Now: func() time.Time { return at }, LocallyHeldTranscriptIDs: func(context.Context, schema.CollectiveObservationScope) ([]schema.TranscriptID, error) {
				return []schema.TranscriptID{}, nil
			}})
			type result struct {
				report schema.CollectiveFetchReport
				err    error
			}
			run := func() result {
				var out result
				switch c.Operation {
				case "fetch", "history":
					out.report, out.err = service.Fetch(ctx)
				case "track":
					_, out.err = service.Track(ctx, collective)
				case "pair":
					_, out.err = service.ObserveOwnPair(ctx, collective, transcript)
				case "association":
					_, out.err = service.ObserveAssociations(ctx, transcript)
				default:
					t.Error("unknown read-fence operation")
				}
				return out
			}
			if initial := run(); initial.err != nil {
				t.Fatal(initial.err)
			}
			if c.DenyIssuance {
				conn, err := db.Pool().Take(ctx)
				if err != nil {
					t.Fatal(err)
				}
				err = conn.SetAuthorizer(sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
					if action.Type() == sqlite.OpInsert && action.Table() == "collective_source_installs" {
						return sqlite.AuthResultDeny
					}
					return sqlite.AuthResultOK
				}))
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
				phase.Store(1)
				failed := run()
				if failed.err == nil || reads.Load() != 0 {
					t.Fatalf("unregistered source read reached network: reads=%d err=%v", reads.Load(), failed.err)
				}
				if c.Operation == "fetch" {
					if failed.report.Domains[0].Issue == nil || failed.report.Domains[0].Issue.Code != schema.OperationIssuePersistence {
						t.Fatalf("issuance failure omitted persistence report: %#v", failed.report)
					}
				}
				conn, err = db.Pool().Take(ctx)
				if err != nil {
					t.Fatal(err)
				}
				err = conn.SetAuthorizer(nil)
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
				saved, err := service.List(ctx)
				if err != nil || len(saved.Collectives) != 1 || saved.Collectives[0].Name != "PriorGood" {
					t.Fatalf("failed issuance changed good payload: %#v %v", saved, err)
				}
				return
			}
			phase.Store(1)
			oldDone, newDone := make(chan result, 1), make(chan result, 1)
			var workers sync.WaitGroup
			defer func() {
				oldOnce.Do(func() { close(releaseOld) })
				newOnce.Do(func() { close(releaseNew) })
				cancel()
				workers.Wait()
			}()
			workers.Add(1)
			go func() { defer workers.Done(); oldDone <- run() }()
			select {
			case <-oldEntered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			workers.Add(1)
			go func() { defer workers.Done(); newDone <- run() }()
			select {
			case <-newEntered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if c.FailFirst {
				newOnce.Do(func() { close(releaseNew) })
				select {
				case out := <-newDone:
					if out.err == nil {
						t.Fatal("newer failed source reported success")
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			oldOnce.Do(func() { close(releaseOld) })
			var oldResult result
			select {
			case oldResult = <-oldDone:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if c.Operation == "fetch" || c.Operation == "history" {
				domain := schema.CollectiveFetchDomainCatalog
				if c.Operation == "history" {
					domain = schema.CollectiveFetchDomainHistory
				}
				found := false
				for _, d := range oldResult.report.Domains {
					if d.Domain == domain && d.State == schema.CollectiveFetchStateSuperseded && d.Commit == schema.CollectiveFetchCommitRetained {
						found = true
					}
				}
				if !found {
					t.Fatalf("obsolete HTTP domain installed: %#v err=%v", oldResult.report, oldResult.err)
				}
			} else if !errors.Is(oldResult.err, store.ErrObservationSuperseded) {
				t.Fatalf("obsolete targeted HTTP read installed: %v", oldResult.err)
			}
			if !c.FailFirst {
				newOnce.Do(func() { close(releaseNew) })
				select {
				case out := <-newDone:
					if out.err == nil {
						t.Fatal("newer failed source reported success")
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			snapshot, err := auth.CaptureCredentials(ctx, home)
			if err != nil {
				t.Fatal(err)
			}
			switch c.Operation {
			case "fetch", "track":
				saved, err := db.ReadCollectiveObservations(ctx, snapshot)
				if err != nil || len(saved.Collectives) != 1 || saved.Collectives[0].Name != "PriorGood" {
					t.Fatalf("prior good catalog lost: %#v %v", saved, err)
				}
			case "history", "pair":
				attempt, err := db.ReadContributionAttempt(ctx, snapshot, collective, transcript, 1)
				if err != nil || attempt == nil || attempt.Attempt.Status != schema.VillageShareStatusPending {
					t.Fatalf("prior attempt replaced: %#v %v", attempt, err)
				}
			case "association":
				rows, err := db.ReadTranscriptAssociations(ctx, snapshot, []schema.TranscriptID{transcript})
				if err != nil || len(rows[transcript].Collectives) != 1 || rows[transcript].Collectives[0].Name != "PriorGood" {
					t.Fatalf("prior disclosure replaced: %#v %v", rows, err)
				}
			}
		})
	}
}
