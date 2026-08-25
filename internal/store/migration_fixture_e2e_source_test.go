//go:build e2e

package store

import (
	"path/filepath"
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestBuildV39E2EFixture_CopiesFromLatestSchemaSource proves the V39 fixture
// builder accepts an ingested source written by this build. The source carries
// every column the latest migration added; the destination is frozen before
// V40. Copying by the destination's own column list is what keeps the two in
// step, and this case fails the moment the builder copies every source column
// again.
func TestBuildV39E2EFixture_CopiesFromLatestSchemaSource(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "ingested-latest.db")
	destination := filepath.Join(dir, "frozen-v39.db")

	latest, err := Open(source)
	if err != nil {
		t.Fatalf("open the ingested source at the latest schema: %v", err)
	}
	if err := latest.Close(); err != nil {
		t.Fatalf("close the ingested source: %v", err)
	}
	conn, err := sqlite.OpenConn(source, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("reopen the ingested source: %v", err)
	}
	if got := upgradeUserVersion(t, conn); got != CurrentSchemaVersion() {
		t.Fatalf("ingested source user_version = %d, want the current schema %d", got, CurrentSchemaVersion())
	}
	if _, present := upgradeColumn(t, conn, "sessions", "session_origin"); !present {
		t.Fatal("ingested source lacks sessions.session_origin, so it does not carry the columns this case exists to prove are dropped")
	}
	const sessionID = "00000000-0000-4000-8000-00000000e2e1"
	seed := []string{
		`INSERT INTO projects (project_hash) VALUES ('project-e2e')`,
		`INSERT INTO host_slugs (opaque_id, host_slug) VALUES ('host-e2e', 'slug-e2e')`,
		`INSERT INTO sessions (session_id, model_harness, model_id, opaque_host_id, project_hash,
		   start_ms, end_ms, ingested_ms, source_path, source_format, session_origin)
		 VALUES ('` + sessionID + `', 'claude-code', 'model', 'host-e2e', 'project-e2e', 1, 2, 3, '/tmp/e2e.jsonl', 'jsonl', 'agent')`,
	}
	for _, statement := range seed {
		if err := sqlitex.ExecuteTransient(conn, statement, nil); err != nil {
			t.Fatalf("seed the ingested source: %v", err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close the seeded source: %v", err)
	}

	request := v39FixtureRequest{
		Destination:    destination,
		IngestedSource: source,
		SessionID:      sessionID,
		CommitHash:     "0123456789abcdef0123456789abcdef01234567",
		Subject:        "seed an observed commit",
		AuthorTime:     4,
		PushedAt:       5,
	}
	if err := request.validate(); err != nil {
		t.Fatalf("validate the fixture request: %v", err)
	}
	if err := buildV39E2EFixture(request); err != nil {
		t.Fatalf("build the V39 fixture from a latest-schema source: %v", err)
	}

	frozen, err := sqlite.OpenConn(destination, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("open the built V39 fixture: %v", err)
	}
	defer frozen.Close()
	if got := upgradeUserVersion(t, frozen); got != 39 {
		t.Fatalf("built fixture user_version = %d, want 39", got)
	}
	if _, present := upgradeColumn(t, frozen, "sessions", "session_origin"); present {
		t.Fatal("built fixture carries sessions.session_origin, so it is not frozen before the origin migration")
	}
	var copied int
	if err := sqlitex.ExecuteTransient(frozen, `SELECT COUNT(*) FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			copied = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("count the copied session: %v", err)
	}
	if copied != 1 {
		t.Fatalf("copied sessions = %d, want 1", copied)
	}
}
