package store

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/migrations/v40_association_replay.yaml
var migrationV40AssociationReplayFixtureData []byte

const migrationV40AssociationReplayFixturePath = "internal/store/testdata/migrations/v40_association_replay.yaml"

type migrationV40Fixtures struct {
	Cases []migrationV40Fixture `yaml:"cases"`
}

type migrationV40Fixture struct {
	Name                     string                       `yaml:"name"`
	ExpectedAssociationCount int                          `yaml:"expectedAssociationCount"`
	ExpectedReplayCount      int                          `yaml:"expectedReplayCount"`
	ReplayAcknowledgedAt     int64                        `yaml:"replayAcknowledgedAt"`
	Sessions                 []migrationV40SessionFixture `yaml:"sessions"`
}

type migrationV40SessionFixture struct {
	SessionID         ingest.SessionID                `yaml:"sessionID"`
	ProjectHash       string                          `yaml:"projectHash"`
	HostSlug          string                          `yaml:"hostSlug"`
	StartMs           int64                           `yaml:"startMs"`
	Pushed            bool                            `yaml:"pushed"`
	Association       *migrationV40AssociationFixture `yaml:"association,omitempty"`
	ExpectedCandidate bool                            `yaml:"expectedCandidate"`
	ExpectedReplay    bool                            `yaml:"expectedReplay"`
}

type migrationV40AssociationFixture struct {
	CommitHash string `yaml:"commitHash"`
	Subject    string `yaml:"subject"`
	AuthorTime int64  `yaml:"authorTime"`
}

type migrationV40CommitSource struct {
	SessionID  ingest.SessionID
	CommitHash string
	Subject    string
	AuthorTime int64
}

type migrationV40AssociationRow struct {
	ID                 schema.AssociationID
	SessionID          ingest.SessionID
	ObservedCommitHash string
	Subject            string
	AuthorTime         int64
	CreatedAt          int64
}

func loadMigrationV40Fixture(t *testing.T) migrationV40Fixture {
	t.Helper()
	var fixtures migrationV40Fixtures
	decoder := yaml.NewDecoder(bytes.NewReader(migrationV40AssociationReplayFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode committed fixture %s: %v", migrationV40AssociationReplayFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("committed fixture %s must contain exactly one YAML document, trailing decode: %v", migrationV40AssociationReplayFixturePath, err)
	}
	if len(fixtures.Cases) != 1 || len(fixtures.Cases[0].Sessions) != 3 {
		t.Fatalf("committed fixture %s must define one three-session scenario", migrationV40AssociationReplayFixturePath)
	}
	fixture := fixtures.Cases[0]
	if fixture.ExpectedAssociationCount < 1 || fixture.ExpectedReplayCount < 1 || fixture.ReplayAcknowledgedAt <= 0 {
		t.Fatalf("committed fixture %s must define positive association, replay, and acknowledgement expectations", migrationV40AssociationReplayFixturePath)
	}
	associationCount := 0
	replayCount := 0
	for _, row := range fixture.Sessions {
		if row.Association != nil {
			associationCount++
			if row.Association.CommitHash == "" || row.Association.Subject == "" || row.Association.AuthorTime <= 0 {
				t.Fatalf("fixture association for session %s must define commitHash, subject, and positive authorTime", row.SessionID)
			}
		}
		if row.ExpectedReplay {
			replayCount++
			if !row.Pushed || row.Association == nil || !row.ExpectedCandidate {
				t.Fatalf("fixture replay session %s must be pushed, backfilled, and selected as an ordinary candidate", row.SessionID)
			}
		}
	}
	if associationCount != fixture.ExpectedAssociationCount || replayCount != fixture.ExpectedReplayCount {
		t.Fatalf("fixture expectations disagree with association rows: associations=%d/%d replays=%d/%d", associationCount, fixture.ExpectedAssociationCount, replayCount, fixture.ExpectedReplayCount)
	}
	return fixture
}

func migrationV40StoreEntry(t *testing.T, row migrationV40SessionFixture) ingest.StoreEntry {
	t.Helper()
	sessionID, err := ingest.NewSessionID(string(row.SessionID))
	if err != nil {
		t.Fatalf("validate fixture session ID %q: %v", row.SessionID, err)
	}
	projectHash, err := ingest.NewProjectHash(row.ProjectHash)
	if err != nil {
		t.Fatalf("validate fixture project hash %q: %v", row.ProjectHash, err)
	}
	hostSlug, err := ingest.NewHostSlug(row.HostSlug)
	if err != nil {
		t.Fatalf("validate fixture host slug %q: %v", row.HostSlug, err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		t.Fatalf("validate fixture model: %v", err)
	}
	sourcePath, err := ingest.NewResolvedPath("/test/migration-v40/session.jsonl")
	if err != nil {
		t.Fatalf("validate fixture source path: %v", err)
	}
	ingestedAt := row.StartMs + 120000
	return ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sessionID,
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         model,
			HostSlug:      hostSlug,
			Timestamp: ingest.TimestampInfo{
				Start: row.StartMs, End: row.StartMs + 60000, Ingested: &ingestedAt,
			},
			Source: ingest.SourceInfo{FilePath: string(sourcePath), Format: ingest.SourceFormatJSONL},
			Project: ingest.ProjectInfo{
				Hash: projectHash, Name: "migration-v40", FilePath: "/test/migration-v40",
			},
			Stats: ingest.StatsInfo{
				TurnCount: 1, DurationMs: 60000, TokensIn: 100, TokensOut: 50,
			},
			Version: "test",
		},
		Session: ingest.DiscoveredSession{
			SessionID: sessionID, Harness: defaults.HarnessClaudeCode,
			SourcePath: sourcePath, SourceFormat: ingest.SourceFormatJSONL,
		},
	}
}

func migrationV40CommitSources(t *testing.T, s *Store) []migrationV40CommitSource {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatalf("take connection to inspect session_commits: %v", err)
	}
	defer s.PoolForTest().Put(conn)

	var rows []migrationV40CommitSource
	if err := sqlitex.ExecuteTransient(conn, `SELECT session_id, commit_hash, message, author_time
FROM session_commits
ORDER BY session_id, commit_hash`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(3) == sqlite.TypeNull {
				return fmt.Errorf("session_commits row for session %q and commit %q has no author_time", stmt.ColumnText(0), stmt.ColumnText(1))
			}
			rows = append(rows, migrationV40CommitSource{
				SessionID:  ingest.SessionID(stmt.ColumnText(0)),
				CommitHash: stmt.ColumnText(1),
				Subject:    stmt.ColumnText(2),
				AuthorTime: stmt.ColumnInt64(3),
			})
			return nil
		},
	}); err != nil {
		t.Fatalf("read session_commits source rows: %v", err)
	}
	return rows
}

func migrationV40AssociationRows(t *testing.T, s *Store) []migrationV40AssociationRow {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatalf("take connection to inspect session_commit_associations: %v", err)
	}
	defer s.PoolForTest().Put(conn)

	var rows []migrationV40AssociationRow
	if err := sqlitex.ExecuteTransient(conn, `SELECT association_id, session_id, observed_commit_hash, subject, author_time, created_at
FROM session_commit_associations
ORDER BY session_id, observed_commit_hash`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			associationID, err := schema.NewAssociationID(stmt.ColumnText(0))
			if err != nil {
				return fmt.Errorf("validate backfilled association ID %q: %w", stmt.ColumnText(0), err)
			}
			if stmt.ColumnType(4) == sqlite.TypeNull {
				return fmt.Errorf("backfilled association %q has no author_time", associationID)
			}
			rows = append(rows, migrationV40AssociationRow{
				ID:                 associationID,
				SessionID:          ingest.SessionID(stmt.ColumnText(1)),
				ObservedCommitHash: stmt.ColumnText(2),
				Subject:            stmt.ColumnText(3),
				AuthorTime:         stmt.ColumnInt64(4),
				CreatedAt:          stmt.ColumnInt64(5),
			})
			return nil
		},
	}); err != nil {
		t.Fatalf("read backfilled association rows: %v", err)
	}
	return rows
}

func migrationV40PushedAt(t *testing.T, s *Store) map[ingest.SessionID]*int64 {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatalf("take connection to inspect replay state: %v", err)
	}
	defer s.PoolForTest().Put(conn)

	rows := make(map[ingest.SessionID]*int64)
	if err := sqlitex.ExecuteTransient(conn, `SELECT session_id, pushed_at FROM sessions`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			id := ingest.SessionID(stmt.ColumnText(0))
			if stmt.ColumnType(1) == sqlite.TypeNull {
				rows[id] = nil
				return nil
			}
			value := stmt.ColumnInt64(1)
			rows[id] = &value
			return nil
		},
	}); err != nil {
		t.Fatalf("read migrated pushed_at state: %v", err)
	}
	return rows
}

func seedMigrationV40PublicationCursor(t *testing.T, s *Store, sessionIDs []ingest.SessionID, pushedAt int64, license schema.License) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("take connection to seed V40 publication cursor: %v", err)
	}
	defer s.pool.Put(conn)
	var licenseArg any
	if license != "" {
		licenseArg = license.String()
	}
	for _, sessionID := range sessionIDs {
		if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET pushed_at = ?, license_id = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{pushedAt, licenseArg, sessionID.String()}}); err != nil {
			t.Fatalf("seed V40 publication cursor for %s: %v", sessionID, err)
		}
		if changes := conn.Changes(); changes != 1 {
			t.Fatalf("seed V40 publication cursor for %s changed %d rows, want 1", sessionID, changes)
		}
	}
}

func assertMigrationV40Backfill(t *testing.T, s *Store, fixture migrationV40Fixture, beforeSources []migrationV40CommitSource) {
	t.Helper()
	if afterSources := migrationV40CommitSources(t, s); !reflect.DeepEqual(afterSources, beforeSources) {
		t.Errorf("V40 changed legacy session_commits rows:\nbefore: %#v\nafter:  %#v", beforeSources, afterSources)
	}

	rows := migrationV40AssociationRows(t, s)
	if len(rows) != fixture.ExpectedAssociationCount {
		t.Fatalf("V40 backfilled %d association rows, want exactly %d", len(rows), fixture.ExpectedAssociationCount)
	}
	byRelationship := make(map[string]migrationV40AssociationRow, len(rows))
	seenIDs := make(map[schema.AssociationID]struct{}, len(rows))
	for _, row := range rows {
		if err := row.ID.Validate(); err != nil || row.ID == "" {
			t.Errorf("backfilled association ID for session %s commit %s is not a valid opaque ID: %q (%v)", row.SessionID, row.ObservedCommitHash, row.ID, err)
		}
		if _, exists := seenIDs[row.ID]; exists {
			t.Errorf("backfilled association ID %q is reused", row.ID)
		}
		seenIDs[row.ID] = struct{}{}
		if row.CreatedAt <= 0 {
			t.Errorf("backfilled association %q has created_at=%d, want migration metadata timestamp", row.ID, row.CreatedAt)
		}
		byRelationship[string(row.SessionID)+"\x00"+row.ObservedCommitHash] = row
	}
	for _, expected := range fixture.Sessions {
		if expected.Association == nil {
			continue
		}
		key := string(expected.SessionID) + "\x00" + expected.Association.CommitHash
		got, ok := byRelationship[key]
		if !ok {
			t.Errorf("V40 did not backfill association for session %s commit %s", expected.SessionID, expected.Association.CommitHash)
			continue
		}
		if got.Subject != expected.Association.Subject || got.AuthorTime != expected.Association.AuthorTime {
			t.Errorf("backfilled association for session %s commit %s = subject=%q author_time=%d, want subject=%q author_time=%d", expected.SessionID, expected.Association.CommitHash, got.Subject, got.AuthorTime, expected.Association.Subject, expected.Association.AuthorTime)
		}
	}

	pushedAt := migrationV40PushedAt(t, s)
	for _, row := range fixture.Sessions {
		got, exists := pushedAt[row.SessionID]
		if !exists {
			t.Errorf("migrated session %s is missing from replay-state query", row.SessionID)
			continue
		}
		if row.ExpectedReplay && got != nil {
			t.Errorf("V40 replay session %s retained pushed_at=%d, want NULL for one ordinary replay", row.SessionID, *got)
		}
		if !row.ExpectedReplay && row.Pushed && got == nil {
			t.Errorf("unaffected pushed session %s lost pushed_at without a backfilled association", row.SessionID)
		}
		if !row.Pushed && got != nil {
			t.Errorf("previously unpushed session %s gained pushed_at=%d during V40", row.SessionID, *got)
		}
	}
}

// TestMigrationV40ReplaysBackfilledAssociations uses the real migration and
// normal candidate query to prove the replay is both scoped and consumed once.
func TestMigrationV40ReplaysBackfilledAssociations(t *testing.T) {
	t.Parallel()
	fixture := loadMigrationV40Fixture(t)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v40-replay.db")

	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{PrepareConn: preparePragmas})
	if err != nil {
		t.Fatalf("open pre-V40 pool: %v", err)
	}
	conn, err := pool.Take(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("take pre-V40 connection: %v", err)
	}
	schemaV39 := sqlitemigration.Schema{
		Migrations:       dbSchema.Migrations[:39],
		MigrationOptions: dbSchema.MigrationOptions[:39],
	}
	if err := sqlitemigration.Migrate(ctx, conn, schemaV39); err != nil {
		pool.Put(conn)
		pool.Close()
		t.Fatalf("migrate through V39: %v", err)
	}
	// The production InsertSessions path below writes through the single,
	// schema-CURRENT sqlInsertSession statement (it has no per-version
	// variant), which as of V45 always sets session_origin. A V39-frozen
	// table has no such column yet, so add it here as a TEMPORARY column —
	// user_version is left at 39, untouched — purely so the real write path
	// can run against this intentionally-old snapshot. It is dropped again
	// below, before the normal migration resumes, so V45's own ALTER TABLE
	// ADD COLUMN still runs untouched and unaware this ever happened.
	if err := sqlitex.ExecuteTransient(conn, `ALTER TABLE sessions ADD COLUMN session_origin TEXT NOT NULL DEFAULT 'unknown'`, nil); err != nil {
		pool.Put(conn)
		pool.Close()
		t.Fatalf("add temporary session_origin column for the V39 snapshot: %v", err)
	}
	pool.Put(conn)
	pool.Close()

	preMigrationStore, err := Open(dbPath, WithSkipMigrations())
	if err != nil {
		t.Fatalf("open V39 store: %v", err)
	}
	for _, row := range fixture.Sessions {
		entry := migrationV40StoreEntry(t, row)
		if err := preMigrationStore.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
			t.Fatalf("insert fixture session %s: %v", row.SessionID, err)
		}
		if row.Pushed {
			seedMigrationV40PublicationCursor(t, preMigrationStore, []ingest.SessionID{row.SessionID}, row.StartMs+500000, schema.LicenseCCBY)
		}
		if row.Association != nil {
			c, err := preMigrationStore.PoolForTest().Take(ctx)
			if err != nil {
				t.Fatalf("take connection for fixture association %s: %v", row.SessionID, err)
			}
			err = sqlitex.ExecuteTransient(c, `INSERT INTO session_commits (session_id, commit_hash, message, author_time) VALUES (?, ?, ?, ?)`, &sqlitex.ExecOptions{
				Args: []any{string(row.SessionID), row.Association.CommitHash, row.Association.Subject, row.Association.AuthorTime},
			})
			preMigrationStore.PoolForTest().Put(c)
			if err != nil {
				t.Fatalf("insert fixture association source %s: %v", row.SessionID, err)
			}
		}
	}
	beforeSources := migrationV40CommitSources(t, preMigrationStore)
	if err := preMigrationStore.Close(); err != nil {
		t.Fatalf("close V39 store: %v", err)
	}

	// Drop the temporary column added above, restoring the exact V39 shape
	// before the real migration path resumes at user_version 39. Without
	// this, V45's own ALTER TABLE ADD COLUMN session_origin would fail with
	// a duplicate-column error when the migration below reaches it.
	dropConn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open connection to drop the temporary column: %v", err)
	}
	if err := sqlitex.ExecuteTransient(dropConn, `ALTER TABLE sessions DROP COLUMN session_origin`, nil); err != nil {
		dropConn.Close()
		t.Fatalf("drop temporary session_origin column: %v", err)
	}
	if err := dropConn.Close(); err != nil {
		t.Fatalf("close drop-column connection: %v", err)
	}

	migratedStore, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open and migrate store: %v", err)
	}
	defer migratedStore.Close()
	assertMigrationV40Backfill(t, migratedStore, fixture, beforeSources)

	candidates, err := migratedStore.UnpushedSessions(ctx)
	if err != nil {
		t.Fatalf("select ordinary push candidates: %v", err)
	}
	gotCandidates := make(map[ingest.SessionID]bool, len(candidates))
	for _, candidate := range candidates {
		gotCandidates[ingest.SessionID(candidate.SessionID)] = true
	}
	var replayed []ingest.SessionID
	for _, row := range fixture.Sessions {
		if gotCandidates[row.SessionID] != row.ExpectedCandidate {
			t.Errorf("session %s candidate = %t, want %t", row.SessionID, gotCandidates[row.SessionID], row.ExpectedCandidate)
		}
		if row.ExpectedReplay && !gotCandidates[row.SessionID] {
			t.Errorf("V40 replay session %s is absent from the ordinary candidate set", row.SessionID)
		}
		if row.ExpectedCandidate {
			replayed = append(replayed, row.SessionID)
		}
	}
	seedMigrationV40PublicationCursor(t, migratedStore, replayed, fixture.ReplayAcknowledgedAt, schema.LicenseCCBY)
	afterMark, err := migratedStore.UnpushedSessions(ctx)
	if err != nil {
		t.Fatalf("select candidates after publication cursor acknowledgement: %v", err)
	}
	if len(afterMark) != 0 {
		t.Errorf("ordinary candidates after publication cursor acknowledgement = %d, want 0", len(afterMark))
	}
	for _, row := range fixture.Sessions {
		if !row.ExpectedReplay {
			continue
		}
		pushedAt := migrationV40PushedAt(t, migratedStore)[row.SessionID]
		if pushedAt == nil || *pushedAt != fixture.ReplayAcknowledgedAt {
			t.Errorf("replay acknowledgement for session %s = %v, want %d", row.SessionID, pushedAt, fixture.ReplayAcknowledgedAt)
		}
	}
}
