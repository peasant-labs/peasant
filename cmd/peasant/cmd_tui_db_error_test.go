package main

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
)

// TestTUINamesAnUnreadableDatabase guards #686: with a corrupt analytics
// database, `peasant tui` dropped the store.Open error and printed "real
// provider not initialized" plus the usage block. It must name the database
// and the open failure, without usage.
func TestTUINamesAnUnreadableDatabase(t *testing.T) {
	// No t.Parallel: the test stubs the process-global TTY probe, which is
	// only safe while no other test runs concurrently.
	previous := tuiTerminalAvailable
	tuiTerminalAvailable = func() bool { return true }
	t.Cleanup(func() { tuiTerminalAvailable = previous })
	dir := t.TempDir()
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	garbage := make([]byte, 50000)
	if _, err := rand.Read(garbage); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, bytes.Clone(garbage), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := executeWithDataDir(t, BuildTUICommand(), dir, nil)
	if err == nil {
		t.Fatalf("tui on a corrupt database succeeded:\n%s", out)
	}
	msg := err.Error()
	if strings.Contains(msg, "real provider not initialized") || !strings.Contains(msg, dbPath) {
		t.Errorf("tui error should name the database it could not open (%s), got: %v", dbPath, err)
	}
	if !strings.Contains(msg, "keep the moved file") {
		t.Errorf("the rebuild hint does not warn that local-only data is not rebuilt: %s", msg)
	}
	if strings.Contains(out, "Usage:") {
		t.Errorf("tui printed usage for a runtime failure:\n%s", out)
	}
}

// TestTUISkipsResidueRebuildWithoutTerminal proves a non-interactive `peasant
// tui` fails on the terminal check before opening the store: on an affected
// store it must not migrate (and rebuild) invisibly on the way to the same
// refusal. The database file is byte-identical and still at version 75
// afterwards. Both TTY detectors are stubbed false so the test is
// TTY-independent.
func TestTUISkipsResidueRebuildWithoutTerminal(t *testing.T) {
	previousTTY := tuiTerminalAvailable
	tuiTerminalAvailable = func() bool { return false }
	t.Cleanup(func() { tuiTerminalAvailable = previousTTY })
	previousStdin := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = previousStdin })
	dir := t.TempDir()
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	storetest.SeedResidueDB(t, dbPath, 75, storetest.AffectedUnion, storetest.AffectedLegacy)
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	out, err := executeWithDataDir(t, BuildTUICommand(), dir, nil)
	if err == nil {
		t.Fatalf("non-interactive tui on an affected store succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "interactive terminal") {
		t.Errorf("non-interactive tui must fail on the terminal check, got: %v", err)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("skipped tui run modified the database file")
	}
	if version, err := store.SchemaVersionAt(dbPath); err != nil || version != 75 {
		t.Errorf("store version = %d, err = %v; want the un-migrated 75", version, err)
	}
}
