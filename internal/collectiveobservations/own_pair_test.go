package collectiveobservations

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/own_pair.yaml
var ownPairYAML []byte

type ownPairFixture struct {
	Name               string                     `yaml:"name"`
	Before             []schema.VillageShareEvent `yaml:"before"`
	After              []schema.VillageShareEvent `yaml:"after"`
	Expected           []schema.VillageShareEvent `yaml:"expected"`
	Status             int                        `yaml:"status"`
	Rotate             bool                       `yaml:"rotate"`
	WantError          string                     `yaml:"wantError"`
	WantGeneration     int64                      `yaml:"wantGeneration"`
	FirstCommitFrames  int                        `yaml:"firstCommitFrames"`
	SecondCommitFrames int                        `yaml:"secondCommitFrames"`
}

func loadOwnPairFixtures(t *testing.T) []ownPairFixture {
	t.Helper()
	var fixtures struct {
		RequiredNames []string         `yaml:"requiredNames"`
		Cases         []ownPairFixture `yaml:"cases"`
	}
	d := yaml.NewDecoder(strings.NewReader(string(ownPairYAML)))
	d.KnownFields(true)
	if err := d.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range fixtures.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatal("missing/duplicate own-pair fixture name")
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required own-pair case %q missing", name)
		}
	}
	return fixtures.Cases
}

func TestObserveOwnPairUsesAuthenticatedSourceAndStore(t *testing.T) {
	for _, fixture := range loadOwnPairFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := t.Context()
			home := t.TempDir()
			dbPath := storetest.CopyGoldenDB(t)
			local, err := store.Open(dbPath, store.WithSkipMigrations(), store.WithWALAutocheckpointDisabled())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = local.Close() })
			commitFrames := func() int {
				t.Helper()
				count, err := storetest.CountWALCommitFrames(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				return count
			}
			creds := auth.Credentials{APIKey: "fixture-key", KeyID: "fixture-key-id", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture"}
			var pairReads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Error("wrong selected credential on source request")
				}
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_ = json.NewEncoder(w).Encode(schema.VillageUser{ID: schema.VillageUUID(creds.UserID)})
				case "/api/v1/users/me/collectives/33333333-3333-4333-8333-333333333333/transcripts/22222222-2222-4222-8222-222222222222/events":
					if pairReads.Add(1) == 1 {
						_ = json.NewEncoder(w).Encode(fixture.Before)
						return
					}
					if fixture.Rotate {
						if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
							t.Error(err)
						}
					}
					if fixture.Status != 0 {
						w.WriteHeader(fixture.Status)
						_ = json.NewEncoder(w).Encode(schema.VillageErrorResponse{Error: "source unavailable"})
						return
					}
					_ = json.NewEncoder(w).Encode(fixture.After)
				default:
					t.Errorf("unexpected observation side effect %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			creds.VillageURL = server.URL
			if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			observed := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
			service := New(Dependencies{ConfigHome: home, Store: local, Now: func() time.Time { return observed }})
			collective := schema.VillageUUID("33333333-3333-4333-8333-333333333333")
			transcript := schema.TranscriptID("22222222-2222-4222-8222-222222222222")
			beforeCommits := commitFrames()
			started := time.Now()
			if _, err := service.ObserveOwnPair(ctx, collective, transcript); err != nil {
				t.Fatal(err)
			}
			t.Logf("first targeted source observation wall=%s", time.Since(started))
			if got := commitFrames() - beforeCommits; got != fixture.FirstCommitFrames {
				t.Fatalf("first targeted source commit count=%d want=%d", got, fixture.FirstCommitFrames)
			}
			observed = observed.Add(time.Hour)
			beforeCommits = commitFrames()
			result, err := service.ObserveOwnPair(ctx, collective, transcript)
			if got := commitFrames() - beforeCommits; got != fixture.SecondCommitFrames {
				t.Fatalf("second targeted source commit count=%d want=%d", got, fixture.SecondCommitFrames)
			}
			switch fixture.WantError {
			case "":
				if err != nil || !result.Changed || result.Generation != fixture.WantGeneration {
					t.Fatalf("targeted effect %#v err=%v", result, err)
				}
			case "credentials":
				if !errors.Is(err, auth.ErrCredentialsChanged) {
					t.Fatalf("late source response crossed credential generation: %v", err)
				}
			case "remote":
				var status *village.StatusError
				if !errors.As(err, &status) || status.StatusCode != fixture.Status {
					t.Fatalf("remote refusal lost typed status: %v", err)
				}
			default:
				t.Fatalf("unknown expected error %q", fixture.WantError)
			}
			snapshot, err := auth.CaptureCredentials(ctx, home)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range fixture.Expected {
				got, err := local.ReadContributionAttempt(ctx, snapshot, collective, transcript, expected.EventNum)
				if err != nil || got == nil || !reflect.DeepEqual(got.Attempt, expected) {
					t.Fatalf("targeted source did not retain exact canonical evidence: %#v err=%v", got, err)
				}
				if !got.FirstSeenAt.Equal(observed.Add(-time.Hour)) {
					t.Fatal("targeted revalidation changed first-seen evidence")
				}
			}
		})
	}
}
