package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/publication_capture.yaml
var publicationCaptureYAML []byte

type publicationCaptureCase struct {
	Name                     string            `yaml:"name"`
	Harness                  string            `yaml:"harness"`
	ID                       string            `yaml:"id"`
	CWD                      string            `yaml:"cwd"`
	Provenance               string            `yaml:"provenance"`
	Files                    map[string]string `yaml:"files"`
	DatabaseFixture          string            `yaml:"database_fixture"`
	StripCWD                 bool              `yaml:"strip_cwd"`
	StripModel               bool              `yaml:"strip_model"`
	RemoveSource             bool              `yaml:"remove_source"`
	MismatchIdentity         bool              `yaml:"mismatch_identity"`
	DisappearDuringExtract   bool              `yaml:"disappear_during_extract"`
	FailSidecar              bool              `yaml:"fail_sidecar"`
	ChangeSource             bool              `yaml:"change_source"`
	MutateIndexFile          string            `yaml:"mutate_index_file"`
	ExpectedManualIndexed    *int              `yaml:"expected_manual_indexed"`
	ExpectedManualReadiness  string            `yaml:"expected_manual_readiness"`
	ReindexRemoveSource      bool              `yaml:"reindex_remove_source"`
	ReindexChangeManaged     bool              `yaml:"reindex_change_managed"`
	ReindexRemoveProof       bool              `yaml:"reindex_remove_proof"`
	ReindexChangeSource      bool              `yaml:"reindex_change_source"`
	ReindexChangeTree        string            `yaml:"reindex_change_tree"`
	ReindexConcurrentCapture bool              `yaml:"reindex_concurrent_capture"`
	ReindexMutateAfterRead   bool              `yaml:"reindex_mutate_after_read"`
	ReindexUnreadableEntries bool              `yaml:"reindex_unreadable_entries"`
}

func loadPublicationCaptureCases(t *testing.T) []publicationCaptureCase {
	t.Helper()
	var doc struct {
		Cases []publicationCaptureCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(publicationCaptureYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{
		"claude_literal_cwd_and_tracked_identity_repair": true, "claude_source_absent": true,
		"codex_literal_cwd": true, "codex_directory_fallback_not_exact": true,
		"cursor_workspace_not_exact": true, "strike_worktree_not_exact": true,
		"opencode_json_literal_cwd": true, "opencode_legacy_sqlite_capture": true,
		"opencode_current_sqlite_capture": true, "missing_source_stays_unrecovered": true,
		"mismatched_internal_identity_refuses": true, "codex_mismatched_internal_identity_refuses": true,
		"disappeared_source_during_extraction": true, "metadata_write_failure_needs_reingest": true,
		"changed_source_updates_metadata_and_entries": true, "opencode_json_index_uses_captured_tree": true,
		"absent_model_is_not_fabricated": true,
		"reindex_fresh_source_ready":     true, "reindex_verified_fallback_ready": true,
		"reindex_changed_fallback_held": true, "reindex_legacy_fallback_held": true,
		"reindex_opencode_unverified_tree_held": true, "reindex_opencode_changed_tree_held": true,
		"reindex_stale_source_capture_held": true, "reindex_stale_fallback_capture_held": true,
		"reindex_opencode_current_projection_ready": true, "reindex_opencode_legacy_projection_ready": true,
		"reindex_fallback_uses_verified_bytes": true, "reindex_unreadable_old_entries_ready": true,
	}
	seen := make(map[string]bool)
	for _, c := range doc.Cases {
		if seen[c.Name] {
			t.Fatalf("duplicate fixture %s", c.Name)
		}
		seen[c.Name] = true
		delete(required, c.Name)
		unrecoverable := c.RemoveSource || c.MismatchIdentity || c.DisappearDuringExtract || c.FailSidecar
		if !unrecoverable && (c.ExpectedManualIndexed == nil || c.ExpectedManualReadiness == "") {
			t.Fatalf("fixture %s has no expected manual indexing outcome", c.Name)
		}
	}
	if len(required) != 0 {
		t.Fatalf("missing required fixtures: %v", required)
	}
	return doc.Cases
}

// The same real adapters, SQLite store, index writers and metrics engine used
// by normal ingest repair legacy rows with no generated metadata file.
func TestPublicationCaptureNormalIngestRecovery(t *testing.T) {
	for _, c := range loadPublicationCaptureCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			filesystem := &publicationCaptureFS{OSFileSystem: &ingest.OSFileSystem{}}
			for name, data := range c.Files {
				if c.StripModel {
					data = strings.ReplaceAll(data, `,"model":"claude-sonnet-4-20250514"`, "")
				}
				if c.StripCWD {
					data = strings.ReplaceAll(data, `,"cwd":"/synthetic/project/../literal/subdir"`, "")
				}
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				old := time.Now().Add(-24 * time.Hour)
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			environment := mountedCurrentEnvironment{"HOME": root, defaults.EnvXDGDataHome.String(): root, defaults.EnvXDGStateHome.String(): root}
			if c.DatabaseFixture != "" {
				source := testfixture.MaterializeByName(t, c.DatabaseFixture)
				before := testfixture.SnapshotSource(t, source)
				if !c.ReindexRemoveSource {
					defer testfixture.AssertUnchanged(t, source, before)
				}
				root = filepath.Dir(source.Path)
				environment["OPENCODE_DB"] = source.Path
			}
			harness := ingest.Harness(c.Harness)
			id, err := ingest.NewSessionID(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			factory := publicationAdapterFactory(t, harness, environment)
			indexer := publicationIndexer(t, harness, filesystem)
			cfg := ingest.PipelineConfig{Sources: map[ingest.Harness]ingest.SourceConfig{harness: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(root)}}}, OutputDir: ingest.ResolvedPath(t.TempDir()), Parallelism: 1}
			dbPath := filepath.Join(t.TempDir(), "peasant.db")
			database, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = database.Close() }()
			git := testutil.DefaultGitResolver()
			installationSalt := database.InstallationSalt()
			adapter := factory(filesystem, git, installationSalt)
			discovered, err := adapter.Discover(ctx, cfg.Sources[harness])
			if err != nil || len(discovered) != 1 || discovered[0].SessionID != id {
				t.Fatalf("discovery = %+v, %v", discovered, err)
			}
			session := discovered[0]
			meta, err := adapter.ExtractMetadata(ctx, session)
			if err != nil {
				t.Fatal(err)
			}
			// Seed the historical row via the old, explicitly unproven writer.
			ingestedAt := time.Now().UnixMilli()
			meta.Timestamp.Ingested = &ingestedAt
			if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta, Session: session}}); err != nil {
				t.Fatal(err)
			}
			if err := database.UpsertSessionCommits(ctx, id, []ingest.CommitInfo{{Hash: testutil.TestHeadCommitHash, Message: "synthetic historical commit"}}); err != nil {
				t.Fatal(err)
			}
			priorLocations, err := database.BulkLookupSessionLocations(ctx, []ingest.SessionID{id})
			if err != nil {
				t.Fatal(err)
			}
			if priorLocations[id].PublicationReadiness != ingest.PublicationNeedsIngest {
				t.Fatal("legacy seed unexpectedly ready")
			}
			// Prove that source time/schema alone would have skipped this row.
			clockOnly := priorLocations[id]
			clockOnly.PublicationReadiness = ingest.PublicationReady
			if got := ingest.ClassifyAgainstStore(session, clockOnly, 0); got != ingest.DiffUnchanged {
				t.Fatalf("legacy source clock = %v", got)
			}
			sourceBytes, err := os.ReadFile(session.SourcePath.String())
			if err != nil {
				t.Fatal(err)
			}
			if c.RemoveSource {
				if err := os.Remove(session.SourcePath.String()); err != nil {
					t.Fatal(err)
				}
			}
			if c.MismatchIdentity {
				sourceBytes = bytes.ReplaceAll(sourceBytes, []byte(c.ID), []byte("00000000-0000-4000-8000-000000000099"))
				if err := os.WriteFile(session.SourcePath.String(), sourceBytes, 0600); err != nil {
					t.Fatal(err)
				}
			}
			filesystem.failSidecar = c.FailSidecar
			if c.DisappearDuringExtract {
				filesystem.disappearPath = session.SourcePath.String()
			}
			if c.MutateIndexFile != "" {
				filesystem.mutatePath = filepath.Join(root, c.MutateIndexFile)
			}
			git.Remote = "https://example.com/changed/current.git"
			run := func() *ingest.PipelineResult {
				writer := &publicationReindexStore{Store: database, recapture: cfg.Reindex && c.ReindexConcurrentCapture, unreadableEntries: cfg.Reindex && c.ReindexUnreadableEntries}
				pipeline, err := ingest.NewPipeline(filesystem, git, map[ingest.Harness]ingest.AdapterFactory{harness: factory}, cfg,
					ingest.WithSalt(installationSalt), ingest.WithStore(writer), ingest.WithMetricsStore(writer),
					ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{harness: indexer}), ingest.WithAnalyzer(metrics.NewEngine(database)))
				if err != nil {
					t.Fatal(err)
				}
				result, err := pipeline.Run(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Reindex && c.ReindexConcurrentCapture && (writer.recapture || writer.captureErr != nil) {
					t.Fatalf("competing capture did not execute successfully: %v", writer.captureErr)
				}
				return result
			}
			result := run()
			if c.RemoveSource || c.MismatchIdentity || c.DisappearDuringExtract || c.FailSidecar {
				bundle, err := database.LoadPublicationInput(ctx, id)
				if err != nil || bundle.Readiness != ingest.PublicationNeedsIngest {
					t.Fatalf("unrecoverable source = %+v, %v", bundle, err)
				}
				// A failed metadata write no longer recovers within the same run
				// from a pending publication intent: the write path installs the
				// pair by rename with no intent, so a metadata write that fails
				// leaves a torn pair with no row. Per the crash-and-fault model
				// (torn pair, no row, crash during a first install): "pair
				// validation refuses it; DIFF finds no row; next harvest
				// re-ingests from native". So the faulted run reports an error
				// and leaves the session needing ingest, and a later clean
				// harvest re-ingests it from its source.
				if (c.MismatchIdentity || c.DisappearDuringExtract || c.FailSidecar) && result.Summary.Errors == 0 {
					t.Fatalf("faulted install accepted: %+v", result)
				}
				return
			}
			if result.Summary.Errors != 0 || result.Summary.StoreError != nil || result.Summary.Indexed != 1 {
				t.Fatalf("recovery failed: %+v", result)
			}
			repairedHost, repairedParent, err := database.LookupSessionLocation(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			metadataPath := filepath.Join(ingest.SessionDir(cfg.OutputDir.String(), repairedHost, c.ID, repairedParent), c.ID+"--metadata.json")
			if err := os.Remove(metadataPath); err != nil && !(c.FailSidecar && os.IsNotExist(err)) {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			database, err = store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := database.LoadPublicationInput(ctx, id)
			if err != nil || bundle.Readiness != ingest.PublicationReady || len(bundle.Entries) == 0 || bundle.Quality == nil {
				t.Fatalf("reopened capture = %+v, %v", bundle, err)
			}
			if bundle.Metadata.CWD != c.CWD {
				t.Fatalf("literal CWD = %q want %q", bundle.Metadata.CWD, c.CWD)
			}
			if c.StripModel && bundle.Metadata.Model != "" {
				t.Fatal("capture fabricated missing model evidence")
			}
			if got := publicationStoredProvenance(t, dbPath, id); got != c.Provenance {
				t.Fatalf("provenance = %q want %q", got, c.Provenance)
			}
			if c.MutateIndexFile != "" {
				entryJSON, err := json.Marshal(bundle.Entries)
				if err != nil || bytes.Contains(entryJSON, []byte("changed response")) || !bytes.Contains(entryJSON, []byte("synthetic response")) {
					t.Fatalf("index mixed a newer source tree into the capture: %s, %v", entryJSON, err)
				}
			}
			wantProject, wantHost, err := ingest.DeriveProjectIdentifiers(installationSalt, git.Remote, session.CWD)
			if err != nil {
				t.Fatal(err)
			}
			if bundle.Metadata.Project.Hash != wantProject || bundle.Metadata.HostSlug != wantHost || bundle.Metadata.Git.Remote == nil || *bundle.Metadata.Git.Remote != git.Remote || bundle.Metadata.SessionID != id {
				t.Fatalf("capture did not follow tracked project repair: %+v", bundle.Metadata)
			}
			meta = &bundle.Metadata
			if len(bundle.Associations) != 1 {
				t.Fatalf("lost historical association: %+v", bundle.Associations)
			}
			if err := database.UpdateOriginState(ctx, id, sessionorigin.User.String(), ingest.OriginRuleVersion); err != nil {
				t.Fatal(err)
			}
			judged, err := database.LoadPublicationInput(ctx, id)
			if err != nil || judged.Readiness != ingest.PublicationReady || judged.CaptureRevision != bundle.CaptureRevision || judged.SessionOrigin != sessionorigin.User {
				t.Fatalf("origin verdict invalidated independent capture proof: %+v, %v", judged, err)
			}
			locations, err := database.BulkLookupSessionLocations(ctx, []ingest.SessionID{id})
			if err != nil || locations[id].HostSlug != repairedHost || locations[id].ProjectHash != wantProject {
				t.Fatalf("repaired host relation disagrees with snapshot: %v", err)
			}
			// Force a same-entry index no-op through a new capture transaction.
			cfg.Force = true
			if repeated := run(); repeated.Summary.Errors != 0 || repeated.Summary.StoreError != nil {
				t.Fatalf("repeat: %+v", repeated)
			}
			repeated, err := database.LoadPublicationInput(ctx, id)
			if err != nil || repeated.Readiness != ingest.PublicationReady || repeated.CaptureRevision <= bundle.CaptureRevision {
				t.Fatalf("no-op failed to align revision: %+v, %v", repeated, err)
			}
			if !reflect.DeepEqual(bundle.Associations, repeated.Associations) {
				t.Fatal("association identity changed")
			}
			if repeated.SessionOrigin != sessionorigin.User {
				t.Fatal("capture overwrote previously judged origin")
			}
			if got, err := os.ReadFile(session.SourcePath.String()); err != nil || !bytes.Equal(got, sourceBytes) {
				t.Fatalf("ingest changed retained source: %v", err)
			}
			if repeated.Metadata.MetadataHash != schema.ComputeMetadataHash(&repeated.Metadata) {
				t.Fatal("normalized metadata hash does not match snapshot")
			}
			if c.ChangeSource {
				changed := bytes.ReplaceAll(sourceBytes, []byte("synthetic response"), []byte("changed response"))
				changed = bytes.ReplaceAll(changed, []byte(c.CWD), []byte("/synthetic/new/literal"))
				if err := os.WriteFile(session.SourcePath.String(), changed, 0600); err != nil {
					t.Fatal(err)
				}
				cfg.Force = false
				// A future mtime proves the ordinary source-newer branch, not --force.
				future := time.Now().Add(time.Hour)
				if err := os.Chtimes(session.SourcePath.String(), future, future); err != nil {
					t.Fatal(err)
				}
				if result := run(); result.Summary.Errors != 0 || result.Summary.Updated != 1 {
					t.Fatalf("changed source: %+v", result)
				}
				if err := database.Close(); err != nil {
					t.Fatal(err)
				}
				database, err = store.Open(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				changedBundle, err := database.LoadPublicationInput(ctx, id)
				if err != nil || changedBundle.Readiness != ingest.PublicationReady || changedBundle.CaptureRevision <= repeated.CaptureRevision || changedBundle.Metadata.CWD != "/synthetic/new/literal" {
					t.Fatalf("changed capture: %+v, %v", changedBundle, err)
				}
				entryJSON, err := json.Marshal(changedBundle.Entries)
				if err != nil || !bytes.Contains(entryJSON, []byte("changed response")) {
					t.Fatalf("changed entries missing: %s, %v", entryJSON, err)
				}
				if changedBundle.Metadata.Project.Hash != meta.Project.Hash {
					t.Fatal("changed CWD overrode tracked project identity")
				}
			}
			// Source-backed reindex captures metadata and index inputs together.
			if c.ReindexRemoveSource {
				if err := os.Remove(session.SourcePath.String()); err != nil {
					t.Fatal(err)
				}
			}
			if c.ReindexChangeManaged || c.ReindexChangeSource || c.ReindexChangeTree != "" {
				path := session.SourcePath.String()
				if c.ReindexChangeManaged {
					path = filepath.Join(filepath.Dir(metadataPath), c.ID+"--transcript."+string(session.SourceFormat))
				}
				if c.ReindexChangeTree != "" {
					path = filepath.Join(root, c.ReindexChangeTree)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data = bytes.ReplaceAll(data, []byte("synthetic response"), []byte("reindexed response"))
				if c.ReindexChangeSource {
					data = bytes.ReplaceAll(data, []byte(c.CWD), []byte("/synthetic/reindex/../literal"))
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if c.ReindexRemoveProof {
				conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
				if err != nil {
					t.Fatal(err)
				}
				err = sqlitex.ExecuteTransient(conn, "DELETE FROM session_publication_metadata WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{c.ID}})
				_ = conn.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			lastGood, err := database.ListEntries(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Reindex = true
			if c.ReindexMutateAfterRead {
				filesystem.mutateAfterRead = filepath.Join(filepath.Dir(metadataPath), c.ID+"--transcript."+string(session.SourceFormat))
			}
			manualResult := run()
			if manualResult.Summary.Errors != 0 || manualResult.Summary.StoreError != nil {
				t.Fatalf("manual reindex failed: %+v", manualResult)
			}
			if manualResult.Summary.Indexed != *c.ExpectedManualIndexed {
				t.Fatalf("manual indexed = %d want %d: %+v", manualResult.Summary.Indexed, *c.ExpectedManualIndexed, manualResult)
			}
			if c.ReindexChangeManaged {
				// A managed pair whose transcript no longer matches its committed
				// metadata is refused, not re-indexed: the refusal is a warning
				// naming the session AND the checksum that failed, the run has no
				// error, the last-good index is untouched, and the verified
				// capture is still served.
				//
				// Either strict reader of the committed pair may be the one that
				// meets it: the reconciliation walk, which reads a pair only when
				// the session's small evidence says it changed, or the index
				// capture, which verifies the pair of every session it evaluates.
				warned := false
				for _, diagnostic := range manualResult.Diagnostics {
					reportedBy := diagnostic.ErrorType == "artifact_recovery_incomplete" || diagnostic.ErrorType == "index_refused"
					if reportedBy && strings.Contains(diagnostic.Message, c.ID) && diagnostic.Remediation != "" &&
						strings.Contains(diagnostic.Message, "transcript checksum does not match committed metadata") {
						warned = true
					}
				}
				if !warned {
					t.Fatalf("inconsistent managed pair was not reported as a recovery warning: %+v", manualResult.Diagnostics)
				}
				untouched, err := database.ListEntries(ctx, id)
				if err != nil || !reflect.DeepEqual(untouched, lastGood) {
					t.Fatalf("refused reindex changed the last-good index: %v", err)
				}
				snapshot, err := database.ReadSessionAvailable(ctx, id)
				if err != nil || !reflect.DeepEqual(snapshot.Entries, lastGood) {
					t.Fatalf("verified capture is no longer served after the refused reindex: %+v %v", snapshot, err)
				}
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			database, err = store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			manual, err := database.LoadPublicationInput(ctx, id)
			if err != nil {
				t.Fatalf("load after manual reindex: %v", err)
			}
			if string(manual.Readiness) != c.ExpectedManualReadiness {
				t.Fatalf("manual reindex readiness = %s want %s", manual.Readiness, c.ExpectedManualReadiness)
			}
			if manual.Readiness == ingest.PublicationReady {
				wantCWD := c.CWD
				if c.ChangeSource {
					wantCWD = "/synthetic/new/literal"
				}
				if c.ReindexChangeSource {
					wantCWD = "/synthetic/reindex/../literal"
				}
				if manual.Metadata.CWD != wantCWD || publicationStoredProvenance(t, dbPath, id) != c.Provenance || manual.Metadata.Project.Hash != meta.Project.Hash || manual.Metadata.HostSlug != meta.HostSlug || !reflect.DeepEqual(manual.Metadata.Git.Remote, meta.Git.Remote) || !reflect.DeepEqual(manual.Associations, repeated.Associations) {
					t.Fatalf("reindex changed source provenance or repaired attribution: %+v", manual)
				}
			}
			if c.ReindexMutateAfterRead {
				data, err := json.Marshal(manual.Entries)
				if err != nil || !filesystem.mutatedAfterRead || bytes.Contains(data, []byte("racing response")) || !bytes.Contains(data, []byte("synthetic response")) {
					t.Fatalf("fallback reread input after verification: %s, %v", data, err)
				}
			}
			if c.ReindexChangeSource {
				data, err := json.Marshal(manual.Entries)
				if err != nil || !bytes.Contains(data, []byte("reindexed response")) || manual.Metadata.CWD != "/synthetic/reindex/../literal" || manual.CaptureRevision <= repeated.CaptureRevision || manual.Metadata.Project.Hash != meta.Project.Hash {
					t.Fatalf("fresh reindex did not capture coherent source facts: %+v, %v", manual, err)
				}
			}
		})
	}
}

// Interpose a real competing capture immediately before the real entry writer.
// Both operations use the production SQLite transactions, including no-op writes.
type publicationReindexStore struct {
	*store.Store
	recapture         bool
	captureErr        error
	unreadableEntries bool
}

var _ ingest.SessionEntryBatchStore = (*publicationReindexStore)(nil)
var _ ingest.PublicationInputReader = (*publicationReindexStore)(nil)

func (s *publicationReindexStore) LoadPublicationInput(ctx context.Context, id ingest.SessionID) (ingest.PublicationInputBundle, error) {
	if s.unreadableEntries {
		return ingest.PublicationInputBundle{}, errors.New("old transcript entries are unavailable during rebuild; recover from the verified captured input")
	}
	return s.Store.LoadPublicationInput(ctx, id)
}

func (s *publicationReindexStore) IndexSessionEntryBatch(ctx context.Context, writes []ingest.SessionEntryWrite) []ingest.SessionEntryWriteResult {
	if s.recapture && len(writes) > 0 {
		s.recapture = false
		bundle, err := s.LoadPublicationInput(ctx, writes[0].SessionID)
		if err == nil {
			_, err = s.InsertSessionsWithRevisions(ctx, []ingest.StoreEntry{{Metadata: &bundle.Metadata, PublicationCapture: true, CWDProvenance: ingest.CWDSourceExact}})
		}
		s.captureErr = err
		if err != nil {
			return []ingest.SessionEntryWriteResult{{SessionID: writes[0].SessionID, Err: err}}
		}
	}
	return s.Store.IndexSessionEntryBatch(ctx, writes)
}

type publicationCaptureFS struct {
	*ingest.OSFileSystem
	disappearPath    string
	failSidecar      bool
	mutatePath       string
	mutateAfterRead  string
	mutatedAfterRead bool
}

var _ ingest.FileSystem = (*publicationCaptureFS)(nil)

func (fs *publicationCaptureFS) ReadSourcePrefix(path string) ([]byte, error) {
	if path == fs.disappearPath {
		_ = os.Remove(path)
	}
	return fs.OSFileSystem.ReadSourcePrefix(path)
}

func (fs *publicationCaptureFS) ReadFile(path string) ([]byte, error) {
	if path == fs.disappearPath {
		_ = os.Remove(path)
	}
	data, err := fs.OSFileSystem.ReadFile(path)
	if err == nil && path == fs.mutateAfterRead && !fs.mutatedAfterRead {
		if err := os.WriteFile(path, bytes.ReplaceAll(data, []byte("synthetic response"), []byte("racing response")), 0600); err != nil {
			return nil, err
		}
		fs.mutatedAfterRead = true
	}
	return data, err
}

func (fs *publicationCaptureFS) WriteFile(path string, data []byte, mode os.FileMode) error {
	if strings.HasSuffix(path, "--metadata.json") {
		if fs.mutatePath != "" {
			original, err := os.ReadFile(fs.mutatePath)
			if err != nil {
				return err
			}
			if err := os.WriteFile(fs.mutatePath, bytes.ReplaceAll(original, []byte("synthetic response"), []byte("changed response")), 0600); err != nil {
				return err
			}
		}
		if fs.failSidecar {
			return os.ErrPermission
		}
	}
	return fs.OSFileSystem.WriteFile(path, data, mode)
}

func publicationStoredProvenance(t *testing.T, path string, id ingest.SessionID) string {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var value string
	if err := sqlitex.ExecuteTransient(conn, `SELECT cwd_provenance_kind FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{id.String()}, ResultFunc: func(stmt *sqlite.Stmt) error { value = stmt.ColumnText(0); return nil }}); err != nil {
		t.Fatal(err)
	}
	return value
}

func publicationAdapterFactory(t *testing.T, harness ingest.Harness, environment mountedCurrentEnvironment) ingest.AdapterFactory {
	t.Helper()
	return func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
		switch harness {
		case ingest.HarnessClaudeCode:
			return ingest.NewClaudeAdapter(fs, git, s)
		case ingest.HarnessCodex:
			return ingest.NewCodexAdapter(fs, git, s)
		case ingest.HarnessCursor:
			return ingest.NewCursorAdapter(fs, git, s)
		case ingest.HarnessStrike:
			return ingest.NewStrikeAdapter(fs, git, s)
		case ingest.HarnessOpenCode:
			adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(fs, git, s, "latest", environment, fs.(ingest.OpenCodeCandidateFileSystem), ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions())
			if err != nil {
				t.Fatal(err)
			}
			return adapter
		default:
			t.Fatalf("unsupported harness %s", harness)
			return nil
		}
	}
}

func publicationIndexer(t *testing.T, harness ingest.Harness, fs ingest.FileSystem) ingest.TranscriptIndexer {
	t.Helper()
	switch harness {
	case ingest.HarnessClaudeCode:
		return ingest.NewClaudeIndexer(fs)
	case ingest.HarnessCodex:
		return ingest.NewCodexIndexer(fs)
	case ingest.HarnessCursor:
		return ingest.NewCursorIndexer(fs)
	case ingest.HarnessStrike:
		return ingest.NewStrikeIndexer(fs)
	case ingest.HarnessOpenCode:
		return ingest.NewOpenCodeIndexer(fs)
	default:
		t.Fatalf("unsupported harness %s", harness)
		return nil
	}
}
