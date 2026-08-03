package store

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/migrations/v42_strike_harness.yaml
var migrationV42FixtureData []byte

const migrationV42FixturePath = "internal/store/testdata/migrations/v42_strike_harness.yaml"

type migrationV42Fixtures struct {
	Cases []migrationV42Fixture `yaml:"cases"`
}

type migrationV42Fixture struct {
	Name            string `yaml:"name"`
	SessionID       string `yaml:"sessionId"`
	ProjectHash     string `yaml:"projectHash"`
	OpaqueHostID    string `yaml:"opaqueHostId"`
	HostSlug        string `yaml:"hostSlug"`
	AssociationID   string `yaml:"associationId"`
	CommitHash      string `yaml:"commitHash"`
	AnnotationID    string `yaml:"annotationId"`
	AnnotationType  string `yaml:"annotationType"`
	Annotator       string `yaml:"annotator"`
	VillageHost     string `yaml:"villageHost"`
	TranscriptID    string `yaml:"transcriptId"`
	ValidLicense    string `yaml:"validLicense"`
	InvalidLicense  string `yaml:"invalidLicense"`
	StrikeSessionID string `yaml:"strikeSessionId"`
}

func loadMigrationV42Fixture(t *testing.T) migrationV42Fixture {
	t.Helper()
	var fixtures migrationV42Fixtures
	decoder := yaml.NewDecoder(bytes.NewReader(migrationV42FixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode committed fixture %s: %v", migrationV42FixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("committed fixture %s must contain exactly one YAML document, trailing decode: %v", migrationV42FixturePath, err)
	}
	if len(fixtures.Cases) != 1 {
		t.Fatalf("committed fixture %s must define one V41-to-V42 scenario, got %d", migrationV42FixturePath, len(fixtures.Cases))
	}
	fixture := fixtures.Cases[0]
	if fixture.Name == "" || fixture.SessionID == "" || fixture.ProjectHash == "" || fixture.OpaqueHostID == "" ||
		fixture.AssociationID == "" || fixture.AnnotationID == "" || fixture.ValidLicense == "" || fixture.InvalidLicense == "" {
		t.Fatalf("committed fixture %s is missing required migration identities", migrationV42FixturePath)
	}
	return fixture
}

func TestMigrationV42PreservesV41RelationshipsAndClosedChecks(t *testing.T) {
	t.Parallel()
	fixture := loadMigrationV42Fixture(t)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v42-upgrade.db")
	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{PoolSize: 1, PrepareConn: preparePragmas})
	if err != nil {
		t.Fatalf("open V42 migration pool: %v", err)
	}
	defer pool.Close()
	conn, err := pool.Take(ctx)
	if err != nil {
		t.Fatalf("take V42 migration connection: %v", err)
	}
	defer pool.Put(conn)

	schemaV41 := sqlitemigration.Schema{
		Migrations:       dbSchema.Migrations[:41],
		MigrationOptions: dbSchema.MigrationOptions[:41],
	}
	if err := sqlitemigration.Migrate(ctx, conn, schemaV41); err != nil {
		t.Fatalf("migrate fixture through V41: %v", err)
	}

	executeMigrationV42Fixture(t, conn, `INSERT INTO projects (project_hash, canonical_cwd) VALUES (?, '/fixture/project')`, fixture.ProjectHash)
	executeMigrationV42Fixture(t, conn, `INSERT INTO host_slugs (opaque_id, host_slug) VALUES (?, ?)`, fixture.OpaqueHostID, fixture.HostSlug)
	executeMigrationV42Fixture(t, conn, `INSERT INTO sessions (
		session_id, model_harness, model_id, opaque_host_id, project_hash,
		start_ms, end_ms, ingested_ms, source_path, source_format, license_id
	) VALUES (?, 'claude-code', 'fixture-model', ?, ?, 1, 2, 3, '/fixture/session.jsonl', 'jsonl', ?)`,
		fixture.SessionID, fixture.OpaqueHostID, fixture.ProjectHash, fixture.ValidLicense)
	executeMigrationV42Fixture(t, conn, `INSERT INTO session_commit_associations (
		association_id, session_id, observed_commit_hash, subject, author_time, created_at
	) VALUES (?, ?, ?, 'preserve through V42', 4, 5)`, fixture.AssociationID, fixture.SessionID, fixture.CommitHash)
	executeMigrationV42Fixture(t, conn, `INSERT INTO annotations (
		id, target_kind_id, annotation_type_id, annotator_id, value, is_primary, created_at
	) VALUES (?, 5,
		(SELECT id FROM annotation_types WHERE type_id = ?),
		(SELECT id FROM annotators WHERE name = ?),
		'resolved', 0, 6)`, fixture.AnnotationID, fixture.AnnotationType, fixture.Annotator)
	executeMigrationV42Fixture(t, conn, `INSERT INTO annotation_target_associations (annotation_id, association_id) VALUES (?, ?)`, fixture.AnnotationID, fixture.AssociationID)
	executeMigrationV42Fixture(t, conn, `INSERT INTO pulled_transcripts (
		village_host, transcript_id, owner_user_id, owner_username, content_hash,
		visibility, pull_dir, first_pulled_at, last_pulled_at, license_id
	) VALUES (?, ?, 'owner-id', 'owner', 'content-hash', 'public', '/fixture/pull', 7, 8, ?)`,
		fixture.VillageHost, fixture.TranscriptID, fixture.ValidLicense)

	if err := sqlitemigration.Migrate(ctx, conn, dbSchema); err != nil {
		t.Fatalf("migrate fixture from V41 through V42: %v", err)
	}
	if got := scalarInt(t, conn, `PRAGMA user_version`); got < 42 {
		t.Errorf("user_version = %d, want at least 42", got)
	}
	if got := scalarText(t, conn, `SELECT license_id FROM sessions LIMIT 1`); got != fixture.ValidLicense {
		t.Errorf("preserved session license = %q, want %q", got, fixture.ValidLicense)
	}
	if got := scalarInt(t, conn, `SELECT COUNT(*) FROM annotation_target_associations ata
		JOIN session_commit_associations sca ON sca.association_id = ata.association_id
		WHERE ata.annotation_id = '`+fixture.AnnotationID+`' AND sca.session_id = '`+fixture.SessionID+`'`); got != 1 {
		t.Errorf("preserved association annotation rows = %d, want 1", got)
	}
	if got := scalarInt(t, conn, `SELECT COUNT(*) FROM pragma_foreign_key_check`); got != 0 {
		t.Errorf("foreign_key_check returned %d violations after V42", got)
	}
	if got := scalarText(t, conn, `SELECT license_id FROM pulled_transcripts LIMIT 1`); got != fixture.ValidLicense {
		t.Errorf("preserved pulled transcript license = %q, want %q", got, fixture.ValidLicense)
	}

	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions (
		session_id, model_harness, model_id, opaque_host_id, project_hash,
		start_ms, end_ms, ingested_ms, source_path, source_format, license_id
	) VALUES (?, 'strike', 'fixture-model', ?, ?, 10, 11, 12, '/fixture/strike.jsonl', 'jsonl', ?)`, &sqlitex.ExecOptions{
		Args: []any{fixture.StrikeSessionID, fixture.OpaqueHostID, fixture.ProjectHash, fixture.ValidLicense},
	}); err != nil {
		t.Errorf("V42 sessions CHECK rejected Strike with a valid license: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO daily_summary_harness (date_utc, model_harness) VALUES ('2026-07-28', 'strike')`, nil); err != nil {
		t.Errorf("V42 daily_summary_harness CHECK rejected Strike: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET license_id = ? WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{fixture.InvalidLicense, fixture.SessionID},
	}); err == nil {
		t.Error("V42 sessions CHECK accepted an invalid license")
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE pulled_transcripts SET license_id = ? WHERE village_host = ? AND transcript_id = ?`, &sqlitex.ExecOptions{
		Args: []any{fixture.InvalidLicense, fixture.VillageHost, fixture.TranscriptID},
	}); err == nil {
		t.Error("V42 upgrade left pulled_transcripts accepting an invalid license")
	}
}

func executeMigrationV42Fixture(t *testing.T, conn *sqlite.Conn, query string, args ...any) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("seed V42 migration fixture: %v\nquery: %s", err, query)
	}
}
