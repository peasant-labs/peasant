package api_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/collective_read_fences.yaml
var mountedReadFencesYAML []byte

func TestMountedMetadataFetchLatestIssuedFence(t *testing.T) {
	var f struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name      string `yaml:"name"`
			FailFirst bool   `yaml:"failFirst"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(mountedReadFencesYAML, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			db := storetest.Open(t)
			home := t.TempDir()
			oldEntered, newEntered := make(chan struct{}), make(chan struct{})
			oldRelease, newRelease := make(chan struct{}), make(chan struct{})
			var oldOnce, newOnce sync.Once
			creds := auth.Credentials{APIKey: "fixture", KeyID: "fixture", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture"}
			var reads atomic.Int32
			at := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
			const collective schema.VillageUUID = "33333333-3333-4333-8333-333333333333"
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var value any
				switch r.URL.Path {
				case "/api/v1/auth/me":
					value = schema.VillageUser{ID: schema.VillageUUID(creds.UserID)}
				case "/api/v1/collectives":
					n := reads.Add(1)
					if n == 2 {
						close(oldEntered)
						select {
						case <-oldRelease:
						case <-r.Context().Done():
							return
						}
					}
					if n == 3 {
						close(newEntered)
						select {
						case <-newRelease:
						case <-r.Context().Done():
							return
						}
						w.WriteHeader(503)
						return
					}
					value = []schema.VillageUserGroup{{ID: collective, Name: "PriorGood", Role: "member", AcceptanceMode: "open", DataAccess: "public", MemberSince: at}}
				case "/api/v1/collectives/" + collective.String():
					value = schema.VillageGroupDetailResponse{Group: schema.VillageGroupDetailRecord{ID: collective, Name: "PriorGood", AcceptanceMode: "open", DataAccess: "public"}}
				case "/api/v1/auth/orgs":
					value = []schema.VillageMetadataUserOrganization{}
				case "/api/v1/users/me/collectives/contributions":
					value = schema.VillageContributedCollectivesResponse{Collectives: []schema.VillageContributedCollective{}}
				default:
					t.Errorf("unexpected metadata source %s", r.URL.Path)
					w.WriteHeader(400)
					return
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer remote.Close()
			defer func() { oldOnce.Do(func() { close(oldRelease) }); newOnce.Do(func() { close(newRelease) }); cancel() }()
			creds.VillageURL = remote.URL
			if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			server := api.NewServer(api.ServerConfig{Port: 0, Store: db, ConfigHome: home})
			if err := server.Listen(ctx); err != nil {
				t.Fatal(err)
			}
			serverDone := make(chan error, 1)
			go func() { serverDone <- server.Serve(ctx) }()
			defer func() {
				cancel()
				select {
				case err := <-serverDone:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					t.Error("mounted server did not stop")
				}
			}()
			base := "http://" + server.Addr().String()
			client := &http.Client{Timeout: 15 * time.Second}
			type result struct {
				report schema.CollectiveFetchReport
				err    error
			}
			fetch := func() result {
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/village/collective-observations/fetch", nil)
				if err != nil {
					return result{err: err}
				}
				response, err := client.Do(request)
				if err != nil {
					return result{err: err}
				}
				defer response.Body.Close()
				var report schema.CollectiveFetchReport
				err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&report)
				if err == nil {
					err = report.Validate()
				}
				return result{report: report, err: err}
			}
			if prior := fetch(); prior.err != nil || prior.report.Outcome != schema.CollectiveFetchOutcomeComplete {
				t.Fatalf("initial mounted fetch %#v %v", prior.report, prior.err)
			}
			oldDone, newDone := make(chan result, 1), make(chan result, 1)
			var workers sync.WaitGroup
			defer func() {
				oldOnce.Do(func() { close(oldRelease) })
				newOnce.Do(func() { close(newRelease) })
				cancel()
				workers.Wait()
			}()
			workers.Add(1)
			go func() { defer workers.Done(); oldDone <- fetch() }()
			select {
			case <-oldEntered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			workers.Add(1)
			go func() { defer workers.Done(); newDone <- fetch() }()
			select {
			case <-newEntered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if c.FailFirst {
				newOnce.Do(func() { close(newRelease) })
				select {
				case out := <-newDone:
					if out.err != nil {
						t.Fatal(out.err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			oldOnce.Do(func() { close(oldRelease) })
			select {
			case out := <-oldDone:
				if out.err != nil {
					t.Fatal(out.err)
				}
				found := false
				for _, domain := range out.report.Domains {
					if domain.Domain == schema.CollectiveFetchDomainCatalog && domain.State == schema.CollectiveFetchStateSuperseded && domain.Commit == schema.CollectiveFetchCommitRetained {
						found = true
					}
				}
				if !found {
					t.Fatalf("mounted older response installed: %#v", out.report)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if !c.FailFirst {
				newOnce.Do(func() { close(newRelease) })
				select {
				case out := <-newDone:
					if out.err != nil {
						t.Fatal(out.err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/village/collective-observations", strings.NewReader(""))
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var saved schema.CollectiveObservationResponse
			if err := json.NewDecoder(response.Body).Decode(&saved); err != nil {
				t.Fatal(err)
			}
			if len(saved.Collectives) != 1 || saved.Collectives[0].Name != "PriorGood" || saved.Fetch.Catalog.State != schema.CollectiveFetchStateFailed {
				t.Fatalf("mounted last good/newest failure lost: %#v", saved)
			}
		})
	}
}
