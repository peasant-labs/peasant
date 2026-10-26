package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/local-collective-observations.yaml
var localObservationYAML []byte

func runVillageSeparated(t *testing.T, dir string, args ...string) (string, string, error) {
	return runVillageSeparatedUsing(t.Context(), t, dir, nil, args...)
}

func runVillageSeparatedUsing(ctx context.Context, t *testing.T, dir string, dependencies *villageListDependencies, args ...string) (string, string, error) {
	t.Helper()
	root := newTestRoot()
	root.SetContext(ctx)
	root.AddCommand(BuildVillageCommand())
	if dependencies != nil {
		command, _, err := root.Find([]string{"village", "transcripts", "list"})
		if err != nil {
			return "", "", err
		}
		parent := command.Parent()
		parent.RemoveCommand(command)
		parent.AddCommand(buildVillageTranscriptsListCommand(*dependencies))
	}
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, "--state-dir", dir, "village"}, args...))
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestLocalCollectiveObservationFixtures(t *testing.T) {
	var fixtures struct {
		UserID      string    `yaml:"userId"`
		OtherUserID string    `yaml:"otherUserId"`
		ObservedAt  time.Time `yaml:"observedAt"`
		Collectives []struct {
			ID   schema.VillageUUID `yaml:"id"`
			Name string             `yaml:"name"`
		} `yaml:"collectives"`
		Transcripts []struct {
			ID          schema.TranscriptID  `yaml:"id"`
			Owner       string               `yaml:"owner"`
			Collectives []schema.VillageUUID `yaml:"collectives"`
			Legacy      bool                 `yaml:"legacy"`
			Invalidated bool                 `yaml:"invalidated"`
			OtherBase   bool                 `yaml:"otherBase"`
		} `yaml:"transcripts"`
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name             string                                       `yaml:"name"`
			Refs             []string                                     `yaml:"refs"`
			Owner            string                                       `yaml:"owner"`
			IDs              []string                                     `yaml:"ids"`
			SignedOut        bool                                         `yaml:"signedOut"`
			OtherAccount     bool                                         `yaml:"otherAccount"`
			Stale            bool                                         `yaml:"stale"`
			ErrorContains    string                                       `yaml:"errorContains"`
			Table            bool                                         `yaml:"table"`
			Disclosures      map[schema.TranscriptID][]schema.VillageUUID `yaml:"disclosures"`
			GenerationChange string                                       `yaml:"generationChange"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(localObservationYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			var requests atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "offline listing must not request Village", 500)
			}))
			defer remote.Close()
			creds := auth.Credentials{UserID: fixtures.UserID, Username: "fixture", APIKey: "fixture-key", KeyID: "fixture-key-id", VillageURL: remote.URL}
			if err := auth.SaveCredentialsFrom(&creds, dir); err != nil {
				t.Fatal(err)
			}
			db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := auth.CaptureCredentials(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			invocation, err := db.BeginCollectiveObservation(t.Context(), snapshot)
			if err != nil {
				t.Fatal(err)
			}
			byID := map[schema.VillageUUID]string{}
			for _, group := range fixtures.Collectives {
				byID[group.ID] = group.Name
				if err := db.TrackCollectiveObservation(t.Context(), invocation, schema.CollectiveObservation{ID: group.ID, Name: group.Name, ObservedAt: fixtures.ObservedAt, State: schema.CollectiveObservationStateObserved, Reasons: []schema.CollectiveObservationReason{schema.CollectiveObservationReasonExplicit}}); err != nil {
					t.Fatal(err)
				}
			}
			u, err := url.Parse(remote.URL)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range fixtures.Transcripts {
				pullDir := filepath.Join(string(defaults.ResolveVillagePullsDirPathWith(dir)), u.Host, row.ID.String())
				if err := os.MkdirAll(pullDir, 0o700); err != nil {
					t.Fatal(err)
				}
				base := remote.URL
				if row.OtherBase {
					base += "/other"
				}
				data, err := json.Marshal(pull.PullManifest{ManifestVersion: pull.PullManifestVersion, VillageURL: base, VillageHost: u.Host, TranscriptID: row.ID})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(pullDir, pull.ManifestFilename), data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := db.CommitPull(t.Context(), store.PullCommit{Transcript: store.PulledTranscriptRow{VillageHost: u.Host, TranscriptID: row.ID, OwnerUserID: fixtures.UserID, OwnerUsername: row.Owner, ContentHash: "fixture-content", PullDir: pullDir, FirstPulledAt: fixtures.ObservedAt.UnixMilli(), LastPulledAt: fixtures.ObservedAt.UnixMilli(), Visibility: schema.VisibilityPrivate}}); err != nil {
					t.Fatal(err)
				}
				if row.Legacy {
					continue
				}
				associations := make([]schema.VillageTranscriptCollective, 0, len(row.Collectives))
				for _, id := range row.Collectives {
					associations = append(associations, schema.VillageTranscriptCollective{ID: id, Name: byID[id], SharedAt: fixtures.ObservedAt})
				}
				if err := db.InstallTranscriptAssociations(t.Context(), invocation, row.ID, associations, fixtures.ObservedAt, false); err != nil {
					t.Fatal(err)
				}
				if row.Invalidated {
					if err := db.InstallTranscriptAssociations(t.Context(), invocation, row.ID, []schema.VillageTranscriptCollective{}, fixtures.ObservedAt.Add(time.Minute), true); err != nil {
						t.Fatal(err)
					}
				}
				if c.Stale && !row.Invalidated {
					if err := db.RecordTranscriptAssociationFailure(t.Context(), invocation, row.ID, fixtures.ObservedAt.Add(time.Hour), schema.OperationIssue{Code: schema.OperationIssueRemote, Operation: schema.CollectiveOperationPull, Message: "Previous optional source failed", Retry: "Fetch explicitly"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if c.SignedOut {
				if err := auth.ClearCredentialsFrom(dir); err != nil {
					t.Fatal(err)
				}
			}
			if c.OtherAccount {
				creds.UserID = fixtures.OtherUserID
				if err := auth.SaveCredentialsFrom(&creds, dir); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"transcripts", "list", "--local"}
			if !c.Table {
				args = append(args, "--json")
			}
			for _, ref := range c.Refs {
				args = append(args, "--collective", ref)
			}
			if c.Owner != "" {
				args = append(args, "--user", c.Owner)
			}
			var stdout, stderr string
			if c.GenerationChange != "" {
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				started, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				dependencies := villageListDependencies{NewProvenanceReader: func(root string) func(string) ([]byte, error) {
					read := pull.NewFileSystem(root).ReadFile
					return func(path string) ([]byte, error) {
						data, err := read(path)
						if err != nil {
							return nil, err
						}
						once.Do(func() { close(started) })
						select {
						case <-release:
							return data, nil
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
				}}
				type output struct {
					stdout, stderr string
					err            error
				}
				done := make(chan output, 1)
				finished := make(chan struct{})
				go func() {
					defer close(finished)
					out, errOut, err := runVillageSeparatedUsing(ctx, t, dir, &dependencies, args...)
					done <- output{out, errOut, err}
				}()
				t.Cleanup(func() {
					cancel()
					select {
					case <-finished:
					case <-time.After(5 * time.Second):
						t.Error("provenance-gated command did not release owned resources after cancellation")
					}
				})
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("command never reached real provenance dependency after observation reads")
				}
				rotated := creds
				if c.GenerationChange == "other-principal" {
					rotated.UserID = fixtures.OtherUserID
				}
				if err := auth.SaveCredentialsFrom(&rotated, dir); err != nil {
					t.Fatal(err)
				}
				close(release)
				select {
				case result := <-done:
					stdout, stderr, err = result.stdout, result.stderr, result.err
				case <-ctx.Done():
					t.Fatal("provenance-gated command did not terminate")
				}
				if stdout != "" {
					t.Fatalf("private stdout escaped after assembled-view generation change: %q", stdout)
				}
			} else {
				stdout, stderr, err = runVillageSeparated(t, dir, args...)
			}
			if requests.Load() != 0 {
				t.Fatalf("offline command contacted Village %d times", requests.Load())
			}
			if c.ErrorContains != "" {
				if err == nil || !strings.Contains(err.Error(), c.ErrorContains) {
					t.Fatalf("missing scoped error %q: %v %s", c.ErrorContains, err, stderr)
				}
				return
			}
			if err != nil {
				t.Fatalf("offline list: %v stderr=%s", err, stderr)
			}
			if c.Table {
				if !strings.Contains(stdout, fixtures.UserID) || !strings.Contains(stdout, remote.URL) || !strings.Contains(stdout, fixtures.ObservedAt.Format(time.RFC3339)) {
					t.Fatalf("table omitted selected scope or true observation date: %s", stdout)
				}
				for _, id := range c.IDs {
					if !strings.Contains(stdout, id) {
						t.Fatal("table omitted matched transcript")
					}
				}
				return
			}
			if !strings.HasPrefix(strings.TrimSpace(stdout), "[") {
				t.Fatalf("local inventory envelope changed: %s", stdout)
			}
			var rows []jsonLocalListRow
			if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(rows))
			returned := map[schema.TranscriptID]bool{}
			for _, row := range rows {
				id, err := schema.NewTranscriptID(row.TranscriptID)
				if err != nil {
					t.Fatal(err)
				}
				if returned[id] {
					t.Fatalf("duplicate returned transcript ID: %s", id)
				}
				returned[id] = true
				ids = append(ids, row.TranscriptID)
				if len(c.Refs) == 0 {
					if row.CollectiveObservation != nil {
						t.Fatal("accountless inventory exposed private observations")
					}
					continue
				}
				if row.CollectiveObservation == nil {
					t.Fatal("matched per-row observation omitted")
				}
				if err := row.CollectiveObservation.Metadata.Validate(); err != nil {
					t.Fatal(err)
				}
				if row.CollectiveObservation.Scope.UserID.String() != fixtures.UserID {
					t.Fatal("wrong account observation escaped")
				}
				if c.Disclosures != nil {
					want, exists := c.Disclosures[id]
					if !exists {
						t.Fatalf("unexpected disclosure row key %s", id)
					}
					actual := map[schema.VillageUUID]bool{}
					for _, collective := range row.CollectiveObservation.Collectives {
						if actual[collective.ID] {
							t.Fatalf("duplicate disclosed collective %s on transcript %s", collective.ID, id)
						}
						actual[collective.ID] = true
						if collective.Name != byID[collective.ID] {
							t.Fatalf("disclosed name differs from saved source on %s", id)
						}
					}
					expected := map[schema.VillageUUID]bool{}
					for _, collective := range want {
						if expected[collective] {
							t.Fatal("duplicate expected disclosure fixture")
						}
						expected[collective] = true
					}
					if !reflect.DeepEqual(actual, expected) {
						t.Fatalf("exact disclosed collective payload for %s differs: got=%v want=%v", id, actual, expected)
					}
				}
				if c.Stale && (row.CollectiveObservation.Metadata.State != schema.PullAssociationMetadataStale || !row.CollectiveObservation.Metadata.ObservedAt.Equal(fixtures.ObservedAt)) {
					t.Fatal("stale evidence falsely refreshed its prior time")
				}
			}
			if c.Disclosures != nil {
				for id := range c.Disclosures {
					if !returned[id] {
						t.Fatalf("missing exact disclosure row key %s", id)
					}
				}
			}
			sort.Strings(ids)
			want := append([]string{}, c.IDs...)
			sort.Strings(want)
			if !reflect.DeepEqual(ids, want) {
				t.Fatalf("actual per-row membership differs: %v != %v", ids, want)
			}
		})
	}
}
