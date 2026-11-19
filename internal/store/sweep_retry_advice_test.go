package store

import (
	_ "embed"
	"strings"
	"testing"

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
	// Code is the SQLite primary result code the sweep's first delete fails
	// with, by its SQLite name.
	Code string `yaml:"code"`
	// Advice must appear in the sweep error; Forbidden must not.
	Advice    string `yaml:"advice"`
	Forbidden string `yaml:"forbidden"`
}

// sweepRetryAdviceCodes is the closed set of result codes the fixture may
// name, by their SQLite names.
var sweepRetryAdviceCodes = map[string]sqlite.ResultCode{
	"SQLITE_BUSY":   sqlite.ResultBusy,
	"SQLITE_LOCKED": sqlite.ResultLocked,
	"SQLITE_FULL":   sqlite.ResultFull,
	"SQLITE_IOERR":  sqlite.ResultIOErr,
}

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
		if _, ok := sweepRetryAdviceCodes[c.Code]; !ok {
			t.Fatalf("sweep_retry_advice.yaml case %q names unknown SQLite code %q", c.Name, c.Code)
		}
		if c.Advice == "" || c.Forbidden == "" {
			t.Fatalf("sweep_retry_advice.yaml case %q needs both advice and forbidden text", c.Name)
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "sweep retry advice"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// TestSweepFailureAdviceMatchesCause fails the sweep's first delete with each
// fixture's SQLite code through the production sweep and proves the error
// names the repair for that cause: lock contention never advises freeing
// disk space, and a full disk never blames another writer. Every refusal
// keeps the flag set and deletes nothing.
func TestSweepFailureAdviceMatchesCause(t *testing.T) {
	for _, c := range loadSweepRetryAdviceCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			s, _ := openGenerationStore(t)
			sid := gcSession(t, s, "e4e4e4e4-e4e4-44e4-84e4-e4e4e4e4e4e4")
			gcActivate(t, s, sid, "gc_advice_old", []string{"gcadviceo0", "gcadviceo1", "gcadviceo2"}, nil)
			gcActivate(t, s, sid, "gc_advice_active", []string{"gcadvicen0", "gcadvicen1", "gcadvicen2"}, nil)
			before := len(gcRemainingDigests(t, s, sid))
			code := sweepRetryAdviceCodes[c.Code]
			contentSweepSeam = func(stage string) error {
				if stage == contentSweepSeamFirstWrite {
					return code.ToError()
				}
				return nil
			}
			t.Cleanup(func() { contentSweepSeam = nil })
			_, err := s.SweepSession(testwait.Context(t), sid)
			if err == nil {
				t.Fatalf("sweep with its first delete failing %s succeeded", c.Code)
			}
			msg := err.Error()
			if !strings.Contains(msg, c.Advice) {
				t.Fatalf("sweep error for %s = %q, want advice %q", c.Code, msg, c.Advice)
			}
			if strings.Contains(msg, c.Forbidden) {
				t.Fatalf("sweep error for %s = %q, must not say %q", c.Code, msg, c.Forbidden)
			}
			if !strings.Contains(msg, "retry harvest or reclaim") {
				t.Fatalf("sweep error for %s = %q, want the retry step", c.Code, msg)
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
