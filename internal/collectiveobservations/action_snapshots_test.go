package collectiveobservations

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/action_snapshots.yaml
var actionSnapshotsYAML []byte

func TestActionAdaptersKeepOriginalSnapshot(t *testing.T) {
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name               string `yaml:"name"`
			Operation          string `yaml:"operation"`
			Rotate             bool   `yaml:"rotate"`
			OtherProfile       bool   `yaml:"otherProfile"`
			SameAccount        bool   `yaml:"sameAccount"`
			SwitchBack         bool   `yaml:"switchBack"`
			RotateDuringDetail bool   `yaml:"rotateDuringDetail"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(actionSnapshotsYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range fixtures.Cases {
		if names[c.Name] {
			t.Fatal("duplicate action snapshot fixture")
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required original-action fixture %s absent", name)
		}
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			db := storetest.Open(t)
			home := t.TempDir()
			var calls atomic.Int32
			var observers, factories atomic.Int32
			const collective schema.VillageUUID = "33333333-3333-4333-8333-333333333333"
			const transcript schema.TranscriptID = "22222222-2222-4222-8222-222222222222"
			creds := auth.Credentials{APIKey: "original-key", KeyID: "original-id", UserID: "11111111-1111-4111-8111-111111111111", Username: "original"}
			var rotateDuringDetail func()
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer original-key" || r.Header.Get("X-Fixture-Selected-Source") != "yes" {
					t.Errorf("wrong action identity: %s %s", r.Method, r.Header.Get("Authorization"))
					w.WriteHeader(401)
					return
				}
				var value any
				switch r.URL.Path {
				case "/api/v1/auth/me":
					value = schema.VillageUser{ID: schema.VillageUUID(creds.UserID)}
				case "/api/v1/collectives/" + collective.String():
					if c.RotateDuringDetail {
						rotateDuringDetail()
					}
					value = schema.VillageGroupDetailResponse{Group: schema.VillageGroupDetailRecord{ID: collective, Name: "Original", AcceptanceMode: "open", DataAccess: "public"}}
				case "/api/v1/transcripts/" + transcript.String() + "/collectives":
					value = schema.VillageTranscriptCollectivesResponse{Collectives: []schema.VillageTranscriptCollective{}}
				case "/api/v1/users/me/collectives/" + collective.String() + "/transcripts/" + transcript.String() + "/events":
					value = []schema.VillageShareEvent{{EventNum: 1, Status: schema.VillageShareStatusPending, RecordedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
				default:
					t.Errorf("unexpected action source %s", r.URL.Path)
					w.WriteHeader(400)
					return
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer remote.Close()
			creds.VillageURL = remote.URL
			if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			original, err := auth.CaptureCredentials(t.Context(), home)
			if err != nil {
				t.Fatal(err)
			}
			selectedHome := home
			other := creds
			other.APIKey, other.KeyID, other.UserID, other.Username = "different-key", "different-id", "44444444-4444-4444-8444-444444444444", "different"
			rotateDuringDetail = func() {
				if err := auth.SaveCredentialsFrom(&other, home); err != nil {
					t.Error(err)
				}
			}
			if c.Rotate {
				if c.SameAccount {
					other = creds
				}
				if err := auth.SaveCredentialsFrom(&other, home); err != nil {
					t.Fatal(err)
				}
				if c.SwitchBack {
					if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
						t.Fatal(err)
					}
				}
			}
			if c.OtherProfile {
				selectedHome = t.TempDir()
				if err := auth.SaveCredentialsFrom(&other, selectedHome); err != nil {
					t.Fatal(err)
				}
			}
			service := New(Dependencies{Store: db, ConfigHome: selectedHome, NewIdentityReader: func(selected auth.Credentials) IdentityReader {
				factories.Add(1)
				client := *remote.Client()
				base := client.Transport
				if base == nil {
					base = http.DefaultTransport
				}
				client.Transport = selectedSourceTransport{base: base}
				source := village.NewVillageClient(selected.VillageURL, selected.APIKey, &client)
				source.SetRequestObserver(func() { observers.Add(1) })
				return source
			}})
			switch c.Operation {
			case "pair":
				_, err = service.ObserveOwnPairFor(t.Context(), original, collective, transcript)
			case "targets":
				var rows []schema.VillageGroupDetailRecord
				rows, err = service.VerifyTargetsFor(t.Context(), original, transcript, []schema.VillageUUID{collective})
				if err == nil && (len(rows) != 1 || rows[0].Name != "Original") {
					t.Fatalf("wrong target projection %#v", rows)
				}
			case "associations":
				var metadata schema.PullAssociationMetadataResult
				metadata, err = service.ObserveAssociationsFor(t.Context(), original, transcript)
				if err == nil && metadata.State != schema.PullAssociationMetadataObserved {
					t.Fatalf("wrong association metadata %#v", metadata)
				}
			case "track":
				var response schema.CollectiveObservationResponse
				response, err = service.TrackFor(t.Context(), original, collective)
				if err == nil && (response.Scope.UserID.String() != creds.UserID || len(response.Collectives) != 1 || response.Collectives[0].ID != collective) {
					t.Fatalf("tracking used wrong original scope: %#v", response)
				}
			case "resolve":
				var id schema.VillageUUID
				id, err = service.ResolveReferenceFor(t.Context(), original, collective.String(), true)
				if err == nil && id != collective {
					t.Fatalf("wrong resolved identity %s", id)
				}
			default:
				t.Fatal("unknown original action operation")
			}
			if c.Rotate || c.RotateDuringDetail {
				if !errors.Is(err, auth.ErrCredentialsChanged) {
					t.Fatalf("rotated action used current account: %v", err)
				}
				if c.Rotate && calls.Load() != 0 {
					t.Fatalf("rotated original action made %d remote requests", calls.Load())
				}
				if c.Rotate && (observers.Load() != 0 || factories.Load() != 0) {
					t.Fatal("rotated original action constructed a client or reported fictional HTTP attempts")
				}
				response, readErr := service.History(t.Context(), schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceRemoteAttempts, Page: 1, Limit: 50})
				if readErr != nil || response.Total != 0 {
					t.Fatalf("old action evidence leaked into new account: %#v %v", response, readErr)
				}
				if c.Operation == "track" {
					current, err := service.SelectedCredentials(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					ids, err := db.CollectiveTrackingIDs(t.Context(), current)
					if err != nil || len(ids) != 0 {
						t.Fatalf("tracking leaked into new account: %v %v", ids, err)
					}
					if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
						t.Fatal(err)
					}
					restored, err := auth.CaptureCredentials(t.Context(), home)
					if err != nil {
						t.Fatal(err)
					}
					ids, err = db.CollectiveTrackingIDs(t.Context(), restored)
					if err != nil || len(ids) != 0 {
						t.Fatalf("rotated original action committed preference: %v %v", ids, err)
					}
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 2 {
					t.Fatalf("original action must use one verification and one targeted read, got %d", calls.Load())
				}
				if observers.Load() != calls.Load() || factories.Load() != 1 {
					t.Fatalf("original factory/observer lost: factories=%d attempts=%d requests=%d", factories.Load(), observers.Load(), calls.Load())
				}
			}
			if c.Operation == "pair" && !c.Rotate {
				attempt, err := db.ReadContributionAttempt(t.Context(), original, collective, transcript, 1)
				if err != nil || attempt == nil || attempt.Attempt.Status != schema.VillageShareStatusPending {
					t.Fatalf("original account attempt missing: %#v %v", attempt, err)
				}
			}
		})
	}
}
