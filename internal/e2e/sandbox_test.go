package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSandbox_ComputationAndLayout verifies the SANDBOX is rooted at the resolved
// state dir (NOT hardcoded), timestamped, and that the XDG sub-dirs nest under it.
func TestSandbox_ComputationAndLayout(t *testing.T) {
	realStateDir := "/some/state/peasant"
	const ts int64 = 1234567890

	sandbox := computeSandbox(realStateDir, ts)
	want := "/some/state/peasant/test/e2e/1234567890"
	if sandbox != want {
		t.Errorf("computeSandbox = %q, want %q", sandbox, want)
	}

	data, config, state := sandboxXDG(sandbox)
	for name, got := range map[string]string{"data": data, "config": config, "state": state} {
		if filepath.Dir(got) != sandbox {
			t.Errorf("%s dir %q is not directly under sandbox %q", name, got, sandbox)
		}
	}
	// The DB lands at <data>/peasant/peasant.db under the sandbox (asserted in the
	// subprocess env by the harness; here we just confirm the data root nests).
	if filepath.Base(data) != "data" {
		t.Errorf("data home basename = %q, want data", filepath.Base(data))
	}
}

// TestSandbox_PruneStale verifies startup cleanup removes only age-stale leftover
// timestamped sandboxes and is a no-op when the base is absent.
func TestSandbox_PruneStale(t *testing.T) {
	realStateDir := t.TempDir()
	base := sandboxBase(realStateDir)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ttl := time.Hour

	// No-op when base absent.
	if err := pruneStaleSandboxesBefore(realStateDir, now, ttl); err != nil {
		t.Fatalf("prune on absent base: %v", err)
	}

	oldSandbox := computeSandbox(realStateDir, 111)
	recentSandbox := computeSandbox(realStateDir, 222)
	for _, dir := range []string{oldSandbox, recentSandbox} {
		mustMkdirAll(t, filepath.Join(dir, "data", "peasant"))
	}
	oldStray := filepath.Join(base, "old-stray")
	recentStray := filepath.Join(base, "recent-stray")
	mustWriteFile(t, oldStray)
	mustWriteFile(t, recentStray)

	setModTime(t, oldSandbox, now.Add(-2*ttl))
	setModTime(t, oldStray, now.Add(-2*ttl))
	setModTime(t, recentSandbox, now.Add(-ttl/2))
	setModTime(t, recentStray, now.Add(-ttl/2))

	if err := pruneStaleSandboxesBefore(realStateDir, now, ttl); err != nil {
		t.Fatalf("prune: %v", err)
	}

	for _, path := range []string{oldSandbox, oldStray} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("stale entry %s still exists, err=%v", path, err)
		}
	}
	for _, path := range []string{recentSandbox, recentStray} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("recent entry %s was pruned: %v", path, err)
		}
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setModTime(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("set modtime for %s: %v", path, err)
	}
}
