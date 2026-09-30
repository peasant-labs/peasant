package store_test

import (
	_ "embed"
	"reflect"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/artifact_mirror_transactions.yaml
var artifactMirrorTransactionsYAML []byte

func TestArtifactMirrorStopsOnOuterTransactionLoss(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Sessions      []string `yaml:"sessions"`
		Cases         []struct {
			Name     string   `yaml:"name"`
			Setup    string   `yaml:"setup"`
			Mirrored []string `yaml:"mirrored"`
			Panic    string   `yaml:"panic"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(artifactMirrorTransactionsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	required := []string{"middle-item-rolls-back-outer", "commit-failure-rolls-back-all", "item-abort-preserves-peers", "panic-rolls-back-all"}
	if !reflect.DeepEqual(required, fixture.RequiredNames) {
		t.Fatal("mirror transaction fixture manifest changed")
	}
	seen := make(map[string]bool)
	input := loadArtifactMirrorFixtures(t)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] || row.Setup == "" {
			t.Fatalf("invalid transaction fixture %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			var db *store.Store
			if row.Panic != "" {
				db = storetest.OpenWith(t, store.WithPoolSize(1))
			} else {
				db = openTestStore(t)
			}
			var requests []ingest.ArtifactMirrorRequest
			before := make(map[string]mirrorState)
			for _, sid := range fixture.Sessions {
				entry := makeStoreEntry(t, sid, input.ProjectHash, input.HostSlug, defaults.HarnessOpenCode, input.StartedAt, 100, 50)
				entry.Session.Origin = input.OriginalOrigin
				if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
					t.Fatal(err)
				}
				if err := db.UpsertOpenCodeSeqCursor(t.Context(), entry.Metadata.SessionID, input.OriginalCursor); err != nil {
					t.Fatal(err)
				}
				before[sid] = mirrorDatabaseState(t, db.Pool(), sid)
				entry.Metadata.Stats.TokensIn++
				entry.Metadata.Git.Commits = []ingest.CommitInfo{{Hash: input.ReplacementCommit}}
				cursor := input.OriginalCursor + 1
				requests = append(requests, ingest.ArtifactMirrorRequest{Artifact: mirrorTestArtifact(t, entry.Metadata, input.Transcript), EventSeq: &cursor})
			}
			conn := takeConn(t, db.Pool())
			if row.Panic != "" {
				if err := conn.CreateFunction("artifact_mirror_panic", &sqlite.FunctionImpl{
					NArgs: 0, AllowIndirect: true,
					Scalar: func(sqlite.Context, []sqlite.Value) (sqlite.Value, error) { panic(row.Panic) },
				}); err != nil {
					db.Pool().Put(conn)
					t.Fatal(err)
				}
			}
			err := sqlitex.ExecuteScript(conn, row.Setup, nil)
			db.Pool().Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			var results []ingest.ArtifactMirrorResult
			var caught any
			func() {
				defer func() {
					caught = recover()
				}()
				results = db.MirrorArtifacts(t.Context(), requests)
			}()
			if row.Panic != "" {
				if caught != row.Panic {
					t.Fatalf("mirror did not preserve original panic: %v", caught)
				}
			} else if caught != nil {
				t.Fatalf("mirror panicked during transaction cleanup: %v", caught)
			}
			if row.Panic == "" && len(results) != len(requests) {
				t.Fatalf("mirror lost per-session outcomes: %+v", results)
			}
			for i, sid := range fixture.Sessions {
				want := slices.Contains(row.Mirrored, sid)
				if row.Panic == "" && (results[i].Mirrored != want || (results[i].Err == nil) != want) {
					t.Errorf("session %s result=%+v want mirrored=%t", sid, results[i], want)
				}
				after := mirrorDatabaseState(t, db.Pool(), sid)
				if !want && !reflect.DeepEqual(before[sid], after) {
					t.Errorf("refused/rolled-back session %s changed: before=%+v after=%+v", sid, before[sid], after)
				}
				if want && after.Hash != requests[i].Artifact.ArtifactHash {
					t.Errorf("successful session %s did not persist its captured identity", sid)
				}
			}
		})
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing transaction fixture %q", name)
		}
	}
}
