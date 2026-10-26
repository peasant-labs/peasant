package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/pull-explicit-tracking.yaml
var pullExplicitTrackingYAML []byte

func TestPullExplicitTrackingFixtures(t *testing.T) {
	var f struct {
		UserID            string             `yaml:"userId"`
		OtherUserID       string             `yaml:"otherUserId"`
		CollectiveID      schema.VillageUUID `yaml:"collectiveId"`
		OtherCollectiveID schema.VillageUUID `yaml:"otherCollectiveId"`
		Name              string             `yaml:"name"`
		RequiredNames     []string           `yaml:"requiredNames"`
		Cases             []struct {
			Name          string   `yaml:"name"`
			Refs          []string `yaml:"refs"`
			BasePath      string   `yaml:"basePath"`
			Mode          string   `yaml:"mode"`
			WantTracked   bool     `yaml:"wantTracked"`
			ErrorContains string   `yaml:"errorContains"`
			RotateDetail  int32    `yaml:"rotateDetail"`
			DryRun        bool     `yaml:"dryRun"`
			ZeroAuth      bool     `yaml:"zeroAuth"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(pullExplicitTrackingYAML, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			var detailCalls, authCalls, foreignCalls, listingCalls atomic.Int32
			foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				foreignCalls.Add(1)
				http.Error(w, "no forwarded credentials", 500)
			}))
			defer foreign.Close()
			var base string
			credentials := func(user string) *auth.Credentials {
				return &auth.Credentials{UserID: user, Username: "fixture", APIKey: "fixture-key", KeyID: "fixture-id", VillageURL: base}
			}
			now := time.Now().UTC()
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, c.BasePath+"/api/v1/") {
					t.Errorf("lost installation base path: %s", r.URL.Path)
					http.Error(w, "wrong base", 404)
					return
				}
				path := strings.TrimPrefix(r.URL.Path, c.BasePath)
				if r.Method != http.MethodGet {
					t.Errorf("tracking changed remote state: %s %s", r.Method, path)
					http.Error(w, "no remote mutation", 500)
					return
				}
				switch path {
				case "/api/v1/auth/me":
					authCalls.Add(1)
					_ = json.NewEncoder(w).Encode(schema.VillageUser{ID: schema.VillageUUID(f.UserID), CreatedAt: now, UpdatedAt: now})
				case "/api/v1/collectives/search":
					rows := []schema.VillageCollectiveSearchResult{}
					if c.Mode != "unknown" {
						rows = append(rows, schema.VillageCollectiveSearchResult{ID: f.CollectiveID, Name: f.Name})
						if c.Mode == "ambiguous" {
							rows = append(rows, schema.VillageCollectiveSearchResult{ID: f.OtherCollectiveID, Name: f.Name})
						}
					}
					_ = json.NewEncoder(w).Encode(schema.VillageCollectiveSearchResponse{Collectives: rows})
				case "/api/v1/collectives/" + f.CollectiveID.String():
					call := detailCalls.Add(1)
					if call == c.RotateDetail {
						if err := auth.SaveCredentialsFrom(credentials(f.OtherUserID), dir); err != nil {
							t.Error(err)
						}
					}
					if c.Mode == "tracking-refused" && call == 2 {
						http.Error(w, "visibility withdrawn", http.StatusForbidden)
						return
					}
					_ = json.NewEncoder(w).Encode(schema.VillageGroupDetailResponse{Group: schema.VillageGroupDetailRecord{Creator: schema.AnonymousVillageOwner(), ID: f.CollectiveID, Name: f.Name, AcceptanceMode: schema.VillageGroupAcceptanceOpen, DataAccess: schema.VillageGroupDataAccessMembersOnly, CreatedAt: now, UpdatedAt: now}})
				case "/api/v1/collectives":
					rows := []schema.VillageUserGroup{}
					if c.DryRun {
						rows = append(rows, schema.VillageUserGroup{Creator: schema.AnonymousVillageOwner(), ID: f.CollectiveID, Name: f.Name})
					}
					_ = json.NewEncoder(w).Encode(rows)
				case "/api/v1/schema/version":
					_, _ = w.Write([]byte(stubSchemaVersionBody()))
				case "/api/v1/pull/transcripts":
					listingCalls.Add(1)
					_ = json.NewEncoder(w).Encode(schema.PullListResponse{Transcripts: []schema.PullTranscriptInfo{}, Page: 1, Limit: 50, Total: 0})
				default:
					t.Errorf("unexpected request %s", path)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer remote.Close()
			base = remote.URL + c.BasePath
			if err := auth.SaveCredentialsFrom(credentials(f.UserID), dir); err != nil {
				t.Fatal(err)
			}
			args := []string{"transcripts", "pull", "--json"}
			for _, ref := range c.Refs {
				ref = strings.NewReplacer("{id}", f.CollectiveID.String(), "{base}", base, "{origin}", remote.URL, "{foreign}", foreign.URL).Replace(ref)
				args = append(args, "--collective", ref)
			}
			if c.DryRun {
				args = append(args, "--dry-run")
			}
			out, stderr, err := runVillageSeparated(t, dir, args...)
			if c.ErrorContains != "" {
				if err == nil || !strings.Contains(err.Error(), c.ErrorContains) {
					t.Fatalf("want error %q got %v stderr=%s", c.ErrorContains, err, stderr)
				}
			} else {
				if err != nil {
					t.Fatalf("visible nonmember explicit selection: %v %s", err, stderr)
				}
				var result jsonPullCollective
				if err := json.Unmarshal([]byte(out), &result); err != nil {
					t.Fatal(err)
				}
				if result.Totals.Total != 0 || len(result.Collectives) != 1 {
					t.Fatalf("empty explicit shelf changed envelope: %s", out)
				}
			}
			if foreignCalls.Load() != 0 {
				t.Fatal("credentials forwarded to another Village")
			}
			if c.ZeroAuth && (authCalls.Load() != 0 || detailCalls.Load() != 0) {
				t.Fatal("rejected URL or dry run introduced auth/detail I/O")
			}
			if c.WantTracked && listingCalls.Load() != 1 {
				t.Fatal("explicit shelf did not execute real required listing")
			}
			// Reopen SQLite and explicitly select each principal through production
			// credentials APIs. No pre/post adapter check substitutes for the actual
			// core guarded transaction during the detail-response rotation above.
			db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			check := func(user string, want bool) {
				t.Helper()
				if err := auth.SaveCredentialsFrom(credentials(user), dir); err != nil {
					t.Fatal(err)
				}
				snapshot, err := auth.CaptureCredentials(t.Context(), dir)
				if err != nil {
					t.Fatal(err)
				}
				response, err := db.ReadCollectiveObservations(t.Context(), snapshot)
				if err != nil {
					t.Fatal(err)
				}
				if !want && len(response.Collectives) > 0 {
					t.Fatalf("unexpected private catalog mutation for principal %s: %+v", user, response.Collectives)
				}
				found := false
				for _, row := range response.Collectives {
					if row.ID != f.CollectiveID {
						t.Fatal("tracking created an unrelated collective")
					}
					if row.ID == f.CollectiveID {
						for _, reason := range row.Reasons {
							if reason != schema.CollectiveObservationReasonExplicit {
								t.Fatalf("tracking invented a membership/contribution reason: %s", reason)
							}
							if reason == schema.CollectiveObservationReasonExplicit {
								found = true
							}
						}
					}
				}
				if found != want {
					t.Fatalf("principal %s explicit preference=%v want=%v", user, found, want)
				}
			}
			check(f.UserID, c.WantTracked)
			check(f.OtherUserID, false)
		})
	}
}
