package village_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/observation_request_observer.yaml
var observationRequestObserverYAML []byte

func TestObservationRequestsPreserveActualAttemptObserver(t *testing.T) {
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name         string `yaml:"name"`
			Mode         string `yaml:"mode"`
			WantCalls    int32  `yaml:"wantCalls"`
			WantRequests int32  `yaml:"wantRequests"`
			WantError    bool   `yaml:"wantError"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(observationRequestObserverYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var requests, observed, forwarded atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1); w.WriteHeader(200) }))
			defer other.Close()
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if observed.Load() != requests.Load() {
					t.Error("HTTP request arrived before its attempt observer")
				}
				if c.Mode == "network-failure" {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = connection.Close()
					return
				}
				if c.Mode == "redirect" {
					http.Redirect(w, r, other.URL+"/private", 302)
					return
				}
				if c.Mode == "malformed" {
					_, _ = w.Write([]byte(`{"id":`))
					return
				}
				_ = json.NewEncoder(w).Encode(schema.VillageUser{ID: "11111111-1111-4111-8111-111111111111"})
			}))
			defer remote.Close()
			base := remote.URL
			client := remote.Client()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch c.Mode {
			case "success", "malformed", "redirect", "network-failure":
			case "invalid-url":
				base = "://invalid"
			case "relative-url":
				base = ""
			case "unsupported-url":
				base = "ftp://village.invalid"
			case "cancelled":
				cancel()
			default:
				t.Fatal("unknown observer fixture mode")
			}
			transport := village.NewVillageClient(base, "fixture-key", client)
			transport.SetRequestObserver(func() { observed.Add(1) })
			_, err := transport.VerifyUser(ctx)
			if (err != nil) != c.WantError {
				t.Fatalf("observer path err=%v wantError=%v", err, c.WantError)
			}
			if c.Mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("pre-I/O cancellation was discarded: %v", err)
			}
			if observed.Load() != c.WantCalls || requests.Load() != c.WantRequests || forwarded.Load() != 0 {
				t.Fatalf("attempts=%d requests=%d forwarded=%d want=%d/%d/0", observed.Load(), requests.Load(), forwarded.Load(), c.WantCalls, c.WantRequests)
			}
		})
	}
}
