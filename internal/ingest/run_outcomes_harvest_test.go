package ingest_test

import (
	_ "embed"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/run_outcomes_harvest.yaml
var runOutcomesHarvestData []byte

type runOutcomesHarvestFixture struct {
	Name          string   `yaml:"name"`
	RequiredNames []string `yaml:"requiredNames"`
	Sessions      []struct {
		Name          string `yaml:"name"`
		SessionID     string `yaml:"sessionId"`
		WorkerFailure bool   `yaml:"workerFailure"`
		Kind          string `yaml:"kind"`
		Reason        string `yaml:"reason"`
	} `yaml:"sessions"`
	PrivateMessage  string `yaml:"privateMessage"`
	PrivateLocation string `yaml:"privateLocation"`
	Transcript      string `yaml:"transcript"`
	Totals          struct {
		New       int `yaml:"new"`
		Errors    int `yaml:"errors"`
		Updated   int `yaml:"updated"`
		Unchanged int `yaml:"unchanged"`
		Indexed   int `yaml:"indexed"`
		Computed  int `yaml:"computed"`
	} `yaml:"totals"`
}

func LoadRunOutcomesHarvestFixture(t *testing.T) runOutcomesHarvestFixture {
	t.Helper()
	var fixture runOutcomesHarvestFixture
	if err := yaml.Unmarshal(runOutcomesHarvestData, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Name != "harvest-worker-failure-and-diagnostic" {
		t.Fatal("missing required harvest fixture")
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Sessions {
		if seen[row.Name] {
			t.Fatalf("duplicate harvest fixture %s", row.Name)
		}
		seen[row.Name] = true
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing required harvest fixture %s", name)
		}
	}
	return fixture
}

func TestRunOutcomesHarvestSQLite(t *testing.T) {
	fixture := LoadRunOutcomesHarvestFixture(t)
	mfs := testutil.NewMemFS()
	db := storetest.Open(t)
	var sessions []ingest.DiscoveredSession
	metadata := make(map[ingest.SessionID]*ingest.UnifiedMetadata)
	failures := make(map[ingest.SessionID]error)
	want := make(map[ingest.SessionID]ingest.RunOutcome)
	for _, row := range fixture.Sessions {
		path := testSourceDir + "/" + row.SessionID + ".jsonl"
		if err := mfs.WriteFile(path, []byte(fixture.Transcript+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		session := makeDiscoveredSession(t, row.SessionID, path, time.Now().Add(-time.Hour))
		sessions = append(sessions, session)
		kind, err := ingest.NewRunOutcomeKind(row.Kind)
		if err != nil {
			t.Fatal(err)
		}
		reason, err := ingest.NewAuditReason(row.Reason)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := want[session.SessionID]; ok {
			t.Fatal("duplicate fixture identity")
		}
		want[session.SessionID] = ingest.RunOutcome{SessionID: session.SessionID, Kind: kind, ReasonCode: reason}
		if row.WorkerFailure {
			failures[session.SessionID] = errors.New(fixture.PrivateMessage + " at " + fixture.PrivateLocation)
		} else {
			meta := makeMinimalMeta(t, row.SessionID)
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, ingest.DiagnosticEntry{ErrorType: row.Reason, Location: fixture.PrivateLocation, Message: fixture.PrivateMessage, Remediation: "restore source"})
			metadata[session.SessionID] = meta
		}
	}
	adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: makeStubAdapterWithErrors(sessions, metadata, failures)}
	pipeline, err := newTestPipeline(mfs, testutil.DefaultGitResolver(), adapters, makePipelineConfig(testOutputDir), ingest.WithStore(db), ingest.WithLogger(db))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.New != fixture.Totals.New ||
		result.Summary.Errors != fixture.Totals.Errors ||
		result.Summary.Updated != fixture.Totals.Updated ||
		result.Summary.Unchanged != fixture.Totals.Unchanged ||
		result.Summary.Indexed != fixture.Totals.Indexed ||
		result.Summary.Computed != fixture.Totals.Computed {
		t.Fatalf("harvest totals = %+v", result.Summary)
	}
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var runID int64
	var aggregates int
	err = sqlitex.ExecuteTransient(conn, `SELECT id, sessions_new, sessions_error, sessions_updated, sessions_unchanged, indexed_count, computed_count, started_at, finished_at FROM ingest_log`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		aggregates++
		runID = stmt.ColumnInt64(0)
		if stmt.ColumnInt(1) != fixture.Totals.New ||
			stmt.ColumnInt(2) != fixture.Totals.Errors ||
			stmt.ColumnInt(3) != fixture.Totals.Updated ||
			stmt.ColumnInt(4) != fixture.Totals.Unchanged ||
			stmt.ColumnInt(5) != fixture.Totals.Indexed ||
			stmt.ColumnInt(6) != fixture.Totals.Computed {
			t.Error("SQLite aggregate differs from harvest totals")
		}
		if stmt.ColumnInt64(7) == 0 || stmt.ColumnInt64(8) < stmt.ColumnInt64(7) {
			t.Error("aggregate lacks completed audit times")
		}
		return nil
	}})
	db.Pool().Put(conn)
	if err != nil {
		t.Fatal(err)
	}
	if aggregates != 1 || runID == 0 {
		t.Fatal("harvest did not write exactly one parent audit")
	}
	got, err := db.ListIngestRunOutcomes(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("harvest outcomes = %+v", got)
	}
	for _, outcome := range got {
		expected, ok := want[outcome.SessionID]
		if !ok || outcome.Kind != expected.Kind ||
			outcome.ReasonCode != expected.ReasonCode ||
			outcome.RunID != runID || outcome.CreatedAt == 0 {
			t.Fatalf("unexpected harvest outcome: %+v", outcome)
		}
		delete(want, outcome.SessionID)
	}
	if len(want) != 0 {
		t.Fatal("harvest lost a session outcome")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), fixture.PrivateMessage) || strings.Contains(string(encoded), fixture.PrivateLocation) || strings.Contains(string(encoded), "synthetic private transcript") {
		t.Fatal("durable outcomes retained private payload")
	}
}
