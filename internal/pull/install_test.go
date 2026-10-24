package pull_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content-install.yaml
var contentInstallYAML []byte

type installCase struct {
	Name             string `yaml:"name"`
	Operation        string `yaml:"operation"`
	Target           string `yaml:"target"`
	After            bool   `yaml:"after"`
	ForceRecovery    bool   `yaml:"forceRecovery"`
	Mode             string `yaml:"mode"`
	CorruptPrevious  bool   `yaml:"corruptPrevious"`
	RecoveryOnly     bool   `yaml:"recoveryOnly"`
	AbsentAfterFault bool   `yaml:"absentAfterFault"`
}

// A second process must not sweep or replace the first process's staging tree.
// This bounded subprocess is necessary because goroutines cannot detect a
// mistaken in-process-only mutex. It uses the actual production pull entry,
// filesystem, SQLite store and advisory-lock implementation.
func TestContentInstallProcessLock(t *testing.T) {
	const rootEnv = "PEASANT_PULL_LOCK_TEST_ROOT"
	const heldEnv = "PEASANT_PULL_LOCK_TEST_HELD"
	if root := os.Getenv(rootEnv); root != "" {
		db, err := store.Open(filepath.Join(root, "index.db"), store.WithSkipMigrations())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		deadline := 5 * time.Second
		if os.Getenv(heldEnv) == "true" {
			deadline = 150 * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		_, err = pull.NewPipeline(happyReader(), pull.NewFileSystem(filepath.Join(root, "pulls")), db, testutil.NewFixedClock(), testCreds(), filepath.Join(root, "pulls")).PullTranscript(ctx, newRef(t), pull.PullOptions{Force: true})
		if os.Getenv(heldEnv) == "true" {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("concurrent process did not honor content ownership: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		return
	}
	root := t.TempDir()
	storetest.CopyGoldenTo(t, filepath.Join(root, "index.db"))
	db, err := store.Open(filepath.Join(root, "index.db"), store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	pulls := filepath.Join(root, "pulls")
	hostRoot := filepath.Join(pulls, testutil.TestVillageHost)
	locker, err := store.NewFileSessionLocker(filepath.Join(hostRoot, ".pull-locks"))
	if err != nil {
		t.Fatal(err)
	}
	lockID, err := schema.NewSessionID(testutil.TestTranscriptUUID)
	if err != nil {
		t.Fatal(err)
	}
	release, err := locker.LockExclusive(context.Background(), lockID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if release != nil {
			_ = release()
		}
	})
	owned := filepath.Join(hostRoot, ".install-"+testutil.TestTranscriptUUID)
	if err := os.MkdirAll(owned, 0o700); err != nil {
		t.Fatal(err)
	}
	unowned := filepath.Join(owned, testutil.TestOwnerUserID)
	if err := os.MkdirAll(unowned, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(unowned, "unrelated-note")
	if err := os.WriteFile(sentinel, []byte("retain"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(held string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestContentInstallProcessLock$", "-test.count=1")
		command.Env = append(os.Environ(), rootEnv+"="+root, heldEnv+"="+held)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("child production pull: %v\n%s", err, output)
		}
	}
	run("true")
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "retain" {
		t.Fatalf("waiting process changed owned resources: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	release = nil
	run("false")
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "retain" {
		t.Fatalf("recovery deleted an unowned file: %v", err)
	}
}

func TestContentInstallDryRunDoesNotRepairOrWrite(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	storetest.CopyGoldenTo(t, filepath.Join(root, "index.db"))
	db, err := store.Open(filepath.Join(root, "index.db"), store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pulls := filepath.Join(root, "pulls")
	osfs := pull.NewFileSystem(pulls)
	fault := &installFaultStore{PullStore: db}
	if _, err := pull.NewPipeline(happyReader(), osfs, fault, testutil.NewFixedClock(), testCreds(), pulls).PullTranscript(ctx, newRef(t), pull.PullOptions{}); !errors.Is(err, errInstallInjected) {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(pulls, testutil.TestVillageHost, ".install-"+testutil.TestTranscriptUUID, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	filesystem := testutil.NewFailingFS(osfs)
	filesystem.WriteFileErr = errors.New("dry run wrote files")
	filesystem.RenameErr = errors.New("dry run published files")
	filesystem.RemoveAllErr = errors.New("dry run cleaned recovery resources")
	filesystem.RemoveErr = errors.New("dry run removed recovery journal")
	neverCommit := &installFaultStore{PullStore: db}
	reader := happyReader()
	reader.Meta.Title = "Must not install"
	result, err := pull.NewPipeline(reader, filesystem, neverCommit, testutil.NewFixedClock(), testCreds(), pulls).PullTranscript(ctx, newRef(t), pull.PullOptions{DryRun: true})
	if err != nil || !result.DryRun ||
		neverCommit.fired || reader.ContentCalls != 0 ||
		reader.AnnotationsCalls != 0 {
		t.Fatalf("dry run performed mutation/transfer: %+v %v", result, err)
	}
	after, err := os.ReadFile(filepath.Join(pulls, testutil.TestVillageHost, ".install-"+testutil.TestTranscriptUUID, "journal.json"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("dry run changed pending recovery: %v", err)
	}
}

func loadContentInstallFixtures(t *testing.T) []installCase {
	t.Helper()
	var corpus struct {
		RequiredNames []string      `yaml:"requiredNames"`
		Cases         []installCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(contentInstallYAML, &corpus); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, c := range corpus.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("empty/duplicate fixture name: %q", c.Name)
		}
		seen[c.Name] = true
	}
	for _, name := range corpus.RequiredNames {
		if !seen[name] {
			t.Fatalf("required content-install fixture missing: %s", name)
		}
	}
	return corpus.Cases
}

func loadReplacementAnnotations(t *testing.T) []schema.PullAnnotation {
	t.Helper()
	var corpus struct {
		Annotations []struct {
			Hash           string `yaml:"hash"`
			ID             string `yaml:"id"`
			AuthorUserID   string `yaml:"authorUserId"`
			AuthorUsername string `yaml:"authorUsername"`
		} `yaml:"replacementAnnotations"`
	}
	if err := yaml.Unmarshal(contentInstallYAML, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Annotations) == 0 {
		t.Fatal("replacement annotation fixture must introduce new foreign evidence")
	}
	annotations := make([]schema.PullAnnotation, 0, len(corpus.Annotations))
	for _, fixture := range corpus.Annotations {
		if fixture.Hash == "" || fixture.ID == "" ||
			fixture.AuthorUserID == "" || fixture.AuthorUsername == "" {
			t.Fatal("incomplete replacement annotation fixture")
		}
		a := foreignAnnotation(fixture.Hash)
		a.ID = fixture.ID
		a.AuthorUserID = fixture.AuthorUserID
		a.AuthorUsername = fixture.AuthorUsername
		annotations = append(annotations, a)
	}
	return annotations
}

var errInstallInjected = errors.New("injected filesystem/index interruption")

// This decorator faults actual OS operations, never the pipeline under test.
type installFaultFS struct {
	ingest.FileSystem
	fixture  installCase
	final    string
	fired    bool
	barriers []string
}

var _ ingest.FileSystem = (*installFaultFS)(nil)

func (f *installFaultFS) operation(op, path string, run func() error) error {
	match := false
	switch f.fixture.Target {
	case "metadata":
		match = filepath.Base(path) == pull.MetadataFilename
	case "journal":
		match = filepath.Base(path) == "journal.json"
	case "backup":
		match = filepath.Base(path) == "previous"
	case "publish":
		match = path == f.final
	case "metadataPublish":
		match = path == filepath.Join(f.final, pull.MetadataFilename)
	case "metadataBackup":
		match = filepath.Base(path) == "previous-metadata.json"
	case "metadataBackupTemp":
		match = filepath.Base(path) == "previous-metadata.json.tmp"
	case "transcript":
		match = filepath.Base(path) == pull.TranscriptFilename
	case "manifest":
		match = filepath.Base(path) == pull.ManifestFilename
	case "journalWrite":
		match = filepath.Base(path) == "journal.json.tmp"
	case "metadataBackupDirectory":
		root := filepath.Join(filepath.Dir(f.final), ".install-"+filepath.Base(f.final))
		_, err := os.Stat(filepath.Join(root, "previous-metadata.json"))
		match = path == root && err == nil
	case "metadataMovedStage":
		data, err := os.ReadFile(filepath.Join(f.final, pull.MetadataFilename))
		var meta schema.PullTranscriptInfo
		match = f.barrierName(path) == "stageDirectory" && err == nil &&
			json.Unmarshal(data, &meta) == nil && meta.Title != happyMeta().Title
	default:
		match = f.barrierName(path) == f.fixture.Target
	}
	if f.fired || op != f.fixture.Operation || !match {
		return run()
	}
	f.fired = true
	if f.fixture.After {
		if err := run(); err != nil {
			return err
		}
	}
	return errInstallInjected
}

func (f *installFaultFS) barrierName(path string) string {
	root := filepath.Join(filepath.Dir(f.final), ".install-"+filepath.Base(f.final))
	switch filepath.Base(path) {
	case pull.TranscriptFilename:
		return "stagedBlob"
	case pull.MetadataFilename:
		return "stagedMetadata"
	case pull.ManifestFilename:
		return "stagedManifest"
	case "journal.json.tmp":
		return "journalFile"
	}
	if path == filepath.Dir(filepath.Dir(filepath.Dir(f.final))) {
		return "ancestor"
	}
	if path == root {
		if _, err := os.Stat(filepath.Join(root, "journal.json")); err == nil {
			return "journalDirectory"
		}
	}
	if filepath.Dir(path) == root {
		if _, err := uuid.Parse(filepath.Base(path)); err == nil {
			return "stageDirectory"
		}
	}
	if path == filepath.Dir(f.final) || path == f.final {
		data, err := os.ReadFile(filepath.Join(f.final, pull.MetadataFilename))
		var meta schema.PullTranscriptInfo
		if err == nil && json.Unmarshal(data, &meta) == nil && meta.Title != happyMeta().Title {
			return "publicationDirectory"
		}
	}
	return ""
}

func (f *installFaultFS) sync(path string) error {
	if point := f.barrierName(path); point != "" {
		f.barriers = append(f.barriers, point)
	}
	return f.operation("sync", path, func() error { return syncActualInstallFile(f.FileSystem, path) })
}

func syncActualInstallFile(filesystem ingest.FileSystem, path string) error {
	if syncing, ok := filesystem.(interface{ Sync(string) error }); ok {
		return syncing.Sync(path)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

type installAuditStore struct {
	pull.PullStore
	commits int
	before  func()
}

var _ pull.PullStore = (*installAuditStore)(nil)

func (s *installAuditStore) CommitPull(ctx context.Context, commit store.PullCommit) error {
	if s.before != nil {
		s.before()
	}
	s.commits++
	return s.PullStore.CommitPull(ctx, commit)
}
func (f *installFaultFS) WriteFile(path string, data []byte, mode os.FileMode) error {
	return f.operation("write", path, func() error { return f.FileSystem.WriteFile(path, data, mode) })
}
func (f *installFaultFS) Rename(src, dst string) error {
	return f.operation("rename", dst, func() error { return f.FileSystem.Rename(src, dst) })
}
func (f *installFaultFS) RemoveAll(path string) error {
	return f.operation("removeAll", path, func() error { return f.FileSystem.RemoveAll(path) })
}
func (f *installFaultFS) Remove(path string) error {
	return f.operation("remove", path, func() error { return f.FileSystem.Remove(path) })
}

func (f *installFaultFS) CopyFile(src, dst string, mode os.FileMode) error {
	return f.operation("copy", dst, func() error { return f.FileSystem.CopyFile(src, dst, mode) })
}

type installFaultStore struct {
	pull.PullStore
	after bool
	fired bool
}

var _ pull.PullStore = (*installFaultStore)(nil)

func (s *installFaultStore) CommitPull(ctx context.Context, commit store.PullCommit) error {
	if s.fired {
		return s.PullStore.CommitPull(ctx, commit)
	}
	s.fired = true
	if s.after {
		if err := s.PullStore.CommitPull(ctx, commit); err != nil {
			return err
		}
	}
	return errInstallInjected
}

func TestContentInstallRestartRecovery(t *testing.T) {
	for _, fixture := range loadContentInstallFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			root := t.TempDir()
			dbPath := filepath.Join(root, "index.db")
			storetest.CopyGoldenTo(t, dbPath)
			db, err := store.Open(dbPath, store.WithSkipMigrations())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			pulls := filepath.Join(root, "pulls")
			osfs := pull.NewFileSystem(pulls)
			ref := newRef(t)
			first := happyReader()
			if _, err := pull.NewPipeline(first, osfs, db, testutil.NewFixedClock(), testCreds(), pulls).PullTranscript(ctx, ref, pull.PullOptions{}); err != nil {
				t.Fatal(err)
			}
			final := filepath.Join(pulls, testutil.TestVillageHost, ref.ID.String())
			oldBlob, err := os.ReadFile(filepath.Join(final, pull.TranscriptFilename))
			if err != nil {
				t.Fatal(err)
			}
			oldBlobInfo, err := os.Stat(filepath.Join(final, pull.TranscriptFilename))
			if err != nil {
				t.Fatal(err)
			}
			oldMetadata, err := os.ReadFile(filepath.Join(final, pull.MetadataFilename))
			if err != nil {
				t.Fatal(err)
			}
			oldManifest, err := os.ReadFile(filepath.Join(final, pull.ManifestFilename))
			if err != nil {
				t.Fatal(err)
			}
			if fixture.CorruptPrevious {
				if err := os.Remove(filepath.Join(final, pull.MetadataFilename)); err != nil {
					t.Fatal(err)
				}
			}
			reader := happyReader()
			reader.Meta.Title = "Updated metadata"
			force := fixture.Mode == ""
			if force {
				reader.ContentBody = []byte(`{"updated":true}`)
				reader.ContentETag = quotedETag(schema.ComputeTranscriptHash(reader.ContentBody))
				reader.Meta.ContentHash = schema.ComputeTranscriptHash(reader.ContentBody)
				for _, annotation := range loadReplacementAnnotations(t) {
					initial, err := db.ListPulledAnnotationHashesByTranscript(ctx, testutil.TestVillageHost)
					if err != nil {
						t.Fatal(err)
					}
					for _, hash := range initial[ref.ID] {
						if annotation.ContentHash != nil && hash == *annotation.ContentHash {
							t.Fatal("replacement fixture must be absent from the initial annotation projection")
						}
					}
					reader.Annotations = append(reader.Annotations, annotation)
				}
				reader.Meta.AnnotationCount = len(reader.Annotations)
			}
			switch fixture.Mode {
			case "etag":
				reader.Meta.ContentHash = ""
				reader.ContentErr = village.ErrNotModified
			case "download":
				reader.Meta.ContentHash = ""
			}
			filesystem := &installFaultFS{FileSystem: osfs, fixture: fixture, final: final}
			audit := &installAuditStore{PullStore: db}
			var persistence pull.PullStore = audit
			var failedStore *installFaultStore
			if fixture.Operation == "index" {
				failedStore = &installFaultStore{PullStore: audit, after: fixture.After}
				persistence = failedStore
			}
			result, err := pull.NewPipeline(reader, filesystem, persistence, testutil.NewFixedClock(), testCreds(), pulls, pull.WithInstallSync(filesystem.sync)).PullTranscript(ctx, ref, pull.PullOptions{Force: force})
			if !errors.Is(err, errInstallInjected) || result.Status != pull.PullStatusError {
				t.Fatalf("fault must be a required failure, result=%+v error=%v", result, err)
			}
			if !filesystem.fired && (failedStore == nil || !failedStore.fired) {
				t.Fatal("fault did not hit actual production operation")
			}
			if fixture.Operation == "sync" && audit.commits != 0 {
				t.Fatal("persistence barrier failure reached index transaction")
			}
			// The previous complete files must survive in the published directory
			// or the owned backup until the replacement index is verified.
			backup := filepath.Join(pulls, testutil.TestVillageHost, ".install-"+ref.ID.String(), "previous")
			oldDir := final
			if _, err := os.Stat(backup); err == nil {
				oldDir = backup
			}
			if fixture.AbsentAfterFault {
				if _, err := os.Lstat(final); !os.IsNotExist(err) {
					t.Fatalf("canonical directory must be absent after interrupted backup rename: %v", err)
				}
				if oldDir != backup {
					t.Fatal("complete previous copy is not retained in backup")
				}
			}
			if fixture.Target != "journal" || fixture.Operation != "remove" {
				got, err := os.ReadFile(filepath.Join(oldDir, pull.TranscriptFilename))
				if err != nil || string(got) != string(oldBlob) {
					t.Fatalf("previous complete blob lost before verified projection: %q %v", got, err)
				}
			}
			if fixture.Mode != "" {
				currentBlobInfo, err := os.Stat(filepath.Join(final, pull.TranscriptFilename))
				if err != nil || !os.SameFile(oldBlobInfo, currentBlobInfo) {
					t.Fatalf("metadata-only update moved or replaced content: %v", err)
				}
				currentMetadata, err := os.ReadFile(filepath.Join(final, pull.MetadataFilename))
				if err != nil {
					t.Fatal(err)
				}
				if string(currentMetadata) != string(oldMetadata) && fixture.Operation != "remove" {
					previous, err := os.ReadFile(filepath.Join(filepath.Dir(backup), "previous-metadata.json"))
					if err != nil || string(previous) != string(oldMetadata) {
						t.Fatalf("previous metadata lost before verified projection: %v", err)
					}
				}
				got, err := os.ReadFile(filepath.Join(final, pull.ManifestFilename))
				if err != nil || string(got) != string(oldManifest) {
					t.Fatalf("metadata-only update changed provenance: %v", err)
				}
				if (fixture.Mode == "hash" && reader.ContentCalls != 0) ||
					(fixture.Mode == "etag" && reader.ContentCalls != 1) {
					t.Fatal("metadata-only update transferred blob")
				}
			}
			// Reopen the actual SQLite store and construct a fresh pipeline: no
			// in-memory journal state or manual SQL is available during recovery.
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(dbPath, store.WithSkipMigrations())
			if err != nil {
				t.Fatal(err)
			}
			retry := happyReader()
			retry.Meta = reader.Meta
			retry.ContentBody = reader.ContentBody
			retry.ContentETag = reader.ContentETag
			retry.Annotations = reader.Annotations
			if fixture.RecoveryOnly {
				metaCopy := *reader.Meta
				metaCopy.ContentHash = schema.ComputeTranscriptHash(reader.ContentBody)
				retry.Meta = &metaCopy
				retry.ContentErr = errors.New("restart must not download replacement bytes")
				retry.AnnotationsErr = errors.New("restart must not fetch replacement annotations")
			}
			result, err = pull.NewPipeline(retry, osfs, db, testutil.NewFixedClock(), testCreds(), pulls).PullTranscript(ctx, ref, pull.PullOptions{Force: fixture.ForceRecovery})
			if err != nil || (result.Status != pull.PullStatusPulled && result.Status != pull.PullStatusUpToDate) {
				t.Fatalf("restart repair: result=%+v error=%v", result, err)
			}
			if fixture.RecoveryOnly && (retry.ContentCalls != 0 || retry.AnnotationsCalls != 0) {
				t.Fatal("remote transfer masked failed local journal recovery")
			}
			assertInstalledContent(t, db, final, reader)
			row, err := db.GetPulledTranscript(ctx, testutil.TestVillageHost, ref.ID)
			if err != nil || row == nil || row.Title != reader.Meta.Title {
				t.Fatalf("index projection was not repaired: row=%+v error=%v", row, err)
			}
			var installed schema.PullTranscriptInfo
			data, err := os.ReadFile(filepath.Join(final, pull.MetadataFilename))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &installed); err != nil || installed.Title != row.Title {
				t.Fatalf("files/index disagree: %+v %v", installed, err)
			}
			entries, err := os.ReadDir(filepath.Dir(backup))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				t.Errorf("owned recovery resource leaked after repair: %s", entry.Name())
			}
		})
	}
}

func TestContentInstallBarrierOrder(t *testing.T) {
	pulls, db, filesystem := newSafetySandbox(t)
	final := filepath.Join(pulls, testutil.TestVillageHost, testutil.TestTranscriptUUID)
	fault := &installFaultFS{FileSystem: filesystem, final: final}
	var corpus struct {
		BarrierOrder []string `yaml:"barrierOrder"`
	}
	if err := yaml.Unmarshal(contentInstallYAML, &corpus); err != nil {
		t.Fatal(err)
	}
	audit := &installAuditStore{PullStore: db, before: func() {
		cursor := 0
		for _, point := range fault.barriers {
			if cursor < len(corpus.BarrierOrder) && point == corpus.BarrierOrder[cursor] {
				cursor++
			}
		}
		if cursor != len(corpus.BarrierOrder) {
			t.Fatalf("index commit preceded required ordered persistence barriers: %v; require %v", fault.barriers, corpus.BarrierOrder)
		}
	}}
	_, err := pull.NewPipeline(replacementReader(), fault, audit, testutil.NewFixedClock(), testCreds(), pulls, pull.WithInstallSync(fault.sync)).PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
	if err != nil {
		t.Fatal(err)
	}
}
