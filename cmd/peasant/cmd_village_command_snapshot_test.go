package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/collectiveobservations"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/collective_command_snapshot.yaml
var collectiveCommandSnapshotYAML []byte

type commandSnapshotCase struct {
	Name      string `yaml:"name"`
	Action    string `yaml:"action"`
	Rotation  string `yaml:"rotation"`
	Reference string `yaml:"reference"`
}

func loadCommandSnapshotCases(t *testing.T) []commandSnapshotCase {
	t.Helper()
	var f struct {
		RequiredNames []string              `yaml:"requiredNames"`
		Cases         []commandSnapshotCase `yaml:"cases"`
	}
	d := yaml.NewDecoder(bytes.NewReader(collectiveCommandSnapshotYAML))
	d.KnownFields(true)
	if err := d.Decode(&f); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		t.Fatalf("extra fixture document: %v", err)
	}
	names := make([]string, 0, len(f.Cases))
	seen := map[string]bool{}
	for _, c := range f.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("duplicate or empty fixture name %q", c.Name)
		}
		seen[c.Name] = true
		switch c.Action {
		case "track", "untrack", "history":
		default:
			t.Fatalf("unknown action %q", c.Action)
		}
		switch c.Rotation {
		case "switch", "renewal", "aba", "none":
		default:
			t.Fatalf("unknown rotation %q", c.Rotation)
		}
		switch c.Reference {
		case "name", "url", "none":
		default:
			t.Fatalf("unknown reference %q", c.Reference)
		}
		names = append(names, c.Name)
	}
	sort.Strings(names)
	sort.Strings(f.RequiredNames)
	if !reflect.DeepEqual(names, f.RequiredNames) {
		t.Fatalf("fixture name manifest: got %v want %v", names, f.RequiredNames)
	}
	return f.Cases
}

// The external profile backend delegates BOTH selection and validation to the
// real disk guard. Rotation occurs only after successful final resolution
// validation, with its lock released; the command then resumes synchronously.
// There are no test goroutines, clocks, sleeps, or mocked service methods.
type rotatingCommandCredentials struct {
	rotate          func() error
	validated       bool
	original        auth.CredentialSnapshot
	rotateOnCapture bool
}

var _ collectiveobservations.CredentialBackend = (*rotatingCommandCredentials)(nil)

func (b *rotatingCommandCredentials) Capture(ctx context.Context, home string) (auth.CredentialSnapshot, error) {
	snapshot, err := auth.CaptureCredentials(ctx, home)
	if err == nil && b.rotateOnCapture && b.rotate != nil {
		b.original = snapshot
		err = b.rotate()
	}
	return snapshot, err
}

func (b *rotatingCommandCredentials) CheckSnapshot(ctx context.Context, snapshot auth.CredentialSnapshot) error {
	if err := snapshot.WithCurrent(ctx, func() error { return nil }); err != nil {
		return err
	}
	if b.validated {
		return nil
	}
	b.validated = true
	b.original = snapshot
	if b.rotate != nil {
		return b.rotate()
	}
	return nil
}

const commandSnapshotCollective schema.VillageUUID = "33333333-3333-4333-8333-333333333333"

func TestVillageOriginalSnapshotOfflineServiceMethods(t *testing.T) {
	for _, c := range loadCommandSnapshotCases(t) {
		if c.Rotation == "none" || c.Action == "track" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			a := auth.Credentials{APIKey: "fixture-a", KeyID: "fixture-a", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture-a", VillageURL: "https://offline.invalid"}
			if err := auth.SaveCredentialsFrom(&a, dir); err != nil {
				t.Fatal(err)
			}
			path := defaults.ResolveDBFilePathWith(dir).String()
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			db, err := openPreparedStore(t, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			service := collectiveobservations.New(collectiveobservations.Dependencies{Store: db, ConfigHome: dir})
			original, err := service.SelectedCredentials(ctx)
			if err != nil {
				t.Fatal(err)
			}
			next := a
			if c.Rotation == "renewal" {
				next.APIKey = "fixture-a-renewed"
			} else {
				next.UserID = "22222222-2222-4222-8222-222222222222"
			}
			if err := auth.SaveCredentialsFrom(&next, dir); err != nil {
				t.Fatal(err)
			}
			if c.Rotation == "aba" {
				if err := auth.SaveCredentialsFrom(&a, dir); err != nil {
					t.Fatal(err)
				}
			}
			if c.Action == "untrack" {
				_, err = service.UntrackFor(ctx, original, commandSnapshotCollective)
			} else {
				_, err = service.HistoryFor(ctx, original, schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceReceipts, Page: 1, Limit: 50})
			}
			if !errors.Is(err, auth.ErrCredentialsChanged) {
				t.Fatalf("original-generation offline method accepted rotated disk profile: %v", err)
			}
		})
	}
}

func TestVillageCommandWholeActionSnapshot(t *testing.T) {
	for _, c := range loadCommandSnapshotCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			a := auth.Credentials{APIKey: "fixture-a", KeyID: "fixture-a", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture-a"}
			b := auth.Credentials{APIKey: "fixture-b", KeyID: "fixture-b", UserID: "22222222-2222-4222-8222-222222222222", Username: "fixture-b"}
			var requests atomic.Int64
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation %s", r.Method)
				}
				var value any
				switch r.URL.Path {
				case "/api/v1/auth/me":
					id := a.UserID
					if strings.Contains(r.Header.Get("Authorization"), b.APIKey) {
						id = b.UserID
					}
					value = schema.VillageUser{ID: schema.VillageUUID(id)}
				case "/api/v1/collectives/search":
					value = schema.VillageCollectiveSearchResponse{Collectives: []schema.VillageCollectiveSearchResult{{ID: commandSnapshotCollective, Name: "shared-name"}}}
				case "/api/v1/collectives/" + commandSnapshotCollective.String():
					value = schema.VillageGroupDetailResponse{Group: schema.VillageGroupDetailRecord{Creator: schema.AnonymousVillageOwner(), ID: commandSnapshotCollective, Name: "shared-name", AcceptanceMode: "open", DataAccess: "public"}}
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(400)
					return
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer remote.Close()
			a.VillageURL, b.VillageURL = remote.URL, remote.URL
			path := defaults.ResolveDBFilePathWith(dir).String()
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			db, err := openPreparedStore(t, path)
			if err != nil {
				t.Fatal(err)
			}
			closed := false
			defer func() {
				if !closed {
					_ = db.Close()
				}
			}()
			seed := func(creds auth.Credentials, marker string) {
				t.Helper()
				if err := auth.SaveCredentialsFrom(&creds, dir); err != nil {
					t.Fatal(err)
				}
				snapshot, err := auth.CaptureCredentials(ctx, dir)
				if err != nil {
					t.Fatal(err)
				}
				inv, err := db.BeginCollectiveObservation(ctx, snapshot)
				if err != nil {
					t.Fatal(err)
				}
				at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				row := schema.CollectiveObservation{ID: commandSnapshotCollective, Name: "shared-name", ObservedAt: at, State: schema.CollectiveObservationStateObserved, Reasons: []schema.CollectiveObservationReason{schema.CollectiveObservationReasonExplicit}}
				if err := db.TrackCollectiveObservation(ctx, inv, row); err != nil {
					t.Fatal(err)
				}
				if c.Action == "track" {
					if err := db.UntrackCollectiveObservation(ctx, inv, commandSnapshotCollective, at); err != nil {
						t.Fatal(err)
					}
				}
				id := commandSnapshotCollective
				if err := db.RecordContributionReceipt(ctx, snapshot, schema.LocalConfirmedContributionReceipt{ReceiptID: marker, Operation: schema.CollectiveOperationShare, TranscriptID: "44444444-4444-4444-8444-444444444444", CollectiveID: &id, ConfirmedAt: at, Outcome: schema.LocalConfirmedContributionSubmitted}); err != nil {
					t.Fatal(err)
				}
			}
			seed(b, "other-account-secret-marker")
			seed(a, "original-account-marker")
			backend := &rotatingCommandCredentials{rotateOnCapture: c.Reference == "none"}
			if c.Rotation != "none" {
				backend.rotate = func() error {
					switch c.Rotation {
					case "switch":
						return auth.SaveCredentialsFrom(&b, dir)
					case "renewal":
						renewed := a
						renewed.APIKey = "fixture-a-renewed"
						return auth.SaveCredentialsFrom(&renewed, dir)
					case "aba":
						if err := auth.SaveCredentialsFrom(&b, dir); err != nil {
							return err
						}
						return auth.SaveCredentialsFrom(&a, dir)
					}
					return nil
				}
			}
			factory := func(*cobra.Command) (*collectiveobservations.Service, func(), error) {
				return collectiveobservations.New(collectiveobservations.Dependencies{Store: db, ConfigHome: dir, Credentials: backend}), func() {}, nil
			}
			var cmd *cobra.Command
			var args []string
			ref := "shared-name"
			if c.Reference == "url" {
				ref = remote.URL + "/collectives/" + commandSnapshotCollective.String()
			}
			if c.Action == "history" {
				cmd = buildVillageContributionsCommand(factory)
				args = []string{"list", "--kind", "receipts", "--json"}
				if c.Reference != "none" {
					args = append(args, "--collective", ref)
				}
			} else {
				cmd = buildVillageCollectivesCommand(factory)
				args = []string{c.Action, ref, "--json"}
			}
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(args)
			err = cmd.ExecuteContext(ctx)
			if c.Reference != "none" && !backend.validated {
				t.Errorf("successful resolver boundary was never reached: %v", err)
			}
			if c.Rotation != "none" {
				if changed := backend.original.WithCurrent(ctx, func() error { return nil }); !errors.Is(changed, auth.ErrCredentialsChanged) {
					t.Errorf("real profile generation did not change after successful resolver validation: %v", changed)
				}
				if !errors.Is(err, auth.ErrCredentialsChanged) {
					t.Errorf("original snapshot must refuse changed generation; got %v; stdout=%s", err, &stdout)
				}
				if err == nil || !strings.Contains(err.Error(), "retry explicitly") {
					t.Errorf("missing explicit retry guidance: %v", err)
				}
				if stdout.Len() != 0 {
					t.Errorf("changed-generation command leaked successful JSON: %s", &stdout)
				}
			} else if err != nil {
				t.Errorf("stable command failed: %v stderr=%s", err, &stderr)
			}
			if c.Action != "track" && requests.Load() != 0 {
				t.Errorf("offline command made %d requests", requests.Load())
			}
			if strings.Contains(stdout.String(), "other-account-secret-marker") {
				t.Errorf("other account history leaked: %s", &stdout)
			}
			if c.Rotation == "none" && c.Action == "history" && !strings.Contains(stdout.String(), "original-account-marker") {
				t.Errorf("original history absent: %s", &stdout)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			closed = true
			// Reopen the real SQL store and independently select each persisted
			// account; neither same-ID visibility nor scope changes can mask writes.
			db, err = store.Open(path, store.WithSkipMigrations())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			verify := func(creds auth.Credentials, original bool) {
				t.Helper()
				if err := auth.SaveCredentialsFrom(&creds, dir); err != nil {
					t.Fatal(err)
				}
				snapshot, err := auth.CaptureCredentials(ctx, dir)
				if err != nil {
					t.Fatal(err)
				}
				ids, err := db.CollectiveTrackingIDs(ctx, snapshot)
				if err != nil {
					t.Fatal(err)
				}
				want := c.Action != "track"
				if original && c.Rotation == "none" && c.Action != "history" {
					want = c.Action == "track"
				}
				expected := []schema.VillageUUID{}
				if want {
					expected = append(expected, commandSnapshotCollective)
				}
				if !reflect.DeepEqual(ids, expected) {
					t.Errorf("persisted tracking changed for original=%v: got %v want tracked=%v", original, ids, want)
				}
			}
			verify(b, false)
			verify(a, true)
		})
	}
}
