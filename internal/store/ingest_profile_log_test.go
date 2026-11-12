package store

import (
	_ "embed"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/ingest_profile_log.yaml
var ingestProfileLogYAML []byte

//go:embed testdata/ingest_profile_log.manifest.yaml
var ingestProfileLogManifestYAML []byte

type profileStageRow struct {
	Stage      string `yaml:"stage"`
	DurationMs int64  `yaml:"durationMs"`
	Done       int    `yaml:"done"`
	Total      int    `yaml:"total"`
	Bytes      int64  `yaml:"bytes"`
	LaneWaitMs int64  `yaml:"laneWaitMs"`
}

func (row profileStageRow) stage() ingest.IndexProfileStage {
	return ingest.IndexProfileStage{
		Stage: ingest.Stage(row.Stage), Duration: time.Duration(row.DurationMs) * time.Millisecond,
		Done: row.Done, Total: row.Total, Bytes: row.Bytes, LaneWait: time.Duration(row.LaneWaitMs) * time.Millisecond,
	}
}

type profileLogFixtures struct {
	Segments     []profileStageRow `yaml:"segments"`
	WantRows     []profileStageRow `yaml:"wantRows"`
	UnknownStage string            `yaml:"unknownStage"`
	IndexLog     struct {
		Session string `yaml:"session"`
		ParseMs int64  `yaml:"parseMs"`
		WriteMs int64  `yaml:"writeMs"`
		Bytes   int64  `yaml:"bytes"`
	} `yaml:"indexLog"`
	Cases []struct {
		Name string `yaml:"name"`
	} `yaml:"cases"`
}

func LoadProfileLogFixtures(t *testing.T) profileLogFixtures {
	t.Helper()
	var fixtures profileLogFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(ingestProfileLogYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode ingest_profile_log.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(ingestProfileLogManifestYAML)
	if err != nil {
		t.Fatalf("decode ingest_profile_log manifest: %v", err)
	}
	names := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "ingest profile log"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func TestIngestProfileLog(t *testing.T) {
	fixtures := LoadProfileLogFixtures(t)
	var segments, want []ingest.IndexProfileStage
	for _, row := range fixtures.Segments {
		segments = append(segments, row.stage())
	}
	for _, row := range fixtures.WantRows {
		want = append(want, row.stage())
	}
	folded := ingest.FoldProfileStages(segments)
	count := func(t *testing.T, s *Store, query string, args ...any) int {
		t.Helper()
		conn, err := s.pool.Take(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer s.pool.Put(conn)
		n := 0
		queryV62Row(t, conn, query, args, func(stmt *sqlite.Stmt) { n = stmt.ColumnInt(0) })
		return n
	}
	exec := func(t *testing.T, s *Store, script string) {
		t.Helper()
		conn, err := s.pool.Take(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer s.pool.Put(conn)
		if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
			t.Fatal(err)
		}
	}

	runners := map[string]func(t *testing.T){
		"run-writes-one-row-per-stage": func(t *testing.T) {
			s, _ := openGenerationStore(t)
			if err := s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 1, Stages: folded}); err != nil {
				t.Fatal(err)
			}
			if got := count(t, s, `SELECT count(*) FROM ingest_run_stages WHERE run_id=1`); got != len(want) {
				t.Fatalf("%d stage rows, want one per recorded stage (%d)", got, len(want))
			}
			if got := count(t, s, `SELECT count(DISTINCT stage) FROM ingest_run_stages WHERE run_id=1`); got != len(want) {
				t.Fatalf("%d distinct stages, want %d", got, len(want))
			}
		},
		"stage-segments-are-summed-into-one-row": func(t *testing.T) {
			s, _ := openGenerationStore(t)
			if err := s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 1, Stages: folded}); err != nil {
				t.Fatal(err)
			}
			got, err := s.ListIngestRunStages(t.Context(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("stored stage rows = %+v, want %+v", got, want)
			}
			// The unfolded segments name one stage twice: the write boundary
			// refuses them instead of storing a partial profile.
			if err := s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 2, Stages: segments}); err == nil {
				t.Fatal("two rows for one stage were admitted")
			}
			if got := count(t, s, `SELECT count(*) FROM ingest_log`); got != 1 {
				t.Fatalf("%d run rows after a refused profile, want 1", got)
			}
		},
		"run-row-and-stage-rows-commit-together": func(t *testing.T) {
			s, _ := openGenerationStore(t)
			// The fault lands after the run row and the first stage row.
			exec(t, s, `CREATE TRIGGER fail_stage BEFORE INSERT ON ingest_run_stages WHEN NEW.stage = 'INDEX' BEGIN SELECT RAISE(ABORT, 'injected stage write failure'); END;`)
			outcomes := LoadRunOutcomeFixtures(t)
			if err := s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 1, Stages: folded, Outcomes: outcomes}); err == nil {
				t.Fatal("the injected stage fault did not fail the audit write")
			}
			if runs, stages, details := count(t, s, `SELECT count(*) FROM ingest_log`), count(t, s, `SELECT count(*) FROM ingest_run_stages`), count(t, s, `SELECT count(*) FROM ingest_run_outcomes`); runs+stages+details != 0 {
				t.Fatalf("after a fault: %d run rows, %d stage rows, %d outcome rows; want none", runs, stages, details)
			}
		},
		"unknown-stage-name-is-refused-at-the-write-boundary": func(t *testing.T) {
			if _, err := ingest.NewStage(fixtures.UnknownStage); err == nil {
				t.Fatal("constructor admitted an unknown stage")
			}
			for _, stage := range ingest.IndexProfileStageOrder {
				if got, err := ingest.NewStage(string(stage)); err != nil || got != stage {
					t.Fatalf("constructor refused stage %s: %v", stage, err)
				}
			}
			s, _ := openGenerationStore(t)
			stages := append([]ingest.IndexProfileStage{}, folded...)
			stages = append(stages, ingest.IndexProfileStage{Stage: ingest.Stage(fixtures.UnknownStage)})
			if err := s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 1, Stages: stages}); err == nil {
				t.Fatal("the store wrote a stage this build does not define")
			}
			if runs, rows := count(t, s, `SELECT count(*) FROM ingest_log`), count(t, s, `SELECT count(*) FROM ingest_run_stages`); runs+rows != 0 {
				t.Fatalf("a refused stage left %d run rows and %d stage rows", runs, rows)
			}
		},
		"deleting-a-run-row-cascades-to-its-stage-rows": func(t *testing.T) {
			s, _ := openGenerationStore(t)
			for range 2 {
				if err := s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 1, Stages: folded}); err != nil {
					t.Fatal(err)
				}
			}
			exec(t, s, `DELETE FROM ingest_log WHERE id=1;`)
			if got := count(t, s, `SELECT count(*) FROM ingest_run_stages WHERE run_id=1`); got != 0 {
				t.Fatalf("%d stage rows survived their run", got)
			}
			if got := count(t, s, `SELECT count(*) FROM ingest_run_stages WHERE run_id=2`); got != len(want) {
				t.Fatalf("the cascade removed another run's rows: %d left", got)
			}
		},
		"retention-keeps-the-stage-rows-of-the-last-n-runs": func(t *testing.T) {
			s, _ := openGenerationStore(t)
			entry := ingest.IngestLogEntry{StartedAt: 1, Stages: folded}
			for range IngestRunStageRetainedRuns {
				if err := s.LogIngestRun(t.Context(), entry); err != nil {
					t.Fatal(err)
				}
			}
			// At the boundary the oldest run is still inside the window.
			if got := count(t, s, `SELECT count(*) FROM ingest_run_stages WHERE run_id=1`); got != len(want) {
				t.Fatalf("the oldest retained run lost stage rows at the boundary: %d", got)
			}
			// A run with no profile advances the window too.
			if err := s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 2}); err != nil {
				t.Fatal(err)
			}
			if got := count(t, s, `SELECT count(*) FROM ingest_run_stages WHERE run_id=1`); got != 0 {
				t.Fatalf("%d stage rows of a run outside the window remain", got)
			}
			if got := count(t, s, `SELECT count(DISTINCT run_id) FROM ingest_run_stages`); got != IngestRunStageRetainedRuns-1 {
				t.Fatalf("stage rows of %d runs remain, want the %d profiled runs inside the window", got, IngestRunStageRetainedRuns-1)
			}
		},
		"retention-deletes-no-run-row-and-no-index-row": func(t *testing.T) {
			s, _ := openGenerationStore(t)
			sid := ingest.SessionID(fixtures.IndexLog.Session)
			if err := s.LogIndexEntry(t.Context(), ingest.IndexLogEntry{SessionID: sid, Harness: ingest.HarnessClaudeCode, Outcome: ingest.IndexOutcomeIndexed, StartedAt: 1}); err != nil {
				t.Fatal(err)
			}
			runs := IngestRunStageRetainedRuns + 3
			for range runs {
				if err := s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 1, Stages: folded}); err != nil {
					t.Fatal(err)
				}
			}
			if got := count(t, s, `SELECT count(*) FROM ingest_log`); got != runs {
				t.Fatalf("%d run rows remain of %d: retention deleted audit history", got, runs)
			}
			if got := count(t, s, `SELECT count(*) FROM index_log`); got != 1 {
				t.Fatalf("%d index rows remain of 1: retention deleted index history", got)
			}
		},
		"indexed-row-carries-parse-write-and-bytes": func(t *testing.T) {
			s, _ := openGenerationStore(t)
			sid := ingest.SessionID(fixtures.IndexLog.Session)
			parse := time.Duration(fixtures.IndexLog.ParseMs) * time.Millisecond
			write := time.Duration(fixtures.IndexLog.WriteMs) * time.Millisecond
			reason := "free text stays free text"
			entry := ingest.IndexLogEntry{SessionID: sid, Harness: ingest.HarnessClaudeCode, Outcome: ingest.IndexOutcomeIndexed, StartedAt: 1,
				Reason: &reason, ParseDuration: &parse, WriteDuration: &write, Bytes: &fixtures.IndexLog.Bytes}
			if err := s.LogIndexEntry(t.Context(), entry); err != nil {
				t.Fatal(err)
			}
			got, err := s.ListIndexLogTimings(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].ParseDuration == nil || got[0].WriteDuration == nil || got[0].Bytes == nil {
				t.Fatalf("stored timings = %+v, want all three measured", got)
			}
			if *got[0].ParseDuration != parse || *got[0].WriteDuration != write || *got[0].Bytes != fixtures.IndexLog.Bytes || got[0].Reason != reason {
				t.Fatalf("stored timings = %v %v %d %q", *got[0].ParseDuration, *got[0].WriteDuration, *got[0].Bytes, got[0].Reason)
			}
		},
		"row-written-before-the-migration-reads-as-not-measured": func(t *testing.T) {
			s, _ := openGenerationStore(t)
			sid := ingest.SessionID(fixtures.IndexLog.Session)
			// The column list of a build before the timing columns existed.
			exec(t, s, `INSERT INTO index_log(session_id, provider, outcome, index_version, entries_count, started_at) VALUES('`+string(sid)+`', 'claude-code', 'indexed', 1, 3, 5);`)
			// An attempt that did not parse carries no measurement either.
			if err := s.LogIndexEntry(t.Context(), ingest.IndexLogEntry{SessionID: sid, Harness: ingest.HarnessClaudeCode, Outcome: ingest.IndexOutcomeSkipped, StartedAt: 6}); err != nil {
				t.Fatal(err)
			}
			got, err := s.ListIndexLogTimings(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 {
				t.Fatalf("%d index rows, want 2", len(got))
			}
			for _, timing := range got {
				if timing.ParseDuration != nil || timing.WriteDuration != nil || timing.Bytes != nil {
					t.Fatalf("an unmeasured row reads as measured: %+v", timing)
				}
			}
		},
	}
	for _, c := range fixtures.Cases {
		run, ok := runners[c.Name]
		if !ok {
			t.Fatalf("ingest_profile_log.yaml: case %q has no runner; add it to the runner", c.Name)
		}
		t.Run(c.Name, run)
	}
}
