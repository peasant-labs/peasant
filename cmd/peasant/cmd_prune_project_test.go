package main

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestPruneProjectAcceptsSessionsListIdentifiers runs the real prune command
// with every project identifier sessions list --project takes (name, path,
// remote in its canonical and URL forms) plus the hash, and checks that a
// near-miss name matches nothing.
func TestPruneProjectAcceptsSessionsListIdentifiers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)
	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = sqlitex.ExecuteTransient(conn, "UPDATE projects SET canonical_remote = 'github.com/example/prune-test' WHERE project_hash = ?",
		&sqlitex.ExecOptions{Args: []any{pruneTestProject}})
	db.Pool().Put(conn)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{
		pruneTestProject,
		"prune-test",
		"PRUNE-TEST",
		"prune", // the directory name of /test/prune
		"/test/prune",
		"github.com/example/prune-test",
		"https://github.com/example/prune-test.git",
		"git@github.com:example/prune-test.git",
	} {
		out, _, err := executePruneCmd(t, dir, []string{"--project", id, "--dry-run"})
		if err != nil {
			t.Errorf("prune --project %q: %v", id, err)
		} else if !strings.Contains(out, "2 session(s) would be deleted") {
			t.Errorf("prune --project %q: want both sessions; got:\n%s", id, out)
		}
	}
	for _, id := range []string{"rune", "prune-tes", "example", "/test", "example/prune"} {
		out, _, err := executePruneCmd(t, dir, []string{"--project", id, "--dry-run"})
		if err != nil {
			t.Errorf("prune --project %q: %v", id, err)
		} else if strings.Contains(out, "would be deleted") {
			t.Errorf("prune --project %q matched a project by a partial name; got:\n%s", id, out)
		}
	}
}

// TestPruneProjectRefusesAnAmbiguousName proves a --project name that two
// projects share is refused with both candidates listed, instead of pruning
// both, and that path matches are case-sensitive.
func TestPruneProjectRefusesAnAmbiguousName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)
	const other = "2222222222222222222222222222222222222222222222222222222222222222"
	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = sqlitex.ExecuteScript(conn, `
CREATE TEMP TABLE copy AS SELECT * FROM projects WHERE project_hash = '`+pruneTestProject+`';
UPDATE copy SET project_hash = '`+other+`', canonical_cwd = '/home/qb/app';
INSERT INTO projects SELECT * FROM copy;
UPDATE projects SET canonical_cwd = '/home/qa/app' WHERE project_hash = '`+pruneTestProject+`';
UPDATE sessions SET project_hash = '`+other+`' WHERE session_id = '`+pruneTestSessionB+`';
`, nil)
	db.Pool().Put(conn)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	out, _, err := executePruneCmd(t, dir, []string{"--project", "app", "--dry-run"})
	if err == nil || !strings.Contains(err.Error(), "/home/qa/app") || !strings.Contains(err.Error(), "/home/qb/app") {
		t.Fatalf("prune --project app over two projects named app: err = %v, want a refusal listing both; out:\n%s", err, out)
	}
	if strings.Contains(out, "session(s) would be deleted") {
		t.Fatalf("an ambiguous --project still selected sessions:\n%s", out)
	}
	out, _, err = executePruneCmd(t, dir, []string{"--project", "qa/app", "--dry-run"})
	if err != nil || !strings.Contains(out, "1 session(s) would be deleted") {
		t.Fatalf("prune --project qa/app: err %v, want exactly the one project's session:\n%s", err, out)
	}
	out, _, err = executePruneCmd(t, dir, []string{"--project", "qa/APP", "--dry-run"})
	if err != nil || strings.Contains(out, "session(s) would be deleted") {
		t.Fatalf("prune --project qa/APP matched /home/qa/app case-insensitively: err %v:\n%s", err, out)
	}
}

// TestPruneRefusesAllCombinedWithAFilter proves --all cannot be combined with a
// narrowing filter: --all used to skip every filter, so
// `prune --project X --all --confirm` deleted every session in the store.
func TestPruneRefusesAllCombinedWithAFilter(t *testing.T) {
	t.Parallel()
	for _, extra := range [][]string{
		{"--project", "app"},
		{"--harness", defaults.HarnessClaudeCode.String()},
		{"--before", "2030-01-01"},
		{"--after", "2000-01-01"},
		{"--session", pruneTestSessionA},
	} {
		dir := t.TempDir()
		seedPruneTestSessions(t, dir)
		args := append([]string{"--all", "--confirm"}, extra...)
		out, _, err := executePruneCmd(t, dir, args)
		if err == nil || !strings.Contains(err.Error(), "--all cannot be combined") {
			t.Fatalf("prune %v: err = %v, want a refusal; out:\n%s", args, err, out)
		}
		db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := db.QueryPrunableSessions(t.Context(), ingest.PruneFilter{All: true})
		db.Close()
		if err != nil || len(rows) != 2 {
			t.Fatalf("prune %v deleted sessions: %d left, err %v", args, len(rows), err)
		}
	}
}
