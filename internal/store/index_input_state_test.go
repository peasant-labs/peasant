package store_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/index_input_state.yaml
var indexInputStateYAML []byte

type indexInputOperation string

const (
	indexInputWrite        indexInputOperation = ""
	indexInputDirect       indexInputOperation = "direct"
	indexInputStamp        indexInputOperation = "stamp"
	indexInputHashStamp    indexInputOperation = "hash-stamp"
	indexInputMetadata     indexInputOperation = "metadata"
	indexInputCommits      indexInputOperation = "commits"
	indexInputEmptyCommits indexInputOperation = "empty-commits"
	indexInputMirror       indexInputOperation = "mirror"
	indexInputMetrics      indexInputOperation = "metrics"
	indexInputOrigin       indexInputOperation = "origin"
)

type indexInputCase struct {
	Name                  string              `yaml:"name"`
	Operation             indexInputOperation `yaml:"operation"`
	NoIndex               bool                `yaml:"noIndex"`
	Empty                 bool                `yaml:"empty"`
	Equal                 bool                `yaml:"equal"`
	NoExpected            bool                `yaml:"noExpected"`
	NoProof               bool                `yaml:"noProof"`
	NoArtifact            bool                `yaml:"noArtifact"`
	ProducerZero          bool                `yaml:"producerZero"`
	StoredProducer        *int                `yaml:"storedProducer"`
	InvalidHash           *string             `yaml:"invalidHash"`
	WrongSession          bool                `yaml:"wrongSession"`
	ExpectedEmptyArtifact bool                `yaml:"expectedEmptyArtifact"`
	SeedSQL               []string            `yaml:"seedSQL"`
	MutationSQL           string              `yaml:"mutationSQL"`
	BeforeMutationSQL     string              `yaml:"beforeMutationSQL"`
	FailColumn            string              `yaml:"failColumn"`
	WantError             bool                `yaml:"wantError"`
	Stale                 bool                `yaml:"stale"`
}

type indexInputDocument struct {
	ArtifactHash   string           `yaml:"artifactHash"`
	ProjectHash    string           `yaml:"projectHash"`
	Transcript     string           `yaml:"transcript"`
	PriorInputHash string           `yaml:"priorInputHash"`
	NewInputHash   string           `yaml:"newInputHash"`
	RequiredNames  []string         `yaml:"requiredNames"`
	Cases          []indexInputCase `yaml:"cases"`
}

func loadIndexInputFixtures(t *testing.T) indexInputDocument {
	t.Helper()
	var document indexInputDocument
	decoder := yaml.NewDecoder(bytes.NewReader(indexInputStateYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("index input fixtures require one document: %v", err)
	}
	names := make(map[string]bool)
	for _, row := range document.Cases {
		if row.Name == "" || names[row.Name] {
			t.Fatalf("invalid index input fixture %q", row.Name)
		}
		switch row.Operation {
		case indexInputWrite, indexInputDirect, indexInputStamp, indexInputHashStamp, indexInputMetadata, indexInputCommits, indexInputEmptyCommits, indexInputMirror, indexInputMetrics, indexInputOrigin:
		default:
			t.Fatalf("unknown index input operation %q", row.Operation)
		}
		switch row.FailColumn {
		case "", "index_format_version", "index_version", "indexed_input_hash", "artifact_hash":
		default:
			t.Fatalf("unknown failure column %q", row.FailColumn)
		}
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("index input state", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document
}

func indexInputReader(t *testing.T, db *store.Store) ingest.SessionIndexStateReader {
	t.Helper()
	reader, ok := any(db).(ingest.SessionIndexStateReader)
	if !ok {
		t.Fatal("Store cannot capture the complete index state before parsing")
	}
	return reader
}

func readInputState(t *testing.T, db *store.Store, sid schema.SessionID) *ingest.SessionIndexState {
	t.Helper()
	state, err := indexInputReader(t, db).ReadIndexState(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func inputSQL(t *testing.T, db *store.Store, sid schema.SessionID, query string, args ...any) {
	t.Helper()
	conn := takeConn(t, db.Pool())
	defer db.Pool().Put(conn)
	args = append(args, string(sid))
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatal(err)
	}
}

func TestIndexInputStateConditionalWritesAndInvalidation(t *testing.T) {
	document := loadIndexInputFixtures(t)
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			db := openTestStore(t)
			indexInputReader(t, db)
			sid := schema.SessionID(testutil.TestSessionUUID)
			metadata := makeStoreEntry(t, string(sid), document.ProjectHash, "fixture-host", defaults.HarnessClaudeCode, 1700000000000, 100, 50)
			if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{metadata}); err != nil {
				t.Fatal(err)
			}
			entries := batchTestEntries(sid, "last-good", 2)
			if row.Empty {
				entries = nil
			}
			if !row.NoIndex {
				producer := 15
				if row.StoredProducer != nil {
					producer = *row.StoredProducer
				}
				seed := db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{{SessionID: sid, Result: indexformat.V1{Entries: entries}, IndexVersion: 1, IndexerVersion: producer, IndexedAtMs: 100}})
				if seed[0].Err != nil {
					t.Fatal(seed[0].Err)
				}
				inputSQL(t, db, sid, "UPDATE sessions SET indexed_input_hash = ? WHERE session_id = ?", document.PriorInputHash)
			}
			if !row.NoArtifact {
				inputSQL(t, db, sid, "UPDATE sessions SET artifact_hash = ? WHERE session_id = ?", document.ArtifactHash)
			}
			// Seeding runs BEFORE the snapshot, so the mutation below is the
			// only difference between the captured state and the current row.
			for _, statement := range row.SeedSQL {
				inputSQL(t, db, sid, statement)
			}
			expected := readInputState(t, db, sid)
			if row.ExpectedEmptyArtifact {
				expected.ArtifactHash = strPtr("")
			}
			if row.WrongSession {
				expected.SessionID = schema.SessionID(testutil.TestSessionUUID2)
			}
			if row.BeforeMutationSQL != "" {
				inputSQL(t, db, sid, row.BeforeMutationSQL)
			}
			if row.MutationSQL != "" {
				inputSQL(t, db, sid, row.MutationSQL)
			}
			if row.FailColumn != "" {
				conn := takeConn(t, db.Pool())
				if err := sqlitex.ExecuteScript(conn, "CREATE TRIGGER reject_input_state BEFORE UPDATE OF "+row.FailColumn+" ON sessions BEGIN SELECT RAISE(ABORT, 'synthetic input state failure'); END;", nil); err != nil {
					t.Fatal(err)
				}
				db.Pool().Put(conn)
			}
			beforeState, beforeRows := readInputState(t, db, sid), readIndexSnapshot(t, db, sid)
			beforeMirror := mirrorDatabaseState(t, db.Pool(), string(sid))
			candidate := entries
			if !row.Equal && !row.Empty {
				candidate = batchTestEntries(sid, "replacement", 3)
			}
			write := ingest.SessionEntryWrite{SessionID: sid, Result: indexformat.V1{Entries: candidate}, IndexVersion: 1, IndexerVersion: 15, IndexedAtMs: 200, ExpectedState: expected, IndexedInputHash: &document.NewInputHash}
			if row.NoExpected {
				write.ExpectedState = nil
			}
			if row.NoProof {
				write.IndexedInputHash = nil
			}
			if row.ProducerZero {
				write.IndexerVersion = 0
			}
			if row.InvalidHash != nil {
				write.IndexedInputHash = row.InvalidHash
			}
			var err error
			var result ingest.SessionEntryWriteResult
			var mirroredHash *string
			switch row.Operation {
			case indexInputWrite:
				result = db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{write})[0]
				err = result.Err
			case indexInputDirect:
				err = db.IndexSessionEntries(t.Context(), sid, candidate)
			case indexInputStamp:
				err = db.UpdateIndexState(t.Context(), sid, 15, 200)
			case indexInputHashStamp:
				err = db.UpdateIndexStateWithSessionEntriesHash(t.Context(), sid, 15, 200, *beforeState.SessionEntriesHash)
			case indexInputMetadata:
				metadata.Metadata.Stats.TokensIn++
				err = db.InsertSessions(t.Context(), []ingest.StoreEntry{metadata})
			case indexInputCommits:
				err = db.UpsertSessionCommits(t.Context(), sid, []ingest.CommitInfo{{Hash: "abcdef1234567890abcdef1234567890abcdef1234"}})
			case indexInputEmptyCommits:
				err = db.UpsertSessionCommits(t.Context(), sid, nil)
			case indexInputMirror:
				metadata.Metadata.Stats.TokensIn++
				metadata.Metadata.Git.Commits = []ingest.CommitInfo{{Hash: "abcdef1234567890abcdef1234567890abcdef1234"}}
				artifact := mirrorTestArtifact(t, metadata.Metadata, document.Transcript)
				mirroredHash = &artifact.ArtifactHash
				mirrored := db.MirrorArtifacts(t.Context(), []ingest.ArtifactMirrorRequest{{Artifact: artifact}})[0]
				err = mirrored.Err
				if mirrored.Mirrored != (err == nil) {
					t.Fatal("mirror completion does not match its commit result")
				}
			case indexInputMetrics:
				err = db.SaveMetrics(t.Context(), &ingest.SessionMetrics{SessionID: sid})
			case indexInputOrigin:
				err = db.UpdateOriginState(t.Context(), sid, "user", 1)
			}
			afterState, afterRows := readInputState(t, db, sid), readIndexSnapshot(t, db, sid)
			if row.WantError || row.Stale {
				if err == nil || result.Written {
					t.Fatalf("refused operation reported success: %+v err=%v", result, err)
				}
				if row.Stale {
					var stale *ingest.StaleIndexWorkError
					if !errors.As(err, &stale) {
						t.Fatalf("expected typed stale work, got %v", err)
					}
				}
				if !reflect.DeepEqual(beforeState, afterState) || !reflect.DeepEqual(beforeRows, afterRows) || !reflect.DeepEqual(beforeMirror, mirrorDatabaseState(t, db.Pool(), string(sid))) {
					t.Fatal("failed operation changed last-good rows or evidence")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if row.Operation == indexInputMetrics || row.Operation == indexInputOrigin {
				if !reflect.DeepEqual(beforeState, afterState) || !reflect.DeepEqual(beforeRows, afterRows) {
					t.Fatal("derived update changed index or artifact proof")
				}
				return
			}
			if row.Operation == indexInputMetadata || row.Operation == indexInputCommits || row.Operation == indexInputEmptyCommits || row.Operation == indexInputMirror {
				if !reflect.DeepEqual(afterState.ArtifactHash, mirroredHash) {
					t.Fatal("unverified metadata retained artifact proof")
				}
				beforeState.ArtifactHash = mirroredHash
				if !reflect.DeepEqual(beforeState, afterState) || !reflect.DeepEqual(beforeRows, afterRows) {
					t.Fatal("metadata replacement changed index history or input proof")
				}
				return
			}
			var wantInput *string
			if row.Operation == indexInputWrite {
				wantInput = write.IndexedInputHash
				if !result.Written {
					t.Fatal("committed write did not report completion")
				}
			}
			if !reflect.DeepEqual(afterState.IndexedInputHash, wantInput) {
				t.Fatalf("input proof=%v, want %v", afterState.IndexedInputHash, wantInput)
			}
			if !reflect.DeepEqual(beforeState.ArtifactHash, afterState.ArtifactHash) {
				t.Fatal("index write altered artifact identity")
			}
			if row.Operation == indexInputDirect {
				if afterState.IndexerVersion != beforeState.IndexerVersion || !reflect.DeepEqual(afterState.IndexedAt, beforeState.IndexedAt) {
					t.Fatal("entry-only write invented parser history")
				}
			} else if afterState.IndexerVersion != 15 || afterState.IndexedAt == nil || *afterState.IndexedAt != 200 {
				t.Fatal("parser stamp was not committed with rows")
			}
			if row.Operation == indexInputWrite && row.Equal && (!result.Skipped || !reflect.DeepEqual(beforeRows.Entries, afterRows.Entries)) {
				t.Fatal("equal projection was replaced instead of recording verified input")
			}
			if row.Empty && (result.EntriesCount != 0 || afterState.IndexVersion == nil || *afterState.IndexVersion != 1) {
				t.Fatal("empty result lacks actual completed representation")
			}
		})
	}
}

func TestIndexInputStateReaderDistinguishesAbsentAndFailed(t *testing.T) {
	db := openTestStore(t)
	reader := indexInputReader(t, db)
	sid := schema.SessionID(testutil.TestSessionUUID)
	state, err := reader.ReadIndexState(t.Context(), sid)
	if err != nil || state != nil {
		t.Fatalf("missing state=%+v, err=%v", state, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadIndexState(t.Context(), sid); err == nil {
		t.Fatal("failed reader reported absent state")
	}
}
