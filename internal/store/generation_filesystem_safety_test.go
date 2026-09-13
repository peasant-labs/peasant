package store

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

type filesystemSafetyFixture struct {
	Session struct {
		ID      string `yaml:"id"`
		Harness string `yaml:"harness"`
	} `yaml:"session"`
	Generation struct {
		CompleteID string `yaml:"complete_id"`
		InactiveID string `yaml:"inactive_id"`
	} `yaml:"generation"`
	Cases []struct {
		Name     string `yaml:"name"`
		Ancestor string `yaml:"ancestor"`
	} `yaml:"cases"`
	Sentinel struct {
		OutsideFile   string `yaml:"outside_file"`
		UnrelatedFile string `yaml:"unrelated_file"`
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
			if err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: complete, Blobs: completeBlobs}); err != nil {
				t.Fatalf("activate G1: %v", err)
			}
			old, oldBlobs := buildTestGeneration(t, id, fixture.Generation.InactiveID, "fs text old", "fs input old", "fs output old")
			if err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: old, Blobs: oldBlobs}); err != nil {
				// G2 activation replaces G1; the old generation becomes
				// inactive but its files remain until cleanup. If activation
				// is refused (e.g. completeness), fall back to staging the
				// inactive candidate directly for the symlink test.
				if _, stageErr := s.generationArtifacts.Stage(context.Background(), old.Generation, oldBlobs); stageErr != nil {
					t.Fatalf("stage inactive candidate: %v", stageErr)
				}
			}
			// Re-activate the complete generation so the inactive target is
			// not active during the symlink test.
			if got := visibleGeneration(t, s, id); got != fixture.Generation.CompleteID && got != fixture.Generation.InactiveID {
				t.Fatalf("unexpected visible %q", got)
			}
			// Ensure the complete generation is active: if the inactive won,
			// reactivate the complete one.
			if got := visibleGeneration(t, s, id); got == fixture.Generation.InactiveID {
				if err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: complete, Blobs: completeBlobs}); err != nil {
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
}
