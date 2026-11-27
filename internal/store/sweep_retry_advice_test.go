package store

import (
	"context"
	_ "embed"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"gopkg.in/yaml.v3"
)

// A refused sweep names the repair its cause needs. Nothing here runs
// parallel: the sweep's seam is process-global.

//go:embed testdata/sweep_retry_advice.yaml
var sweepRetryAdviceYAML []byte

//go:embed testdata/sweep_retry_advice.manifest.yaml
var sweepRetryAdviceManifestYAML []byte

type sweepRetryAdviceCase struct {
	Name string `yaml:"name"`
	// Cause is what the sweep's first delete fails with: a SQLite result
	// code by its SQLite name, or a context error by its Go name.
	Cause string `yaml:"cause"`
	// Paths lists the sweep paths the case runs on.
	Paths []sweepPath `yaml:"paths"`
	// Every Advice fragment must appear in the sweep error; no Forbidden
	// fragment may.
	Advice    []string `yaml:"advice"`
	Forbidden []string `yaml:"forbidden"`
	// NamesBusyTimeout requires the error to name the store's busy timeout,
	// derived from defaults.SQLiteBusyTimeout rather than written in YAML.
	NamesBusyTimeout bool `yaml:"namesBusyTimeout"`
}

// sweepPath names which of the sweep's two delete paths a case drives.
type sweepPath string

const (
	// sweepPathBounded is the one-transaction sweep the default budget takes.
	sweepPathBounded sweepPath = "bounded"
	// sweepPathBatch is the resumable fallback a one-row budget forces.
	sweepPathBatch sweepPath = "batch"
)

// openSweepPathStore opens a generation store whose sweep takes path: the
// default budget fits a test session in one bounded transaction, and a
// one-row budget forces the resumable batch fallback.
func openSweepPathStore(t *testing.T, path sweepPath) (*Store, string) {
	t.Helper()
	switch path {
	case sweepPathBounded:
		return openGenerationStore(t)
	case sweepPathBatch:
		return openGenerationStoreWith(t, WithWriteConfig(ingest.WriteConfig{BatchBytes: 64, BatchSessions: 64, SweepRows: 1}.WithDefaults(1)))
	default:
		t.Fatalf("unknown sweep path %q", path)
		return nil, ""
	}
}

// sweepRetryAdviceCauses is the closed set of causes the fixture may name.
var sweepRetryAdviceCauses = map[string]error{
	"SQLITE_BUSY":              sqlite.ResultBusy.ToError(),
	"SQLITE_LOCKED":            sqlite.ResultLocked.ToError(),
	"SQLITE_FULL":              sqlite.ResultFull.ToError(),
	"SQLITE_IOERR":             sqlite.ResultIOErr.ToError(),
	"SQLITE_IOERR_WRITE":       sqlite.ResultIOErrWrite.ToError(),
	"SQLITE_INTERRUPT":         sqlite.ResultInterrupt.ToError(),
	"SQLITE_CORRUPT":           sqlite.ResultCorrupt.ToError(),
	"context.Canceled":         context.Canceled,
	"context.DeadlineExceeded": context.DeadlineExceeded,
}

// busyTimeoutAdvice is the busy-timeout text a lock-contention error must
// carry, derived from the store's real timeout so the two cannot drift.
var busyTimeoutAdvice = defaults.SQLiteBusyTimeout.String() + " busy timeout"

func loadSweepRetryAdviceCases(t *testing.T) []sweepRetryAdviceCase {
	t.Helper()
	var fixture struct {
		Cases []sweepRetryAdviceCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(sweepRetryAdviceYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode sweep_retry_advice.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(sweepRetryAdviceManifestYAML)
	if err != nil {
		t.Fatalf("decode sweep_retry_advice manifest: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if _, ok := sweepRetryAdviceCauses[c.Cause]; !ok {
			t.Fatalf("sweep_retry_advice.yaml case %q names unknown cause %q", c.Name, c.Cause)
		}
		if len(c.Advice) == 0 || len(c.Forbidden) == 0 || len(c.Paths) == 0 {
			t.Fatalf("sweep_retry_advice.yaml case %q needs advice, forbidden text, and paths", c.Name)
		}
		for _, path := range c.Paths {
			if path != sweepPathBounded && path != sweepPathBatch {
				t.Fatalf("sweep_retry_advice.yaml case %q names unknown sweep path %q", c.Name, path)
			}
		}
		if c.NamesBusyTimeout && slices.Contains(c.Forbidden, "busy timeout") {
			t.Fatalf("sweep_retry_advice.yaml case %q both requires and forbids the busy timeout", c.Name)
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "sweep retry advice"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// TestSweepFailureAdviceMatchesCause fails the sweep's first delete with each
// fixture's cause through the production sweep, on the bounded path and on
// the batch fallback, and proves the error names the repair for that cause:
// lock contention never advises freeing disk space, a full disk never blames
// another writer, and a cancellation blames neither. Every refusal keeps the
// flag set and deletes nothing.
func TestSweepFailureAdviceMatchesCause(t *testing.T) {
	for _, c := range loadSweepRetryAdviceCases(t) {
		for _, path := range c.Paths {
			t.Run(c.Name+"/"+string(path), func(t *testing.T) {
				s, _ := openSweepPathStore(t, path)
				sid := gcSession(t, s, "e4e4e4e4-e4e4-44e4-84e4-e4e4e4e4e4e4")
				gcActivate(t, s, sid, "gc_advice_old", []string{"gcadviceo0", "gcadviceo1", "gcadviceo2"}, nil)
				gcActivate(t, s, sid, "gc_advice_active", []string{"gcadvicen0", "gcadvicen1", "gcadvicen2"}, nil)
				before := len(gcRemainingDigests(t, s, sid))
				cause := sweepRetryAdviceCauses[c.Cause]
				contentSweepSeam = func(stage string) error {
					if stage == contentSweepSeamFirstWrite {
						return cause
					}
					return nil
				}
				t.Cleanup(func() { contentSweepSeam = nil })
				_, err := s.SweepSession(testwait.Context(t), sid)
				if err == nil {
					t.Fatalf("sweep with its first delete failing with %s succeeded", c.Cause)
				}
				msg := err.Error()
				// Each path reports through its own message; checking it proves
				// the budget sent the sweep down the path the case names.
				pathMessage := map[sweepPath]string{
					sweepPathBounded: "bounded sweep of session",
					sweepPathBatch:   "failed at its first delete",
				}[path]
				if !strings.Contains(msg, pathMessage) {
					t.Fatalf("sweep error for %s = %q, want the %s path's %q", c.Cause, msg, path, pathMessage)
				}
				for _, advice := range c.Advice {
					if !strings.Contains(msg, advice) {
						t.Fatalf("sweep error for %s = %q, want advice %q", c.Cause, msg, advice)
					}
				}
				for _, forbidden := range c.Forbidden {
					if strings.Contains(msg, forbidden) {
						t.Fatalf("sweep error for %s = %q, must not say %q", c.Cause, msg, forbidden)
					}
				}
				if c.NamesBusyTimeout && !strings.Contains(msg, busyTimeoutAdvice) {
					t.Fatalf("sweep error for %s = %q, want the store's %q", c.Cause, msg, busyTimeoutAdvice)
				}
				if !strings.Contains(msg, "retry harvest or reclaim") {
					t.Fatalf("sweep error for %s = %q, want the retry step", c.Cause, msg)
				}
				if !readSweepFlag(t, s, sid) {
					t.Fatal("refused sweep cleared its flag")
				}
				if got := len(gcRemainingDigests(t, s, sid)); got != before {
					t.Fatalf("refused sweep left %d bodies, want all %d", got, before)
				}
			})
		}
	}
}
