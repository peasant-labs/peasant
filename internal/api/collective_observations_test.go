package api_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/collectiveobservations"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

func TestMountedContributionEvidenceTraversal(t *testing.T) {
	f := testutil.LoadCollectiveEvidenceFixtures(t)
	for _, c := range f.Cases {
		if c.Mode != "traverse" && c.Mode != "diagnostic-between-pages" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			db := storetest.Open(t)
			home := t.TempDir()
			creds := auth.Credentials{APIKey: "fixture-key", KeyID: "fixture-key-id", UserID: "11111111-1111-4111-8111-111111111111", VillageURL: "https://offline.invalid/installation", Username: "fixture"}
			if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			snapshot, err := auth.CaptureCredentials(t.Context(), home)
			if err != nil {
				t.Fatal(err)
			}
			testutil.SeedCollectiveEvidence(t, t.Context(), db, snapshot, f)
			ctx, cancel := context.WithCancel(t.Context())
			server := api.NewServer(api.ServerConfig{Port: 0, Store: db, ConfigHome: home})
			if err := server.Listen(ctx); err != nil {
				cancel()
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- server.Serve(ctx) }()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("mounted API shutdown did not complete")
				}
			}()
			client := &http.Client{Timeout: 5 * time.Second}
			base := "http://" + server.Addr().String()
			token := ""
			got := []string{}
			for page := 1; int64(len(got)) < c.WantTotal; page++ {
				query := url.Values{"kind": {string(c.Kind)}, "page": {fmt.Sprint(page)}, "limit": {fmt.Sprint(c.Limit)}}
				if token != "" {
					query.Set("revision", token)
				}
				response, err := client.Get(base + "/api/v1/village/contributions?" + query.Encode())
				if err != nil {
					t.Fatal(err)
				}
				raw, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
				response.Body.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}
				if response.StatusCode != 200 {
					t.Fatalf("mounted page %d HTTP %d: %s", page, response.StatusCode, raw)
				}
				var envelope schema.LocalContributionHistoryResponse
				if err := json.Unmarshal(raw, &envelope); err != nil {
					t.Fatal(err)
				}
				if token == "" {
					token = string(envelope.TraversalRevision)
				}
				if token != string(envelope.TraversalRevision) || envelope.Total != c.WantTotal {
					t.Fatal("independent diagnostics changed selected traversal")
				}
				ids := testutil.CollectiveEvidenceIDs(envelope)
				if len(ids) == 0 {
					t.Fatal("selected traversal stopped before total")
				}
				got = append(got, ids...)
				if c.Mode == "diagnostic-between-pages" {
					id := schema.VillageUUID(fmt.Sprintf("90000000-0000-4000-8000-%012d", page))
					if err := db.RecordOperationDiagnostic(t.Context(), snapshot, testutil.FixtureCollectiveDiagnostic(f, id)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if !reflect.DeepEqual(got, c.WantIDs) {
				t.Fatalf("mounted IDs=%v want=%v", got, c.WantIDs)
			}
			if err := auth.ClearCredentialsFrom(home); err != nil {
				t.Fatal(err)
			}
			response, err := client.Get(base + "/api/v1/village/contributions")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != 401 {
				t.Fatalf("signed-out mounted history HTTP %d", response.StatusCode)
			}
			var body map[string]any
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if _, exists := body["scope"]; exists {
				t.Fatal("signed-out history leaked selected cache scope")
			}
		})
	}
}

//go:embed testdata/collective_observation_routes.yaml
var observationRoutesYAML []byte

func TestMountedCollectiveObservationRoutes(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name         string `yaml:"name"`
			Method       string `yaml:"method"`
			Path         string `yaml:"path"`
			Status       int    `yaml:"status"`
			RemoteStatus int    `yaml:"remoteStatus"`
			CloseStore   bool   `yaml:"closeStore"`
			Timeout      string `yaml:"timeout"`
			StallHistory bool   `yaml:"stallHistory"`
			StartupError bool   `yaml:"startupError"`
			Origin       string `yaml:"origin"`
			Network      bool   `yaml:"network"`
			Shape        string `yaml:"shape"`
		} `yaml:"cases"`
	}
	d := yaml.NewDecoder(strings.NewReader(string(observationRoutesYAML)))
	d.KnownFields(true)
	if err := d.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range fixture.Cases {
		if names[c.Name] {
			t.Fatal("duplicate mounted route fixture")
		}
		names[c.Name] = true
	}
	for _, name := range fixture.RequiredNames {
		if !names[name] {
			t.Fatalf("required mounted route fixture %s absent", name)
		}
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			timeout := c.Timeout
			if timeout == "" {
				timeout = "2m"
			}
			t.Setenv(collectiveobservations.EnvFetchTimeout, timeout)
			db := storetest.Open(t)
			home := t.TempDir()
			var calls atomic.Int32
			creds := auth.Credentials{APIKey: "fixture-key", KeyID: "fixture-key-id", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture"}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" {
					t.Error("fetch performed remote mutation")
				}
				if c.RemoteStatus != 0 {
					w.WriteHeader(c.RemoteStatus)
					_, _ = w.Write([]byte(`{"error":"fixture unavailable"}`))
					return
				}
				var value any
				switch r.URL.Path {
				case "/api/v1/auth/me":
					value = schema.VillageUser{ID: schema.VillageUUID(creds.UserID)}
				case "/api/v1/collectives":
					value = []schema.VillageUserGroup{}
				case "/api/v1/auth/orgs":
					value = []schema.VillageMetadataUserOrganization{}
				case "/api/v1/users/me/collectives/contributions":
					value = schema.VillageContributedCollectivesResponse{Collectives: []schema.VillageContributedCollective{}}
					if c.StallHistory {
						value = schema.VillageContributedCollectivesResponse{Collectives: []schema.VillageContributedCollective{{ID: "33333333-3333-4333-8333-333333333333", Name: "Visible", PendingCount: 1}}}
					}
				case "/api/v1/users/me/collectives/33333333-3333-4333-8333-333333333333/submissions":
					if !c.StallHistory {
						t.Error("unexpected owner history source")
					}
					<-r.Context().Done()
					return
				case "/api/v1/collectives/33333333-3333-4333-8333-333333333333":
					value = schema.VillageGroupDetailResponse{Group: schema.VillageGroupDetailRecord{ID: "33333333-3333-4333-8333-333333333333", Name: "Visible", AcceptanceMode: "open", DataAccess: "public"}}
				default:
					t.Errorf("unexpected source %s", r.URL.Path)
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
			if c.StallHistory {
				snapshot, err := auth.CaptureCredentials(t.Context(), home)
				if err != nil {
					t.Fatal(err)
				}
				testutil.SeedCollectiveEvidence(t, t.Context(), db, snapshot, testutil.LoadCollectiveEvidenceFixtures(t))
			}
			ctx, cancel := context.WithCancel(t.Context())
			server := api.NewServer(api.ServerConfig{Port: 0, Store: db, ConfigHome: home})
			if c.StartupError {
				err := server.Listen(ctx)
				cancel()
				if err == nil || !strings.Contains(err.Error(), collectiveobservations.EnvFetchTimeout) || server.Addr() != nil || calls.Load() != 0 {
					t.Fatalf("invalid runtime config reached startup/I/O: %v requests=%d", err, calls.Load())
				}
				return
			}
			if err := server.Listen(ctx); err != nil {
				cancel()
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- server.Serve(ctx) }()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("mounted API shutdown deadline")
				}
			}()
			if c.CloseStore {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			request, err := http.NewRequestWithContext(t.Context(), c.Method, "http://"+server.Addr().String()+c.Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.Origin != "" {
				request.Header.Set("Origin", c.Origin)
			}
			response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != c.Status {
				t.Fatalf("HTTP %d want %d: %s", response.StatusCode, c.Status, raw)
			}
			if (calls.Load() > 0) != c.Network {
				t.Fatalf("network calls=%d expected online=%v", calls.Load(), c.Network)
			}
			switch c.Shape {
			case "catalog":
				var value schema.CollectiveObservationResponse
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
				if err := value.Validate(); err != nil {
					t.Fatal(err)
				}
			case "report":
				var value schema.CollectiveFetchReport
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatalf("typed non-2xx report lost: %s: %v", raw, err)
				}
				if err := value.Validate(); err != nil {
					t.Fatal(err)
				}
				if schema.CollectiveFetchHTTPStatus(value) != c.Status {
					t.Fatal("report/status precedence mismatch")
				}
				if c.StallHistory {
					if value.Domains[0].State != schema.CollectiveFetchStateComplete || value.Domains[1].State != schema.CollectiveFetchStateCancelled || value.Domains[1].Issue == nil || value.Domains[1].Issue.Code != schema.OperationIssueDeadline {
						t.Fatalf("real local deadline lost independent effects: %#v", value)
					}
					snapshot, err := auth.CaptureCredentials(t.Context(), home)
					if err != nil {
						t.Fatal(err)
					}
					saved, err := db.ReadCollectiveObservations(t.Context(), snapshot)
					if err != nil || len(saved.Collectives) != 1 || saved.Collectives[0].Name != "Visible" || saved.Fetch.LastCompletedFetchAt != nil {
						t.Fatalf("deadline lost catalog or fabricated full completion: %#v %v", saved, err)
					}
					history, err := db.ReadContributionHistory(t.Context(), snapshot, schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceRemoteAttempts, Page: 1, Limit: 200})
					if err != nil || history.Total != 7 {
						t.Fatalf("deadline replaced old history: %#v %v", history, err)
					}
				}
			case "error":
				var value map[string]any
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("unknown fixture shape")
			}
		})
	}
}
