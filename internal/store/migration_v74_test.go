package store

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/conversation_turn_count_migration.yaml
var conversationTurnCountMigrationYAML []byte

type conversationTurnCountMigrationSession struct {
	Name             string `yaml:"name"`
	SessionID        string `yaml:"sessionID"`
	ActiveGeneration bool   `yaml:"activeGeneration"`
	LegacyTurnCount  *int   `yaml:"legacyTurnCount"`
	Entries          []struct {
		Index     int    `yaml:"index"`
		Role      string `yaml:"role"`
		EntryType string `yaml:"entryType"`
		Depth     int    `yaml:"depth"`
		Content   string `yaml:"content"`
	} `yaml:"entries"`
	Expected          int  `yaml:"expected"`
	ReportedEntryLess bool `yaml:"reportedEntryLess"`
}

type conversationTurnCountMigrationFixture struct {
	ExpectedCaseCount int                                     `yaml:"expectedCaseCount"`
	RequiredNames     []string                                `yaml:"requiredNames"`
	Sessions          []conversationTurnCountMigrationSession `yaml:"sessions"`
}

func loadConversationTurnCountMigrationFixture(t *testing.T) conversationTurnCountMigrationFixture {
	t.Helper()
	var fixture conversationTurnCountMigrationFixture
	decoder := yaml.NewDecoder(bytes.NewReader(conversationTurnCountMigrationYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("conversation turn count migration fixture must contain exactly one document: %v", err)
	}
	if fixture.ExpectedCaseCount != len(fixture.RequiredNames) || fixture.ExpectedCaseCount != len(fixture.Sessions) {
		t.Fatalf("conversation turn count migration fixture declares %d required names, %d cases, %d sessions", len(fixture.RequiredNames), fixture.ExpectedCaseCount, len(fixture.Sessions))
	}
	seen := make(map[string]bool, len(fixture.Sessions))
	for _, name := range fixture.RequiredNames {
		if strings.TrimSpace(name) == "" {
			t.Fatal("conversation turn count migration fixture declares a blank required name")
		}
		if seen[name] {
			t.Fatalf("conversation turn count migration fixture repeats required name %q", name)
		}
		seen[name] = true
	}
	for _, session := range fixture.Sessions {
		if session.Name == "" || !seen[session.Name] {
			t.Fatalf("conversation turn count migration fixture has unlisted session %q", session.Name)
		}
	}
	return fixture
}

func seedConversationTurnCountSessions(t *testing.T, conn *sqlite.Conn, fixture conversationTurnCountMigrationFixture) {
	t.Helper()
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('conversation-turns-host','conversation-turns-host');
INSERT INTO projects(project_hash) VALUES('conversation-turns-project');`, nil); err != nil {
		t.Fatal(err)
	}
	for _, session := range fixture.Sessions {
		activeGeneration := any(nil)
		if session.ActiveGeneration {
			activeGeneration = "generation-" + session.SessionID
		}
		if err := sqlitex.ExecuteTransient(conn, `
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, active_generation_id)
VALUES(?, 'claude-code', 'fixture-model', 'conversation-turns-host', 'conversation-turns-project', 1, 2, 3, '/synthetic/conversation.jsonl', 'jsonl', ?)`,
			&sqlitex.ExecOptions{Args: []any{session.SessionID, activeGeneration}}); err != nil {
			t.Fatal(err)
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_metrics(session_id) VALUES(?)`, &sqlitex.ExecOptions{Args: []any{session.SessionID}}); err != nil {
			t.Fatal(err)
		}
		statsArgs := []any{session.SessionID, nil}
		if session.LegacyTurnCount != nil {
			statsArgs[1] = *session.LegacyTurnCount
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_captured_stats(session_id, turn_count, source, updated_at_ms) VALUES(?, ?, 'harness', 1)`, &sqlitex.ExecOptions{Args: statsArgs}); err != nil {
			t.Fatal(err)
		}
		if session.ActiveGeneration && len(session.Entries) > 0 {
			seedConversationTurnCountGeneration(t, conn, session)
			continue
		}
		// A legacy representation has no generation; its mirror rows are the
		// only stored entries.
		for _, entry := range session.Entries {
			if err := sqlitex.ExecuteTransient(conn, `
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview)
VALUES(?, ?, 'claude-code', ?, ?, ?, ?)`,
				&sqlitex.ExecOptions{Args: []any{session.SessionID, entry.Index, entry.EntryType, entry.Role, entry.Depth, entry.Content}}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// seedConversationTurnCountGeneration installs a managed generation the way the
// native activation lane does: catalog rows with source refs plus body rows,
// and no session_entries mirror. A native mirror clears the refs and drops
// previews, so folding it undercounts the turns a reader serves; the backfill
// must read these rows instead.
func seedConversationTurnCountGeneration(t *testing.T, conn *sqlite.Conn, session conversationTurnCountMigrationSession) {
	t.Helper()
	generationID := "generation-" + session.SessionID
	digest := strings.Repeat("a", 64)
	if err := sqlitex.ExecuteTransient(conn, `
INSERT INTO session_generations(session_id, generation_id, schema_version, harness, model, version, ts_start, ts_end, ts_ingested,
  source_file_path, source_format, project_hash, project_name, host_slug, content_hash, metadata_hash,
  redaction_applied, completeness, source_evidence_digest, index_format_version, candidate_digest, installed_at_ms)
VALUES(?, ?, 12, 'claude-code', 'fixture-model', '', 1, 2, 3,
  '/synthetic/conversation.jsonl', 'jsonl', 'conversation-turns-project', 'conversation-turns', 'conversation-turns-host', ?, ?,
  0, 'complete', ?, 2, ?, 1)`,
		&sqlitex.ExecOptions{Args: []any{session.SessionID, generationID, digest, digest, digest, digest}}); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `
INSERT INTO session_projection_sections(session_id, generation_id, partition_id) VALUES(?, ?, 0)`,
		&sqlitex.ExecOptions{Args: []any{session.SessionID, generationID}}); err != nil {
		t.Fatal(err)
	}
	sessionPrefix, err := strconv.ParseInt(session.SessionID[:8], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range session.Entries {
		// The body identity is global, so it must not collide across fixture
		// sessions. The digest is derived from it as well.
		bodyID := int64(1125899906842624) + sessionPrefix<<4 + int64(entry.Index)
		bodyDigest := fmt.Sprintf("%064x", bodyID)
		sourceRef := fmt.Sprintf("e_%s_%d", session.SessionID[:8], entry.Index)
		var preview any
		if entry.Content != "" {
			preview = entry.Content
		}
		if err := sqlitex.ExecuteTransient(conn, `
INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, depth,
  content_preview, has_tool_use, has_thinking, is_error, source_entry_ref)
VALUES(?, ?, ?, ?, 'claude-code', ?, ?, ?, ?, 0, 0, 0, ?)`,
			&sqlitex.ExecOptions{Args: []any{
				bodyID, session.SessionID, bodyDigest, entry.Index,
				entry.EntryType, entry.Role, entry.Depth, preview, sourceRef,
			}}); err != nil {
			t.Fatal(err)
		}
		if err := sqlitex.ExecuteTransient(conn, `
INSERT INTO session_generation_entries(session_id, generation_id, partition_id, entry_index, source_entry_ref, body_digest)
VALUES(?, ?, 0, ?, ?, ?)`,
			&sqlitex.ExecOptions{Args: []any{session.SessionID, generationID, entry.Index, sourceRef, bodyDigest}}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMigrationV74BackfillsConversationTurnCounts proves the upgrade path: a
// V73 store is folded once at open, an entry-less session copies its legacy
// value, and a NULL legacy value stays unknown (the column's zero) rather than
// becoming a measured conversation. It also proves a legacy session with no
// active generation is backfilled, because every count surface now reads the
// conversation column. The managed fixture has no session_entries mirror, so a
// fold over mirror rows would copy the legacy 9 instead of the served 2; the
// expected values are the served detail turn counts.
func TestMigrationV74BackfillsConversationTurnCounts(t *testing.T) {
	t.Parallel()
	fixture := loadConversationTurnCountMigrationFixture(t)
	path := filepath.Join(t.TempDir(), "upgrade.db")
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(73)); err != nil {
		t.Fatal(err)
	}
	seedConversationTurnCountSessions(t, conn, fixture)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conn, err = s.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool().Put(conn)
	for _, session := range fixture.Sessions {
		got := -1
		if err := sqlitex.ExecuteTransient(conn, `SELECT conversation_turn_count FROM session_captured_stats WHERE session_id = ?`,
			&sqlitex.ExecOptions{Args: []any{session.SessionID}, ResultFunc: func(stmt *sqlite.Stmt) error {
				got = stmt.ColumnInt(0)
				return nil
			}}); err != nil {
			t.Fatal(err)
		}
		if got != session.Expected {
			t.Errorf("%s: conversation_turn_count = %d, want %d", session.Name, got, session.Expected)
		}
	}
}

// TestBackfillConversationTurnCountsReportsEntryLessSessions pins the migration
// report: the sessions whose entries are gone are named so an operator can
// re-index them, and their copied value is the legacy column even when that
// column is NULL.
func TestBackfillConversationTurnCountsReportsEntryLessSessions(t *testing.T) {
	t.Parallel()
	fixture := loadConversationTurnCountMigrationFixture(t)
	s, err := Open(filepath.Join(t.TempDir(), "head.db"), WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conn, err := s.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seedConversationTurnCountSessions(t, conn, fixture)
	s.Pool().Put(conn)

	report, err := backfillConversationTurnCounts(s.Pool())
	if err != nil {
		t.Fatal(err)
	}
	var wantEntryLess []ingest.SessionID
	for _, session := range fixture.Sessions {
		if session.ReportedEntryLess {
			wantEntryLess = append(wantEntryLess, ingest.SessionID(session.SessionID))
		}
	}
	if len(report.EntryLessSessions) != len(wantEntryLess) {
		t.Fatalf("entry-less report = %v, want %v", report.EntryLessSessions, wantEntryLess)
	}
	for i := range wantEntryLess {
		if report.EntryLessSessions[i] != wantEntryLess[i] {
			t.Fatalf("entry-less report = %v, want %v", report.EntryLessSessions, wantEntryLess)
		}
	}
	if report.SessionsUpdated != len(fixture.Sessions) {
		t.Fatalf("sessions updated = %d, want %d", report.SessionsUpdated, len(fixture.Sessions))
	}
}

func TestConversationTurnCountBackfillGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		version int64
		want    bool
	}{
		{0, false}, {1, true}, {62, true}, {70, true}, {71, true}, {72, true}, {73, true}, {74, false}, {75, false},
	} {
		if got := shouldBackfillConversationTurnCounts(tc.version); got != tc.want {
			t.Errorf("shouldBackfillConversationTurnCounts(%d) = %t, want %t", tc.version, got, tc.want)
		}
	}
}

// TestConversationTurnCountBackfillDoesNotReRunAtHead proves the gate is tied
// to the V74 upgrade, not to every future head version: a store already at V74
// reopens without re-folding, so a deliberately changed count survives.
func TestConversationTurnCountBackfillDoesNotReRunAtHead(t *testing.T) {
	t.Parallel()
	fixture := loadConversationTurnCountMigrationFixture(t)
	path := filepath.Join(t.TempDir(), "head.db")
	s, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seedConversationTurnCountSessions(t, conn, fixture)
	s.Pool().Put(conn)
	if _, err := backfillConversationTurnCounts(s.Pool()); err != nil {
		t.Fatal(err)
	}
	conn, err = s.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_captured_stats SET conversation_turn_count = 99 WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{fixture.Sessions[0].SessionID}}); err != nil {
		t.Fatal(err)
	}
	s.Pool().Put(conn)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	conn, err = reopened.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Pool().Put(conn)
	got := -1
	if err := sqlitex.ExecuteTransient(conn, `SELECT conversation_turn_count FROM session_captured_stats WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{fixture.Sessions[0].SessionID}, ResultFunc: func(stmt *sqlite.Stmt) error { got = stmt.ColumnInt(0); return nil }}); err != nil {
		t.Fatal(err)
	}
	if got != 99 {
		t.Fatalf("reopened store re-ran the backfill: conversation_turn_count = %d, want the stored 99", got)
	}
}
