package ingest

import (
	"context"
	_ "embed"
	"errors"
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/run_outcomes.yaml
var runOutcomeFixtureData []byte

type runOutcomeCase struct {
	Name        string `yaml:"name"`
	SessionID   string `yaml:"sessionId"`
	WorkerError bool   `yaml:"workerError"`
	Acquisition bool   `yaml:"acquisition"`
	Reason      string `yaml:"reason"`
	Kind        string `yaml:"kind"`
}

func LoadRunOutcomeFixtures(t *testing.T) []runOutcomeCase {
	t.Helper()
	var fixture struct {
		RequiredNames []string         `yaml:"requiredNames"`
		Cases         []runOutcomeCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(runOutcomeFixtureData, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if seen[row.Name] {
			t.Fatalf("duplicate case %s", row.Name)
		}
		seen[row.Name] = true
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing required case %s", name)
		}
	}
	return fixture.Cases
}

type outcomeAuditLogger struct {
	entry IngestLogEntry
	err   error
}

var _ IngestLogger = (*outcomeAuditLogger)(nil)

func (l *outcomeAuditLogger) LogIngestRun(_ context.Context, entry IngestLogEntry) error {
	l.entry = entry
	return l.err
}

func TestFinalizeRunOutcomesAudit(t *testing.T) {
	logger := &outcomeAuditLogger{}
	p := &Pipeline{logger: logger, fs: &OSFileSystem{}, config: PipelineConfig{OutputDir: ResolvedPath(t.TempDir())}}
	var results []SessionResult
	var want []RunOutcome
	for _, row := range LoadRunOutcomeFixtures(t) {
		var sid SessionID
		var err error
		if row.SessionID != "" {
			sid, err = NewSessionID(row.SessionID)
			if err != nil {
				t.Fatal(err)
			}
		}
		kind, err := NewRunOutcomeKind(row.Kind)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, RunOutcome{SessionID: sid, Kind: kind, ReasonCode: row.Reason})
		if row.WorkerError {
			err = errors.New("synthetic source payload must not enter the durable audit")
			if row.Acquisition {
				err = &adapterAcquisitionError{cause: err}
			}
			results = append(results, SessionResult{SessionID: sid, Error: err})
		} else {
			diagnostic := DiagnosticEntry{ErrorType: row.Reason, Location: "/synthetic/private/source", Message: "synthetic source payload", Remediation: "restore source"}
			if sid == "" {
				p.reportDiagnostic(diagnostic)
			} else {
				p.reportSessionDiagnostic(sid, diagnostic)
				p.retainSessionDiagnostic(sid, diagnostic)
			}
		}
	}
	result, err := p.indexComputeAndFinalize(t.Context(), nil, nil, results, nil, time.Now(), nil, IndexOutcomeIndexed, "pipeline", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Errors != 2 || logger.entry.SessionsError != 2 {
		t.Fatalf("worker totals changed: %+v", result.Summary)
	}
	got := logger.entry.Outcomes
	if len(got) != len(want) {
		t.Fatalf("audit outcomes = %+v", got)
	}
	for _, expected := range want {
		found := false
		for _, outcome := range got {
			if outcome.CreatedAt == 0 {
				t.Fatal("outcome missing audit timestamp")
			}
			outcome.CreatedAt = 0
			if reflect.DeepEqual(outcome, expected) {
				found = true
			}
		}
		if !found {
			t.Fatalf("audit missing %+v in %+v", expected, got)
		}
	}
	logger.err = errors.New("synthetic audit write failure")
	if _, err := p.indexComputeAndFinalize(t.Context(), nil, nil, results, nil, time.Now(), nil, IndexOutcomeIndexed, "pipeline", nil, nil); err != nil {
		t.Fatalf("best-effort audit failed pipeline: %v", err)
	}
	p.resetDiagnostics()
	if outcomes := p.runOutcomes(nil, 1); len(outcomes) != 0 {
		t.Fatalf("diagnostics leaked into next run: %+v", outcomes)
	}
}

func TestRunOutcomeKindRejectsUnknown(t *testing.T) {
	if _, err := NewRunOutcomeKind("unrecognized"); err == nil {
		t.Fatal("unknown run outcome kind admitted")
	}
}
