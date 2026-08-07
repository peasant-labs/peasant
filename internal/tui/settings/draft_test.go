package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// connectedAccessor edits Village.Connected — a simple bool setting used to
// exercise draft dirty tracking and commit.
func connectedAccessor() Accessor[bool] {
	return Accessor[bool]{
		Get: func(c *config.Config) bool { return c.Village.Connected },
		Set: func(c *config.Config, v bool) { c.Village.Connected = v },
	}
}

// writeConfigFile writes a valid config to a temp path via the real atomic save
// and returns the path plus the loaded config.
func writeConfigFile(t *testing.T) (string, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := config.BaseConfig()
	cfg.User.Email = testutil.TestEmail
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seed config: %v", err)
	}
	loaded, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse seed config: %v", err)
	}
	return path, loaded
}

func TestDraft_DirtyTracking(t *testing.T) {
	path, loaded := writeConfigFile(t)
	acc := connectedAccessor()

	d, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	if d.Dirty() {
		t.Fatalf("fresh draft is dirty")
	}
	// Editing the working copy makes it dirty; the loaded config is untouched.
	acc.Set(d.Working(), !acc.Get(loaded))
	if !d.Dirty() {
		t.Fatalf("draft not dirty after edit")
	}
	if acc.Get(loaded) == acc.Get(d.Working()) {
		t.Fatalf("edit leaked into the loaded config (not deep-copied)")
	}
	// Reverting the value clears dirty.
	acc.Set(d.Working(), acc.Get(d.Baseline()))
	if d.Dirty() {
		t.Fatalf("draft still dirty after reverting to baseline")
	}
}

func TestDraft_CommitAtomicRoundTrip(t *testing.T) {
	path, loaded := writeConfigFile(t)
	acc := connectedAccessor()
	want := !acc.Get(loaded)

	d, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	acc.Set(d.Working(), want)
	if err := d.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Re-read from disk through the real parse path: the edit persisted.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed config: %v", err)
	}
	reloaded, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse committed config: %v", err)
	}
	if got := acc.Get(reloaded); got != want {
		t.Fatalf("committed Village.Connected = %v, want %v", got, want)
	}
}

func TestDraft_DiscardLeavesFileBytesUnchanged(t *testing.T) {
	path, loaded := writeConfigFile(t)
	acc := connectedAccessor()

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	d, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	acc.Set(d.Working(), !acc.Get(loaded))
	if err := d.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if d.Dirty() {
		t.Fatalf("draft still dirty after Discard")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("Discard changed the on-disk config bytes")
	}
}

func TestDraft_CommitDriftFailsClosed(t *testing.T) {
	path, loaded := writeConfigFile(t)
	acc := connectedAccessor()

	d, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	acc.Set(d.Working(), !acc.Get(loaded))

	// Another process changes the file after the draft opened.
	external := config.BaseConfig()
	external.User.Email = "someone-else@example.com"
	if err := config.SaveAtomic(path, external); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	before, _ := os.ReadFile(path)

	err = d.Commit()
	if err == nil {
		t.Fatalf("Commit succeeded despite drift")
	}
	if !strings.Contains(err.Error(), "changed") {
		t.Fatalf("drift error not actionable: %v", err)
	}
	// Fail closed: the external file is untouched.
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("drift-blocked commit still wrote to disk")
	}
}
