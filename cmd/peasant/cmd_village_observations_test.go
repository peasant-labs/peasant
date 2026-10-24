package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

func TestVillageContributionCommandUsesSavedStore(t *testing.T) {
	f := testutil.LoadCollectiveEvidenceFixtures(t)
	for _, c := range f.Cases {
		if c.Mode != "traverse" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			path := defaults.ResolveDBFilePathWith(dir).String()
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			db, err := openPreparedStore(t, path)
			if err != nil {
				t.Fatal(err)
			}
			creds := auth.Credentials{APIKey: "fixture-key", KeyID: "fixture-key-id", UserID: "11111111-1111-4111-8111-111111111111", VillageURL: "https://offline.invalid/installation", Username: "fixture"}
			if err := auth.SaveCredentialsFrom(&creds, dir); err != nil {
				t.Fatal(err)
			}
			snapshot, err := auth.CaptureCredentials(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			testutil.SeedCollectiveEvidence(t, t.Context(), db, snapshot, f)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			root := buildRootCommand()
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"--data-dir", dir, "--config-dir", dir, "--state-dir", dir, "village", "contributions", "list", "--kind", string(c.Kind), "--limit", "200", "--json"})
			if err := root.ExecuteContext(t.Context()); err != nil {
				t.Fatalf("mounted CLI failed: %v stderr=%s stdout=%s", err, &stderr, &stdout)
			}
			var response schema.LocalContributionHistoryResponse
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatalf("JSON stdout %q: %v", stdout.String(), err)
			}
			if response.Total != c.WantTotal || !reflect.DeepEqual(testutil.CollectiveEvidenceIDs(response), c.WantIDs) {
				t.Fatalf("CLI evidence=%#v want=%v", response, c.WantIDs)
			}
			if err := auth.ClearCredentialsFrom(dir); err != nil {
				t.Fatal(err)
			}
			root = buildRootCommand()
			stdout.Reset()
			stderr.Reset()
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"--data-dir", dir, "--config-dir", dir, "village", "collectives", "list", "--json"})
			if err := root.ExecuteContext(t.Context()); err == nil {
				t.Fatal("signed-out saved CLI used previous account cache")
			}
			if stdout.Len() != 0 {
				t.Fatalf("signed-out CLI leaked %s", &stdout)
			}
		})
	}
}

//go:embed testdata/collective_fetch.yaml
var collectiveFetchCommandYAML []byte

func TestVillageExplicitFetchKeepsPartialJSONAndNonzeroExit(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name       string                                                       `yaml:"name"`
			WantStatus int                                                          `yaml:"wantStatus"`
			WantName   string                                                       `yaml:"wantName"`
			States     map[schema.CollectiveFetchDomain]schema.CollectiveFetchState `yaml:"states"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(collectiveFetchCommandYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range fixture.Cases {
		if names[c.Name] {
			t.Fatal("duplicate CLI partial-fetch fixture")
		}
		names[c.Name] = true
	}
	for _, name := range fixture.RequiredNames {
		if !names[name] {
			t.Fatalf("partial-fetch fixture %s absent", name)
		}
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			creds := auth.Credentials{APIKey: "fixture-key", KeyID: "fixture-id", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture"}
			const collective schema.VillageUUID = "33333333-3333-4333-8333-333333333333"
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("metadata fetch mutated remote state")
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
					value = schema.VillageContributedCollectivesResponse{Collectives: []schema.VillageContributedCollective{{ID: collective, Name: c.WantName, PendingCount: 1}}}
				case "/api/v1/collectives/" + collective.String():
					value = schema.VillageGroupDetailResponse{Group: schema.VillageGroupDetailRecord{ID: collective, Name: c.WantName, AcceptanceMode: "open", DataAccess: "public"}}
				case "/api/v1/users/me/collectives/" + collective.String() + "/submissions":
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"error":"fixture history unavailable"}`))
					return
				default:
					t.Errorf("non-narrow metadata request: %s", r.URL.Path)
					w.WriteHeader(400)
					return
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer remote.Close()
			creds.VillageURL = remote.URL
			if err := auth.SaveCredentialsFrom(&creds, dir); err != nil {
				t.Fatal(err)
			}
			root := buildRootCommand()
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"--data-dir", dir, "--config-dir", dir, "village", "collectives", "fetch", "--json"})
			if err := root.ExecuteContext(t.Context()); err == nil {
				t.Fatal("explicit incomplete fetch exited zero")
			}
			var report schema.CollectiveFetchReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatalf("partial JSON was discarded/corrupted: %s; %v", &stdout, err)
			}
			if err := report.Validate(); err != nil {
				t.Fatal(err)
			}
			if schema.CollectiveFetchHTTPStatus(report) != c.WantStatus {
				t.Fatalf("wrong report/status %#v", report)
			}
			for _, domain := range report.Domains {
				if domain.State != c.States[domain.Domain] {
					t.Fatalf("wrong independent state %#v", domain)
				}
			}
			root = buildRootCommand()
			stdout.Reset()
			stderr.Reset()
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"--data-dir", dir, "--config-dir", dir, "village", "collectives", "list", "--json"})
			if err := root.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			var saved schema.CollectiveObservationResponse
			if err := json.Unmarshal(stdout.Bytes(), &saved); err != nil {
				t.Fatal(err)
			}
			if len(saved.Collectives) != 1 || saved.Collectives[0].Name != c.WantName || saved.Fetch.LastCompletedFetchAt != nil {
				t.Fatalf("partial catalog or all-complete date lied: %#v", saved)
			}
		})
	}
}
