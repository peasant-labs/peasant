package store

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_concurrency.yaml
var contentConcurrencyYAML []byte

//go:embed testdata/content_concurrency.manifest.yaml
var contentConcurrencyManifestYAML []byte

type contentConcurrencyCase struct {
	Name         string `yaml:"name"`
	Mode         string `yaml:"mode"`
	Seam         string `yaml:"seam"`
	Expect       string `yaml:"expect"`
	CrossProcess bool   `yaml:"crossProcess"`
	FirstText    string `yaml:"firstText,omitempty"`
	NextText     string `yaml:"nextText,omitempty"`
}

func loadContentConcurrencyFixture(t *testing.T) []contentConcurrencyCase {
	t.Helper()
	var fixtures struct {
		Cases []contentConcurrencyCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contentConcurrencyYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	manifest, err := decodeRecoveryRequiredNames(contentConcurrencyManifestYAML)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		if c.Mode == "" || c.Seam == "" || c.Expect == "" {
			t.Fatalf("incomplete concurrency fixture: %+v", c)
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "content concurrency"); err != nil {
		t.Fatal(err)
	}
	return fixtures.Cases
}

func TestContentConcurrency(t *testing.T) {
	if os.Getenv("PEASANT_CONTENT_CONCURRENCY_CHILD") == "1" {
		runConcurrencyChild(t)
		return
	}
	for _, c := range loadContentConcurrencyFixture(t) {
		if c.Mode == "reader-stage" || c.Mode == "reader-commit" {
			// The external reader runner owns both cases because transcript
			// imports store; no placeholder subtest is reported here.
			if c.Seam != "snapshot-callback" || c.FirstText == "" || c.NextText == "" {
				t.Fatalf("invalid reader fixture: %+v", c)
			}
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			switch c.Mode {
			case "same-candidate":
				if c.Expect != "committed-once" || c.Seam != "before-commit" {
					t.Fatalf("invalid activation interleave %+v", c)
				}
				t.Run("in-process", runConcurrencySameCandidate)
				if c.CrossProcess {
					t.Run("cross-process", runConcurrencyAcrossProcesses)
				}
			case "staged-cas":
				if c.Expect != "stale-state-refused" {
					t.Fatal("unknown staging expectation")
				}
				runConcurrencyStagedCAS(t)
			case "sweep":
				if c.Expect != "fk-refused-then-committed" {
					t.Fatal("unknown sweep expectation")
				}
				runConcurrencySweepVsStage(t)
			case "reclaim":
				if c.Expect != "settled-no-orphans" {
					t.Fatal("unknown reclaim expectation")
				}
				runConcurrencyHarvestVsReclaim(t)
			case "migrate":
				if c.Expect != "lock-refused-then-committed" {
					t.Fatal("unknown migration expectation")
				}
				runConcurrencyHarvestVsMigrate(t)
			case "consolidation":
				if c.Expect != "committed-body-searchable" {
					t.Fatal("unknown consolidation expectation")
				}
				runConcurrencyConsolidation(t)
			default:
				t.Fatalf("unknown concurrency mode %q", c.Mode)
			}
		})
	}
}

func runConcurrencySameCandidate(t *testing.T) {
	s, _ := openGenerationStore(t)
	ctx, cancel := context.WithCancel(testwait.Context(t))
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	sid := gcSession(t, s, "b5b5b5b5-b5b5-45b5-85b5-b5b5b5b5b5b5")
	v2, blobs := buildTestGeneration(t, sid, "same-candidate", "same durable text", "same input", "same output")
	activation := GenerationActivation{Generation: filledCandidateForValidation(t, v2, blobs), Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1}
	reached := make(chan struct{}, 2)
	release := make(chan struct{})
	harmonizedWriterSeam = func(stage string) error {
		if stage == harmonizedSeamBeforeCommit {
			reached <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	t.Cleanup(clearHarmonizedFault)
	type result struct {
		outcome ingest.ActivationOutcome
		err     error
	}
	done := make(chan result, 2)
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			outcome, err := s.ActivateGeneration(ctx, activation)
			done <- result{outcome, err}
		}()
	}
	testwait.Receive(t, reached, "first activation staged")
	testwait.Receive(t, reached, "second activation staged")
	close(release)
	dispositions := map[ingest.ActivationDisposition]int{}
	for i := 0; i < 2; i++ {
		r := testwait.Receive(t, done, "same-candidate activation completion")
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.outcome.CandidateID != activation.Generation.Generation.ID {
			t.Fatalf("wrong candidate disposition: %+v", r.outcome)
		}
		dispositions[r.outcome.Disposition]++
	}
	if dispositions[ingest.ActivationCommittedNow] != 1 || dispositions[ingest.ActivationAlreadyCommitted] != 1 || len(dispositions) != 2 {
		t.Fatalf("same candidate dispositions = %v", dispositions)
	}
	if _, err := s.SweepSession(t.Context(), sid); err != nil {
		t.Fatal(err)
	}
	assertGCDigestsEqual(t, s, sid, "same-candidate", gcBodyDigests(t, sid, v2, blobs))
	if readSweepFlag(t, s, sid) {
		t.Fatal("same-candidate sweep left its flag set")
	}
}

func runConcurrencyStagedCAS(t *testing.T) {
	s, _ := openGenerationStore(t)
	ctx, cancel := context.WithCancel(testwait.Context(t))
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	sid := gcSession(t, s, "b6b6b6b6-b6b6-46b6-86b6-b6b6b6b6b6b6")
	gcActivate(t, s, sid, "cas-original", []string{"original0", "original1", "original2"}, nil)
	state, err := s.ReadIndexState(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	g2, b2 := buildTestGeneration(t, sid, "cas-first", "first text", "first input", "first output")
	g3, b3 := buildTestGeneration(t, sid, "cas-stale", "stale text", "stale input", "stale output")
	a2 := GenerationActivation{Generation: filledCandidateForValidation(t, g2, b2), Blobs: b2, ExpectedState: state, IndexerVersion: 1, IndexedAtMs: 2}
	a3 := GenerationActivation{Generation: filledCandidateForValidation(t, g3, b3), Blobs: b3, ExpectedState: state, IndexerVersion: 1, IndexedAtMs: 3}
	type staged struct {
		handle *PreparedGeneration
		err    error
	}
	done2, done3 := make(chan staged, 1), make(chan staged, 1)
	workers.Add(2)
	go func() { defer workers.Done(); h, e := s.StageGeneration(ctx, a2); done2 <- staged{h, e} }()
	go func() { defer workers.Done(); h, e := s.StageGeneration(ctx, a3); done3 <- staged{h, e} }()
	r2, r3 := testwait.Receive(t, done2, "first staging"), testwait.Receive(t, done3, "second staging")
	if r2.err != nil || r3.err != nil {
		t.Fatalf("concurrent staging: %v %v", r2.err, r3.err)
	}
	a2.Prepared, a3.Prepared = r2.handle, r3.handle
	if _, err := s.ActivateGeneration(t.Context(), a2); err != nil {
		t.Fatal(err)
	}
	outcome, err := s.ActivateGeneration(t.Context(), a3)
	var stale *ingest.StaleIndexWorkError
	if !errors.As(err, &stale) || outcome.Disposition != ingest.ActivationNotCommitted {
		t.Fatalf("stale staged candidate outcome %+v error %v", outcome, err)
	}
	if got := visibleGeneration(t, s, sid); got != "cas-first" {
		t.Fatalf("stale work changed active generation to %q", got)
	}
	if _, err := s.SweepSession(t.Context(), sid); err != nil {
		t.Fatal(err)
	}
	assertGCDigestsEqual(t, s, sid, "staged-cas", gcBodyDigests(t, sid, g2, b2))
}

func runConcurrencyConsolidation(t *testing.T) {
	s, _ := openGenerationStore(t)
	ctx, cancel := context.WithCancel(testwait.Context(t))
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	sid := gcSession(t, s, "b7b7b7b7-b7b7-47b7-87b7-b7b7b7b7b7b7")
	v2, blobs := buildTestGeneration(t, sid, "consolidation-next", "consolidationharvestterm", "next input", "next output")
	activation := GenerationActivation{Generation: filledCandidateForValidation(t, v2, blobs), Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1}
	prepared := make(chan struct{}, 1)
	done := make(chan error, 1)
	harmonizedWriterSeam = func(stage string) error {
		if stage == harmonizedSeamAfterPrepare {
			prepared <- struct{}{}
		}
		return nil
	}
	t.Cleanup(clearHarmonizedFault)
	entered := false
	authorizer := sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
		if action.Table() == "session_entries_fts" && action.Type() == sqlite.OpDropVTable {
			entered = true
			workers.Add(1)
			go func() { defer workers.Done(); _, err := s.ActivateGeneration(ctx, activation); done <- err }()
			testwait.Receive(t, prepared, "harvest prepared while search consolidation owns the write transaction")
		}
		return sqlite.AuthResultOK
	})
	first := searchTakeConn(t, s)
	second := searchTakeConn(t, s)
	if err := first.SetAuthorizer(authorizer); err != nil {
		t.Fatal(err)
	}
	if err := second.SetAuthorizer(authorizer); err != nil {
		t.Fatal(err)
	}
	s.pool.Put(first)
	s.pool.Put(second)
	consolidated, err := s.migrateConsolidateSearch(t.Context())
	if err != nil || !consolidated || !entered {
		t.Fatalf("consolidation interleave: consolidated=%v entered=%v error=%v", consolidated, entered, err)
	}
	if err := testwait.Receive(t, done, "harvest after consolidation commit"); err != nil {
		t.Fatal(err)
	}
	if got := visibleGeneration(t, s, sid); got != "consolidation-next" {
		t.Fatalf("active generation %q", got)
	}
	assertRepairMatch(t, s, "consolidationharvestterm")
}

func runConcurrencyAcrossProcesses(t *testing.T) {
	s, root := openGenerationStore(t)
	sid := gcSession(t, s, "b8b8b8b8-b8b8-48b8-88b8-b8b8b8b8b8b8")
	dir := filepath.Dir(root)
	ready, releaseFile := filepath.Join(dir, "child-ready"), filepath.Join(dir, "child-release")
	cmd := exec.CommandContext(testwait.Context(t), os.Args[0], "-test.run=^TestContentConcurrency$")
	cmd.Env = append(os.Environ(), "PEASANT_CONTENT_CONCURRENCY_CHILD=1", "PEASANT_CONTENT_CONCURRENCY_ROOT="+root, "PEASANT_CONTENT_CONCURRENCY_READY="+ready, "PEASANT_CONTENT_CONCURRENCY_RELEASE="+releaseFile)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	testwait.Until(t, "child owns the OS session lock", func() bool { _, err := os.Stat(ready); return err == nil })
	v2, blobs := buildTestGeneration(t, sid, "cross-process-candidate", "cross process text", "cross process input", "cross process output")
	activation := GenerationActivation{Generation: filledCandidateForValidation(t, v2, blobs), Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	if outcome, err := s.ActivateGeneration(ctx, activation); !errors.Is(err, context.DeadlineExceeded) || outcome.Disposition != ingest.ActivationNotCommitted {
		t.Fatalf("parent ignored child OS lock: %+v %v", outcome, err)
	}
	if err := os.WriteFile(releaseFile, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	waited = true
	if err != nil {
		t.Fatalf("child activation: %v", err)
	}
	outcome, err := s.ActivateGeneration(t.Context(), activation)
	if err != nil || outcome.Disposition != ingest.ActivationAlreadyCommitted {
		t.Fatalf("parent idempotent retry: %+v %v", outcome, err)
	}
	if _, err := s.SweepSession(t.Context(), sid); err != nil {
		t.Fatal(err)
	}
	assertGCDigestsEqual(t, s, sid, "cross-process", gcBodyDigests(t, sid, v2, blobs))
}

func runConcurrencyChild(t *testing.T) {
	root := os.Getenv("PEASANT_CONTENT_CONCURRENCY_ROOT")
	artifacts, err := NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(filepath.Dir(root), "generations.db"), WithSkipMigrations(), WithPoolSize(2), WithIndexFormats(V2IndexFormat()), WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sid, err := schema.NewSessionID("b8b8b8b8-b8b8-48b8-88b8-b8b8b8b8b8b8")
	if err != nil {
		t.Fatal(err)
	}
	release, err := locker.LockExclusive(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	if err := os.WriteFile(os.Getenv("PEASANT_CONTENT_CONCURRENCY_READY"), []byte("ready"), 0600); err != nil {
		_ = release()
		t.Fatal(err)
	}
	testwait.Until(t, "parent releases the held-lock barrier", func() bool { _, err := os.Stat(os.Getenv("PEASANT_CONTENT_CONCURRENCY_RELEASE")); return err == nil })
	if err := release(); err != nil {
		t.Fatal(err)
	}
	v2, blobs := buildTestGeneration(t, sid, "cross-process-candidate", "cross process text", "cross process input", "cross process output")
	outcome, err := s.ActivateGeneration(t.Context(), GenerationActivation{Generation: filledCandidateForValidation(t, v2, blobs), Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1})
	if err != nil || outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("child activation: %+v %v", outcome, err)
	}
}
