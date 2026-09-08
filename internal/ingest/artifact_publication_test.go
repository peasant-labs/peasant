package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// The owned-file fault corpus covers rollback/no-partial-file guarantees at the
// actual publication boundary, replacing the retired directory-copy injection.
//
//go:embed testdata/artifact_publication.yaml
var artifactPublicationYAML []byte

type artifactPublicationCase struct {
	Name            string `yaml:"name"`
	Phase           string `yaml:"phase"`
	Operation       string `yaml:"operation"`
	Target          string `yaml:"target"`
	After           bool   `yaml:"after"`
	Database        bool   `yaml:"database"`
	MirrorFailure   bool   `yaml:"mirrorFailure"`
	WantNew         bool   `yaml:"wantNew"`
	Partial         bool   `yaml:"partial"`
	WithDebug       bool   `yaml:"withDebug"`
	SameTranscript  bool   `yaml:"sameTranscript"`
	RollbackFailure bool   `yaml:"rollbackFailure"`
	Crash           bool   `yaml:"crash"`
	Relocate        bool   `yaml:"relocate"`
}

type artifactPublicationFault struct {
	row             artifactPublicationCase
	armed           atomic.Bool
	reached         atomic.Bool
	rollbackReached atomic.Bool
}
type artifactPublicationFS struct {
	ingest.OSFileSystem
	fault *artifactPublicationFault
}
type artifactPublicationRoot struct {
	ingest.ArtifactRoot
	fault *artifactPublicationFault
}

var _ ingest.DurableFileSystem = (*artifactPublicationFS)(nil)
var _ ingest.ArtifactRoot = (*artifactPublicationRoot)(nil)

func (f *artifactPublicationFS) OpenArtifactRoot(path string) (ingest.ArtifactRoot, error) {
	root, err := f.OSFileSystem.OpenArtifactRoot(path)
	if err != nil {
		return nil, err
	}
	return &artifactPublicationRoot{ArtifactRoot: root, fault: f.fault}, nil
}
func (f *artifactPublicationFS) CreateArtifactRoot(path string) (ingest.ArtifactRoot, error) {
	root, err := f.OSFileSystem.CreateArtifactRoot(path)
	if err != nil {
		return nil, err
	}
	return &artifactPublicationRoot{ArtifactRoot: root, fault: f.fault}, nil
}
func (r *artifactPublicationRoot) fail(operation, path string, after bool) bool {
	if r.fault == nil || r.fault.row.Operation != operation || r.fault.row.After != after || !strings.HasSuffix(path, r.fault.row.Target) {
		return false
	}
	if !r.fault.armed.CompareAndSwap(true, false) {
		return false
	}
	r.fault.reached.Store(true)
	if r.fault.row.Crash {
		os.Exit(93)
	}
	return true
}
func (r *artifactPublicationRoot) Rename(from, to string) error {
	if r.fail("rename", to, false) {
		return errors.New("synthetic artifact rename failure")
	}
	if err := r.ArtifactRoot.Rename(from, to); err != nil {
		return err
	}
	if r.fault != nil && r.fault.row.RollbackFailure && r.fault.reached.Load() && strings.HasSuffix(to, "--metadata.json") && r.fault.rollbackReached.CompareAndSwap(false, true) {
		return errors.New("synthetic interrupted rollback after metadata restore")
	}
	if r.fail("rename", to, true) {
		return errors.New("synthetic post-rename failure")
	}
	return nil
}
func (r *artifactPublicationRoot) CreateFile(path string, data []byte, mode fs.FileMode) error {
	if r.fail("create", path, false) {
		if r.fault.row.Partial {
			if err := r.ArtifactRoot.CreateFile(path, data[:len(data)/2], mode); err != nil {
				return err
			}
		}
		return errors.New("synthetic artifact write failure")
	}
	return r.ArtifactRoot.CreateFile(path, data, mode)
}

func (r *artifactPublicationRoot) Remove(path string) error {
	if r.fail("remove", path, false) {
		return errors.New("synthetic artifact cleanup failure")
	}
	if err := r.ArtifactRoot.Remove(path); err != nil {
		return err
	}
	if r.fail("remove", path, true) {
		return errors.New("synthetic post-removal cleanup failure")
	}
	return nil
}

func (r *artifactPublicationRoot) SyncFile(path string) error {
	if r.fail("sync", path, false) {
		return errors.New("synthetic candidate fsync failure")
	}
	return r.ArtifactRoot.SyncFile(path)
}

type artifactPublicationMirror struct {
	db   *store.Store
	fail atomic.Bool
}

var _ ingest.ArtifactMirrorStore = (*artifactPublicationMirror)(nil)

func (m *artifactPublicationMirror) MirrorArtifacts(ctx context.Context, requests []ingest.ArtifactMirrorRequest) []ingest.ArtifactMirrorResult {
	if !m.fail.Load() {
		return m.db.MirrorArtifacts(ctx, requests)
	}
	results := make([]ingest.ArtifactMirrorResult, len(requests))
	for i, request := range requests {
		results[i] = ingest.ArtifactMirrorResult{SessionID: request.Artifact.Metadata.SessionID, Err: errors.New("synthetic mirror unavailable")}
	}
	return results
}

type artifactPublicationFixtures struct {
	RequiredNames []string                  `yaml:"requiredNames"`
	OldTranscript string                    `yaml:"oldTranscript"`
	NewTranscript string                    `yaml:"newTranscript"`
	DebugNames    []string                  `yaml:"debugNames"`
	OldDebug      string                    `yaml:"oldDebug"`
	NewDebug      string                    `yaml:"newDebug"`
	LegacyHost    string                    `yaml:"legacyHost"`
	Cases         []artifactPublicationCase `yaml:"cases"`
}

func loadArtifactPublicationFixtures(t *testing.T) artifactPublicationFixtures {
	t.Helper()
	var fixture artifactPublicationFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(artifactPublicationYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("publication fixture requires one YAML document")
	}
	required := []string{"file-only-commit", "before-transcript-swap", "after-transcript-swap", "before-metadata-commit", "after-metadata-commit", "database-failure-after-commit", "derived-cache-write-failure", "before-intent-write", "partial-intent-write", "candidate-sync-failure", "after-intent-deletion", "identical-metadata-debug-interruption", "identical-metadata-after-commit", "identical-metadata-rollback-interruption", "crash-before-intent", "crash-after-transcript", "crash-after-metadata", "crash-after-derived", "crash-after-intent-deletion", "crash-identical-debug", "identical-input-relocation-before-commit", "identical-input-relocation-after-commit"}
	if !reflect.DeepEqual(fixture.RequiredNames, required) {
		t.Fatal("publication required-name manifest changed")
	}
	return fixture
}

func TestArtifactPublicationRecoversOwnedFilesAndHonestCache(t *testing.T) {
	t.Parallel()
	fixture := loadArtifactPublicationFixtures(t)
	required := fixture.RequiredNames
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid publication fixture %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			output := filepath.Join(t.TempDir(), "managed")
			fault := &artifactPublicationFault{row: row}
			filesystem := &artifactPublicationFS{fault: fault}
			options := ingest.ArtifactPublisherOptions{}
			var mirror *artifactPublicationMirror
			dbPath := ""
			if row.Database {
				dbPath = filepath.Join(t.TempDir(), "peasant.db")
				db, err := store.Open(dbPath, store.WithPoolSize(1))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				mirror = &artifactPublicationMirror{db: db}
				options.Mirror = mirror
			}
			publisher, err := ingest.NewArtifactPublisher(filesystem, output, options)
			if err != nil {
				t.Fatal(err)
			}
			old := publicationTestArtifact(t, fixture.OldTranscript)
			session := ingest.DiscoveredSession{SessionID: old.Metadata.SessionID, Harness: old.Metadata.ModelHarness}
			oldDebug, newDebug := make(map[string][]byte), make(map[string][]byte)
			if row.WithDebug {
				for _, name := range fixture.DebugNames {
					session.DebugPaths = append(session.DebugPaths, ingest.ResolvedPath(filepath.Join("/fixtures", name)))
					oldDebug[name], newDebug[name] = []byte(fixture.OldDebug), []byte(fixture.NewDebug)
				}
			}
			observation, err := publisher.Observe(t.Context(), session, "")
			if err != nil {
				t.Fatal(err)
			}
			committed, err := publisher.Publish(t.Context(), ingest.ArtifactPublication{Artifact: old, Observation: observation, DebugFiles: oldDebug})
			if err != nil {
				t.Fatal(err)
			}
			old, err = publisher.Reconcile(t.Context(), committed)
			if err != nil {
				t.Fatal(err)
			}
			metadataPath := ingest.SessionMetadataPath(output, string(old.Metadata.HostSlug), string(session.SessionID), "")
			observedPath := metadataPath
			if row.Relocate {
				// A legacy locator can disagree with its already-recorded metadata.
				// Repairing the location need not change semantic artifact bytes.
				observedPath = ingest.SessionMetadataPath(output, fixture.LegacyHost, string(session.SessionID), "")
				if err := filesystem.MkdirAll(filepath.Dir(filepath.Dir(observedPath)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := filesystem.Rename(filepath.Dir(metadataPath), filepath.Dir(observedPath)); err != nil {
					t.Fatal(err)
				}
			}
			observation, err = publisher.Observe(t.Context(), session, observedPath)
			if err != nil {
				t.Fatal(err)
			}
			candidate := publicationTestArtifact(t, fixture.NewTranscript)
			if row.SameTranscript {
				candidate = publicationTestArtifact(t, fixture.OldTranscript)
			}
			if row.Crash {
				runPublicationCrashHelper(t, output, dbPath, row.Name)
			} else {
				if row.Phase == "publish" {
					fault.armed.Store(true)
				}
				committed, publishErr := publisher.Publish(t.Context(), ingest.ArtifactPublication{Artifact: candidate, Observation: observation, DebugFiles: newDebug})
				if row.Phase == "publish" {
					if publishErr == nil || !fault.reached.Load() {
						t.Fatalf("publication fault was not exercised: %v", publishErr)
					}
					if row.RollbackFailure && !fault.rollbackReached.Load() {
						t.Fatal("rollback interruption was not exercised")
					}
				} else if publishErr != nil {
					t.Fatal(publishErr)
				}
				if row.Phase != "publish" {
					if row.MirrorFailure {
						mirror.fail.Store(true)
					}
					if row.Phase == "reconcile" {
						fault.armed.Store(true)
					}
					_, err = publisher.Reconcile(t.Context(), committed)
					if (err != nil) != (row.MirrorFailure || row.Phase == "reconcile") {
						t.Fatalf("unexpected reconciliation outcome: %v", err)
					}
					if row.Phase == "reconcile" && !fault.reached.Load() {
						t.Fatal("derived-cache failure was not reached")
					}
					if row.MirrorFailure && publicationStoredHash(t, mirror.db, session.SessionID) != old.ArtifactHash {
						t.Fatal("failed DB mirror changed committed database identity")
					}
				}
			}
			if mirror != nil {
				mirror.fail.Store(false)
			}
			restarted, err := ingest.NewArtifactPublisher(&ingest.OSFileSystem{}, output, options)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := restarted.PendingSessions()
			if err != nil {
				t.Fatal(err)
			}
			for _, sid := range pending {
				if err := restarted.Recover(t.Context(), sid); err != nil {
					t.Fatal(err)
				}
			}
			var after *ingest.ManagedArtifact
			if row.Relocate && !row.WantNew {
				// Restore the exact pre-repair bytes; do not claim the legacy bad
				// locator itself became a valid captured production view.
				data, readErr := filesystem.ReadFile(observedPath)
				if readErr != nil || !bytes.Equal(data, old.MetadataJSON) {
					t.Fatalf("relocation rollback lost original metadata: %v", readErr)
				}
				transcript, readErr := filesystem.ReadFile(filepath.Join(filepath.Dir(observedPath), string(session.SessionID)+"--transcript.jsonl"))
				if readErr != nil {
					t.Fatal(readErr)
				}
				after, err = ingest.NewManagedArtifact(data, transcript)
				metadataPath = observedPath
			} else {
				after, err = restarted.Capture(t.Context(), session.SessionID, metadataPath)
			}
			if err != nil {
				t.Fatal(err)
			}
			want := old
			if row.WantNew {
				want = candidate
			}
			if !bytes.Equal(after.Transcript, want.Transcript) || after.ArtifactHash != want.ArtifactHash {
				t.Fatalf("wrong recovered generation: got %s want %s", after.ArtifactHash, want.ArtifactHash)
			}
			if (after.Metadata.DerivedAt != nil) != row.Database {
				t.Fatal("DerivedAt did not reflect actual database reconciliation")
			}
			if row.Database && publicationStoredHash(t, mirror.db, session.SessionID) != after.ArtifactHash {
				t.Fatal("database mirror and committed artifact disagree after recovery")
			}
			if row.WithDebug {
				wantDebug := fixture.OldDebug
				if row.WantNew {
					wantDebug = fixture.NewDebug
				}
				for _, name := range fixture.DebugNames {
					data, err := filesystem.ReadFile(filepath.Join(filepath.Dir(metadataPath), "debug", name))
					if err != nil || string(data) != wantDebug {
						t.Fatalf("debug recovery chose partial/wrong state for %s: %q %v", name, data, err)
					}
				}
			}
			if _, err := restarted.Observe(t.Context(), session, metadataPath); err != nil {
				t.Fatalf("completed/failed lifecycle permanently blocked a fresh observation: %v", err)
			}
			if pending, err := restarted.PendingSessions(); err != nil || len(pending) != 0 {
				t.Fatalf("completed publication retained pending intents: %v %v", pending, err)
			}
			cleanupDiagnostics := restarted.Cleanup(t.Context(), nil)
			if row.Partial {
				if len(cleanupDiagnostics) == 0 {
					t.Fatal("malformed preparation was silently discarded or ignored")
				}
			} else if len(cleanupDiagnostics) != 0 {
				t.Fatalf("validated lifecycle leftovers were not recoverable: %+v", cleanupDiagnostics)
			}
			entries, readErr := filesystem.ReadDir(filepath.Join(output, ".peasant-state", "transactions"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if !row.Partial {
					t.Errorf("completed recovery retained redundant lifecycle copy %q", entry.Name())
				}
			}
		})
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing publication fixture %q", name)
		}
	}
}

const publicationHelperRootEnv = "PEASANT_ARTIFACT_TEST_PUBLICATION_ROOT"
const publicationHelperDBEnv = "PEASANT_ARTIFACT_TEST_PUBLICATION_DB"
const publicationHelperCaseEnv = "PEASANT_ARTIFACT_TEST_PUBLICATION_CASE"

func runPublicationCrashHelper(t *testing.T, output, dbPath, name string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestArtifactPublicationCrashHelper$")
	child.Env = append(os.Environ(), publicationHelperRootEnv+"="+output, publicationHelperDBEnv+"="+dbPath, publicationHelperCaseEnv+"="+name)
	data, err := child.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 93 {
		t.Fatalf("publisher did not terminate at the requested persistence boundary: %v\n%s", err, data)
	}
}

func TestArtifactPublicationCrashHelper(t *testing.T) {
	output := os.Getenv(publicationHelperRootEnv)
	if output == "" {
		return
	}
	fixture := loadArtifactPublicationFixtures(t)
	var selected *artifactPublicationCase
	for _, row := range fixture.Cases {
		if row.Name == os.Getenv(publicationHelperCaseEnv) {
			copy := row
			selected = &copy
		}
	}
	if selected == nil || !selected.Crash {
		t.Fatal("unknown publication crash fixture")
	}
	row := *selected
	fault := &artifactPublicationFault{row: row}
	filesystem := &artifactPublicationFS{fault: fault}
	options := ingest.ArtifactPublisherOptions{}
	if row.Database {
		path := os.Getenv(publicationHelperDBEnv)
		if path == "" || !filepath.IsAbs(path) {
			t.Fatal("missing synthetic publication database path")
		}
		db, err := store.Open(path, store.WithPoolSize(1))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		options.Mirror = db
	}
	publisher, err := ingest.NewArtifactPublisher(filesystem, output, options)
	if err != nil {
		t.Fatal(err)
	}
	text := fixture.NewTranscript
	if row.SameTranscript {
		text = fixture.OldTranscript
	}
	artifact := publicationTestArtifact(t, text)
	session := ingest.DiscoveredSession{SessionID: artifact.Metadata.SessionID, Harness: artifact.Metadata.ModelHarness}
	debug := make(map[string][]byte)
	if row.WithDebug {
		for _, name := range fixture.DebugNames {
			session.DebugPaths = append(session.DebugPaths, ingest.ResolvedPath(filepath.Join("/fixtures", name)))
			debug[name] = []byte(fixture.NewDebug)
		}
	}
	path := ingest.SessionMetadataPath(output, string(artifact.Metadata.HostSlug), string(session.SessionID), "")
	observation, err := publisher.Observe(t.Context(), session, path)
	if err != nil {
		t.Fatal(err)
	}
	if row.Phase == "publish" {
		fault.armed.Store(true)
	}
	committed, err := publisher.Publish(t.Context(), ingest.ArtifactPublication{Artifact: artifact, Observation: observation, DebugFiles: debug})
	if err != nil {
		t.Fatal(err)
	}
	if row.Phase == "reconcile" {
		fault.armed.Store(true)
	}
	if _, err := publisher.Reconcile(t.Context(), committed); err != nil {
		t.Fatal(err)
	}
	t.Fatal("publication did not reach its configured crash boundary")
}

func publicationTestArtifact(t *testing.T, transcript string) *ingest.ManagedArtifact {
	t.Helper()
	var fixture struct {
		Metadata string `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(artifactCaptureYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal([]byte(fixture.Metadata), &meta); err != nil {
		t.Fatal(err)
	}
	version := 1
	meta.AdapterVersion = &version
	meta.ContentHash = schema.ComputeTranscriptHash([]byte(transcript))
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := ingest.NewManagedArtifact(data, []byte(transcript))
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func publicationStoredHash(t *testing.T, db *store.Store, sid ingest.SessionID) string {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var hash string
	if err := sqlitex.ExecuteTransient(conn, "SELECT artifact_hash FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error { hash = stmt.ColumnText(0); return nil }}); err != nil {
		t.Fatal(err)
	}
	return hash
}
