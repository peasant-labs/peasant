package store

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestSandboxStoreUpgradeMeasure upgrades a synthetic schema-69 store of a
// known text size and logs, per step, the duration and the write-ahead-log
// growth, then the free-space change and the log's high-water mark. It is
// the measurement behind the upgrade's free-disk factors and the per-step
// split of a long upgrade. It never runs in the gate and never touches a
// real store: it builds its own under /tmp/opencode.
//
//	PEASANT_SANDBOX_UPGRADE_DIR=/tmp/opencode/<dir> \
//	PEASANT_SANDBOX_UPGRADE_TEXT_MB=256 \
//	go test ./internal/store -run TestSandboxStoreUpgradeMeasure -v -timeout 0
func TestSandboxStoreUpgradeMeasure(t *testing.T) {
	dir := os.Getenv("PEASANT_SANDBOX_UPGRADE_DIR")
	if dir == "" {
		t.Skip("sandbox measurement only: set PEASANT_SANDBOX_UPGRADE_DIR to a directory under /tmp/opencode")
	}
	rel, err := filepath.Rel("/tmp/opencode", dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatal("the upgrade measurement refuses a directory outside /tmp/opencode")
	}
	textMB := 64
	if raw := os.Getenv("PEASANT_SANDBOX_UPGRADE_TEXT_MB"); raw != "" {
		textMB, err = strconv.Atoi(raw)
		if err != nil || textMB <= 0 {
			t.Fatalf("PEASANT_SANDBOX_UPGRADE_TEXT_MB = %q, want a positive whole number of megabytes", raw)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "upgrade-measure.db")
	for _, stale := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Remove(stale)
	}

	const (
		entriesPerSession = 50
		entryBytes        = 4096
		vocabulary        = 50000
	)
	sessions := textMB * 1024 * 1024 / (entriesPerSession * entryBytes)
	words := rand.New(rand.NewPCG(69, 76))
	entryText := func() string {
		var text strings.Builder
		for text.Len() < entryBytes {
			fmt.Fprintf(&text, "w%d ", words.IntN(vocabulary))
		}
		return text.String()
	}
	var textBytes int64
	buildBehindStore(t, path, 69, func(conn *sqlite.Conn) {
		if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('measure-host','measure-host');
INSERT INTO projects(project_hash) VALUES('measure-project');`, nil); err != nil {
			t.Fatal(err)
		}
		for s := 0; s < sessions; s++ {
			end := sqlitex.Transaction(conn)
			var txErr error
			sessionID := fmt.Sprintf("%08x-0000-4000-8000-000000000000", s)
			txErr = sqlitex.ExecuteTransient(conn, `
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES(?, 'claude-code', 'measure-model', 'measure-host', 'measure-project', 1, 2, 3, '/synthetic/measure.jsonl', 'jsonl')`,
				&sqlitex.ExecOptions{Args: []any{sessionID}})
			for e := 0; e < entriesPerSession && txErr == nil; e++ {
				text := entryText()
				textBytes += int64(len(text))
				txErr = sqlitex.ExecuteTransient(conn, `
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview)
VALUES(?, ?, 'claude-code', 'text', 'assistant', 0, ?)`,
					&sqlitex.ExecOptions{Args: []any{sessionID, e, text}})
			}
			end(&txErr)
			if txErr != nil {
				t.Fatal(txErr)
			}
		}
	})

	sizeOf := func(p string) int64 {
		info, err := os.Stat(p)
		if err != nil {
			return 0
		}
		return info.Size()
	}
	storeBefore := sizeOf(path)
	freeBefore, freeKnown := migrateFreeBytes(dir)
	plan := mustPlan(t, path)
	t.Logf("store: %d sessions, %d text bytes, database %d bytes; plan asks for %d bytes free", sessions, textBytes, storeBefore, plan.RequiredFreeBytes)

	var walHighWater int64
	started := time.Now()
	report, err := Upgrade(t.Context(), path, UpgradeOptions{
		Progress: func(event UpgradeEvent) {
			if size := sizeOf(path + "-wal"); size > walHighWater {
				walHighWater = size
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range report.Steps {
		t.Logf("step %d  %-9s %-18s %12s  wal +%d bytes  %s", step.Step.Version, step.Step.Kind, step.Step.Cost, step.Duration.Round(time.Millisecond), step.WALBytes, step.Step.Title)
	}
	freeAfter, _ := migrateFreeBytes(dir)
	t.Logf("total %s; database %d -> %d bytes; write-ahead log high-water %d bytes (%.3f of the database, %.3f of the text), %d bytes after",
		time.Since(started).Round(time.Millisecond), storeBefore, sizeOf(path), walHighWater,
		float64(walHighWater)/float64(storeBefore), float64(walHighWater)/float64(textBytes), sizeOf(path+"-wal"))
	if freeKnown {
		t.Logf("free space changed by %d bytes", int64(freeAfter)-int64(freeBefore))
	}
}
