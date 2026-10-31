package main

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// TestTUINamesAnUnreadableDatabase guards #686: with a corrupt analytics
// database, `peasant tui` dropped the store.Open error and printed "real
// provider not initialized" plus the usage block. It must name the database
// and the open failure, without usage.
func TestTUINamesAnUnreadableDatabase(t *testing.T) {
	t.Parallel()
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
