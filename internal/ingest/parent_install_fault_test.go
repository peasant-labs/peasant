package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/parent_install_faults.yaml
var parentInstallFaultYAML []byte

type parentInstallFaultMode string

const (
	installSuccess        parentInstallFaultMode = "success"
	installFailOnce       parentInstallFaultMode = "fail_once"
	installFailPersistent parentInstallFaultMode = "fail_persistent"
	installExitBefore     parentInstallFaultMode = "exit_before"
	installExitAfter      parentInstallFaultMode = "exit_after"
)

type parentInstallFaultFixture struct {
	ParentID      string `yaml:"parent_id"`
	ChildID       string `yaml:"child_id"`
	Parent        string `yaml:"parent"`
	UpdatedParent string `yaml:"updated_parent"`
	Child         string `yaml:"child"`
	Cases         []struct {
		Name   string                 `yaml:"name"`
		Mode   parentInstallFaultMode `yaml:"mode"`
		Remote string                 `yaml:"remote"`
	} `yaml:"cases"`
}

// The real filesystem performs every successful operation. Once tripped, the
// persistent fault also refuses copies and writes (including hypothetical
// rollback), so this cannot mistake a copy/delete helper for atomic rename.
type parentInstallFaultFS struct {
	*ingest.OSFileSystem
	// output is the managed output directory. An artifact root confines every
	// path to that directory, so a root operation names its file relatively and
	// has to be rejoined with output before it can be compared with target.
	output  string
	target  string
	mode    parentInstallFaultMode
	tripped bool
}

var _ ingest.FileSystem = (*parentInstallFaultFS)(nil)
var _ ingest.DurableFileSystem = (*parentInstallFaultFS)(nil)

// OpenArtifactRoot and CreateArtifactRoot are the seam the shipped install
// actually crosses. An owned file is installed by the pinned artifact root's
// transaction apply, not through the plain filesystem, so a fault injected only
// at the FileSystem level never sees the install and proves nothing. Both
// openers are wrapped: publication takes the creating one.
func (f *parentInstallFaultFS) OpenArtifactRoot(path string) (ingest.ArtifactRoot, error) {
	root, err := f.OSFileSystem.OpenArtifactRoot(path)
	if err != nil {
		return nil, err
	}
	return &parentInstallFaultRoot{ArtifactRoot: root, fault: f}, nil
}

func (f *parentInstallFaultFS) CreateArtifactRoot(path string) (ingest.ArtifactRoot, error) {
	root, err := f.OSFileSystem.CreateArtifactRoot(path)
	if err != nil {
		return nil, err
	}
	return &parentInstallFaultRoot{ArtifactRoot: root, fault: f}, nil
}

// parentInstallFaultRoot injects the fault at the install of one owned file.
type parentInstallFaultRoot struct {
	ingest.ArtifactRoot
	fault *parentInstallFaultFS
}

func (r *parentInstallFaultRoot) Rename(oldPath, newPath string) error {
	return r.fault.install(filepath.Join(r.fault.output, newPath), func() error {
		return r.ArtifactRoot.Rename(oldPath, newPath)
	})
}

func (f *parentInstallFaultFS) Rename(src, dst string) error {
	if !strings.Contains(src, defaults.TempDirPrefix) {
		return f.OSFileSystem.Rename(src, dst)
	}
	return f.install(dst, func() error { return f.OSFileSystem.Rename(src, dst) })
}

func (f *parentInstallFaultFS) install(dst string, operation func() error) error {
	if f.tripped && f.mode == installFailPersistent {
		return syscall.ENOSPC
	}
	if dst != f.target || f.tripped {
		return operation()
	}
	f.tripped = true
	switch f.mode {
	case installFailOnce, installFailPersistent:
		return syscall.ENOSPC
	case installExitBefore:
		os.Exit(77) // Abrupt process death: no defers or pipeline cleanup.
	}
	err := operation()
	if err == nil && f.mode == installExitAfter {
		os.Exit(77)
	}
	return err
}

func (f *parentInstallFaultFS) CopyFile(src, dst string, mode fs.FileMode) error {
	return f.install(dst, func() error { return f.OSFileSystem.CopyFile(src, dst, mode) })
}

func (f *parentInstallFaultFS) WriteFile(path string, data []byte, mode fs.FileMode) error {
	if f.tripped && f.mode == installFailPersistent {
		return syscall.ENOSPC
	}
	return f.OSFileSystem.WriteFile(path, data, mode)
}

func runParentInstallPipeline(t *testing.T, filesystem ingest.FileSystem, database *store.Store, source, output, remote, parentID string) *ingest.PipelineResult {
	t.Helper()
	cfg := ingest.PipelineConfig{OutputDir: ingest.ResolvedPath(output), Parallelism: 1, Sources: map[ingest.Harness]ingest.SourceConfig{
		ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}},
	}}
	git := testutil.DefaultGitResolver()
	if remote != "" {
		git.Remote = remote
		id, err := ingest.NewSessionID(parentID)
		if err != nil {
			t.Fatal(err)
		}
		cfg.AllowedSessionIDs = map[ingest.SessionID]bool{id: true}
	}
	pipeline, err := ingest.NewPipeline(filesystem, git, ingest.DefaultAdapterRegistry, cfg,
		ingest.WithSalt(database.InstallationSalt()), ingest.WithStore(database), ingest.WithMetricsStore(database),
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessClaudeCode: ingest.NewClaudeIndexer(filesystem)}), ingest.WithAnalyzer(metrics.NewEngine(database)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil || result.Summary.StoreError != nil {
		t.Fatalf("pipeline: %+v, %v", result, err)
	}
	return result
}

func TestParentInstallFaultProcess(t *testing.T) {
	root := os.Getenv("PEASANT_INSTALL_TEST_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	mode := parentInstallFaultMode(os.Getenv("PEASANT_INSTALL_TEST_MODE"))
	database, err := store.Open(filepath.Join(root, "peasant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	filesystem := &parentInstallFaultFS{OSFileSystem: &ingest.OSFileSystem{}, output: filepath.Join(root, "output"), target: os.Getenv("PEASANT_INSTALL_TEST_TARGET"), mode: mode}
	result := runParentInstallPipeline(t, filesystem, database, filepath.Join(root, "source"), filepath.Join(root, "output"), os.Getenv("PEASANT_INSTALL_TEST_REMOTE"), os.Getenv("PEASANT_INSTALL_TEST_PARENT"))
	if !filesystem.tripped {
		t.Fatal("installation fault was not exercised")
	}
	wantErrors := 0
	if mode == installFailOnce || mode == installFailPersistent {
		wantErrors = 1
	}
	if result.Summary.Errors != wantErrors {
		t.Fatalf("summary: %+v", result.Summary)
	}
	if wantErrors != 0 {
		var message string
		for _, session := range result.Sessions {
			if session.Error != nil {
				message = session.Error.Error()
			}
		}
		t.Logf("installation error: %s", message)
	}
}

func TestParentInstallFaultRecovery(t *testing.T) {
	var fixture parentInstallFaultFixture
	decoder := yaml.NewDecoder(bytes.NewReader(parentInstallFaultYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	parentID, err := ingest.NewSessionID(fixture.ParentID)
	if err != nil {
		t.Fatal(err)
	}
	childID, err := ingest.NewSessionID(fixture.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range fixture.Cases {
		if seen[c.Name] {
			t.Fatalf("duplicate fixture %s", c.Name)
		}
		seen[c.Name] = true
		switch c.Mode {
		case installSuccess, installFailOnce, installFailPersistent, installExitBefore, installExitAfter:
		default:
			t.Fatalf("unknown fault mode %s", c.Mode)
		}
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			source, output := filepath.Join(root, "source"), filepath.Join(root, "output")
			parentSource := filepath.Join(source, "project", fixture.ParentID+".jsonl")
			childSource := filepath.Join(source, "project", fixture.ParentID, "subagents", fixture.ChildID+".jsonl")
			write := func(path, data string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				old := time.Now().Add(-time.Hour)
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			write(parentSource, fixture.Parent)
			write(childSource, fixture.Child)
			databasePath := filepath.Join(root, "peasant.db")
			database, err := store.Open(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			filesystem := &ingest.OSFileSystem{}
			if result := runParentInstallPipeline(t, filesystem, database, source, output, "", ""); result.Summary.Errors != 0 {
				t.Fatalf("initial ingest: %+v", result.Summary)
			}
			parent, err := database.LoadPublicationInput(t.Context(), parentID)
			if err != nil || parent.Readiness != ingest.PublicationReady {
				t.Fatalf("parent: %+v, %v", parent, err)
			}
			child, err := database.LoadPublicationInput(t.Context(), childID)
			if err != nil || child.Readiness != ingest.PublicationReady {
				t.Fatalf("child: %+v, %v", child, err)
			}
			parentDir := ingest.SessionDir(output, string(parent.Metadata.HostSlug), fixture.ParentID, "")
			childDir := ingest.SessionDir(output, string(child.Metadata.HostSlug), fixture.ChildID, fixture.ParentID)
			// Include deeper opaque bytes: preservation is for the whole subtree,
			// not just recognized transcript/metadata filenames.
			write(filepath.Join(childDir, "subagents", "nested", "opaque.bin"), "\x00\xffnested child bytes")
			before := snapshotManagedTree(t, childDir)
			parentTranscript := filepath.Join(parentDir, fixture.ParentID+"--transcript.jsonl")
			parentMetadata := filepath.Join(parentDir, fixture.ParentID+"--metadata.json")
			beforeParentMetadata, err := os.ReadFile(parentMetadata)
			if err != nil {
				t.Fatal(err)
			}
			stale := filepath.Join(parentDir, "debug", "obsolete.log")
			write(stale, "obsolete parent debug file")
			write(parentSource, fixture.UpdatedParent)
			// The updated recording carries a CURRENT modification time, the way
			// a harness that just wrote to it leaves it. write() backdates every
			// file it makes so the first ingest sees settled sessions; keeping
			// that backdate on an update would describe something no harness
			// does, a file whose bytes changed while its clock went backwards,
			// and the run would settle the session from retained evidence and
			// never install the file this fault is aimed at.
			if err := os.Chtimes(parentSource, time.Now(), time.Now()); err != nil {
				t.Fatal(err)
			}
			// Force normal recovery of the parent only; the child remains DB-ready.
			if err := database.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: &parent.Metadata, Session: ingest.DiscoveredSession{SessionID: parentID, Harness: ingest.HarnessClaudeCode}}}); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			var sentinel string
			// A relocated parent has TWO possible homes while its publication is
			// mid-flight: the directory it was published in and the one its new
			// project identity names. Both are kept so the assertions below can
			// say where the transcript actually is instead of assuming the move
			// finished.
			originalParentTranscript := parentTranscript
			if c.Remote != "" {
				_, host, err := ingest.DeriveProjectIdentifiers(database.InstallationSalt(), c.Remote, "/synthetic/parent")
				if err != nil {
					t.Fatal(err)
				}
				parentDir = ingest.SessionDir(output, host.String(), fixture.ParentID, "")
				parentTranscript = filepath.Join(parentDir, fixture.ParentID+"--transcript.jsonl")
				parentMetadata = filepath.Join(parentDir, fixture.ParentID+"--metadata.json")
				sentinel = filepath.Join(parentDir, "unrelated.txt")
				write(sentinel, "retain unrelated destination member")
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestParentInstallFaultProcess$", "-test.v")
			cmd.Env = append(os.Environ(), "PEASANT_INSTALL_TEST_ROOT="+root, "PEASANT_INSTALL_TEST_MODE="+string(c.Mode), "PEASANT_INSTALL_TEST_TARGET="+parentTranscript, "PEASANT_INSTALL_TEST_REMOTE="+c.Remote, "PEASANT_INSTALL_TEST_PARENT="+fixture.ParentID)
			log, err := cmd.CombinedOutput()
			if c.Mode == installExitBefore || c.Mode == installExitAfter {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 77 {
					t.Fatalf("expected abrupt exit: %v\n%s", err, log)
				}
			} else if err != nil {
				t.Fatalf("fault ingest: %v\n%s", err, log)
			}
			if !reflect.DeepEqual(before, snapshotManagedTree(t, childDir)) {
				t.Fatal("child bytes changed during fault/cleanup")
			}
			wantParent := fixture.Parent
			if c.Mode == installSuccess || c.Mode == installExitAfter {
				wantParent = fixture.UpdatedParent
			}
			assertParentTranscriptPreserved(t, []string{parentTranscript, originalParentTranscript}, []byte(wantParent), []byte(fixture.Parent))
			if c.Mode != installSuccess {
				assertParentMetadataRecoverable(t, output, parentMetadata, beforeParentMetadata)
			}
			if c.Mode == installFailOnce || c.Mode == installFailPersistent {
				if !strings.Contains(string(log), parentDir) || !strings.Contains(string(log), "rerun ingest") {
					t.Fatalf("installation error lacks location/recovery instruction: %s", log)
				}
			}
			// A fresh process/pipeline sees leftover staging and runs normal CLEANUP.
			orphan := filepath.Join(output, defaults.TempDirPrefix+"cleanup-proof", "new-only")
			write(orphan, "disposable staging")
			database, err = store.Open(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			result := runParentInstallPipeline(t, filesystem, database, source, output, c.Remote, fixture.ParentID)
			if result.Summary.Errors != 0 || result.Summary.Unchanged < 1 {
				t.Fatalf("restart: %+v", result.Summary)
			}
			if !reflect.DeepEqual(before, snapshotManagedTree(t, childDir)) {
				t.Fatal("child bytes changed after restart cleanup")
			}
			if sentinel != "" {
				assertFileBytes(t, filesystem, sentinel, []byte("retain unrelated destination member"))
				repaired, err := database.LoadPublicationInput(t.Context(), parentID)
				if err != nil || repaired.Readiness != ingest.PublicationReady || repaired.Metadata.Git.Remote == nil || *repaired.Metadata.Git.Remote != c.Remote || repaired.Metadata.Project.Hash == parent.Metadata.Project.Hash {
					t.Fatalf("parent relocation did not recover a coherent capture: %+v, %v", repaired, err)
				}
			}
			assertFileBytes(t, filesystem, parentTranscript, []byte(fixture.UpdatedParent))
			assertPublishedPairIsCoherent(t, parentMetadata, parentTranscript, parentID, beforeParentMetadata)
			assertFileBytes(t, filesystem, parentSource, []byte(fixture.UpdatedParent))
			assertFileBytes(t, filesystem, childSource, []byte(fixture.Child))
			if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("stale parent file remains: %v", err)
			}
			entries, err := os.ReadDir(output)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), defaults.TempDirPrefix) {
					t.Fatalf("cleanup did not remove %s", entry.Name())
				}
			}
			after, err := database.LoadPublicationInput(t.Context(), childID)
			if err != nil || after.CaptureRevision != child.CaptureRevision || !reflect.DeepEqual(after.Metadata, child.Metadata) || !reflect.DeepEqual(after.Entries, child.Entries) {
				t.Fatalf("unchanged child was rewritten: %+v, %v", after, err)
			}
		})
	}
	for _, name := range []string{"successful_install_prunes_stale_parent_files", "failed_install_preserves_canonical_files_after_cleanup", "persistent_io_failure_needs_no_rollback", "interrupted_before_install_survives_restart_cleanup", "interrupted_after_install_survives_restart_cleanup", "relocated_parent_preserves_old_child_and_destination", "relocated_parent_io_failure_recovers", "relocated_parent_interruption_recovers"} {
		if !seen[name] {
			t.Fatalf("missing required fixture %s", name)
		}
	}
}

func snapshotManagedTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// managedMetadataDerivedAt is the one field of a managed metadata document that
// a reconciliation refreshes without changing anything the document SAYS about
// the session. It is a cache of when the artifact was derived, so two documents
// that differ only here describe the same session identically.
const managedMetadataDerivedAt = "derivedAt"

// artifactJournalTransactions and artifactJournalPreviousGroup name the two
// directories a publication transaction writes its previous-bytes group under:
// <transactions>/<key>/old/NNNN, the layout intentFilePath builds.
const (
	artifactJournalTransactions  = "transactions"
	artifactJournalPreviousGroup = "old"
)

// sameManagedMetadata reports whether two managed metadata documents describe
// the same thing, and how their derived-at caches compare. Every other field is
// compared byte for byte on its raw JSON, so a change to any of them is a
// difference; only the cache is allowed to move, and the caller decides which
// way it may move.
func sameManagedMetadata(t *testing.T, got, want []byte) (same bool, gotDerivedAt, wantDerivedAt float64) {
	t.Helper()
	gotFields, wantFields := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	if json.Unmarshal(got, &gotFields) != nil || json.Unmarshal(want, &wantFields) != nil {
		return false, 0, 0
	}
	read := func(fields map[string]json.RawMessage) float64 {
		var value float64
		if raw, ok := fields[managedMetadataDerivedAt]; ok {
			_ = json.Unmarshal(raw, &value)
		}
		delete(fields, managedMetadataDerivedAt)
		return value
	}
	gotDerivedAt, wantDerivedAt = read(gotFields), read(wantFields)
	return reflect.DeepEqual(gotFields, wantFields), gotDerivedAt, wantDerivedAt
}

// assertParentMetadataRecoverable pins what a faulted publication owes the user
// BEFORE anything is restarted.
//
// The final metadata install is the commit point of the publication
// transaction: the old metadata is deliberately detached first, and between
// that detach and the install the journal is the only holder of the previous
// bytes, by design. Requiring the canonical path to hold them throughout would
// pin the older whole-directory design, which no longer exists.
//
// What the user is still owed is that the previous metadata is RECOVERABLE. It
// is either back at the canonical path, because an in-process rollback already
// restored it, or held in the pending transaction journal for the recovery the
// next run performs. Whichever holds it, it is the same document as before the
// fault apart from the derived-at cache the run legitimately refreshed, whose
// value may only have moved forward; and if a canonical file exists at all it
// is never anything else, so a partially installed or foreign metadata still
// fails here.
func assertParentMetadataRecoverable(t *testing.T, output, canonicalPath string, before []byte) {
	t.Helper()
	recoverable := false
	check := func(where string, data []byte, mustMatch bool) {
		same, derivedAt, previous := sameManagedMetadata(t, data, before)
		if same && derivedAt >= previous {
			recoverable = true
			return
		}
		if mustMatch {
			t.Fatalf("the faulted publication left %s holding metadata that is not the pre-fault document (same-except-derived-at=%v, derivedAt %v against the pre-fault %v):\n%s", where, same, derivedAt, previous, data)
		}
	}
	if data, err := os.ReadFile(canonicalPath); err == nil {
		check("the canonical metadata path", data, true)
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	// A journal backup is <transactions>/<key>/old/NNNN, the layout the
	// transaction writes its previous-bytes group in. The two directory names
	// are the only structural fact this needs, so the managed state directory
	// itself is never spelled out here.
	if err := filepath.WalkDir(output, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		group := filepath.Dir(path)
		if filepath.Base(group) != artifactJournalPreviousGroup || filepath.Base(filepath.Dir(filepath.Dir(group))) != artifactJournalTransactions {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		check("the pending transaction journal", data, false)
		return nil
	}); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if !recoverable {
		t.Fatal("the pre-fault parent metadata is neither at the canonical path nor in a pending transaction journal, so a restart has nothing to recover it from")
	}
}

// assertPublishedPairIsCoherent pins what the completed restart owes the user:
// the metadata beside a transcript describes THAT transcript and names this
// session. A publication that finished with metadata pointing at other bytes
// would be one session's artifact wearing another's identity.
func assertPublishedPairIsCoherent(t *testing.T, metadataPath, transcriptPath string, sessionID ingest.SessionID, previous []byte) {
	t.Helper()
	metadataJSON, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("the completed restart left no published metadata: %v", err)
	}
	transcript, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("the completed restart left no published transcript: %v", err)
	}
	var published ingest.UnifiedMetadata
	if err := json.Unmarshal(metadataJSON, &published); err != nil {
		t.Fatalf("decode the published metadata: %v", err)
	}
	if published.SessionID != sessionID {
		t.Fatalf("the published artifact names session %s, not %s", published.SessionID, sessionID)
	}
	if published.ContentHash != schema.ComputeTranscriptHash(transcript) {
		t.Fatal("the published metadata's content hash does not match the transcript beside it")
	}
	_, derivedAt, wasDerivedAt := sameManagedMetadata(t, metadataJSON, previous)
	if derivedAt < wasDerivedAt {
		t.Fatalf("the published metadata's derived-at cache moved backwards, from %v to %v", wasDerivedAt, derivedAt)
	}
}

// assertParentTranscriptPreserved pins the parent transcript after a fault,
// across every home it can legitimately have at that moment. A relocation that
// was interrupted may have installed the file under the new project identity
// while the old directory still holds the previous bytes, and one that was
// rolled back leaves it where it was; both are the artifact being preserved,
// and neither may leave a partial or foreign file behind.
func assertParentTranscriptPreserved(t *testing.T, candidates []string, want, previous []byte) {
	t.Helper()
	found := false
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case bytes.Equal(data, want):
			found = true
		case bytes.Equal(data, previous):
			// The other home still holds the pre-fault artifact, which is what
			// preserving it means while the move is unfinished.
		default:
			t.Fatalf("the faulted publication left %s holding neither the expected transcript nor the pre-fault one:\n%s", path, data)
		}
	}
	if !found {
		t.Fatalf("the expected parent transcript is at none of %v, so the fault lost the artifact rather than preserving it", candidates)
	}
}
