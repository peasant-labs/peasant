package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// makeTreeReadOnly removes write permission from every file and directory
// under root, the way `chmod -R a-w` does, and restores it when t ends so
// t.TempDir can clean up.
func makeTreeReadOnly(t *testing.T, root string) {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		paths = append(paths, p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	modes := map[string]os.FileMode{}
	for i := len(paths) - 1; i >= 0; i-- {
		info, err := os.Lstat(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		modes[paths[i]] = info.Mode().Perm()
		if err := os.Chmod(paths[i], info.Mode().Perm()&^0o222); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, p := range paths {
			_ = os.Chmod(p, modes[p])
		}
	})
	if f, err := os.CreateTemp(root, "probe"); err == nil {
		f.Close()
		t.Skip("running with privileges that ignore directory permissions")
	}
}

// TestReadOnlyDataDirServesReadOnlyCommands seeds a store, makes the data
// directory read-only, and runs the read-only commands from #691 through the
// real command tree. They must succeed, and a writing command must name the
// directory instead of failing inside SQLite.
func TestReadOnlyDataDirServesReadOnlyCommands(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)
	dataDir := string(defaults.ResolveDataDirPathWith(dir))
	makeTreeReadOnly(t, dataDir)

	for _, tc := range []struct {
		name string
		run  func() (string, error)
		want string
	}{
		{"sessions list", func() (string, error) {
			return executeWithDataDir(t, BuildSessionsCommand(), dir, []string{"list"})
		}, pruneTestSessionA[:8]},
		{"harvest verify", func() (string, error) {
			return executeWithDataDir(t, BuildHarvestCommand(), dir, []string{"verify"})
		}, "Peasant Database Verification"},
		{"harvest verify --content", func() (string, error) {
			return executeWithDataDir(t, BuildHarvestCommand(), dir, []string{"verify", "--content"})
		}, "Content verification"},
		{"metrics compute --dry-run", func() (string, error) {
			return executeWithDataDir(t, BuildMetricsCommand(), dir, []string{"compute", "--dry-run"})
		}, ""},
		{"prune --dry-run", func() (string, error) {
			return executeWithDataDir(t, BuildPruneCommand(), dir, []string{"--all", "--dry-run"})
		}, "2 session(s) would be deleted"},
	} {
		out, err := tc.run()
		if err != nil {
			t.Errorf("%s on a read-only data dir: %v\n%s", tc.name, err, out)
		} else if !strings.Contains(out, tc.want) {
			t.Errorf("%s: want %q in output; got:\n%s", tc.name, tc.want, out)
		}
	}

	out, err := executeWithDataDir(t, BuildMetricsCommand(), dir, []string{"compute"})
	if err == nil {
		t.Fatalf("metrics compute on a read-only data dir succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not writable") || !strings.Contains(err.Error(), dataDir) {
		t.Errorf("write on a read-only data dir: want a 'not writable: %s' error, got: %v", dataDir, err)
	}
}

// TestReadOnlyDataDirWritingCommandsRefuseUpFront pins #738: a writing
// command on a read-only data dir refuses before it does any work, with the
// same "data dir is not writable" error, instead of SQLite's after the work.
func TestReadOnlyDataDirWritingCommandsRefuseUpFront(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)
	dataDir := string(defaults.ResolveDataDirPathWith(dir))
	sourceDir := t.TempDir()
	outputDir := filepath.Join(t.TempDir(), "out")
	makeTreeReadOnly(t, dataDir)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"harvest verify --content --repair", []string{"verify", "--content", "--repair"}},
		{"harvest", []string{"--source-harness", string(defaults.HarnessClaudeCode), "--source-path", sourceDir, "--output", outputDir}},
	} {
		out, err := executeWithDataDir(t, BuildHarvestCommand(), dir, tc.args)
		if err == nil {
			t.Errorf("%s on a read-only data dir succeeded:\n%s", tc.name, out)
			continue
		}
		if !strings.Contains(err.Error(), "data dir is not writable: "+dataDir) {
			t.Errorf("%s: want a 'data dir is not writable: %s' error, got: %v", tc.name, dataDir, err)
		}
		if strings.Contains(strings.ToLower(err.Error()), "sqlite") {
			t.Errorf("%s: want no raw SQLite error, got: %v", tc.name, err)
		}
		for _, work := range []string{"Peasant Database Verification", "Content verification"} {
			if strings.Contains(out, work) {
				t.Errorf("%s: refused only after running the check (%q printed):\n%s", tc.name, work, out)
			}
		}
	}
}
