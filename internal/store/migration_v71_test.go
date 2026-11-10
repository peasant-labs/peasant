package store

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"path/filepath"
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

// TestMigrationV71BackfillsConversationTurnCounts proves the upgrade path: a
// V70 store is folded once at open, an entry-less session copies its legacy
// value, and a NULL legacy value stays unknown (the column's zero) rather than
// becoming a measured conversation. It also proves a legacy session with no
// active generation is backfilled, because every count surface now reads the
// conversation column.
func TestMigrationV71BackfillsConversationTurnCounts(t *testing.T) {
	t.Parallel()
	fixture := loadConversationTurnCountMigrationFixture(t)
	path := filepath.Join(t.TempDir(), "upgrade.db")
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(70)); err != nil {
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
