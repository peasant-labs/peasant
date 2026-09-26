package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

type filesystemSafetyFixture struct {
	Session struct {
		ID      string `yaml:"id"`
		Harness string `yaml:"harness"`
	} `yaml:"session"`
	ForeignSession struct {
		ID      string `yaml:"id"`
		Harness string `yaml:"harness"`
	} `yaml:"foreign_session"`
	Generation struct {
		CompleteID string `yaml:"complete_id"`
		InactiveID string `yaml:"inactive_id"`
		ForeignID  string `yaml:"foreign_id"`
	} `yaml:"generation"`
	Layout struct {
		HoldingDir        string `yaml:"holding_dir"`
		ForeignHoldingDir string `yaml:"foreign_holding_dir"`
	} `yaml:"layout"`
	Cases []struct {
		Name     string `yaml:"name"`
		Ancestor string `yaml:"ancestor"`
	} `yaml:"cases"`
	Sentinel struct {
		OutsideFile     string `yaml:"outside_file"`
		UnrelatedFile   string `yaml:"unrelated_file"`
		CandidateFile   string `yaml:"candidate_file"`
		PrivateIdentity string `yaml:"private_identity"`
	} `yaml:"sentinel"`
}

func loadFilesystemSafetyFixture(t *testing.T) filesystemSafetyFixture {
	t.Helper()
	var fixture filesystemSafetyFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(generationFilesystemSafetyYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode generation_filesystem_safety.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(generationFilesystemSafetyManifestYAML)
	if err != nil {
		t.Fatalf("decode generation_filesystem_safety manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "generation filesystem safety"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestGenerationFilesystemSafety proves a symlink at any ancestor inside the
// owned root cannot redirect recursive removal outside the root, that
// unrelated files survive cleanup, and that no diagnostic exposes the private
// root path.
func TestGenerationFilesystemSafety(t *testing.T) {
	fixture := loadFilesystemSafetyFixture(t)
	id, err := schema.NewSessionID(fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			s, root := openGenerationStore(t)
			// Sentinel root carries a distinctive private segment; every
			// formatted diagnostic must omit it.
			sentinelRoot := root + "-sentinel-private-9f3a"
			if err := os.Rename(root, sentinelRoot); err != nil {
				t.Fatalf("rename root for sentinel: %v", err)
			}
			root = sentinelRoot
			// Reopen the artifact store and locker against the sentinel root
			// so all operations run under the private path.
			artifacts, err := NewOSGenerationArtifactStore(root)
			if err != nil {
				t.Fatal(err)
			}
			locker, err := NewFileSessionLocker(root)
			if err != nil {
				t.Fatal(err)
			}
			s.generationArtifacts = artifacts
			s.sessionLocker = locker

			seedGenerationSession(t, s, fixture.Session.ID)
			complete, completeBlobs := buildTestGeneration(t, id, fixture.Generation.CompleteID, "fs text G1", "fs input G1", "fs output G1")
			if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: complete, Blobs: completeBlobs}); err != nil {
				t.Fatalf("activate G1: %v", err)
			}
			old, oldBlobs := buildTestGeneration(t, id, fixture.Generation.InactiveID, "fs text old", "fs input old", "fs output old")
			if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: old, Blobs: oldBlobs}); err != nil {
				// A complete candidate must activate; a failure here is a
				// production regression, never a reason to stage directly.
				t.Fatalf("activate inactive candidate G2: %v", err)
			}
			// Re-activate the complete generation so the inactive target is
			// not active during the symlink test.
			if got := visibleGeneration(t, s, id); got != fixture.Generation.CompleteID && got != fixture.Generation.InactiveID {
				t.Fatalf("unexpected visible %q", got)
			}
			// Ensure the complete generation is active: if the inactive won,
			// reactivate the complete one.
			if got := visibleGeneration(t, s, id); got == fixture.Generation.InactiveID {
				if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: complete, Blobs: completeBlobs}); err != nil {
					t.Fatalf("reactivate G1: %v", err)
				}
			}

			outsideDir := t.TempDir()
			sentinelOutside := filepath.Join(outsideDir, fixture.Sentinel.OutsideFile)
			if err := os.WriteFile(sentinelOutside, []byte("external sentinel must survive"), 0o600); err != nil {
				t.Fatal(err)
			}
			unrelatedPath := filepath.Join(root, fixture.Session.ID, "generations", fixture.Sentinel.UnrelatedFile)
			if err := os.MkdirAll(filepath.Dir(unrelatedPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(unrelatedPath, []byte("unrelated content must survive"), 0o600); err != nil {
				t.Fatal(err)
			}

			switch tc.Ancestor {
			case "session":
				// Replace <root>/<session> with a symlink to the outside
				// directory, then prove removal of the inactive generation
				// is refused and the external sentinel survives.
				sessionPath := filepath.Join(root, fixture.Session.ID)
				// Move the real session aside through the owned root? Use a
				// host rename: the test owns both directories.
				backup := sessionPath + ".backup"
				if err := os.Rename(sessionPath, backup); err != nil {
					t.Fatalf("backup session dir: %v", err)
				}
				if err := os.Symlink(outsideDir, sessionPath); err != nil {
					_ = os.Rename(backup, sessionPath)
					t.Fatalf("plant session symlink: %v", err)
				}
				err := s.CleanupInactiveGeneration(context.Background(), id, fixture.Generation.InactiveID)
				_ = os.Remove(sessionPath)
				_ = os.Rename(backup, sessionPath)
				if err == nil {
					t.Fatal("cleanup through a session symlink succeeded; it must be refused")
				}
				assertSentinelAbsent(t, err, root)
				if _, statErr := os.Stat(sentinelOutside); statErr != nil {
					t.Fatalf("external sentinel missing after refused symlink cleanup: %v", statErr)
				}
			case "generations":
				generationsPath := filepath.Join(root, fixture.Session.ID, "generations")
				backup := generationsPath + ".backup"
				if err := os.Rename(generationsPath, backup); err != nil {
					t.Fatalf("backup generations dir: %v", err)
				}
				if err := os.Symlink(outsideDir, generationsPath); err != nil {
					_ = os.Rename(backup, generationsPath)
					t.Fatalf("plant generations symlink: %v", err)
				}
				err := s.CleanupInactiveGeneration(context.Background(), id, fixture.Generation.InactiveID)
				_ = os.Remove(generationsPath)
				_ = os.Rename(backup, generationsPath)
				if err == nil {
					t.Fatal("cleanup through a generations symlink succeeded; it must be refused")
				}
				assertSentinelAbsent(t, err, root)
				if _, statErr := os.Stat(sentinelOutside); statErr != nil {
					t.Fatalf("external sentinel missing after refused generations-symlink cleanup: %v", statErr)
				}
			case "unrelated":
				if err := s.CleanupInactiveGeneration(context.Background(), id, fixture.Generation.InactiveID); err != nil {
					t.Fatalf("cleanup inactive: %v", err)
				}
				if _, statErr := os.Stat(unrelatedPath); statErr != nil {
					t.Fatalf("unrelated file missing after cleanup: %v", statErr)
				}
				if _, statErr := os.Stat(sentinelOutside); statErr != nil {
					t.Fatalf("external sentinel missing after unrelated cleanup: %v", statErr)
				}
			case "locks":
				// A symlink at the lock namespace must not divert locking
				// outside the root; the lock still serializes.
				locksPath := filepath.Join(root, "locks")
				backup := locksPath + ".backup"
				if err := os.Rename(locksPath, backup); err != nil {
					t.Fatalf("backup locks dir: %v", err)
				}
				if err := os.Symlink(outsideDir, locksPath); err != nil {
					_ = os.Rename(backup, locksPath)
					t.Fatalf("plant locks symlink: %v", err)
				}
				_, lockErr := s.sessionLocker.LockExclusive(context.Background(), id)
				_ = os.Remove(locksPath)
				_ = os.Rename(backup, locksPath)
				if lockErr != nil {
					assertSentinelAbsent(t, lockErr, root)
					// A lock refusal through a compromised namespace is
					// acceptable as long as it fails closed without exposing
					// the private path; reacquire after repair.
					release, reacquireErr := s.sessionLocker.LockExclusive(context.Background(), id)
					if reacquireErr != nil {
						t.Fatalf("reacquire after locks repair: %v", reacquireErr)
					}
					_ = release()
				} else {
					// If the lock succeeded despite the symlink, it must have
					// stayed confined (os.Root refused the escape and recreated
					// the namespace); verify the outside dir gained no lock.
					entries, _ := os.ReadDir(outsideDir)
					for _, entry := range entries {
						if strings.HasSuffix(entry.Name(), ".lock") {
							t.Fatalf("lock file %q escaped the owned root", entry.Name())
						}
					}
				}
				if _, statErr := os.Stat(sentinelOutside); statErr != nil {
					t.Fatalf("external sentinel missing after locks test: %v", statErr)
				}
			case "empty-manifest", "malformed-manifest":
				// A directory that is not an owned generation must survive
				// cleanup even when its manifest merely decodes ({}), and a
				// malformed manifest must be refused rather than removed.
				candidateID := "gen_fs_unowned"
				manifest := "{}"
				if tc.Ancestor == "malformed-manifest" {
					manifest = "{ this is not a manifest"
				}
				candidateDir := filepath.Join(root, fixture.Session.ID, "generations", candidateID)
				if err := os.MkdirAll(candidateDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(candidateDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
					t.Fatal(err)
				}
				candidateSentinel := filepath.Join(candidateDir, fixture.Sentinel.CandidateFile)
				if err := os.WriteFile(candidateSentinel, []byte("unowned candidate must survive"), 0o600); err != nil {
					t.Fatal(err)
				}
				err := s.CleanupInactiveGeneration(context.Background(), id, candidateID)
				if err == nil {
					t.Fatal("cleanup removed a directory that is not an owned generation; it must be refused")
				}
				assertSentinelAbsent(t, err, root)
				if _, statErr := os.Stat(candidateSentinel); statErr != nil {
					t.Fatalf("unowned candidate sentinel missing after refused cleanup: %v", statErr)
				}
			case "unknown-manifest":
				// A directory with no manifest cannot prove ownership; cleanup
				// must leave it and its sentinel untouched.
				candidateID := "gen_fs_unknown"
				candidateDir := filepath.Join(root, fixture.Session.ID, "generations", candidateID)
				if err := os.MkdirAll(candidateDir, 0o700); err != nil {
					t.Fatal(err)
				}
				candidateSentinel := filepath.Join(candidateDir, fixture.Sentinel.CandidateFile)
				if err := os.WriteFile(candidateSentinel, []byte("unknown candidate must survive"), 0o600); err != nil {
					t.Fatal(err)
				}
				err := s.CleanupInactiveGeneration(context.Background(), id, candidateID)
				if err == nil {
					t.Fatal("cleanup removed a directory with no manifest; it must be refused")
				}
				assertSentinelAbsent(t, err, root)
				if _, statErr := os.Stat(candidateSentinel); statErr != nil {
					t.Fatalf("unknown candidate sentinel missing after refused cleanup: %v", statErr)
				}
			case "mismatched-manifest":
				// A decodable manifest that names another session or generation
				// is not this owner's candidate and must be left in place.
				foreign := stageForeignGeneration(t, s, fixture.ForeignSession.ID, fixture.Generation.ForeignID)
				mismatchedID := "gen_fs_mismatch"
				candidateDir := filepath.Join(root, fixture.Session.ID, "generations", mismatchedID)
				if err := os.MkdirAll(candidateDir, 0o700); err != nil {
					t.Fatal(err)
				}
				foreignManifest, err := jsonMarshalForTest(foreign)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(candidateDir, "manifest.json"), foreignManifest, 0o600); err != nil {
					t.Fatal(err)
				}
				candidateSentinel := filepath.Join(candidateDir, fixture.Sentinel.CandidateFile)
				if err := os.WriteFile(candidateSentinel, []byte("mismatched candidate must survive"), 0o600); err != nil {
					t.Fatal(err)
				}
				err = s.CleanupInactiveGeneration(context.Background(), id, mismatchedID)
				if err == nil {
					t.Fatal("cleanup removed a mismatched-manifest candidate; it must be refused")
				}
				assertSentinelAbsent(t, err, root)
				if _, statErr := os.Stat(candidateSentinel); statErr != nil {
					t.Fatalf("mismatched candidate sentinel missing after refused cleanup: %v", statErr)
				}
			case "foreign-owner-symlink":
				// An in-root symlink ancestor resolves inside the owned root but
				// points at another session's generation. os.Root confinement
				// allows that resolution, so ownership must be proven from the
				// manifest identity before any recursive removal.
				foreign := stageForeignGeneration(t, s, fixture.ForeignSession.ID, fixture.Generation.ForeignID)
				foreignSentinel := filepath.Join(root, fixture.ForeignSession.ID, "generations", fixture.Generation.ForeignID, fixture.Sentinel.CandidateFile)
				if err := os.WriteFile(foreignSentinel, []byte("foreign generation must survive"), 0o600); err != nil {
					t.Fatal(err)
				}
				_ = foreign
				linkPath := filepath.Join(root, fixture.Session.ID, "generations", fixture.Generation.ForeignID)
				if err := os.Symlink(filepath.Join(root, fixture.ForeignSession.ID, "generations", fixture.Generation.ForeignID), linkPath); err != nil {
					t.Fatal(err)
				}
				err := s.CleanupInactiveGeneration(context.Background(), id, fixture.Generation.ForeignID)
				_ = os.Remove(linkPath)
				if err == nil {
					t.Fatal("cleanup followed an in-root foreign-owner symlink; it must be refused")
				}
				assertSentinelAbsent(t, err, root)
				if _, statErr := os.Stat(foreignSentinel); statErr != nil {
					t.Fatalf("foreign generation sentinel missing after refused symlink cleanup: %v", statErr)
				}
			case "relative-ancestor-matching-owner":
				// Move the real generations directory to an in-root sibling and
				// replace the generations component with a RELATIVE symlink that
				// stays inside the root. os.Root follows the link, and the moved
				// manifest still names this exact owner, so only the anchored
				// no-follow component walk can refuse the recursive removal.
				generationsPath := filepath.Join(root, fixture.Session.ID, "generations")
				holdingPath := filepath.Join(root, fixture.Layout.HoldingDir)
				if err := os.Rename(generationsPath, holdingPath); err != nil {
					t.Fatalf("relocate generations directory: %v", err)
				}
				sentinel := filepath.Join(holdingPath, fixture.Generation.InactiveID, fixture.Sentinel.CandidateFile)
				if err := os.WriteFile(sentinel, []byte("relocated generation must survive"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../"+fixture.Layout.HoldingDir, generationsPath); err != nil {
					t.Fatalf("plant relative generations symlink: %v", err)
				}
				assertRemovalBoundariesRefused(t, s, id, fixture.Generation.InactiveID, sentinel, "", root)
			case "relative-ancestor-foreign-owner":
				// The generations component is a relative in-root symlink to a
				// sibling that holds a foreign session's generation under the
				// same identifier. Confinement follows the link; the removal
				// walk must refuse the symlinked component on both boundaries.
				generationsPath := filepath.Join(root, fixture.Session.ID, "generations")
				if err := os.Rename(generationsPath, generationsPath+".backup"); err != nil {
					t.Fatalf("set aside generations directory: %v", err)
				}
				foreignDir := filepath.Join(root, fixture.Layout.ForeignHoldingDir, fixture.Generation.ForeignID)
				if err := os.MkdirAll(foreignDir, 0o700); err != nil {
					t.Fatal(err)
				}
				foreign := stageForeignGeneration(t, s, fixture.ForeignSession.ID, fixture.Generation.ForeignID)
				foreignManifest, err := jsonMarshalForTest(foreign)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(foreignDir, "manifest.json"), foreignManifest, 0o600); err != nil {
					t.Fatal(err)
				}
				sentinel := filepath.Join(foreignDir, fixture.Sentinel.CandidateFile)
				if err := os.WriteFile(sentinel, []byte("foreign generation must survive"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../"+fixture.Layout.ForeignHoldingDir, generationsPath); err != nil {
					t.Fatalf("plant relative foreign symlink: %v", err)
				}
				assertRemovalBoundariesRefused(t, s, id, fixture.Generation.ForeignID, sentinel, "", root)
			case "private-identity-manifest":
				// A manifest whose identity fields carry a private path must be
				// refused without echoing that path into the diagnostic, and the
				// candidate and its sentinel must survive both removal boundaries.
				candidateID := "gen_fs_private_identity"
				candidateDir := filepath.Join(root, fixture.Session.ID, "generations", candidateID)
				if err := os.MkdirAll(candidateDir, 0o700); err != nil {
					t.Fatal(err)
				}
				manifest, err := json.Marshal(indexformat.Generation{
					ID:       fixture.Sentinel.PrivateIdentity,
					Metadata: schema.UnifiedMetadata{SessionID: schema.SessionID(fixture.Sentinel.PrivateIdentity)},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(candidateDir, "manifest.json"), manifest, 0o600); err != nil {
					t.Fatal(err)
				}
				sentinel := filepath.Join(candidateDir, fixture.Sentinel.CandidateFile)
				if err := os.WriteFile(sentinel, []byte("private identity candidate must survive"), 0o600); err != nil {
					t.Fatal(err)
				}
				assertRemovalBoundariesRefused(t, s, id, candidateID, sentinel, fixture.Sentinel.PrivateIdentity, root)
			case "private-content-path-manifest":
				// A manifest with a matching identity but a private path in its
				// validation detail must also be refused without echoing the
				// path-bearing validator text.
				candidateID := "gen_fs_private_content"
				installed, err := s.generationArtifacts.ReadManifest(context.Background(), id, fixture.Generation.CompleteID)
				if err != nil {
					t.Fatalf("read installed manifest: %v", err)
				}
				installed.ID = candidateID
				installed.TitleRefs = []schema.SourceEntryRef{schema.SourceEntryRef(fixture.Sentinel.PrivateIdentity)}
				manifest, err := jsonMarshalForTest(installed)
				if err != nil {
					t.Fatal(err)
				}
				candidateDir := filepath.Join(root, fixture.Session.ID, "generations", candidateID)
				if err := os.MkdirAll(candidateDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(candidateDir, "manifest.json"), manifest, 0o600); err != nil {
					t.Fatal(err)
				}
				sentinel := filepath.Join(candidateDir, fixture.Sentinel.CandidateFile)
				if err := os.WriteFile(sentinel, []byte("private content-path candidate must survive"), 0o600); err != nil {
					t.Fatal(err)
				}
				assertRemovalBoundariesRefused(t, s, id, candidateID, sentinel, fixture.Sentinel.PrivateIdentity, root)
			default:
				t.Fatalf("unknown filesystem safety scenario %q; add it to the fixture, the required-names manifest and this runner", tc.Ancestor)
			}
		})
	}
}

func assertSentinelAbsent(t *testing.T, err error, sentinelRoot string) {
	t.Helper()
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), sentinelRoot) {
		t.Fatalf("diagnostic exposes the private root path: %v", err)
	}
	// os.Root sanitization must never leak PathError/LinkError strings with
	// embedded paths.
	if strings.Contains(err.Error(), "open "+sentinelRoot) || strings.Contains(err.Error(), "stat "+sentinelRoot) {
		t.Fatalf("diagnostic leaks a path-bearing OS error: %v", err)
	}
}

// assertRemovalBoundariesRefused proves both destructive boundaries
// (Store.CleanupInactiveGeneration and the artifact store's direct
// RemoveGeneration) refuse the requested generation, that the sentinel survives
// both refusals, and that neither diagnostic echoes the private root or a
// private identity.
func assertRemovalBoundariesRefused(t *testing.T, s *Store, id schema.SessionID, generationID, sentinelPath, privateIdentity, root string) {
	t.Helper()
	cleanupErr := s.CleanupInactiveGeneration(context.Background(), id, generationID)
	if cleanupErr == nil {
		t.Fatalf("cleanup of generation %s succeeded; the destructive boundary must refuse it", generationID)
	}
	assertRefusalDiagnostic(t, cleanupErr, sentinelPath, privateIdentity, root, "cleanup")
	removeErr := s.generationArtifacts.RemoveGeneration(context.Background(), id, generationID)
	if removeErr == nil {
		t.Fatalf("direct removal of generation %s succeeded; the destructive boundary must refuse it", generationID)
	}
	assertRefusalDiagnostic(t, removeErr, sentinelPath, privateIdentity, root, "removal")
}

func assertRefusalDiagnostic(t *testing.T, err error, sentinelPath, privateIdentity, root, boundary string) {
	t.Helper()
	assertSentinelAbsent(t, err, root)
	if privateIdentity != "" && strings.Contains(err.Error(), privateIdentity) {
		t.Fatalf("%s diagnostic echoes the private identity: %v", boundary, err)
	}
	if _, statErr := os.Stat(sentinelPath); statErr != nil {
		t.Fatalf("%s sentinel %s missing after refused %s: %v", boundary, sentinelPath, boundary, statErr)
	}
}

// stageForeignGeneration stages one generation owned by another session so an
// ownership test can point a candidate at, or copy bytes from, a real
// foreign-owned generation.
func stageForeignGeneration(t *testing.T, s *Store, foreignSessionID, generationID string) indexformat.Generation {
	t.Helper()
	foreign, err := schema.NewSessionID(foreignSessionID)
	if err != nil {
		t.Fatal(err)
	}
	v2, blobs := buildTestGeneration(t, foreign, generationID, "foreign text", "foreign input", "foreign output")
	staged, err := s.generationArtifacts.Stage(context.Background(), v2.Generation, blobs)
	if err != nil {
		t.Fatalf("stage foreign generation: %v", err)
	}
	return staged
}

func jsonMarshalForTest(g indexformat.Generation) ([]byte, error) {
	return json.Marshal(g)
}

// TestGenerationDiagnosticsSanitized proves lock, stage, read, repair and
// cleanup failures omit the private root path across failure seams.
func TestGenerationDiagnosticsSanitized(t *testing.T) {
	dir := t.TempDir()
	sentinelSegment := "sentinel-private-7c2e"
	root := filepath.Join(dir, sentinelSegment)
	artifacts, err := NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileSessionLocker(""); err == nil {
		t.Fatal("empty lock root was accepted; it must be refused")
	} else {
		assertSentinelAbsent(t, err, sentinelSegment)
	}
	locker, err := NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "sanitized.db"), WithPoolSize(1), WithIndexFormats(generationIndexFormat{}), WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Invalid session identifier at the lock boundary.
	badID := schema.SessionID("../escape")
	if _, err := locker.LockExclusive(context.Background(), badID); err == nil {
		t.Fatal("unconfined lock succeeded; it must be refused")
	} else {
		assertSentinelAbsent(t, err, sentinelSegment)
	}
	// Invalid generation identifier at staging and removal.
	sid, _ := schema.NewSessionID("99999999-9999-4999-8999-999999999999")
	v2, blobs := buildTestGeneration(t, sid, "gen_ok", "text", "input", "output")
	v2.Generation.ID = "."
	if _, err := artifacts.Stage(context.Background(), v2.Generation, blobs); err == nil {
		t.Fatal("dot staging succeeded; it must be refused")
	} else {
		assertSentinelAbsent(t, err, sentinelSegment)
	}
	if err := artifacts.RemoveGeneration(context.Background(), sid, "."); err == nil {
		t.Fatal("dot removal succeeded; it must be refused")
	} else {
		assertSentinelAbsent(t, err, sentinelSegment)
	}

	// Real path-bearing OS read failure: a directory where the manifest file is
	// expected forces a non-ENOENT read error whose path-bearing OS error must
	// be sanitized and rendered actionable.
	validSID, err := schema.NewSessionID("99999999-9999-4999-8999-999999999999")
	if err != nil {
		t.Fatal(err)
	}
	manifestDir := filepath.Join(root, string(validSID), "generations", "gen_read_seam", "manifest.json")
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := artifacts.ReadManifest(context.Background(), validSID, "gen_read_seam"); err == nil {
		t.Fatal("reading a manifest that is a directory succeeded; it must fail")
	} else {
		assertSentinelAbsent(t, err, sentinelSegment)
		if !strings.Contains(err.Error(), "read staged manifest") || !strings.Contains(err.Error(), "cannot be recovered") {
			t.Fatalf("read failure diagnostic is not actionable: %v", err)
		}
	}

	// Real path-bearing OS repair failure: a directory where metadata.json is
	// expected makes the atomic rename fail; the diagnostic must stay sanitized
	// and name the caller effect and recovery.
	metadataDir := filepath.Join(root, string(validSID), "metadata.json")
	if err := os.MkdirAll(metadataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := artifacts.RepairMetadata(context.Background(), validSID, []byte("{}")); err == nil {
		t.Fatal("repairing metadata onto a directory succeeded; it must fail")
	} else {
		assertSentinelAbsent(t, err, sentinelSegment)
		if !strings.Contains(err.Error(), "repair the exported metadata") || !strings.Contains(err.Error(), "retry") {
			t.Fatalf("repair failure diagnostic is not actionable: %v", err)
		}
	}
}
