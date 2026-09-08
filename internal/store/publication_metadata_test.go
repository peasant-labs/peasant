package store_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/publication-metadata.yaml
var publicationMetadataYAML []byte

type publicationMetadataFixture struct {
	Name         string                      `yaml:"name"`
	Kind         string                      `yaml:"kind"`
	CWD          string                      `yaml:"cwd"`
	Child        bool                        `yaml:"child"`
	Legacy       bool                        `yaml:"legacy"`
	Action       string                      `yaml:"action"`
	Readiness    ingest.PublicationReadiness `yaml:"readiness"`
	ReadError    bool                        `yaml:"read_error"`
	CaptureError bool                        `yaml:"capture_error"`
}

func loadPublicationMetadataFixtures(t *testing.T) []publicationMetadataFixture {
	t.Helper()
	var cases []publicationMetadataFixture
	if err := yaml.Unmarshal(publicationMetadataYAML, &cases); err != nil {
		t.Fatal(err)
	}
	required := strings.Fields(`exact-root-reopened exact-child-reopened confirmed-absent workspace-is-not-exact worktree-is-not-exact legacy-seed capture-before-index noop-stamps-new-capture stale-writer-refused stale-noop-refused manual-entry-reindex manual-index-state manual-hashed-index-state zero-revision-noop legacy-upsert-invalidates metrics-are-independent unsupported-snapshot malformed-snapshot corrupt-snapshot-digest conflicting-cwd-column conflicting-snapshot-identity unknown-capture-intent exact-without-literal workspace-with-fake-cwd repaired-project-capture model-absence-is-consumer-policy`)
	seen := make(map[string]bool)
	required = append(required, strings.Fields("captured-parent-transition repaired-host-capture repaired-remote-capture equivalent-remote-capture incompatible-source-identity corrupt-capture-digest manual-reindex-restamped")...)
	for _, c := range cases {
		if seen[c.Name] {
			t.Fatalf("duplicate fixture %s", c.Name)
		}
		seen[c.Name] = true
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("required fixture %s missing", name)
		}
	}
	return cases
}

func publicationEntry(t *testing.T, id string) ingest.StoreEntry {
	t.Helper()
	e := makeStoreEntry(t, id, strings.Repeat("a", 64), "github.com-example-project", defaults.HarnessClaudeCode, 1700000000000, 100, 200)
	e.PublicationCapture = true
	e.CWDProvenance = ingest.CWDSourceAbsent
	e.Session.Origin = sessionorigin.User
	e.Metadata.ContentHash = schema.ComputeTranscriptHash([]byte("captured transcript"))
	e.Metadata.MetadataHash = schema.ComputeMetadataHash(e.Metadata)
	return e
}

func capturePublication(t *testing.T, s *store.Store, e ingest.StoreEntry) int64 {
	t.Helper()
	revisions, err := s.InsertSessionsWithRevisions(context.Background(), []ingest.StoreEntry{e})
	if err != nil {
		t.Fatal(err)
	}
	return revisions[e.Metadata.SessionID]
}

func indexPublication(t *testing.T, s *store.Store, e ingest.StoreEntry, revision int64, entries []schema.SessionEntry) ingest.SessionEntryWriteResult {
	t.Helper()
	results := s.IndexSessionEntryBatch(context.Background(), []ingest.SessionEntryWrite{{SessionID: e.Metadata.SessionID, Result: indexformat.V1{Entries: entries}, IndexVersion: 1, RequireFullContent: true, CaptureRevision: revision, IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, IndexedAtMs: 1700000000001}})
	if len(results) != 1 {
		t.Fatalf("index results: %v", results)
	}
	return results[0]
}

func publicationSQL(t *testing.T, s *store.Store, query string, args ...any) {
	t.Helper()
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationMetadataFixtures(t *testing.T) {
	for _, tc := range loadPublicationMetadataFixtures(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			path := storetest.CopyGoldenDB(t)
			s, err := store.Open(path, store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if s != nil {
					s.Close()
				}
			})
			e := publicationEntry(t, "11111111-1111-4111-8111-111111111111")
			id := e.Metadata.SessionID
			e.CWDProvenance, err = ingest.NewCWDProvenanceKind(tc.Kind)
			if err != nil {
				t.Fatal(err)
			}
			e.Metadata.CWD = tc.CWD
			e.PublicationCapture = !tc.Legacy
			if tc.Child {
				parent := publicationEntry(t, "22222222-2222-4222-8222-222222222222")
				capturePublication(t, s, parent)
				e.Metadata.ParentUUID = &parent.Metadata.SessionID
			}
			if tc.Action == "missing-model" {
				e.Metadata.Model = ""
			}
			if tc.Action == "equivalent-remote" {
				remote := "https://example.com/project.git"
				e.Metadata.Git.Remote = &remote
			}
			e.Metadata.MetadataHash = schema.ComputeMetadataHash(e.Metadata)
			if tc.Action == "capture-bad-hash" {
				e.Metadata.MetadataHash = strings.Repeat("b", 64)
			}
			revisions, err := s.InsertSessionsWithRevisions(context.Background(), []ingest.StoreEntry{e})
			if tc.CaptureError {
				if err == nil {
					t.Fatal("unproven capture accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			revision := revisions[id]
			entries := batchTestEntries(id, "original", 1)
			entries[0].Extra = strPtr(`{"model_id":"claude-opus-4-6"}`)
			if tc.Action != "skip-index" {
				result := indexPublication(t, s, e, revision, entries)
				if result.Err != nil {
					t.Fatal(result.Err)
				}
			}
			ctx := context.Background()
			switch tc.Action {
			case "recapture-noop", "stale-write", "stale-noop":
				next := capturePublication(t, s, e)
				if next != revision+1 {
					t.Fatalf("revision %d -> %d", revision, next)
				}
				if tc.Action == "recapture-noop" {
					result := indexPublication(t, s, e, next, entries)
					if result.Err != nil || !result.Skipped {
						t.Fatalf("noop did not stamp: %+v", result)
					}
				} else {
					attempt := entries
					if tc.Action == "stale-write" {
						attempt = batchTestEntries(id, "stale", 1)
					}
					result := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: id, Result: indexformat.V1{Entries: attempt}, IndexVersion: 1, CaptureRevision: revision, IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion + 17, IndexedAtMs: 1700000000999}})[0]
					if result.Err == nil || result.Written {
						t.Fatal("stale revision accepted")
					}
					assertEntryContent(t, s, id, "original-0")
					assertIndexState(t, s, id, ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, 1700000000001)
				}
			case "manual-entries":
				err = s.IndexSessionEntries(ctx, id, entries)
			case "manual-restamp":
				if err = s.IndexSessionEntries(ctx, id, entries); err != nil {
					t.Fatal(err)
				}
				pending, readErr := s.LoadPublicationInput(ctx, id)
				if readErr != nil || pending.Readiness != ingest.PublicationNeedsIngest {
					t.Fatal("unproven manual index remained ready")
				}
				result := indexPublication(t, s, e, revision, entries)
				if result.Err != nil || result.Skipped || capture(t, s, id).Status != ingest.ContentCaptureComplete {
					t.Fatalf("checked manual index failed to restore full capture: %+v", result)
				}
			case "manual-state":
				err = s.UpdateIndexState(ctx, id, ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, 1700000000002)
			case "manual-hash-state":
				err = s.UpdateIndexStateWithSessionEntriesHash(ctx, id, ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, 1700000000002, sessionEntriesHash(t, s, id))
			case "zero-noop":
				result := indexPublication(t, s, e, 0, entries)
				err = result.Err
				if !result.Skipped {
					t.Fatal("expected noop")
				}
			case "legacy-upsert":
				legacy := e
				legacy.PublicationCapture = false
				err = s.InsertSessions(ctx, []ingest.StoreEntry{legacy})
			case "metrics":
				m, getErr := s.GetMetrics(ctx, id)
				if getErr != nil {
					t.Fatal(getErr)
				}
				turns := 777
				m.TurnCount = &turns
				err = s.SaveMetrics(ctx, m)
			case "unsupported":
				publicationSQL(t, s, `UPDATE session_publication_metadata SET schema_version=999 WHERE session_id=?`, string(id))
			case "malformed":
				publicationSQL(t, s, `UPDATE session_publication_metadata SET metadata_json='[]' WHERE session_id=?`, string(id))
			case "digest":
				publicationSQL(t, s, `UPDATE session_publication_metadata SET metadata_hash=? WHERE session_id=?`, strings.Repeat("b", 64), string(id))
			case "cwd-column":
				publicationSQL(t, s, `UPDATE sessions SET session_cwd='/different' WHERE session_id=?`, string(id))
			case "identity":
				m := *e.Metadata
				m.SessionID = "33333333-3333-4333-8333-333333333333"
				m.MetadataHash = schema.ComputeMetadataHash(&m)
				body, _ := json.Marshal(m)
				publicationSQL(t, s, `UPDATE session_publication_metadata SET metadata_json=?,metadata_hash=? WHERE session_id=?`, string(body), m.MetadataHash, string(id))
			case "changed-identity", "changed-parent", "changed-host", "changed-remote", "changed-source-id", "equivalent-remote":
				m := *e.Metadata
				changed := e
				changed.Metadata = &m
				switch tc.Action {
				case "changed-identity":
					m.Project.Hash = schema.ProjectHash(strings.Repeat("b", 64))
				case "changed-parent":
					parent := publicationEntry(t, "33333333-3333-4333-8333-333333333333")
					capturePublication(t, s, parent)
					m.ParentUUID = &parent.Metadata.SessionID
				case "changed-host":
					m.HostSlug = "example-other-host"
				case "changed-remote":
					remote := "https://example.com/other.git"
					m.Git.Remote = &remote
				case "equivalent-remote":
					remote := "https://example.com/project"
					m.Git.Remote = &remote
				case "changed-source-id":
					changed.Session.SessionID = "33333333-3333-4333-8333-333333333333"
				}
				m.MetadataHash = schema.ComputeMetadataHash(&m)
				next, captureErr := s.InsertSessionsWithRevisions(ctx, []ingest.StoreEntry{changed})
				if tc.Action == "changed-source-id" {
					if captureErr == nil {
						t.Fatal("incompatible source identity accepted")
					}
					break
				}
				if captureErr != nil || next[id] != revision+1 {
					t.Fatalf("attribution capture transition failed: %v, %v", next, captureErr)
				}
				pending, readErr := s.LoadPublicationInput(ctx, id)
				if readErr != nil || pending.Readiness != ingest.PublicationNeedsIngest || !reflect.DeepEqual(pending.Metadata, m) {
					t.Fatalf("repair was not coherent and held before indexing: %+v, %v", pending, readErr)
				}
				if stale := indexPublication(t, s, e, revision, entries); stale.Err == nil {
					t.Fatal("pre-repair writer certified the new attribution")
				}
				if indexed := indexPublication(t, s, changed, next[id], entries); indexed.Err != nil {
					t.Fatal(indexed.Err)
				}
				e = changed
			}
			if err != nil {
				t.Fatal(err)
			}
			// Source paths never existed. Close/reopen proves there is no retained
			// in-memory metadata or sidecar dependency. One pool connection exposes
			// any nested reader acquisition as a deadline failure.
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s = nil
			s, err = store.Open(path, store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			bundle, err := s.LoadPublicationInput(readCtx, id)
			projections, projectionErr := s.LoadPublicationMetadata(readCtx, []ingest.SessionID{id})
			if projectionErr != nil {
				t.Fatal(projectionErr)
			}
			projection := projections[id]
			if (projection.Error != nil) != (err != nil) || projection.Readiness != bundle.Readiness || projection.CaptureRevision != bundle.CaptureRevision || !reflect.DeepEqual(projection.Metadata, bundle.Metadata) {
				t.Fatalf("lightweight projection disagrees with coherent bundle: projection=%+v bundle=%+v error=%v", projection, bundle, err)
			}
			if tc.ReadError {
				if err == nil {
					t.Fatal("corrupt/conflicting snapshot accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if bundle.Readiness != tc.Readiness {
					t.Fatalf("readiness %s, want %s", bundle.Readiness, tc.Readiness)
				}
				if bundle.Metadata.SessionID != "" && !reflect.DeepEqual(bundle.Metadata, *e.Metadata) {
					t.Fatalf("metadata did not round trip losslessly")
				}
				if bundle.SessionOrigin != sessionorigin.User || bundle.ReceiptProjectHash != e.Metadata.Project.Hash {
					t.Fatal("stored attribution lost")
				}
				if bundle.Readiness == ingest.PublicationReady && !reflect.DeepEqual(bundle.Entries, entries) {
					t.Fatal("indexed entries and extension fields did not round trip")
				}
				if bundle.Readiness == ingest.PublicationNeedsIngest && len(bundle.Entries) != 0 {
					t.Fatal("unready bundle hydrated indexed entries")
				}
				if tc.Action != "skip-index" {
					stored, readErr := s.ListEntries(readCtx, id)
					if readErr != nil || !reflect.DeepEqual(stored, entries) {
						t.Fatal("unready state changed indexed entries or extension fields")
					}
				}
				if tc.Action == "metrics" && (bundle.Quality == nil || bundle.Quality.TurnCount == nil || *bundle.Quality.TurnCount != 777 || bundle.Metadata.Stats.TurnCount == 777) {
					t.Fatal("current metrics conflated with extracted stats")
				}
			}
			locations, lookupErr := s.BulkLookupSessionLocations(readCtx, []ingest.SessionID{id})
			if lookupErr != nil {
				t.Fatal(lookupErr)
			}
			loc := locations[id]
			want := tc.Readiness
			if tc.ReadError {
				want = ingest.PublicationNeedsIngest
			}
			if loc.PublicationReadiness != want || loc.ProjectHash != e.Metadata.Project.Hash || loc.HostSlug != string(e.Metadata.HostSlug) || loc.OpaqueHostID == "" {
				t.Fatalf("bulk recovery facts: %+v", loc)
			}
		})
	}
}

func TestPublicationCaptureRollbackAfterPartialProgress(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := publicationEntry(t, "11111111-1111-4111-8111-111111111111")
	revision := capturePublication(t, s, e)
	if result := indexPublication(t, s, e, revision, batchTestEntries(e.Metadata.SessionID, "old", 1)); result.Err != nil {
		t.Fatal(result.Err)
	}
	before, err := s.LoadPublicationInput(context.Background(), e.Metadata.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	publicationSQL(t, s, `CREATE TRIGGER fail_publication_capture BEFORE INSERT ON session_publication_metadata
 WHEN NEW.session_id='11111111-1111-4111-8111-111111111111' BEGIN SELECT RAISE(ABORT,'injected snapshot failure'); END`)
	newEntry := publicationEntry(t, "22222222-2222-4222-8222-222222222222")
	e.Metadata.Stats.TurnCount = 999
	e.Metadata.MetadataHash = schema.ComputeMetadataHash(e.Metadata)
	if revisions, err := s.InsertSessionsWithRevisions(context.Background(), []ingest.StoreEntry{newEntry, e}); err == nil || revisions != nil {
		t.Fatal("failed snapshot returned successful revisions")
	}
	after, err := s.LoadPublicationInput(context.Background(), e.Metadata.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("partial session/metrics/capture changes escaped rollback")
	}
	locations, err := s.BulkLookupSessionLocations(context.Background(), []ingest.SessionID{newEntry.Metadata.SessionID})
	if err != nil || len(locations) != 0 {
		t.Fatalf("earlier batch session escaped rollback: %v %v", locations, err)
	}
}

func TestPublicationBundleRetainsCurrentDurableAssociations(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := publicationEntry(t, "11111111-1111-4111-8111-111111111111")
	revision := capturePublication(t, s, e)
	if result := indexPublication(t, s, e, revision, batchTestEntries(e.Metadata.SessionID, "entry", 1)); result.Err != nil {
		t.Fatal(result.Err)
	}
	ctx := context.Background()
	if err := s.UpsertSessionCommits(ctx, e.Metadata.SessionID, []ingest.CommitInfo{{Hash: strings.Repeat("a", 40), Message: "old binding"}}); err != nil {
		t.Fatal(err)
	}
	before, err := s.LoadPublicationInput(ctx, e.Metadata.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Associations) != 1 || before.Associations[0].ID == "" {
		t.Fatal("durable association absent")
	}
	if err := s.UpsertSessionCommits(ctx, e.Metadata.SessionID, []ingest.CommitInfo{{Hash: strings.Repeat("b", 40), Message: "current binding"}}); err != nil {
		t.Fatal(err)
	}
	after, err := s.LoadPublicationInput(ctx, e.Metadata.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Associations) != 1 || after.Associations[0].ObservedCommitHash != strings.Repeat("b", 40) || after.Associations[0].ID == before.Associations[0].ID {
		t.Fatal("bundle included historical rather than current binding")
	}
	if after.Readiness != ingest.PublicationReady {
		t.Fatal("durable binding update invalidated unrelated capture")
	}
	if err := s.UpsertSessionCommits(ctx, e.Metadata.SessionID, []ingest.CommitInfo{{Hash: strings.Repeat("b", 40), Message: "current binding"}}); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.LoadPublicationInput(ctx, e.Metadata.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Associations, replayed.Associations) {
		t.Fatal("durable binding ID changed during replay")
	}
}

func TestPublicationBundleNeverReportsMixedRevisionsReady(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := publicationEntry(t, "11111111-1111-4111-8111-111111111111")
	e.Metadata.Version = "capture-0"
	e.Metadata.MetadataHash = schema.ComputeMetadataHash(e.Metadata)
	revision := capturePublication(t, s, e)
	entries := batchTestEntries(e.Metadata.SessionID, "initial", 1)
	entries[0].ContentPreview = &e.Metadata.Version
	if result := indexPublication(t, s, e, revision, entries); result.Err != nil {
		t.Fatal(result.Err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		for i := 1; i <= 30; i++ {
			m := *e.Metadata
			m.Version = fmt.Sprintf("capture-%d", i)
			m.MetadataHash = schema.ComputeMetadataHash(&m)
			next := e
			next.Metadata = &m
			revisions, err := s.InsertSessionsWithRevisions(ctx, []ingest.StoreEntry{next})
			if err != nil {
				done <- err
				return
			}
			newEntries := append([]schema.SessionEntry(nil), entries...)
			newEntries[0].ContentPreview = &m.Version
			results := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: m.SessionID, Result: indexformat.V1{Entries: newEntries}, IndexVersion: 1, RequireFullContent: true, CaptureRevision: revisions[m.SessionID]}})
			if results[0].Err != nil {
				done <- results[0].Err
				return
			}
		}
		done <- nil
	}()
	for {
		bundle, err := s.LoadPublicationInput(ctx, e.Metadata.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if bundle.Readiness == ingest.PublicationReady && (len(bundle.Entries) != 1 || bundle.Entries[0].ContentPreview == nil || *bundle.Entries[0].ContentPreview != bundle.Metadata.Version) {
			t.Fatal("ready bundle mixed capture metadata and indexed entries")
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
	}
}
