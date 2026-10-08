package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/schema"
)

func runMigrateDrainCase(t *testing.T, c contentMigrationCase) {
	s, root := openGenerationStore(t)
	sid := migrateCaseSessionID(t, c.Name, 0)
	if c.DrainRow {
		seedGenerationSession(t, s, string(sid))
		recallExec(t, s, `UPDATE sessions SET indexed_input_hash=? WHERE session_id=?`, strings.Repeat("a", 64), string(sid))
	}
	sessionDir := filepath.Join(root, string(sid))
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	var dir string
	switch c.Drain {
	case "intent":
		if err := os.WriteFile(filepath.Join(sessionDir, "generation-intent.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
	case "directory":
		dir = filepath.Join(sessionDir, "generations", "g_orphan")
	case "temporary":
		dir = filepath.Join(sessionDir, "generations", ".tmp-gen-crashed")
	default:
		t.Fatalf("unhandled drain %s", c.Drain)
	}
	if dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("orphan"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	retained := filepath.Join(root, string(sid)+"--transcript.jsonl")
	if err := os.WriteFile(retained, []byte("keep retained source"), 0600); err != nil {
		t.Fatal(err)
	}
	ids, err := s.migrateDrainSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		if id == sid {
			found = true
		}
	}
	if !found {
		t.Fatal("raw owned namespace not discovered without catalog rows")
	}
	plan, err := s.PlanMigration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if c.DrainIntent && plan.PendingIntents != 1 {
		t.Fatalf("plan intents=%d", plan.PendingIntents)
	}
	previous := migrateDrainAfterIntentClear
	t.Cleanup(func() { migrateDrainAfterIntentClear = previous })
	fired := false
	if c.DrainIntent {
		migrateDrainAfterIntentClear = func(id schema.SessionID) error {
			if id != sid {
				return nil
			}
			fired = true
			if hash := queryMigrateString(t, s, `SELECT indexed_input_hash FROM sessions WHERE session_id='`+string(sid)+`'`); hash != nil {
				t.Fatal("input proof survived destructive intent clear")
			}
			if flag := queryMigrateInt(t, s, `SELECT content_sweep_pending FROM sessions WHERE session_id='`+string(sid)+`'`); flag != 1 {
				t.Fatal("flag not durable before intent clear")
			}
			if c.DrainCrash {
				return errors.New("injected stop after intent clear")
			}
			return nil
		}
	}
	var result MigrateResult
	_, _, err = s.migrateDrain(context.Background(), &result)
	if err != nil {
		t.Fatal(err)
	}
	if c.DrainIntent && !fired {
		t.Fatal("intent-clear boundary never executed")
	}
	if c.DrainCrash {
		if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "injected stop") {
			t.Fatalf("partial drain warnings=%v", result.Warnings)
		}
		if queryMigrateInt(t, s, `SELECT content_sweep_pending FROM sessions WHERE session_id='`+string(sid)+`'`) != 1 {
			t.Fatal("crash lost retry flag")
		}
		migrateDrainAfterIntentClear = nil
		if _, _, err := s.migrateDrain(context.Background(), &result); err != nil {
			t.Fatal(err)
		}
	}
	if c.DrainRow {
		if hash := queryMigrateString(t, s, `SELECT indexed_input_hash FROM sessions WHERE session_id='`+string(sid)+`'`); hash != nil {
			t.Fatal("drained session not marked for re-index")
		}
		if queryMigrateInt(t, s, `SELECT content_sweep_pending FROM sessions WHERE session_id='`+string(sid)+`'`) != 0 {
			t.Fatal("successful sweep retained flag")
		}
	}
	if dir != "" {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan directory remains: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(sessionDir, "generation-intent.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("intent remains: %v", err)
	}
	if data, err := os.ReadFile(retained); err != nil || string(data) != "keep retained source" {
		t.Fatalf("retained source changed: %v", err)
	}
	if _, _, err := s.migrateDrain(context.Background(), &result); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
}
