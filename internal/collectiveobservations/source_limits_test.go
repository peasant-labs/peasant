package collectiveobservations

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/source_limits.yaml
var sourceLimitsYAML []byte

func TestSourceConcurrencyBoundOnAuthenticatedProductionReads(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name         string `yaml:"name"`
			Limit        int    `yaml:"limit"`
			Requests     int    `yaml:"requests"`
			ContentionMs int    `yaml:"contentionMs"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(sourceLimitsYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range fixture.Cases {
		if names[c.Name] {
			t.Fatal("duplicate source bound case")
		}
		names[c.Name] = true
		if c.Limit < 1 || c.Requests <= c.Limit || c.ContentionMs < 1 {
			t.Fatal("source bound case must exercise contention")
		}
	}
	for _, name := range fixture.RequiredNames {
		if !names[name] {
			t.Fatalf("required source bound case %s absent", name)
		}
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			home := t.TempDir()
			entered := make(chan struct{}, c.Requests)
			release := make(chan struct{})
			var releaseOnce sync.Once
			var active, peak atomic.Int32
			creds := auth.Credentials{APIKey: "fixture-key", KeyID: "fixture-id", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture"}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/auth/me" {
					t.Error("source bound exercised wrong path")
					w.WriteHeader(400)
					return
				}
				now := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); now > old; old = peak.Load() {
					if peak.CompareAndSwap(old, now) {
						break
					}
				}
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_ = json.NewEncoder(w).Encode(schema.VillageUser{ID: schema.VillageUUID(creds.UserID)})
			}))
			defer func() { releaseOnce.Do(func() { close(release) }); cancel(); remote.Close() }()
			creds.VillageURL = remote.URL
			if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			snapshot, err := auth.CaptureCredentials(ctx, home)
			if err != nil {
				t.Fatal(err)
			}
			service := New(Dependencies{ConfigHome: home})
			results := make(chan error, c.Requests)
			var workers sync.WaitGroup
			workers.Add(c.Requests)
			for range c.Requests {
				go func() {
					defer workers.Done()
					_, _, err := service.AuthenticatedSourceFor(ctx, snapshot)
					results <- err
				}()
			}
			done := make(chan struct{})
			go func() { workers.Wait(); close(done) }()
			defer func() {
				releaseOnce.Do(func() { close(release) })
				cancel()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("source workers did not stop before cleanup")
				}
			}()
			for range c.Limit {
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			select {
			case <-entered:
				t.Fatal("source admitted more than four active authenticated reads")
			case <-time.After(time.Duration(c.ContentionMs) * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			releaseOnce.Do(func() { close(release) })
			for range c.Requests {
				select {
				case err := <-results:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if peak.Load() != int32(c.Limit) {
				t.Fatalf("actual HTTP peak=%d want=%d", peak.Load(), c.Limit)
			}
		})
	}
}
