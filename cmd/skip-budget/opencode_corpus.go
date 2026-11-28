//go:build skipbudget

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

const (
	openCodeCorpusProjectID   = "proj_skip_budget"
	openCodeCorpusProjectDir  = "/work/project"
	openCodeCorpusProjectName = "skip-budget-project"
)

var openCodeCorpusMtime = time.Unix(1_700_000_000, 0)

const openCodeCorpusSchemaSQL = `
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
CREATE TABLE session_v2 (
  id TEXT PRIMARY KEY,
  parent_id TEXT,
  time_created INTEGER NOT NULL DEFAULT 0,
  time_updated INTEGER NOT NULL DEFAULT 0,
  directory TEXT,
  title TEXT,
  agent TEXT,
  tokens_input INTEGER,
  tokens_output INTEGER,
  tokens_reasoning INTEGER,
  tokens_cache_read INTEGER,
  tokens_cache_write INTEGER,
  cost REAL,
  version TEXT,
  slug TEXT,
  revert TEXT,
  project_id TEXT,
  workspace_id TEXT,
  fork_session_id TEXT,
  fork_boundary TEXT,
  path TEXT,
  share_url TEXT,
  summary_additions INTEGER,
  summary_deletions INTEGER,
  summary_files INTEGER,
  summary_diffs TEXT,
  metadata TEXT,
  permission TEXT,
  model TEXT,
  time_idle INTEGER,
  time_viewed INTEGER,
  idle_outcome TEXT,
  time_compacting INTEGER,
  time_archived INTEGER,
  time_suspended INTEGER,
  resume_attempts INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE session_message (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  type TEXT NOT NULL,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  data TEXT NOT NULL,
  seq INTEGER NOT NULL,
  FOREIGN KEY (session_id) REFERENCES session_v2(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message(session_id, seq);
CREATE INDEX session_message_seq_idx ON session_message(seq);
`

// writeOpenCodeCorpus creates one deterministic synthetic OpenCode current-layout
// SQLite database (project, project_directory, session_v2, session_message)
// for smoke runs and 240x1000 measurement.
func writeOpenCodeCorpus(root string, sessions, entries int) (int64, error) {
	dbPath := filepath.Join(root, "opencode.db")
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		return 0, fmt.Errorf("open synthetic opencode db %s: %w", dbPath, err)
	}
	if err := populateOpenCodeCorpus(conn, sessions, entries); err != nil {
		_ = conn.Close()
		return 0, err
	}
	if err := conn.Close(); err != nil {
		return 0, fmt.Errorf("close synthetic opencode db %s: %w", dbPath, err)
	}
	if err := os.Chtimes(dbPath, openCodeCorpusMtime, openCodeCorpusMtime); err != nil {
		return 0, fmt.Errorf("set synthetic opencode db mtime %s: %w", dbPath, err)
	}
	return fileSize(dbPath), nil
}

func populateOpenCodeCorpus(conn *sqlite.Conn, sessions, entries int) error {
	for _, pragma := range []string{
		"PRAGMA journal_mode=DELETE;",
		"PRAGMA synchronous=OFF;",
		"PRAGMA temp_store=MEMORY;",
	} {
		if err := sqlitex.Execute(conn, pragma, nil); err != nil {
			return fmt.Errorf("configure synthetic opencode db (%s): %w", pragma, err)
		}
	}
	if err := sqlitex.ExecuteScript(conn, openCodeCorpusSchemaSQL, nil); err != nil {
		return fmt.Errorf("create synthetic opencode schema: %w", err)
	}
	if err := sqlitex.Execute(conn, `INSERT INTO project (id, worktree, vcs, name, sandboxes) VALUES (?, ?, 'git', ?, '[]')`,
		&sqlitex.ExecOptions{Args: []any{openCodeCorpusProjectID, openCodeCorpusProjectDir, openCodeCorpusProjectName}}); err != nil {
		return fmt.Errorf("insert synthetic opencode project: %w", err)
	}
	if err := sqlitex.Execute(conn, `INSERT INTO project_directory (project_id, directory, type, strategy) VALUES (?, ?, 'git_worktree', 'git')`,
		&sqlitex.ExecOptions{Args: []any{openCodeCorpusProjectID, openCodeCorpusProjectDir}}); err != nil {
		return fmt.Errorf("insert synthetic opencode project_directory: %w", err)
	}
	if err := sqlitex.Execute(conn, "BEGIN", nil); err != nil {
		return err
	}
	filler := strings.Repeat("synthetic transcript text for the skip budget measurement. ", 12)
	insertSession := `INSERT INTO session_v2 (
  id, time_created, time_updated, directory, title, agent,
  tokens_input, tokens_output, cost, version, slug, project_id
) VALUES (?, ?, ?, ?, ?, 'build', 120, 80, 0.01, 'synthetic-1', ?, ?)`
	insertMessage := `INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES (?, ?, ?, ?, ?, ?, ?)`

	for n := range sessions {
		sid := fmt.Sprintf("ses_skipbudget%06d", n+1)
		timeCreated := int64(1_700_000_000_000 + int64(n)*1_000_000)
		timeUpdated := timeCreated + int64(entries)*1000
		title := fmt.Sprintf("synthetic opencode session %d", n+1)
		if err := sqlitex.Execute(conn, insertSession, &sqlitex.ExecOptions{
			Args: []any{sid, timeCreated, timeUpdated, openCodeCorpusProjectDir, title, sid, openCodeCorpusProjectID},
		}); err != nil {
			_ = sqlitex.Execute(conn, "ROLLBACK", nil)
			return fmt.Errorf("insert synthetic opencode session %s: %w", sid, err)
		}
		for e := range entries {
			msgID := fmt.Sprintf("msg_skipbudget%06d%06d", n+1, e)
			msgTime := timeCreated + int64(e)*1000
			msgType := "user"
			var data string
			if e%2 == 0 {
				data = fmt.Sprintf(`{"text":"prompt %d: %s","time":{"created":%d}}`, e, filler, msgTime)
			} else {
				msgType = "assistant"
				data = fmt.Sprintf(
					`{"agent":"build","content":[{"type":"text","text":"reply %d: %s"}],"cost":0,"finish":"stop","model":{"id":"synthetic-model","providerID":"synthetic-provider"},"rawFinish":"stop","time":{"created":%d,"streamed":%d,"completed":%d},"tokens":{"cache":{"read":0,"write":0},"input":120,"output":80,"reasoning":0}}`,
					e, filler, msgTime, msgTime+500, msgTime+500,
				)
			}
			if err := sqlitex.Execute(conn, insertMessage, &sqlitex.ExecOptions{
				Args: []any{msgID, sid, msgType, msgTime, msgTime, data, int64(e)},
			}); err != nil {
				_ = sqlitex.Execute(conn, "ROLLBACK", nil)
				return fmt.Errorf("insert synthetic opencode message %s: %w", msgID, err)
			}
		}
	}
	if err := sqlitex.Execute(conn, "COMMIT", nil); err != nil {
		return fmt.Errorf("commit synthetic opencode corpus: %w", err)
	}
	return nil
}

// renameOpenCodeSQLiteProject updates only project.name in the synthetic
// OpenCode SQLite database, leaving project.worktree and session timestamps
// fixed so HostSlug and transcript bytes stay unchanged.
func renameOpenCodeSQLiteProject(source, name string) (int64, error) {
	dbPath := filepath.Join(source, "opencode.db")
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		return 0, fmt.Errorf("open synthetic opencode db %s: %w", dbPath, err)
	}
	err = sqlitex.ExecuteTransient(conn, `UPDATE project SET name = ?`, &sqlitex.ExecOptions{Args: []any{name}})
	changed := int64(conn.Changes())
	closeErr := conn.Close()
	if err != nil {
		return 0, fmt.Errorf("update project.name in %s: %w", dbPath, err)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("close synthetic opencode db %s: %w", dbPath, closeErr)
	}
	if err := os.Chtimes(dbPath, openCodeCorpusMtime, openCodeCorpusMtime); err != nil {
		return 0, fmt.Errorf("restore synthetic opencode db mtime %s: %w", dbPath, err)
	}
	return changed, nil
}

// assertOpenCodeHostSlugUnchanged verifies via OpenCode discovery and metadata
// extraction that mutating project.name left every session's HostSlug fixed
// while updating Project.Name.
func assertOpenCodeHostSlugUnchanged(ctx context.Context, source string, s salt.Salt, prior map[ingest.SessionID]string, wantProjectName string) error {
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), s)
	discovered, err := adapter.Discover(ctx, ingest.SourceConfig{
		Enabled: true,
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(source)},
	})
	if err != nil {
		return fmt.Errorf("discover opencode corpus after project.name update: %w", err)
	}
	if len(discovered) != len(prior) {
		return fmt.Errorf("discovered %d opencode sessions after project.name update, want %d", len(discovered), len(prior))
	}
	for _, ds := range discovered {
		wantSlug, ok := prior[ds.SessionID]
		if !ok {
			return fmt.Errorf("unexpected opencode session %s after project.name update", ds.SessionID)
		}
		meta, err := adapter.ExtractMetadata(ctx, ds)
		if err != nil {
			return fmt.Errorf("extract metadata for %s after project.name update: %w", ds.SessionID, err)
		}
		if string(meta.HostSlug) != wantSlug {
			return fmt.Errorf("session %s HostSlug changed from %q to %q after project.name update", ds.SessionID, wantSlug, meta.HostSlug)
		}
		if meta.Project.Name != wantProjectName {
			return fmt.Errorf("session %s Project.Name = %q, want %q", ds.SessionID, meta.Project.Name, wantProjectName)
		}
	}
	return nil
}
