package collectiveobservations

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/credential_selection.yaml
var credentialSelectionYAML []byte

type credentialSelectionCase struct {
	Name            string                          `yaml:"name"`
	Online          bool                            `yaml:"online"`
	SavedUserID     string                          `yaml:"savedUserId"`
	RemoteUserID    string                          `yaml:"remoteUserId"`
	Status          int                             `yaml:"status"`
	Rotate          bool                            `yaml:"rotate"`
	Invalid         bool                            `yaml:"invalid"`
	NoRequest       bool                            `yaml:"noRequest"`
	Source          bool                            `yaml:"source"`
	WantCredentials *credentialSelectionExpectation `yaml:"wantCredentials"`
}

type credentialSelectionExpectation struct {
	APIKey      string `yaml:"apiKey"`
	KeyID       string `yaml:"keyId"`
	UserID      string `yaml:"userId"`
	Username    string `yaml:"username"`
	VillagePath string `yaml:"villagePath"`
}

func loadCredentialSelectionFixtures(t *testing.T) []credentialSelectionCase {
	t.Helper()
	var fixtures struct {
		Cases []credentialSelectionCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(credentialSelectionYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatal("missing/duplicate credential fixture name")
		}
		names[fixture.Name] = true
		if !fixture.Invalid && fixture.WantCredentials == nil {
			t.Fatalf("missing selected credential expectation for %s", fixture.Name)
		}
	}
	for _, required := range strings.Fields("offline-no-network online-stable-user-not-username authenticated-source-reuses-verified-client username-reuse invalid-saved-stable-id authentication-refused credentials-rotated-during-authentication") {
		if !names[required] {
			t.Fatalf("missing required fixture %s", required)
		}
	}
	return fixtures.Cases
}

func TestCredentialScopeSelection(t *testing.T) {
	for _, fixture := range loadCredentialSelectionFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			home := t.TempDir()
			var calls atomic.Int32
			var creds auth.Credentials
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if fixture.Source && r.Header.Get("X-Fixture-Selected-Source") != "yes" {
					t.Error("observation did not reuse the verified injected HTTP transport")
				}
				if fixture.Source && r.URL.Path == "/installation/api/v1/transcripts/22222222-2222-4222-8222-222222222222/collectives" {
					if r.Header.Get("Authorization") != "Bearer fixture-key" {
						t.Error("source request lost selected credential")
					}
					_ = json.NewEncoder(w).Encode(schema.VillageTranscriptCollectivesResponse{Collectives: []schema.VillageTranscriptCollective{}})
					return
				}
				if r.URL.Path != "/installation/api/v1/auth/me" || r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Errorf("wrong selected source %s", r.URL.Path)
				}
				if fixture.Rotate {
					if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
						t.Error(err)
					}
				}
				status := fixture.Status
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(schema.VillageUser{ID: schema.VillageUUID(fixture.RemoteUserID), GithubUsername: "same-username"})
			}))
			defer server.Close()
			creds = auth.Credentials{APIKey: "fixture-key", KeyID: "fixture-key-id", UserID: fixture.SavedUserID, Username: "same-username", VillageURL: server.URL + "/installation/"}
			if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			deps := Dependencies{ConfigHome: home}
			var created *village.VillageClient
			var factoryCalls int
			if fixture.Source {
				deps.NewIdentityReader = func(selected auth.Credentials) IdentityReader {
					factoryCalls++
					client := *server.Client()
					base := client.Transport
					if base == nil {
						base = http.DefaultTransport
					}
					client.Transport = selectedSourceTransport{base: base}
					created = village.NewVillageClient(selected.VillageURL, selected.APIKey, &client)
					return created
				}
			}
			service := New(deps)
			var err error
			var snapshot auth.CredentialSnapshot
			if fixture.Source {
				var source Source
				snapshot, source, err = service.AuthenticatedSource(context.Background())
				if err == nil {
					if factoryCalls != 1 {
						t.Fatal("source transport is not the same instance that verified the account")
					}
					rows, readErr := source.ReadTranscriptCollectives(context.Background(), schema.TranscriptID("22222222-2222-4222-8222-222222222222"))
					if readErr != nil || rows == nil || len(rows) != 0 {
						t.Fatalf("authenticated source did not return complete empty association set: %v", readErr)
					}
				}
			} else if fixture.Online {
				snapshot, err = service.AuthenticatedCredentials(context.Background())
			} else {
				snapshot, err = service.SelectedCredentials(context.Background())
			}
			if (err != nil) != fixture.Invalid {
				t.Fatalf("invalid=%v err=%v", fixture.Invalid, err)
			}
			if err == nil {
				expected := fixture.WantCredentials
				want := auth.Credentials{APIKey: expected.APIKey, KeyID: expected.KeyID, UserID: expected.UserID, Username: expected.Username, VillageURL: server.URL + expected.VillagePath}
				if snapshot.Credentials() != want {
					t.Fatal("returned credentials do not match the exact selected profile")
				}
				committed := false
				if err := snapshot.WithCurrent(context.Background(), func() error { committed = true; return nil }); err != nil || !committed {
					t.Fatalf("selected snapshot did not execute its current-generation callback: committed=%v err=%v", committed, err)
				}
			}
			if fixture.Rotate && !errors.Is(err, auth.ErrCredentialsChanged) {
				t.Fatalf("credential generation was not checked after auth/me: %v", err)
			}
			wantCalls := int32(1)
			if fixture.Source {
				wantCalls = 2
			}
			if !fixture.Online || fixture.NoRequest {
				wantCalls = 0
			}
			if calls.Load() != wantCalls {
				t.Fatalf("network requests=%d want=%d", calls.Load(), wantCalls)
			}
		})
	}
}

type selectedSourceTransport struct{ base http.RoundTripper }

var _ http.RoundTripper = selectedSourceTransport{}

func (t selectedSourceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.Header.Set("X-Fixture-Selected-Source", "yes")
	return t.base.RoundTrip(copy)
}

func TestSignedOutPrivateScopeDoesNotLogin(t *testing.T) {
	service := New(Dependencies{ConfigHome: t.TempDir(), NewIdentityReader: func(auth.Credentials) IdentityReader { t.Fatal("signed-out reader contacted Village"); return nil }})
	if _, err := service.SelectedCredentials(context.Background()); !errors.Is(err, auth.ErrSignedOut) {
		t.Fatalf("signed-out private selection: %v", err)
	}
	if _, err := service.AuthenticatedCredentials(context.Background()); !errors.Is(err, auth.ErrSignedOut) {
		t.Fatalf("signed-out online selection: %v", err)
	}
}
