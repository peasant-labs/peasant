package collectiveobservations

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/references.yaml
var referenceYAML []byte

func TestReferencesAndTrackingUseSelectedScope(t *testing.T) {
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name      string             `yaml:"name"`
			Reference string             `yaml:"reference"`
			Rename    bool               `yaml:"rename"`
			Ambiguous bool               `yaml:"ambiguous"`
			Signout   bool               `yaml:"signout"`
			Refused   bool               `yaml:"refused"`
			Untrack   bool               `yaml:"untrack"`
			WantError bool               `yaml:"wantError"`
			WantID    schema.VillageUUID `yaml:"wantID"`
		} `yaml:"cases"`
	}
	d := yaml.NewDecoder(strings.NewReader(string(referenceYAML)))
	d.KnownFields(true)
	if err := d.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range fixtures.Cases {
		if names[c.Name] {
			t.Fatal("duplicate reference fixture")
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("reference fixture %s missing", name)
		}
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			db := storetest.Open(t)
			home := t.TempDir()
			var calls atomic.Int32
			name := "Saved"
			if c.Rename {
				name = "Old"
			}
			creds := auth.Credentials{APIKey: "fixture", KeyID: "fixture-key", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture"}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet {
					t.Error("tracking mutated remote membership or consent")
				}
				if r.URL.Path == "/installation/api/v1/auth/me" {
					_ = json.NewEncoder(w).Encode(schema.VillageUser{ID: schema.VillageUUID(creds.UserID)})
					return
				}
				if c.Refused {
					w.WriteHeader(404)
					return
				}
				id := strings.TrimPrefix(r.URL.Path, "/installation/api/v1/collectives/")
				if id == r.URL.Path {
					t.Errorf("tracking contacted non-targeted source %s", r.URL.Path)
					w.WriteHeader(400)
					return
				}
				_ = json.NewEncoder(w).Encode(schema.VillageGroupDetailResponse{Group: schema.VillageGroupDetailRecord{Creator: schema.AnonymousVillageOwner(), ID: schema.VillageUUID(id), Name: name, AcceptanceMode: "open", DataAccess: "public"}})
			}))
			defer remote.Close()
			creds.VillageURL = remote.URL + "/installation"
			if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			service := New(Dependencies{Store: db, ConfigHome: home})
			const id schema.VillageUUID = "33333333-3333-4333-8333-333333333333"
			if c.Refused {
				if _, err := service.Track(t.Context(), id); err == nil {
					t.Fatal("hidden target became tracked")
				}
				response, err := service.List(t.Context())
				if err != nil || len(response.Collectives) != 0 {
					t.Fatalf("failed track installed preference: %#v %v", response, err)
				}
				return
			}
			if _, err := service.Track(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			if c.Rename {
				name = "New"
				if _, err := service.Track(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			}
			if c.Ambiguous {
				if _, err := service.Track(t.Context(), "44444444-4444-4444-8444-444444444444"); err != nil {
					t.Fatal(err)
				}
			}
			if c.Signout {
				if err := auth.ClearCredentialsFrom(home); err != nil {
					t.Fatal(err)
				}
			}
			beforeCalls := calls.Load()
			reference := strings.ReplaceAll(c.Reference, "{base}", remote.URL)
			got, err := service.ResolveReference(t.Context(), reference, false)
			if (err != nil) != c.WantError || got != c.WantID {
				t.Fatalf("offline resolution=%s err=%v want=%s error=%v", got, err, c.WantID, c.WantError)
			}
			if calls.Load() != beforeCalls {
				t.Fatal("offline reference resolution contacted Village")
			}
			if c.Untrack {
				response, err := service.Untrack(t.Context(), got)
				if err != nil || len(response.Collectives) != 0 {
					t.Fatalf("untrack retained sole explicit reason: %#v %v", response, err)
				}
				again, err := service.Untrack(t.Context(), got)
				if err != nil || again.CacheRevision != response.CacheRevision {
					t.Fatalf("idempotent untrack changed payload revision: %#v %v", again, err)
				}
				if calls.Load() != beforeCalls {
					t.Fatal("untrack contacted Village")
				}
			}
		})
	}
}
