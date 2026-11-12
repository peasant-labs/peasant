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
