package store_test

import (
	_ "embed"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/publication_index_bind.yaml
var publicationIndexBindYAML []byte

//go:embed testdata/publication_index_bind.manifest.yaml
var publicationIndexBindManifestYAML []byte

type publicationIndexBindCase struct {
	Name              string `yaml:"name"`
	Action            string `yaml:"action"`
	Revision          int64  `yaml:"revision"`
	Move              bool   `yaml:"move"`
	WrongInput        bool   `yaml:"wrongInput"`
	TranscriptKept    bool   `yaml:"transcriptKept"`
	WantStale         bool   `yaml:"wantStale"`
	WantBoundRevision int64  `yaml:"wantBoundRevision"`
	WantIndexedAt     int64  `yaml:"wantIndexedAt"`
	WantProofKept     bool   `yaml:"wantProofKept"`
}

func LoadPublicationIndexBindFixtures(t *testing.T) []publicationIndexBindCase {
	t.Helper()
	var document struct {
		Cases []publicationIndexBindCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(publicationIndexBindYAML, &document); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(publicationIndexBindManifestYAML, "publication index bind")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, c := range document.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "publication index bind"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

func TestBindPublicationIndex(t *testing.T) {
	t.Parallel()
	const proof = "abcdef1234abcdef1234abcdef1234abcdef1234abcdef1234abcdef12345679"
	const otherInput = "1234561234abcdef1234abcdef1234abcdef1234abcdef1234abcdef12345679"
	for _, c := range LoadPublicationIndexBindFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			db := openTestStore(t)
			sid := schema.SessionID(testutil.TestSessionUUID)
			entry := makeStoreEntry(t, string(sid), string(testutil.TestProjectHash), "fixture-host", defaults.HarnessClaudeCode, 1700000000000, 100, 50)
			if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
				t.Fatal(err)
			}
			seed := db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{{
				SessionID: sid, Result: indexformat.V1{Entries: batchTestEntries(sid, "same", 2)}, IndexVersion: 1,
				IndexerVersion: 15, IndexedAtMs: 100,
			}})[0]
			if seed.Err != nil {
				t.Fatal(seed.Err)
			}
			inputSQL(t, db, sid, "UPDATE sessions SET artifact_hash = ?, indexed_input_hash = ?, publication_capture_revision = 3, cwd_provenance_kind = 'source_exact', indexed_publication_capture_revision = 0 WHERE session_id = ?", string(testutil.TestProjectHash), proof)
			entriesBefore, err := db.ListEntries(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			if c.Action == "mirror" {
				// A pair whose identity moved: the metadata differs from the
				// one the row recorded.
				transcript := []byte("{\"type\":\"user\"}\n")
				meta := *entry.Metadata
				meta.ContentHash = schema.ComputeTranscriptHash(transcript)
				encoded, err := json.Marshal(&meta)
				if err != nil {
					t.Fatal(err)
				}
				artifact, err := ingest.NewManagedArtifact(encoded, transcript)
				if err != nil {
					t.Fatal(err)
				}
				results := db.MirrorArtifacts(t.Context(), []ingest.ArtifactMirrorRequest{{Artifact: artifact, TranscriptKept: c.TranscriptKept}})
				if len(results) != 1 || results[0].Err != nil {
					t.Fatalf("mirror: %+v", results)
				}
				after := readInputState(t, db, sid)
				if after.ArtifactHash == nil || *after.ArtifactHash != artifact.ArtifactHash {
					t.Fatalf("the mirror did not record the new pair identity: %+v", after.ArtifactHash)
				}
				if kept := after.IndexedInputHash != nil && *after.IndexedInputHash == proof; kept != c.WantProofKept {
					t.Fatalf("index input proof kept = %t, want %t", kept, c.WantProofKept)
				}
				return
			}
			captured := readInputState(t, db, sid)
			if c.Move {
				inputSQL(t, db, sid, "UPDATE sessions SET indexed_at = 300 WHERE session_id = ?")
			}
			input := proof
			if c.WrongInput {
				input = otherInput
			}
			err = db.BindPublicationIndex(t.Context(), ingest.PublicationIndexBind{
				SessionID: sid, ExpectedState: captured, IndexedInputHash: input, CaptureRevision: c.Revision, IndexedAtMs: 500,
			})
			var stale *ingest.StaleIndexWorkError
			if c.WantStale != errors.As(err, &stale) || (!c.WantStale && err != nil) {
				t.Fatalf("bind error = %v, want stale refusal %t", err, c.WantStale)
			}
			after := readInputState(t, db, sid)
			if got := indexedPublicationRevision(t, db, sid); got != c.WantBoundRevision {
				t.Errorf("indexed publication capture revision = %d, want %d", got, c.WantBoundRevision)
			}
			if after.IndexedAt == nil || *after.IndexedAt != c.WantIndexedAt {
				t.Errorf("indexed_at = %v, want %d", after.IndexedAt, c.WantIndexedAt)
			}
			if after.IndexedInputHash == nil || *after.IndexedInputHash != proof || after.IndexerVersion != 15 {
				t.Errorf("the bind changed the input proof or the producer revision: %+v", after)
			}
			entriesAfter, err := db.ListEntries(t.Context(), sid)
			if err != nil || !reflect.DeepEqual(entriesAfter, entriesBefore) {
				t.Errorf("the bind changed stored entries: %v", err)
			}
		})
	}
}
