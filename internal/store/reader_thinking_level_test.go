package store_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestSessionReadDiagnosesCorruptThinkingLevelHistory proves a stored history
// value that is not valid JSON is surfaced as corruption rather than silently
// read as an absent history. The session still resolves (the read stays total),
// but the corruption is reported with an actionable diagnostic naming the
// session and the fix.
func TestSessionReadDiagnosesCorruptThinkingLevelHistory(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	const sessionID = "62000000-0000-4000-8000-0000000000c1"
	if err := db.InsertSessions(ctx, []ingest.StoreEntry{thinkingLevelEntry(t, sessionID, thinkingLevelPair{Level: "high"}, nil)}); err != nil {
		t.Fatal(err)
	}
	conn := takeConn(t, db.Pool())
	err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET thinking_level_history = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{"{not json", sessionID}})
	db.Pool().Put(conn)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	row, err := db.SessionByID(ctx, sessionID)
	if err != nil || row == nil {
		t.Fatalf("SessionByID: row=%v err=%v", row, err)
	}
	if row.ThinkingLevelHistory != nil {
		t.Fatalf("corrupt history read back as %+v, want nil", row.ThinkingLevelHistory)
	}
	logged := buf.String()
	if !strings.Contains(logged, "corrupt thinking_level_history") {
		t.Fatalf("expected a corrupt-history diagnostic, got %q", logged)
	}
	if !strings.Contains(logged, sessionID) {
		t.Fatalf("the diagnostic did not name the session, got %q", logged)
	}
	if !strings.Contains(logged, "re-index the session") {
		t.Fatalf("the diagnostic carried no fix, got %q", logged)
	}
}
