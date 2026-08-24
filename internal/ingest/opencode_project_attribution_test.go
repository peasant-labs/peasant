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

// buildOpenCodeProjectAttributionDatabase writes a synthetic legacy OpenCode
// database with the project and project_directory tables, so discovery resolves
// a session's project from the tables rather than from git. The single session's
// working directory is a worktree the project_directory table maps to a project
// whose root is a different path.
func buildOpenCodeProjectAttributionDatabase(t *testing.T, dbPath, sessionID, worktreeDir, projectRoot, projectName string) {
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
  time_updated INTEGER NOT NULL DEFAULT 0,
  directory TEXT,
  title TEXT
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
CREATE TABLE project (
  id TEXT PRIMARY KEY,
  worktree TEXT,
  vcs TEXT,
  name TEXT,
  sandboxes TEXT
);
CREATE TABLE project_directory (
  project_id TEXT NOT NULL,
  directory TEXT NOT NULL,
  type TEXT,
  strategy TEXT
);
`
	if err := sqlitex.ExecuteScript(conn, schema, nil); err != nil {
		t.Fatalf("create synthetic schema: %v", err)
	}
	exec := func(statement string, args ...any) {
		if err := sqlitex.Execute(conn, statement, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("insert into synthetic database: %v", err)
		}
	}
	exec(`INSERT INTO session (id, time_created, time_updated, directory, title) VALUES (?1, 3000, 3000, ?2, 'project session');`, sessionID, worktreeDir)
	exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_proj_1', ?1, 3000, 3000, '{"role":"assistant"}');`, sessionID)
	exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('part_proj_1', 'msg_proj_1', ?1, 3001, 3001, '{"type":"text","text":"synthetic legacy projection"}');`, sessionID)
	exec(`INSERT INTO project (id, worktree, vcs, name, sandboxes) VALUES ('proj_1', ?1, 'git', ?2, 'unreadable');`, projectRoot, projectName)
	exec(`INSERT INTO project_directory (project_id, directory, type, strategy) VALUES ('proj_1', ?1, 'git_worktree', 'unreadable');`, worktreeDir)
}

// TestOpenCodeProjectTablesGroupSessionUnderProjectRoot proves that a session
// whose working directory is a worktree listed in project_directory groups under
// the project root: its project name and worktree come from the project tables
// while its CWD stays its own directory. Removing the project attribution leaves
// the project resolved from the session directory instead.
func TestOpenCodeProjectTablesGroupSessionUnderProjectRoot(t *testing.T) {
	base := t.TempDir()
	worktreeDir := filepath.Join(base, "peasant-labs", "peasant", "feat--worktree")
	projectRoot := filepath.Join(base, "peasant-labs", "peasant")
	dbPath := filepath.Join(base, "opencode.db")
	sessionID := "ses_3cd91f52effeXd3QAJ54jOyzP1"
	buildOpenCodeProjectAttributionDatabase(t, dbPath, sessionID, worktreeDir, projectRoot, "peasant-project")

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
	session := discovered[0]
	if session.CWD != worktreeDir {
		t.Fatalf("session CWD = %q, want the unchanged worktree directory %q", session.CWD, worktreeDir)
	}
	if session.ProjectWorktree != projectRoot {
		t.Fatalf("session project worktree = %q, want the project root %q", session.ProjectWorktree, projectRoot)
	}
	if session.ProjectName != "peasant-project" {
		t.Fatalf("session project name = %q, want the project-table name %q", session.ProjectName, "peasant-project")
	}
	metadata, err := adapter.ExtractMetadata(t.Context(), session)
	if err != nil {
		t.Fatalf("extract metadata: %v", err)
	}
	if metadata.Project.FilePath != projectRoot {
		t.Fatalf("metadata project path = %q, want the project root %q", metadata.Project.FilePath, projectRoot)
	}
	if metadata.Project.Name != "peasant-project" {
		t.Fatalf("metadata project name = %q, want %q", metadata.Project.Name, "peasant-project")
	}
	if metadata.CWD != worktreeDir {
		t.Fatalf("metadata CWD = %q, want the unchanged worktree directory %q", metadata.CWD, worktreeDir)
	}
}
