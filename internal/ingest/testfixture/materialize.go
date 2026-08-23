package testfixture

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// MaterializedSource is a synthetic source confined to a test-owned temporary
// directory. Path is the database or corrupt-file path passed to production readers.
type MaterializedSource struct {
	Path     string
	root     string
	expected CatalogExpectation
}

// ExpectedCatalog returns immutable catalog evidence declared by the named fixture.
func (s MaterializedSource) ExpectedCatalog() CatalogExpectation {
	return CatalogExpectation{
		tables:  append([]string(nil), s.expected.tables...),
		indexes: append([]string(nil), s.expected.indexes...),
		seq:     s.expected.seq,
	}
}

// SourceSnapshot records exact database and existing sidecar bytes.
type SourceSnapshot struct {
	files map[string]fileSnapshot
}

type fileSnapshot struct {
	present bool
	data    []byte
}

// MaterializeByName creates a named synthetic source below a helper-created
// TempDir. Callers cannot bypass the strict embedded corpus with inline cases.
// SQLite setup connections are closed before the source is returned.
func MaterializeByName(t testing.TB, name string) MaterializedSource {
	t.Helper()
	fixtureCase, err := caseByName(name)
	if err != nil {
		t.Fatalf("materialize named synthetic OpenCode source: %v", err)
	}
	return materialize(t, fixtureCase)
}

func materialize(t testing.TB, fixtureCase caseSpec) MaterializedSource {
	t.Helper()
	if err := fixtureCase.validate(); err != nil {
		t.Fatalf("materialize synthetic OpenCode source: %v", err)
	}
	root := t.TempDir()
	destination := filepath.Join(root, filepath.FromSlash(fixtureCase.LogicalPath))
	if err := requireConfinedPath(root, destination); err != nil {
		t.Fatalf("materialize synthetic OpenCode source %q: %v", fixtureCase.Name, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatalf("materialize synthetic OpenCode source %q: create test-owned parent directory: %v", fixtureCase.Name, err)
	}

	expected := CatalogExpectation{
		tables:  append([]string(nil), fixtureCase.ExpectedCatalog.Tables...),
		indexes: append([]string(nil), fixtureCase.ExpectedCatalog.Indexes...),
		seq:     fixtureCase.ExpectedCatalog.Seq,
	}
	for index := 0; index < fixtureCase.CatalogPadding.Tables; index++ {
		expected.tables = append(expected.tables, fmt.Sprintf("padding_table_%03d", index))
	}
	for index := 0; index < fixtureCase.CatalogPadding.Indexes; index++ {
		expected.indexes = append(expected.indexes, fmt.Sprintf("padding_index_%03d", index))
	}
	if fixtureCase.Format == sourceFormatCorrupt {
		if err := os.WriteFile(destination, corruptBytes(fixtureCase.Corruption), 0o600); err != nil {
			t.Fatalf("materialize synthetic OpenCode source %q: write corrupt synthetic file: %v", fixtureCase.Name, err)
		}
		return MaterializedSource{Path: destination, root: root, expected: expected}
	}

	conn, err := sqlite.OpenConn(destination, sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("materialize synthetic OpenCode source %q: open setup database: %v", fixtureCase.Name, err)
	}
	setupErr := buildSQLite(conn, fixtureCase)
	closeErr := conn.Close()
	if setupErr != nil {
		t.Fatalf("materialize synthetic OpenCode source %q: construct database: %v", fixtureCase.Name, setupErr)
	}
	if closeErr != nil {
		t.Fatalf("materialize synthetic OpenCode source %q: close setup database before production read: %v", fixtureCase.Name, closeErr)
	}
	return MaterializedSource{Path: destination, root: root, expected: expected}
}

func corruptBytes(kind corruptionKind) []byte {
	switch kind {
	case corruptionNonSQLite:
		return []byte("synthetic non-SQLite source\n")
	case corruptionTruncatedSQLite:
		return []byte("SQLite format 3\x00truncated")
	default:
		return nil
	}
}

// SnapshotSource captures the database plus the existence and exact bytes of
// its WAL and shared-memory sidecars.
func SnapshotSource(t testing.TB, source MaterializedSource) SourceSnapshot {
	t.Helper()
	snapshot, err := snapshotSource(source)
	if err != nil {
		t.Fatalf("snapshot synthetic OpenCode source before production read: %v", err)
	}
	return snapshot
}

// AssertUnchanged compares a source with an earlier database-and-sidecar snapshot.
func AssertUnchanged(t testing.TB, source MaterializedSource, before SourceSnapshot) {
	t.Helper()
	after, err := snapshotSource(source)
	if err != nil {
		t.Fatalf("snapshot synthetic OpenCode source after production read: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		beforeFile, ok := before.files[suffix]
		if !ok {
			t.Errorf("assert synthetic OpenCode source unchanged: baseline does not cover %q", suffix)
			continue
		}
		afterFile := after.files[suffix]
		if beforeFile.present != afterFile.present {
			t.Errorf("assert synthetic OpenCode source unchanged: %s presence changed from %t to %t", snapshotLabel(suffix), beforeFile.present, afterFile.present)
			continue
		}
		if beforeFile.present && !bytes.Equal(beforeFile.data, afterFile.data) {
			t.Errorf("assert synthetic OpenCode source unchanged: %s bytes changed", snapshotLabel(suffix))
		}
	}
}

func snapshotSource(source MaterializedSource) (SourceSnapshot, error) {
	if err := requireConfinedPath(source.root, source.Path); err != nil {
		return SourceSnapshot{}, fmt.Errorf("refuse to read a path outside the materializer-owned test root: %w", err)
	}
	files := make(map[string]fileSnapshot, 3)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		filename := source.Path + suffix
		if err := requireReadableConfinedPath(source.root, filename, suffix != ""); err != nil {
			return SourceSnapshot{}, fmt.Errorf("validate %s path before snapshot: %w", snapshotLabel(suffix), err)
		}
		data, err := os.ReadFile(filename)
		if errors.Is(err, os.ErrNotExist) {
			files[suffix] = fileSnapshot{}
			continue
		}
		if err != nil {
			return SourceSnapshot{}, fmt.Errorf("read %s %q: %w", snapshotLabel(suffix), filename, err)
		}
		files[suffix] = fileSnapshot{present: true, data: data}
	}
	return SourceSnapshot{files: files}, nil
}

func requireReadableConfinedPath(root, candidate string, allowMissing bool) error {
	if err := requireConfinedPath(root, candidate); err != nil {
		return err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve materializer-owned test root %q: %w", root, err)
	}
	info, err := os.Lstat(candidate)
	if errors.Is(err, os.ErrNotExist) && allowMissing {
		resolvedParent, parentErr := filepath.EvalSymlinks(filepath.Dir(candidate))
		if parentErr != nil {
			return fmt.Errorf("resolve parent of absent sidecar %q: %w", candidate, parentErr)
		}
		return requireConfinedPath(resolvedRoot, filepath.Join(resolvedParent, filepath.Base(candidate)))
	}
	if err != nil {
		return fmt.Errorf("inspect source file %q: %w", candidate, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source file %q is a symbolic link; snapshots only read regular files created by the materializer", candidate)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source file %q has mode %s; snapshots only read regular files", candidate, info.Mode())
	}
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return fmt.Errorf("resolve source file %q: %w", candidate, err)
	}
	return requireConfinedPath(resolvedRoot, resolvedCandidate)
}

func requireConfinedPath(root, candidate string) error {
	if root == "" || candidate == "" {
		return errors.New("test root and source path are required")
	}
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve test root %q: %w", root, err)
	}
	cleanCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return fmt.Errorf("resolve source path %q: %w", candidate, err)
	}
	relative, err := filepath.Rel(cleanRoot, cleanCandidate)
	if err != nil {
		return fmt.Errorf("compare source path %q with test root %q: %w", candidate, root, err)
	}
	if relative == "." || relative == ".." || filepath.IsAbs(relative) || len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
		return fmt.Errorf("source path %q is not a file beneath test root %q", candidate, root)
	}
	return nil
}

func buildSQLite(conn *sqlite.Conn, fixtureCase caseSpec) error {
	journalSQL := "PRAGMA journal_mode=DELETE;"
	if fixtureCase.JournalMode == journalWAL {
		journalSQL = "PRAGMA journal_mode=WAL;"
	}
	if err := sqlitex.Execute(conn, journalSQL, nil); err != nil {
		return fmt.Errorf("set %s journal mode during setup: %w", fixtureCase.JournalMode, err)
	}

	if err := createSchema(conn, fixtureCase.Schema, fixtureCase.SessionClock); err != nil {
		return err
	}
	if (fixtureCase.Schema == schemaLegacy || fixtureCase.Schema == schemaCurrent || fixtureCase.Schema == schemaHybrid) && fixtureCase.SessionClock != sessionClockAbsent {
		if err := insertSessionRows(conn, fixtureCase); err != nil {
			return err
		}
	}
	if err := insertLegacyMessages(conn, fixtureCase.LegacyMessages); err != nil {
		return err
	}
	if err := insertLegacyParts(conn, fixtureCase.LegacyParts); err != nil {
		return err
	}
	if err := insertCurrentMessages(conn, fixtureCase.CurrentMessages); err != nil {
		return err
	}
	if err := deleteSessionRows(conn, fixtureCase.DeletedSessionRows); err != nil {
		return err
	}
	if err := createHistoryTables(conn, fixtureCase.IgnoredHistory); err != nil {
		return err
	}
	if err := applyCatalogPadding(conn, fixtureCase.CatalogPadding); err != nil {
		return err
	}
	if err := insertHistoryRows(conn, fixtureCase.IgnoredHistory); err != nil {
		return err
	}
	return nil
}

func applyCatalogPadding(conn *sqlite.Conn, padding catalogPaddingSpec) error {
	for index := 0; index < padding.Tables; index++ {
		statement := fmt.Sprintf("CREATE TABLE padding_table_%03d (id INTEGER)", index)
		if err := sqlitex.ExecuteTransient(conn, statement, nil); err != nil {
			return fmt.Errorf("create synthetic catalog padding table %d: %w", index, err)
		}
	}
	for index := 0; index < padding.Columns; index++ {
		statement := fmt.Sprintf("ALTER TABLE session_message ADD COLUMN padding_column_%03d TEXT", index)
		if err := sqlitex.ExecuteTransient(conn, statement, nil); err != nil {
			return fmt.Errorf("create synthetic catalog padding column %d: %w", index, err)
		}
	}
	for index := 0; index < padding.Indexes; index++ {
		statement := fmt.Sprintf("CREATE INDEX padding_index_%03d ON session_message(type)", index)
		if err := sqlitex.ExecuteTransient(conn, statement, nil); err != nil {
			return fmt.Errorf("create synthetic catalog padding index %d: %w", index, err)
		}
	}
	return nil
}

func createSchema(conn *sqlite.Conn, schema schemaKind, clock sessionClockMode) error {
	sessionSchema := sessionSchemaSQL
	if clock == sessionClockAbsent {
		// A session table without the time_updated column has no usable clock,
		// so discovery falls back to the database and WAL mtime floor.
		sessionSchema = sessionSchemaNoClockSQL
	}
	var script string
	switch schema {
	case schemaEmpty:
		script = `CREATE TABLE fixture_header_seed (id INTEGER); DROP TABLE fixture_header_seed;`
	case schemaLegacy:
		script = sessionSchema + legacySchemaSQL
	case schemaCurrent:
		script = sessionSchema + currentSchemaSQL
	case schemaHybrid:
		script = sessionSchema + legacySchemaSQL + currentSchemaSQL
	case schemaCurrentMissingSeq:
		script = currentMissingSeqSchemaSQL
	case schemaCurrentNullableSeq:
		script = currentNullableSeqSchemaSQL
	case schemaCurrentPartialSeq:
		script = currentPartialSeqSchemaSQL
	case schemaUnsupported:
		script = `CREATE TABLE future_projection (id TEXT PRIMARY KEY, payload BLOB NOT NULL);`
	default:
		return fmt.Errorf("create synthetic schema: unsupported validated schema kind %q", schema)
	}
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		return fmt.Errorf("create synthetic %s schema: %w", schema, err)
	}
	return nil
}

// sessionSchemaSQL mirrors the upstream session table columns that Peasant
// reads: the parent link and the per-session update clock.
const sessionSchemaSQL = `
CREATE TABLE session (
  id TEXT PRIMARY KEY,
  parent_id TEXT,
  time_created INTEGER NOT NULL DEFAULT 0,
  time_updated INTEGER NOT NULL DEFAULT 0
);
`

// sessionSchemaNoClockSQL is an older session table with no update clock, so
// discovery reports no usable clock and every session falls back to the
// database and WAL mtime floor. It exercises the floor path end to end.
const sessionSchemaNoClockSQL = `
CREATE TABLE session (
  id TEXT PRIMARY KEY,
  parent_id TEXT,
  time_created INTEGER NOT NULL DEFAULT 0
);
`

const legacySchemaSQL = `
CREATE TABLE message (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  data TEXT NOT NULL
);
CREATE INDEX message_session_time_idx ON message(session_id, time_created, id);
CREATE TABLE part (
  id TEXT PRIMARY KEY,
  message_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  data TEXT NOT NULL
);
CREATE INDEX part_message_id_idx ON part(message_id, id);
`

const currentSchemaSQL = `
CREATE TABLE session_message (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  type TEXT NOT NULL,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  data TEXT NOT NULL,
  seq INTEGER NOT NULL,
  FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message(session_id, seq);
CREATE INDEX session_message_seq_idx ON session_message(seq);
`

const currentMissingSeqSchemaSQL = `
CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT);
CREATE TABLE session_message (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  type TEXT NOT NULL,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  data TEXT NOT NULL,
  FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE
);
CREATE INDEX session_message_session_idx ON session_message(session_id);
`

const currentNullableSeqSchemaSQL = `
CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT);
CREATE TABLE session_message (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  type TEXT NOT NULL,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  data TEXT NOT NULL,
  seq INTEGER,
  FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message(session_id, seq);
`

const currentPartialSeqSchemaSQL = `
CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT);
CREATE TABLE session_message (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  type TEXT NOT NULL,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  data TEXT NOT NULL,
  seq INTEGER NOT NULL,
  FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX session_message_partial_seq_idx ON session_message(session_id, seq) WHERE type = 'message';
`

func insertLegacyMessages(conn *sqlite.Conn, rows []legacyMessage) error {
	const query = `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?1, ?2, ?3, ?4, ?5);`
	for _, row := range rows {
		if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: []any{row.ID, row.SessionID, row.TimeCreated, row.TimeUpdated, row.Data}}); err != nil {
			return fmt.Errorf("insert synthetic legacy message %q with explicit columns: %w", row.ID, err)
		}
	}
	return nil
}

func insertLegacyParts(conn *sqlite.Conn, rows []legacyPart) error {
	const query = `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?1, ?2, ?3, ?4, ?5, ?6);`
	for _, row := range rows {
		if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: []any{row.ID, row.MessageID, row.SessionID, row.TimeCreated, row.TimeUpdated, row.Data}}); err != nil {
			return fmt.Errorf("insert synthetic legacy part %q with explicit columns: %w", row.ID, err)
		}
	}
	return nil
}

// insertSessionRows writes one session row per distinct session id. The
// session clock mirrors upstream: time_updated is the newest row time of the
// session, so a later row change or an upstream revert moves it.
func insertSessionRows(conn *sqlite.Conn, fixtureCase caseSpec) error {
	type clock struct{ created, updated int64 }
	clocks := make(map[string]clock)
	order := make([]string, 0)
	observe := func(sessionID string, created, updated int64) {
		value, exists := clocks[sessionID]
		if !exists {
			order = append(order, sessionID)
			value = clock{created: created, updated: updated}
		}
		if created < value.created {
			value.created = created
		}
		if updated > value.updated {
			value.updated = updated
		}
		if created > value.updated {
			value.updated = created
		}
		clocks[sessionID] = value
	}
	for _, row := range fixtureCase.LegacyMessages {
		observe(row.SessionID, row.TimeCreated, row.TimeUpdated)
	}
	for _, row := range fixtureCase.LegacyParts {
		observe(row.SessionID, row.TimeCreated, row.TimeUpdated)
	}
	for _, row := range fixtureCase.CurrentMessages {
		observe(row.SessionID, row.TimeCreated, row.TimeUpdated)
	}
	for _, sessionID := range order {
		value := clocks[sessionID]
		updated := value.updated
		if fixtureCase.SessionClock == sessionClockLagging {
			// The clock lags the session's row times, so the newest row time,
			// not the clock, is the changed time and a row change is still seen.
			updated = value.created
		}
		if err := sqlitex.Execute(conn, `INSERT INTO session (id, time_created, time_updated) VALUES (?1, ?2, ?3);`, &sqlitex.ExecOptions{Args: []any{sessionID, value.created, updated}}); err != nil {
			return fmt.Errorf("insert synthetic session %q with explicit columns: %w", sessionID, err)
		}
	}
	return nil
}

// deleteSessionRows removes the named session rows while leaving their message
// and session_message rows in place. Foreign keys stay disabled during the
// delete so the cascade does not remove the historical rows the deletion case
// relies on, modelling a session OpenCode dropped from its session list while
// its rows linger.
func deleteSessionRows(conn *sqlite.Conn, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := sqlitex.Execute(conn, `PRAGMA foreign_keys=OFF;`, nil); err != nil {
		return fmt.Errorf("disable foreign keys before removing synthetic session rows: %w", err)
	}
	for _, id := range ids {
		if err := sqlitex.Execute(conn, `DELETE FROM session WHERE id = ?1;`, &sqlitex.ExecOptions{Args: []any{id}}); err != nil {
			return fmt.Errorf("remove synthetic session row %q: %w", id, err)
		}
	}
	return nil
}

func insertCurrentMessages(conn *sqlite.Conn, rows []currentMessage) error {
	for _, row := range rows {
		const query = `INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7);`
		if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: []any{row.ID, row.SessionID, row.Type, row.TimeCreated, row.TimeUpdated, row.Data, row.Seq}}); err != nil {
			return fmt.Errorf("insert synthetic current message %q with explicit columns: %w", row.ID, err)
		}
	}
	return nil
}

const historyTableSchema = `(
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  stable_id TEXT NOT NULL,
  time_created INTEGER NOT NULL,
  data TEXT NOT NULL
);`

func createHistoryTables(conn *sqlite.Conn, rows []historyRow) error {
	if hasHistoryKind(rows, historyEvent) {
		if err := createHistoryTable(conn, historyEvent); err != nil {
			return err
		}
	}
	if hasHistoryKind(rows, historyDelta) {
		if err := createHistoryTable(conn, historyDelta); err != nil {
			return err
		}
	}
	if hasHistoryKind(rows, historyInput) {
		if err := createHistoryTable(conn, historyInput); err != nil {
			return err
		}
	}
	if hasHistoryKind(rows, historyContext) {
		if err := createHistoryTable(conn, historyContext); err != nil {
			return err
		}
	}
	if hasHistoryKind(rows, historyMigration) {
		if err := createHistoryTable(conn, historyMigration); err != nil {
			return err
		}
	}
	return nil
}

func createHistoryTable(conn *sqlite.Conn, kind historyKind) error {
	query := "CREATE TABLE " + historyTableName(kind) + " " + historyTableSchema
	if err := sqlitex.Execute(conn, query, nil); err != nil {
		return fmt.Errorf("create synthetic ignored %s history table: %w", kind, err)
	}
	return nil
}

func insertHistoryRows(conn *sqlite.Conn, rows []historyRow) error {
	for _, row := range rows {
		query := "INSERT INTO " + historyTableName(row.Kind) + " (id, session_id, stable_id, time_created, data) VALUES (?1, ?2, ?3, ?4, ?5);"
		if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: []any{row.ID, row.SessionID, row.StableID, row.TimeCreated, row.Data}}); err != nil {
			return fmt.Errorf("insert synthetic ignored %s history row %q with explicit columns: %w", row.Kind, row.ID, err)
		}
	}
	return nil
}

func hasHistoryKind(rows []historyRow, kind historyKind) bool {
	for _, row := range rows {
		if row.Kind == kind {
			return true
		}
	}
	return false
}

func historyTableName(kind historyKind) string {
	switch kind {
	case historyEvent:
		return "event"
	case historyDelta:
		return "delta"
	case historyInput:
		return "input"
	case historyContext:
		return "context"
	case historyMigration:
		return "migration"
	default:
		panic(fmt.Sprintf("validated history kind %q has no static table", kind))
	}
}

func snapshotLabel(suffix string) string {
	switch suffix {
	case "":
		return "database"
	case "-wal":
		return "WAL sidecar"
	case "-shm":
		return "shared-memory sidecar"
	default:
		return suffix
	}
}
