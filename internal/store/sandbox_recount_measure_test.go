//go:build upgrade_measure

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestSandboxTurnCountRecountMeasure builds synthetic schema-76 stores of a
// chosen number of sessions, each with a chosen number of entries, half of
// them managed generations and half mirror rows, upgrades each to the head
// schema, and logs the time and the write-ahead-log growth of the turn count
// recount step per session. It is the measurement behind the recount's cost
// class. It never runs in the gate and never touches a real store: it builds
// its own under /tmp/opencode.
//
//	PEASANT_SANDBOX_UPGRADE_DIR=/tmp/opencode/<dir> \
//	PEASANT_SANDBOX_RECOUNT_SESSIONS=2000,5000 \
//	PEASANT_SANDBOX_RECOUNT_ENTRIES=40 \
//	go test -tags upgrade_measure ./internal/store -run TestSandboxTurnCountRecountMeasure -v -timeout 0
func TestSandboxTurnCountRecountMeasure(t *testing.T) {
	dir := os.Getenv("PEASANT_SANDBOX_UPGRADE_DIR")
	if dir == "" {
		t.Skip("sandbox measurement only: set PEASANT_SANDBOX_UPGRADE_DIR to a directory under /tmp/opencode")
	}
	if !sandboxDirUnderTmpOpencode(dir) {
		t.Fatalf("PEASANT_SANDBOX_UPGRADE_DIR = %q is outside /tmp/opencode; the measurement builds stores only there", dir)
	}
	sizes := []int{2000, 5000}
	if raw := os.Getenv("PEASANT_SANDBOX_RECOUNT_SESSIONS"); raw != "" {
		sizes = nil
		for _, part := range strings.Split(raw, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n <= 0 {
				t.Fatalf("PEASANT_SANDBOX_RECOUNT_SESSIONS = %q, want positive whole numbers separated by commas", raw)
			}
			sizes = append(sizes, n)
		}
	}
	entries := 40
	if raw := os.Getenv("PEASANT_SANDBOX_RECOUNT_ENTRIES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 120 {
			t.Fatalf("PEASANT_SANDBOX_RECOUNT_ENTRIES = %q, want a whole number from 1 to 120", raw)
		}
		entries = n
	}
	for _, sessions := range sizes {
		t.Run(fmt.Sprintf("%d sessions x %d entries", sessions, entries), func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("recount-%d.db", sessions))
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(path + suffix)
			}
			t.Cleanup(func() {
				for _, suffix := range []string{"", "-wal", "-shm"} {
					_ = os.Remove(path + suffix)
				}
			})
			buildBehindStore(t, path, 76, func(conn *sqlite.Conn) {
				if err := sqlitex.ExecuteTransient(conn, "BEGIN", nil); err != nil {
					t.Fatal(err)
				}
				seedConversationTurnCountSessions(t, conn, syntheticRecountFixture(sessions, entries))
				if err := sqlitex.ExecuteTransient(conn, "COMMIT", nil); err != nil {
					t.Fatal(err)
				}
			})
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			plan := mustPlan(t, path)
			started := time.Now()
			report, err := Upgrade(t.Context(), path, UpgradeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range report.Steps {
				line := fmt.Sprintf("step %d %-7s %12s  wal +%d bytes  %s", step.Step.Version, step.Step.Kind, step.Duration.Round(time.Millisecond), step.WALBytes, step.Step.Title)
				if step.Step.Kind == UpgradeStepRecount {
					line += fmt.Sprintf("  (%.3f ms and %.0f log bytes per session; %.5f of the database)",
						float64(step.Duration.Microseconds())/1000/float64(sessions), float64(step.WALBytes)/float64(sessions), float64(step.WALBytes)/float64(info.Size()))
				}
				t.Log(line)
			}
			t.Logf("database %d bytes, %d sessions x %d entries; upgrade %s; the plan asked for %d bytes free",
				info.Size(), sessions, entries, time.Since(started).Round(time.Millisecond), plan.RequiredFreeBytes)
		})
	}
}

// syntheticRecountFixture builds sessions with entries for the recount
// measurement: alternating user prompts, assistant replies, tool calls and
// tool results, every other session a managed generation. Session id
// prefixes are spaced so the generation body ids never collide.
func syntheticRecountFixture(sessions, entries int) conversationTurnCountMigrationFixture {
	shapes := []struct{ role, entryType string }{
		{"user", "text"}, {"assistant", "text"}, {"assistant", "tool_use"}, {"tool", "tool_result"},
	}
	text := strings.Repeat("synthetic transcript text for the recount measurement. ", 4)
	var fixture conversationTurnCountMigrationFixture
	for i := 0; i < sessions; i++ {
		legacy := entries
		session := conversationTurnCountMigrationSession{
			Name:             fmt.Sprintf("session-%d", i),
			SessionID:        fmt.Sprintf("%08x-0000-4000-8000-%012x", (i+1)*8, i+1),
			ActiveGeneration: i%2 == 0,
			LegacyTurnCount:  &legacy,
		}
		for index := 0; index < entries; index++ {
			shape := shapes[index%len(shapes)]
			session.Entries = append(session.Entries, struct {
				Index     int    `yaml:"index"`
				Role      string `yaml:"role"`
				EntryType string `yaml:"entryType"`
				Depth     int    `yaml:"depth"`
				Content   string `yaml:"content"`
			}{Index: index, Role: shape.role, EntryType: shape.entryType, Content: text})
		}
		fixture.Sessions = append(fixture.Sessions, session)
	}
	return fixture
}

// TestSandboxHarnessWideningMeasure builds synthetic schema-77 stores with a
// chosen number of sessions rows (and one daily harness summary row per
// session day), upgrades each, and logs the time and the write-ahead-log
// growth of step 78, which rebuilds the sessions and daily harness summary
// tables to widen their harness CHECK. It is the measurement behind that
// step's cost class and disk need. It never runs in the gate and builds its
// stores only under /tmp/opencode.
//
//	PEASANT_SANDBOX_UPGRADE_DIR=/tmp/opencode/<dir> \
//	PEASANT_SANDBOX_WIDENING_SESSIONS=20000,50000 \
//	go test -tags upgrade_measure ./internal/store -run TestSandboxHarnessWideningMeasure -v -timeout 0
func TestSandboxHarnessWideningMeasure(t *testing.T) {
	dir := os.Getenv("PEASANT_SANDBOX_UPGRADE_DIR")
	if dir == "" {
		t.Skip("sandbox measurement only: set PEASANT_SANDBOX_UPGRADE_DIR to a directory under /tmp/opencode")
	}
	if !sandboxDirUnderTmpOpencode(dir) {
		t.Fatalf("PEASANT_SANDBOX_UPGRADE_DIR = %q is outside /tmp/opencode; the measurement builds stores only there", dir)
	}
	sizes := []int{20000, 50000}
	if raw := os.Getenv("PEASANT_SANDBOX_WIDENING_SESSIONS"); raw != "" {
		sizes = nil
		for _, part := range strings.Split(raw, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n <= 0 {
				t.Fatalf("PEASANT_SANDBOX_WIDENING_SESSIONS = %q, want positive whole numbers separated by commas", raw)
			}
			sizes = append(sizes, n)
		}
	}
	for _, sessions := range sizes {
		t.Run(fmt.Sprintf("%d sessions", sessions), func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("widening-%d.db", sessions))
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(path + suffix)
			}
			t.Cleanup(func() {
				for _, suffix := range []string{"", "-wal", "-shm"} {
					_ = os.Remove(path + suffix)
				}
			})
			buildBehindStore(t, path, 77, func(conn *sqlite.Conn) { seedSyntheticSessionRows(t, conn, sessions) })
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			plan := mustPlan(t, path)
			probe, err := openReadOnlyNoSidecars(path)
			if err != nil {
				t.Fatal(err)
			}
			basisStarted := time.Now()
			basis, measured, err := estimateSessionTablesOnConn(probe, defaults.StoreUpgradeEstimateMaxSessionRows)
			basisTook := time.Since(basisStarted)
			_ = probe.Close()
			if err != nil || !measured {
				t.Fatalf("estimate the session tables: measured=%t err=%v", measured, err)
			}
			report, err := Upgrade(t.Context(), path, UpgradeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range report.Steps {
				line := fmt.Sprintf("step %d %-7s %12s  wal +%d bytes  %s", step.Step.Version, step.Step.Kind, step.Duration.Round(time.Millisecond), step.WALBytes, step.Step.Title)
				if step.Step.Version == 78 {
					line += fmt.Sprintf("  (session tables estimated at %d bytes in %s; the log grew %.2f times that; the plan asked for %.2f times what the log used)",
						basis, basisTook.Round(time.Microsecond), float64(step.WALBytes)/float64(basis), float64(plan.RequiredFreeBytes)/float64(step.WALBytes))
					line += fmt.Sprintf("  (%.1f µs and %.0f log bytes per session; %.3f of the database)",
						float64(step.Duration.Microseconds())/float64(sessions), float64(step.WALBytes)/float64(sessions), float64(step.WALBytes)/float64(info.Size()))
				}
				t.Log(line)
			}
			t.Logf("database %d bytes, %d sessions rows; the plan asked for %d bytes free", info.Size(), sessions, plan.RequiredFreeBytes)
		})
	}
}

// seedSyntheticSessionRows inserts sessions rows shaped like stored ones (a
// git context, a source path, a metric seed and the latest attempt stats)
// and one daily harness summary row per session day; the script runs in one
// transaction.
func seedSyntheticSessionRows(t *testing.T, conn *sqlite.Conn, sessions int) {
	t.Helper()
	script := fmt.Sprintf(`
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('measure-host','measure-host');
INSERT INTO projects(project_hash) VALUES('measure-project');
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %[1]d)
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms,
  source_path, source_format, git_branch, git_worktree, tool_version, index_version, indexed_at, session_cwd,
  metric_seed_json, latest_attempt_stats_json)
SELECT printf('%%08x-0000-4000-8000-%%012x', i, i), 'claude-code', 'claude-synthetic-model', 'measure-host', 'measure-project',
  1767225600000 + i * 60000, 1767225600000 + i * 60000 + 900000, 1767225600000 + i * 60000 + 960000,
  printf('/home/synthetic/.claude/projects/-home-synthetic-project-%%d/%%08x-0000-4000-8000-%%012x.jsonl', i %% 40, i, i), 'jsonl',
  'feature/synthetic-branch-' || (i %% 25), '/home/synthetic/project-' || (i %% 40), '2.1.0', 24, 1767225600000 + i * 60000,
  '/home/synthetic/project-' || (i %% 40),
  json_object('turns', i %% 90, 'toolCalls', i %% 60, 'tokensIn', i * 13, 'tokensOut', i * 7, 'model', 'claude-synthetic-model'),
  json_object('entries', i %% 400, 'bytes', i * 311, 'parseMs', i %% 900)
FROM n;
WITH RECURSIVE d(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM d WHERE i < %[1]d / 24)
INSERT INTO daily_summary_harness(date_utc, model_harness, session_count, tokens_in, tokens_out, tokens_total)
SELECT date('2026-01-01', '+' || i || ' days'), 'claude-code', 24, 1000, 500, 1500 FROM d;`, sessions)
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatal(err)
	}
}
