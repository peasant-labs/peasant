package defaults_test

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// TestResolveOutputBasePath_HonorsXDGDataHome verifies the ingest transcript
// output base honors XDG_DATA_HOME — it nests under the
// resolved data dir exactly like the DB path, so XDG_DATA_HOME fully sandboxes
// ingest output and the two no longer diverge.
func TestResolveOutputBasePath_HonorsXDGDataHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(defaults.EnvXDGDataHome.String(), tmp)

	got := string(defaults.ResolveOutputBasePath())
	want := filepath.Join(tmp, string(defaults.AppName), defaults.OutputSyncSubdir)
	if got != want {
		t.Errorf("ResolveOutputBasePath() = %q, want %q (must honor XDG_DATA_HOME)", got, want)
	}

	// It must live under the SAME data dir as the DB (no divergence).
	dbDataDir := filepath.Dir(string(defaults.ResolveDBFilePath()))
	if filepath.Dir(got) != dbDataDir {
		t.Errorf("output base %q is not under the DB's data dir %q — XDG sandboxing would diverge", got, dbDataDir)
	}
}

// TestResolveOutputBasePath_DefaultsToHome verifies that with XDG_DATA_HOME unset,
// the output base falls back to ~/.local/share/peasant/peasant-sync (no behavior
// change from the previous hardcoded default).
func TestResolveOutputBasePath_DefaultsToHome(t *testing.T) {
	t.Setenv(defaults.EnvXDGDataHome.String(), "")
	t.Setenv("HOME", "/home/example")

	got := string(defaults.ResolveOutputBasePath())
	want := filepath.Join("/home/example", ".local", "share", string(defaults.AppName), defaults.OutputSyncSubdir)
	if got != want {
		t.Errorf("ResolveOutputBasePath() with XDG unset = %q, want %q", got, want)
	}
}

// TestResolveVillagePullsDirPath_HonorsXDGDataHome verifies the village-pulls/
// root honors XDG_DATA_HOME and lives under the SAME data dir as the DB — a
// SEPARATE namespace from peasant-sync (pulled data is foreign and one-way).
func TestResolveVillagePullsDirPath_HonorsXDGDataHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(defaults.EnvXDGDataHome.String(), tmp)

	got := string(defaults.ResolveVillagePullsDirPath())
	want := filepath.Join(tmp, string(defaults.AppName), defaults.VillagePullsSubdir)
	if got != want {
		t.Errorf("ResolveVillagePullsDirPath() = %q, want %q (must honor XDG_DATA_HOME)", got, want)
	}

	// It must live under the SAME data dir as the DB, and be a SEPARATE leaf from
	// the ingest output (peasant-sync) — never the same tree.
	dbDataDir := filepath.Dir(string(defaults.ResolveDBFilePath()))
	if filepath.Dir(got) != dbDataDir {
		t.Errorf("village-pulls root %q is not under the DB's data dir %q", got, dbDataDir)
	}
	if got == string(defaults.ResolveOutputBasePath()) {
		t.Errorf("village-pulls root must be a SEPARATE namespace from peasant-sync, got same path %q", got)
	}
}

// TestResolveVillagePullsDirPathWith_PrefersOverride verifies the parallel-safe
// override path mirrors ResolveDataDirPathWith.
func TestResolveVillagePullsDirPathWith_PrefersOverride(t *testing.T) {
	got := string(defaults.ResolveVillagePullsDirPathWith("/tmp/override"))
	want := filepath.Join("/tmp/override", string(defaults.AppName), defaults.VillagePullsSubdir)
	if got != want {
		t.Errorf("ResolveVillagePullsDirPathWith() = %q, want %q", got, want)
	}
}
