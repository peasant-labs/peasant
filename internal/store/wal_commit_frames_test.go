package store_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/wal_commit_frames.yaml
var walCommitFramesYAML []byte

// walCommitAction is the closed set of store operations the baseline drives.
type walCommitAction string

const (
	walCommitActionMirror         walCommitAction = "mirror"
	walCommitActionMirrorTwice    walCommitAction = "mirror-twice"
	walCommitActionEntryBatch     walCommitAction = "entry-batch"
	walCommitActionReadIndexState walCommitAction = "read-index-state"
)

func newWALCommitAction(value string) (walCommitAction, error) {
	switch walCommitAction(value) {
	case walCommitActionMirror, walCommitActionMirrorTwice, walCommitActionEntryBatch, walCommitActionReadIndexState:
		return walCommitAction(value), nil
	}
	return "", fmt.Errorf("load write-ahead-log commit fixture: action %q is not a supported store operation", value)
}

type walCommitFramesFixtures struct {
	RequiredNames []string `yaml:"requiredNames"`
	HostSlug      string   `yaml:"hostSlug"`
	ProjectHash   string   `yaml:"projectHash"`
	Transcript    string   `yaml:"transcript"`
	Cases         []struct {
		Name        string `yaml:"name"`
		Action      string `yaml:"action"`
		Sessions    int    `yaml:"sessions"`
		WantCommits int    `yaml:"wantCommits"`
	} `yaml:"cases"`
}

func loadWALCommitFramesFixtures(t *testing.T) walCommitFramesFixtures {
	t.Helper()
	var fixture walCommitFramesFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(walCommitFramesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("write-ahead-log commit fixture requires one YAML document")
	}
	present := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || present[row.Name] {
			t.Fatalf("invalid write-ahead-log commit case %q", row.Name)
		}
		present[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("write-ahead-log commit fixture testdata/wal_commit_frames.yaml", "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func walFixtureArtifacts(t *testing.T, fixture walCommitFramesFixtures, count int) []ingest.ArtifactMirrorRequest {
	t.Helper()
	requests := make([]ingest.ArtifactMirrorRequest, 0, count)
	for i := range count {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", i+1, i+1)
		entry := makeStoreEntry(t, id, fixture.ProjectHash, fixture.HostSlug, defaults.HarnessClaudeCode, 1700000000000+int64(i), 100, 50)
		requests = append(requests, ingest.ArtifactMirrorRequest{Artifact: mirrorTestArtifact(t, entry.Metadata, fixture.Transcript)})
	}
	return requests
}

// TestWALCommitFrameCounterBaseline pins the commit counter to known store
// operations before any test trusts it to count the write path's commits.
func TestWALCommitFrameCounterBaseline(t *testing.T) {
	t.Parallel()
	fixture := loadWALCommitFramesFixtures(t)
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			action, err := newWALCommitAction(row.Action)
			if err != nil {
				t.Fatal(err)
			}
			dbPath := filepath.Join(t.TempDir(), "commits.db")
			db, err := store.Open(dbPath, store.WithWALAutocheckpointDisabled(), store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := t.Context()
			requests := walFixtureArtifacts(t, fixture, row.Sessions)
			mirror := func(page []ingest.ArtifactMirrorRequest) {
				t.Helper()
				for _, result := range db.MirrorArtifacts(ctx, page) {
					if result.Err != nil || !result.Mirrored {
						t.Fatalf("mirror %s: %v", result.SessionID, result.Err)
					}
				}
			}
			// Every case except the mirror ones needs a stored row to work on;
			// that seed commits before the measurement starts.
			if action == walCommitActionEntryBatch || action == walCommitActionReadIndexState {
				mirror(requests)
			}
			before, err := storetest.CountWALCommitFrames(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			sid := requests[0].Artifact.Metadata.SessionID
			switch action {
			case walCommitActionMirror:
				mirror(requests)
			case walCommitActionMirrorTwice:
				half := len(requests) / 2
				mirror(requests[:half])
				mirror(requests[half:])
			case walCommitActionEntryBatch:
				state, err := db.ReadIndexState(ctx, sid)
				if err != nil || state == nil {
					t.Fatalf("read seeded index state: %v", err)
				}
				results := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
					SessionID: sid, Result: indexformat.V1{Entries: []schema.SessionEntry{}}, IndexVersion: 1,
					IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, IndexedAtMs: 1700000001000, ExpectedState: state,
				}})
				if len(results) != 1 || results[0].Err != nil || !results[0].Written {
					t.Fatalf("entry batch: %+v", results)
				}
			case walCommitActionReadIndexState:
				if _, err := db.ReadIndexState(ctx, sid); err != nil {
					t.Fatal(err)
				}
			}
			after, err := storetest.CountWALCommitFrames(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := after - before; got != row.WantCommits {
				t.Fatalf("%s: the log gained %d commit frames, want %d (before %d, after %d)", row.Action, got, row.WantCommits, before, after)
			}
		})
	}
}
