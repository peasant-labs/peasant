package store

import (
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/write_retry_advice.yaml
var writeRetryAdviceYAML []byte

//go:embed testdata/write_retry_advice.manifest.yaml
var writeRetryAdviceManifestYAML []byte

type writeRetryAdviceCase struct {
	Name             string   `yaml:"name"`
	Cause            string   `yaml:"cause"`
	Retry            string   `yaml:"retry"`
	Advice           []string `yaml:"advice"`
	Forbidden        []string `yaml:"forbidden"`
	NamesBusyTimeout bool     `yaml:"namesBusyTimeout"`
}

func loadWriteRetryAdviceCases(t *testing.T) []writeRetryAdviceCase {
	t.Helper()
	var fixture struct {
		Cases []writeRetryAdviceCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(writeRetryAdviceYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode write_retry_advice.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(writeRetryAdviceManifestYAML)
	if err != nil {
		t.Fatalf("decode write_retry_advice manifest: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if _, ok := sweepRetryAdviceCauses[c.Cause]; !ok {
			t.Fatalf("write_retry_advice.yaml case %q names unknown cause %q", c.Name, c.Cause)
		}
		complete := c.Retry != "" && len(c.Advice) > 0 && len(c.Forbidden) > 0
		if !complete {
			t.Fatalf("write_retry_advice.yaml case %q needs retry, advice and forbidden text", c.Name)
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "write retry advice"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// TestWriteRetryAdviceMatchesCause calls writeRetryAdvice with each fixture
// cause wrapped the way a store error carries it, and proves the advice names
// the repair for that cause and ends with the caller's retry step.
func TestWriteRetryAdviceMatchesCause(t *testing.T) {
	for _, c := range loadWriteRetryAdviceCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			wrapped := fmt.Errorf("store: write the probe: %w", sweepRetryAdviceCauses[c.Cause])
			advice := writeRetryAdvice(wrapped, c.Retry)
			for _, want := range c.Advice {
				if !strings.Contains(advice, want) {
					t.Fatalf("advice for %s = %q, want %q", c.Cause, advice, want)
				}
			}
			for _, forbidden := range c.Forbidden {
				if strings.Contains(advice, forbidden) {
					t.Fatalf("advice for %s = %q, must not say %q", c.Cause, advice, forbidden)
				}
			}
			if c.NamesBusyTimeout && !strings.Contains(advice, busyTimeoutAdvice) {
				t.Fatalf("advice for %s = %q, want the store's %q", c.Cause, advice, busyTimeoutAdvice)
			}
			if !strings.HasSuffix(advice, c.Retry) {
				t.Fatalf("advice for %s = %q, want it to end with the retry step %q", c.Cause, advice, c.Retry)
			}
		})
	}
}

// TestBeginWriteRefusalKeepsCodeAndAdvice begins a write while another
// connection holds the writer and the store's connection waits for no time,
// and proves the refusal keeps SQLite's BUSY code for callers that branch on
// it and carries the operation, the no-write statement and the advice.
func TestBeginWriteRefusalKeepsCodeAndAdvice(t *testing.T) {
	s, root := openGenerationStoreWith(t, WithPoolSize(1))
	competitor := openCompetingWriter(t, root)
	if err := competingWriteBegin(competitor, 1); err != nil {
		t.Fatalf("competing writer takes the writer: %v", err)
	}
	t.Cleanup(func() { _ = sqlitex.ExecuteTransient(competitor, "ROLLBACK", nil) })
	conn := searchTakeConn(t, s)
	defer s.pool.Put(conn)
	conn.SetBusyTimeout(0)
	end, err := beginWrite(conn, "write the probe")
	if err == nil {
		var endErr error
		end(&endErr)
		t.Fatal("beginWrite beside a held writer succeeded; want a refusal")
	}
	if code := sqlite.ErrCode(err).ToPrimary(); code != sqlite.ResultBusy {
		t.Fatalf("beginWrite refusal code = %v, want BUSY kept through the wrap", code)
	}
	if !conn.AutocommitEnabled() {
		t.Fatal("a refused beginWrite left a transaction open on the connection")
	}
	msg := err.Error()
	for _, want := range []string{"take the SQLite writer to write the probe", "nothing was written", writeRetryAdvice(sqlite.ResultBusy.ToError(), "retry")} {
		if !strings.Contains(msg, want) {
			t.Fatalf("beginWrite refusal = %q, want %q", msg, want)
		}
	}
}
