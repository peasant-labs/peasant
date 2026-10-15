package store

// V67 CHECK-matrix test: the seven nullable latest-attempt pointer columns added
// by migrationV67 must accept exactly the well-formed values and refuse the
// malformed ones. SQLite cannot ALTER a CHECK, so a dropped or loosened CHECK
// (the 64-character lowercase hex bound, the positive indexer revision, the
// non-negative captured-at, or the JSON validity) fails the named case until a
// rebuild migration lands.

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/migrations/v67_latest_attempt.yaml
var v67LatestAttemptFixtureBytes []byte

type v67LatestAttemptFixture struct {
	RequiredRecordNames    []string          `yaml:"required_record_names"`
	RequiredRejectionNames []string          `yaml:"required_rejection_names"`
	Records                []v67Record       `yaml:"records"`
	Rejections             []v67NamedRefusal `yaml:"rejections"`
}

type v67Record struct {
	Name           string  `yaml:"name"`
	SessionID      string  `yaml:"session_id"`
	GenerationID   *string `yaml:"generation_id"`
	ArtifactHash   *string `yaml:"artifact_hash"`
	IndexerVersion *string `yaml:"indexer_version"`
	FailureCode    *string `yaml:"failure_code"`
	FailureMessage *string `yaml:"failure_message"`
	CapturedAtMs   *string `yaml:"captured_at_ms"`
	StatsJSON      *string `yaml:"stats_json"`
}

type v67NamedRefusal struct {
	Name string `yaml:"name"`
	SQL  string `yaml:"sql"`
}

func requireV67Names(kind string, required []string, present map[string]bool) error {
	for _, name := range required {
		if !present[name] {
			return fmt.Errorf("v67 latest-attempt fixture: required %s %q is missing; restore the case in testdata/migrations/v67_latest_attempt.yaml", kind, name)
		}
	}
	return nil
}

func loadV67Fixture(t *testing.T) v67LatestAttemptFixture {
	t.Helper()
	var fixture v67LatestAttemptFixture
	d := yaml.NewDecoder(bytes.NewReader(v67LatestAttemptFixtureBytes))
	d.KnownFields(true)
	if err := d.Decode(&fixture); err != nil {
		t.Fatalf("decode v67 latest-attempt fixture: %v", err)
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("v67 latest-attempt fixture must contain exactly one YAML document: %v", err)
	}
	records := map[string]bool{}
	for _, r := range fixture.Records {
		if records[r.Name] {
			t.Fatalf("v67 latest-attempt fixture: duplicate record name %q", r.Name)
		}
		records[r.Name] = true
	}
	rejections := map[string]bool{}
	for _, r := range fixture.Rejections {
		if rejections[r.Name] {
			t.Fatalf("v67 latest-attempt fixture: duplicate rejection name %q", r.Name)
		}
		rejections[r.Name] = true
	}
	if err := requireV67Names("record", fixture.RequiredRecordNames, records); err != nil {
		t.Fatal(err)
	}
	if err := requireV67Names("rejection", fixture.RequiredRejectionNames, rejections); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func nullableV67Arg(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// TestMigrationV67CheckMatrix migrates a populated V66 database to V67 and
// proves the pre-existing row reads back with every latest-attempt column NULL
// (the migration's no-backfill contract), then round-trips every fixture record
// through the upgraded table and confirms the CHECKs refuse every rejection.
func TestMigrationV67CheckMatrix(t *testing.T) {
	t.Parallel()
	fixture := loadV67Fixture(t)
	ctx := t.Context()
	conn, err := sqlite.OpenConn(filepath.Join(t.TempDir(), "v67.db"), sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	predecessor := sqlitemigration.Schema{
		Migrations:       dbSchema.Migrations[:66],
		MigrationOptions: dbSchema.MigrationOptions[:66],
	}
	if err := sqlitemigration.Migrate(ctx, conn, predecessor); err != nil {
		t.Fatalf("migrate to V66: %v", err)
	}
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('h','h');
INSERT INTO projects(project_hash, canonical_cwd) VALUES('p','/synthetic/v67');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('v67-preexisting','codex','m','h','p',1,2,3,'/synthetic/v67.jsonl','jsonl');`, nil); err != nil {
		t.Fatalf("seed V66 row: %v", err)
	}
	if err := sqlitemigration.Migrate(ctx, conn, dbSchema); err != nil {
		t.Fatalf("migrate to V67: %v", err)
	}

	const selectColumns = `latest_attempt_generation_id, latest_attempt_artifact_hash,
		latest_attempt_indexer_version, latest_attempt_failure_code,
		latest_attempt_failure_message, latest_attempt_captured_at_ms,
		latest_attempt_stats_json`

	readBack := func(id string) [7]*string {
		t.Helper()
		var out [7]*string
		found := false
		if err := sqlitex.ExecuteTransient(conn, `SELECT `+selectColumns+` FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{id},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				found = true
				for i := 0; i < len(out); i++ {
					if stmt.ColumnType(i) != sqlite.TypeNull {
						v := stmt.ColumnText(i)
						out[i] = &v
					}
				}
				return nil
			},
		}); err != nil {
			t.Fatalf("select %s: %v", id, err)
		}
		if !found {
			t.Fatalf("row %s missing after insert", id)
		}
		return out
	}

	pre := readBack("v67-preexisting")
	for i, v := range pre {
		if v != nil {
			t.Fatalf("pre-existing V66 row must read back with every latest-attempt column NULL, column %d = %q", i, *v)
		}
	}

	const insert = `INSERT INTO sessions
		(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format,
		 latest_attempt_generation_id, latest_attempt_artifact_hash, latest_attempt_indexer_version, latest_attempt_failure_code,
		 latest_attempt_failure_message, latest_attempt_captured_at_ms, latest_attempt_stats_json)
		VALUES (?, 'claude-code', 'm', 'h', 'p', 1, 2, 3, '/x.jsonl', 'jsonl', ?, ?, ?, ?, ?, ?, ?)`
	for _, record := range fixture.Records {
		if err := sqlitex.ExecuteTransient(conn, insert, &sqlitex.ExecOptions{
			Args: []any{
				record.SessionID,
				nullableV67Arg(record.GenerationID),
				nullableV67Arg(record.ArtifactHash),
				nullableV67Arg(record.IndexerVersion),
				nullableV67Arg(record.FailureCode),
				nullableV67Arg(record.FailureMessage),
				nullableV67Arg(record.CapturedAtMs),
				nullableV67Arg(record.StatsJSON),
			},
		}); err != nil {
			t.Fatalf("case %q: insert: %v", record.Name, err)
		}
		got := readBack(record.SessionID)
		assertNullableEqual(t, record.Name, "latest_attempt_generation_id", got[0], record.GenerationID)
		assertNullableEqual(t, record.Name, "latest_attempt_artifact_hash", got[1], record.ArtifactHash)
		assertNullableEqual(t, record.Name, "latest_attempt_indexer_version", got[2], record.IndexerVersion)
		assertNullableEqual(t, record.Name, "latest_attempt_failure_code", got[3], record.FailureCode)
		assertNullableEqual(t, record.Name, "latest_attempt_failure_message", got[4], record.FailureMessage)
		assertNullableEqual(t, record.Name, "latest_attempt_captured_at_ms", got[5], record.CapturedAtMs)
		assertNullableEqual(t, record.Name, "latest_attempt_stats_json", got[6], record.StatsJSON)
	}

	for _, rejection := range fixture.Rejections {
		if err := sqlitex.ExecuteTransient(conn, rejection.SQL, nil); err == nil {
			t.Errorf("case %q: expected a latest-attempt CHECK to refuse this insert, it succeeded", rejection.Name)
		}
	}
}
