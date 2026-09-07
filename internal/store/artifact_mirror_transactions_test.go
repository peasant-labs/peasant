package store_test

import (
	"bytes"
	_ "embed"
	"io"
	"reflect"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
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
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(artifactMirrorTransactionsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("mirror transaction fixture must contain one document")
	}
	required := []string{"middle-item-rolls-back-outer", "commit-failure-rolls-back-all", "item-abort-preserves-peers"}
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
			db := openTestStore(t)
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
			err := sqlitex.ExecuteScript(conn, row.Setup, nil)
			db.Pool().Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			var results []ingest.ArtifactMirrorResult
			func() {
				defer func() {
					if panicValue := recover(); panicValue != nil {
						t.Errorf("mirror panicked during transaction cleanup: %v", panicValue)
					}
				}()
				results = db.MirrorArtifacts(t.Context(), requests)
			}()
			if len(results) != len(requests) {
				t.Fatalf("mirror lost per-session outcomes: %+v", results)
			}
			for i, sid := range fixture.Sessions {
				want := slices.Contains(row.Mirrored, sid)
				if results[i].Mirrored != want || (results[i].Err == nil) != want {
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
