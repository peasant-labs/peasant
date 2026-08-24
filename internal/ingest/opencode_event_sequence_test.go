package ingest_test

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestOpenCodeDiscoveryReadsEventSequence proves discovery reads the newest
// event sequence per session from the event_sequence table and carries it onto
// the discovered session. The event table itself, with its payload, is never
// projected. It also proves the payload-free MAX(seq) fallback reads the same
// value when the event_sequence table is absent.
func TestOpenCodeDiscoveryReadsEventSequence(t *testing.T) {
	for _, withEventSequence := range []bool{true, false} {
		withEventSequence := withEventSequence
		name := "event-sequence-table"
		if !withEventSequence {
			name = "event-table-fallback"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			dbPath := filepath.Join(base, "opencode.db")
			sessionID := "ses_3cd91f52effeXd3QAJ54jOyzS1"
			buildOpenCodeEventSequenceDatabase(t, dbPath, sessionID, withEventSequence, 42)

			root, err := ingest.NewResolvedPath(base)
			if err != nil {
				t.Fatalf("resolve synthetic root: %v", err)
			}
			adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
			discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
			if err != nil {
				t.Fatalf("run production discovery: %v", err)
			}
			if len(discovered) != 1 {
				t.Fatalf("discovered %d sessions, want 1", len(discovered))
			}
			if discovered[0].EventSeq != 42 {
				t.Fatalf("discovered session event sequence = %d, want 42 from the %s", discovered[0].EventSeq, name)
			}
		})
	}
}

func buildOpenCodeEventSequenceDatabase(t *testing.T, dbPath, sessionID string, withEventSequence bool, seq int64) {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open synthetic database: %v", err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			t.Fatalf("close synthetic database: %v", closeErr)
		}
	}()
	schema := `
CREATE TABLE session (
  id TEXT PRIMARY KEY,
  parent_id TEXT,
  time_created INTEGER NOT NULL DEFAULT 0,
  time_updated INTEGER NOT NULL DEFAULT 0
);
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
CREATE TABLE event (
  id TEXT PRIMARY KEY,
  aggregate_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  type TEXT,
  data TEXT
);
`
	if withEventSequence {
		schema += `CREATE TABLE event_sequence (aggregate_id TEXT PRIMARY KEY, seq INTEGER NOT NULL, owner_id TEXT);
`
	}
	if err := sqlitex.ExecuteScript(conn, schema, nil); err != nil {
		t.Fatalf("create synthetic schema: %v", err)
	}
	exec := func(statement string, args ...any) {
		if err := sqlitex.Execute(conn, statement, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("insert into synthetic database: %v", err)
		}
	}
	exec(`INSERT INTO session (id, time_created, time_updated) VALUES (?1, 3000, 3000);`, sessionID)
	exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_seq_1', ?1, 3000, 3000, '{"role":"assistant"}');`, sessionID)
	exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('part_seq_1', 'msg_seq_1', ?1, 3001, 3001, '{"type":"text","text":"synthetic legacy projection"}');`, sessionID)
	// The event table carries payload rows; the newest seq is the change cursor.
	exec(`INSERT INTO event (id, aggregate_id, seq, type, data) VALUES ('evt_1', ?1, 7, 'message.updated', 'unreadable payload');`, sessionID)
	exec(`INSERT INTO event (id, aggregate_id, seq, type, data) VALUES ('evt_2', ?1, ?2, 'message.updated', 'unreadable payload');`, sessionID, seq)
	if withEventSequence {
		exec(`INSERT INTO event_sequence (aggregate_id, seq, owner_id) VALUES (?1, ?2, 'owner');`, sessionID, seq)
	}
}
