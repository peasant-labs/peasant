package main

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/filelock"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/maintenance_run_lock.yaml
var maintenanceRunLockYAML []byte

// maintenanceRunLockSeed names the store a maintenance run-lock case starts
// from.
type maintenanceRunLockSeed string

const (
	maintenanceRunLockSeedGolden         maintenanceRunLockSeed = "golden"
	maintenanceRunLockSeedPruneSessions  maintenanceRunLockSeed = "prune-sessions"
	maintenanceRunLockSeedMigrateSession maintenanceRunLockSeed = "migrate-session"
)

type maintenanceRunLockFixture struct {
	Name         string                 `yaml:"name"`
	Seed         maintenanceRunLockSeed `yaml:"seed"`
	Args         []string               `yaml:"args"`
	WaitingLabel string                 `yaml:"waiting_label"`
	DoneStdout   string                 `yaml:"done_stdout"`
}

func loadMaintenanceRunLockFixtures(t testing.TB) []maintenanceRunLockFixture {
	t.Helper()
	var document struct {
		RequiredNames []string                    `yaml:"required_names"`
		Cases         []maintenanceRunLockFixture `yaml:"cases"`
	}
	if err := yaml.Unmarshal(maintenanceRunLockYAML, &document); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range document.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate maintenance run-lock fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("maintenance run lock", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

// noticeWriter is a goroutine-safe buffer that closes seen once its contents
// contain notice: the wake source for "the command is waiting on the lock".
type noticeWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	notice string
	seen   chan struct{}
	once   sync.Once
}

func (w *noticeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if w.notice != "" && strings.Contains(w.buf.String(), w.notice) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func (w *noticeWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func seedMaintenanceRunLockStore(t *testing.T, seed maintenanceRunLockSeed, dir, ownedRoot string) {
	t.Helper()
	switch seed {
	case maintenanceRunLockSeedGolden:
		if err := os.MkdirAll(string(defaults.ResolveDataDirPathWith(dir)), defaults.PrivateDirPerm); err != nil {
			t.Fatalf("seed: create data directory: %v", err)
		}
		storetest.CopyGoldenTo(t, string(defaults.ResolveDBFilePathWith(dir)))
	case maintenanceRunLockSeedPruneSessions:
		seedPruneTestSessions(t, dir)
	case maintenanceRunLockSeedMigrateSession:
		seedMigrateCmdStore(t, dir, ownedRoot)
	default:
		t.Fatalf("unknown maintenance run-lock seed %q", seed)
	}
}

// TestMaintenanceWritersWaitForTheHarvestRunLock proves migrate, reclaim,
// prune and verify --content --repair do not write the data dir while a
// harvest holds its run lock: each says on stderr that it is waiting, has not
// reported its write, and finishes its write once the lock is released.
func TestMaintenanceWritersWaitForTheHarvestRunLock(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadMaintenanceRunLockFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ownedRoot := filepath.Join(dir, "sync")
			seedMaintenanceRunLockStore(t, fixture.Seed, dir, ownedRoot)
			configPath := filepath.Join(dir, "run-lock-config.yaml")
			if err := os.WriteFile(configPath, []byte("version: 1\noutput:\n  basePath: "+ownedRoot+"\n"), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}

			lockPath := filepath.Join(string(defaults.ResolveDataDirPathWith(dir)), ingest.RunLockFileName)
			release, err := filelock.Acquire(lockPath, time.Now().Add(10*time.Second))
			if err != nil {
				t.Fatalf("hold the harvest run lock: %v", err)
			}
			released := false
			releaseLock := func() {
				if !released {
					released = true
					_ = release()
				}
			}
			t.Cleanup(releaseLock)

			root := newTestRoot()
			root.AddCommand(BuildMigrateCommand(), BuildReclaimCommand(), BuildPruneCommand(), BuildHarvestCommand())
			stdout := &noticeWriter{}
			stderr := &noticeWriter{notice: fixture.WaitingLabel + ": " + ingest.RunLockWaitNotice, seen: make(chan struct{})}
			root.SetOut(stdout)
			root.SetErr(stderr)
			root.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, "--state-dir", dir, "--config", configPath}, fixture.Args...))
			done := make(chan error, 1)
			go func() { done <- root.Execute() }()

			waitCtx := testwait.Context(t)
			select {
			case <-stderr.seen:
			case err := <-done:
				t.Fatalf("%v finished while a harvest held the run lock (err=%v)\nstdout:\n%s\nstderr:\n%s", fixture.Args, err, stdout, stderr)
			case <-waitCtx.Done():
				releaseLock()
				<-done
				t.Fatalf("%v never said it was waiting for the harvest run lock\nstderr:\n%s", fixture.Args, stderr)
			}
			if got := stdout.String(); strings.Contains(got, fixture.DoneStdout) {
				t.Fatalf("%v reported its write %q while a harvest held the run lock:\n%s", fixture.Args, fixture.DoneStdout, got)
			}

			releaseLock()
			if err := testwait.Receive(t, done, "the command to finish after the harvest run lock was released"); err != nil {
				t.Fatalf("%v after the lock was released: %v\nstdout:\n%s\nstderr:\n%s", fixture.Args, err, stdout, stderr)
			}
			if got := stdout.String(); !strings.Contains(got, fixture.DoneStdout) {
				t.Errorf("%v output misses %q:\n%s", fixture.Args, fixture.DoneStdout, got)
			}
		})
	}
}
