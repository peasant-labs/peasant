package ingest_test

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/artifact_root.yaml
var artifactRootYAML []byte

func TestArtifactRootConfinesWritesAndLocks(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		File          string   `yaml:"file"`
		Original      string   `yaml:"original"`
		Replacement   string   `yaml:"replacement"`
		LockFile      string   `yaml:"lockFile"`
		Cases         []struct {
			Name string `yaml:"name"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(artifactRootYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("artifact root fixtures require one YAML document")
	}
	required := []string{"create-no-overwrite", "escaping-symlink-refused", "readonly-missing-lock", "cancelled-contender", "process-death-releases", "root-alias-shares-lock", "capture-callback-retains-ownership"}
	if !reflect.DeepEqual(fixture.RequiredNames, required) {
		t.Fatal("artifact root required-name manifest changed")
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid artifact root fixture %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			filesystem, ok := any(&ingest.OSFileSystem{}).(ingest.DurableFileSystem)
			if !ok {
				t.Fatal("production filesystem has no root-confined durable publication boundary")
			}
			directory := t.TempDir()
			root, err := filesystem.OpenArtifactRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			switch row.Name {
			case "create-no-overwrite":
				if err := root.CreateFile(fixture.File, []byte(fixture.Original), 0600); err != nil {
					t.Fatal(err)
				}
				if err := root.SyncFile(fixture.File); err != nil {
					t.Fatal(err)
				}
				if err := root.SyncDir("."); err != nil {
					t.Fatal(err)
				}
				if err := root.CreateFile(fixture.File, []byte(fixture.Replacement), 0600); !errors.Is(err, os.ErrExist) {
					t.Fatalf("candidate overwrote an existing owned file: %v", err)
				}
				data, err := root.ReadFile(fixture.File)
				if err != nil || string(data) != fixture.Original {
					t.Fatalf("previous bytes changed: %q %v", data, err)
				}
			case "escaping-symlink-refused":
				outside := t.TempDir()
				if err := os.Symlink(outside, filepath.Join(directory, "escape")); err != nil {
					t.Fatal(err)
				}
				if err := root.CreateFile(filepath.Join("escape", fixture.File), []byte(fixture.Replacement), 0600); err == nil {
					t.Fatal("publication escaped through a symlink")
				}
				if _, err := os.Stat(filepath.Join(outside, fixture.File)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("outside path changed: %v", err)
				}
			case "readonly-missing-lock":
				if lock, err := root.Lock(t.Context(), fixture.LockFile, ingest.ArtifactLockRead, false); err == nil {
					lock.Close()
					t.Fatal("read-only capture created an absent lock")
				}
				if _, err := os.Stat(filepath.Join(directory, fixture.LockFile)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("read-only lock lookup mutated the managed tree")
				}
			case "cancelled-contender":
				owner, err := root.Lock(t.Context(), fixture.LockFile, ingest.ArtifactLockWrite, true)
				if err != nil {
					t.Fatal(err)
				}
				defer owner.Close()
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if contender, err := root.Lock(ctx, fixture.LockFile, ingest.ArtifactLockWrite, false); !errors.Is(err, context.Canceled) {
					if contender != nil {
						contender.Close()
					}
					t.Fatalf("contender did not respect cancellation: %v", err)
				}
				if err := owner.Close(); err != nil {
					t.Fatal(err)
				}
				reader, err := root.Lock(t.Context(), fixture.LockFile, ingest.ArtifactLockRead, false)
				if err != nil {
					t.Fatal(err)
				}
				if err := reader.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(directory, fixture.LockFile)); err != nil {
					t.Fatal("releasing a lock removed its coordination inode")
				}
			case "process-death-releases", "root-alias-shares-lock":
				childRoot := directory
				if row.Name == "root-alias-shares-lock" {
					childRoot = filepath.Join(t.TempDir(), "output-alias")
					if err := os.Symlink(directory, childRoot); err != nil {
						t.Fatal(err)
					}
				}
				checkArtifactProcessOwnership(t, root, childRoot, fixture.LockFile)
			case "capture-callback-retains-ownership":
				checkArtifactCaptureOwnership(t, directory)
			default:
				t.Fatalf("unhandled root fixture %q", row.Name)
			}
		})
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing artifact root fixture %q", name)
		}
	}
}

func checkArtifactCaptureOwnership(t *testing.T, output string) {
	t.Helper()
	fixture := loadArtifactPublicationFixtures(t)
	publisher, err := ingest.NewArtifactPublisher(&ingest.OSFileSystem{}, output, ingest.ArtifactPublisherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	artifact := publicationTestArtifact(t, fixture.OldTranscript)
	session := ingest.DiscoveredSession{SessionID: artifact.Metadata.SessionID, Harness: artifact.Metadata.ModelHarness}
	observation, err := publisher.Observe(t.Context(), session, "")
	if err != nil {
		t.Fatal(err)
	}
	committed, err := publisher.Publish(t.Context(), ingest.ArtifactPublication{Artifact: artifact, Observation: observation})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Reconcile(t.Context(), committed); err != nil {
		t.Fatal(err)
	}
	path := ingest.SessionMetadataPath(output, string(artifact.Metadata.HostSlug), string(session.SessionID), "")
	consumerFailure := errors.New("synthetic snapshot consumer failure")
	err = publisher.WithCapture(t.Context(), session.SessionID, path, func(captured *ingest.ManagedArtifact) error {
		if captured.ArtifactHash != artifact.ArtifactHash {
			t.Fatal("capture substituted another generation")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		if _, err := publisher.Observe(ctx, session, path); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("publisher bypassed held snapshot ownership: %v", err)
		}
		return consumerFailure
	})
	if !errors.Is(err, consumerFailure) {
		t.Fatalf("snapshot consumer failure was lost: %v", err)
	}
	if _, err := publisher.Observe(t.Context(), session, path); err != nil {
		t.Fatalf("snapshot ownership leaked after callback failure: %v", err)
	}
}

const artifactLockHelperRootEnv = "PEASANT_ARTIFACT_TEST_LOCK_ROOT"
const artifactLockHelperFileEnv = "PEASANT_ARTIFACT_TEST_LOCK_FILE"

func TestArtifactLockProcessHelper(t *testing.T) {
	directory := os.Getenv(artifactLockHelperRootEnv)
	if directory == "" {
		return
	}
	root, err := (&ingest.OSFileSystem{}).OpenArtifactRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := root.Lock(t.Context(), os.Getenv(artifactLockHelperFileEnv), ingest.ArtifactLockWrite, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	fmt.Fprintln(os.Stdout, "artifact-lock-held")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func checkArtifactProcessOwnership(t *testing.T, root ingest.ArtifactRoot, childRoot, lockPath string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), executable, "-test.run=^TestArtifactLockProcessHelper$")
	child.Env = append(os.Environ(), artifactLockHelperRootEnv+"="+childRoot, artifactLockHelperFileEnv+"="+lockPath)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stderr bytes.Buffer
	child.Stderr = &stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "artifact-lock-held\n" {
			t.Fatalf("child did not acquire publication ownership: %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child lock-readiness barrier did not arrive")
	}
	// The child has crossed the explicit ownership barrier and remains blocked
	// on stdin. A reader must time out, not observe an unlocked second inode.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	contender, err := root.Lock(ctx, lockPath, ingest.ArtifactLockRead, false)
	cancel()
	if contender != nil {
		_ = contender.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reader bypassed live process ownership: %v", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("terminated lock holder unexpectedly exited successfully")
	}
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reader, err := root.Lock(ctx, lockPath, ingest.ArtifactLockRead, false)
	if err != nil {
		t.Fatalf("process death did not release OS ownership: %v; child stderr=%s", err, &stderr)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}
