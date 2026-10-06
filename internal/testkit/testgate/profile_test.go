package testgate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFakeGo writes an executable stand-in for `go test` that parses the
// -cpuprofile and -run flags, echoes the run name to stdout (captured by the
// per-test log), optionally waits for a peer re-run to prove concurrency,
// optionally fails, and touches the profile path. It keeps the CPU-profile
// re-run tests hermetic and fast: no compilation and no real profiling.
func writeFakeGo(t *testing.T) string {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no POSIX shell for the fake go binary: %v", err)
	}
	path := filepath.Join(t.TempDir(), "fake-go")
	script := "#!" + shell + `
set -eu
profile=""
run=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -cpuprofile) profile="$2"; shift 2 ;;
    -run) run="$2"; shift 2 ;;
    *) shift ;;
  esac
done
name=$(printf '%s' "$run" | tr -d '^$')
printf 'profile %s\n' "$name"
if [ -n "${FAKE_GO_FAIL_NAME:-}" ] && [ "$name" = "$FAKE_GO_FAIL_NAME" ]; then
  printf 'simulated failure: %s\n' "$name" >&2
  exit 3
fi
if [ -n "${FAKE_GO_SYNC_DIR:-}" ]; then
  : > "$FAKE_GO_SYNC_DIR/$name"
  n=0
  while :; do
    count=$(ls -1 "$FAKE_GO_SYNC_DIR" | wc -l)
    if [ "$count" -ge "${FAKE_GO_SYNC_MIN:-2}" ]; then
      break
    fi
    n=$((n + 1))
    if [ "$n" -gt 200 ]; then
      printf 'rendezvous timeout: %s\n' "$name" >&2
      exit 4
    fi
    sleep 0.01
  done
fi
: > "$profile"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	return path
}

// TestRunCPUProfiles_ConcurrentIsolatedProfiles proves the slowest-test
// re-runs overlap rather than running one at a time (each fake leaves a marker
// and blocks until a peer starts), and that each writes its own
// cpuprofile/<slug>.pprof and <slug>.log carrying only its own run name.
func TestRunCPUProfiles_ConcurrentIsolatedProfiles(t *testing.T) {
	fakeGo := writeFakeGo(t)
	outDir := t.TempDir()
	syncDir := t.TempDir()
	names := []string{"TestAlpha", "TestBeta", "TestGamma", "TestDelta"}
	tests := make([]TestTiming, len(names))
	for i, name := range names {
		tests[i] = TestTiming{Test: name, WallMS: int64(len(names)-i) * 100}
	}
	cfg := BatchProfileConfig{
		Root:          testRepoRoot(t),
		GoBin:         fakeGo,
		Package:       "./internal/moduleboundary",
		OutDir:        outDir,
		Timeout:       30 * time.Second,
		Parallel:      1,
		CPUProfileTop: len(names),
		Env: append(os.Environ(),
			"FAKE_GO_SYNC_DIR="+syncDir,
			"FAKE_GO_SYNC_MIN=2",
		),
	}

	profiles, errs := runCPUProfiles(context.Background(), cfg, tests, len(names))
	if len(errs) != 0 {
		t.Fatalf("runCPUProfiles errors = %v, want none: the re-runs must overlap", errs)
	}
	if len(profiles) != len(names) {
		t.Fatalf("profiles = %v, want %d", profiles, len(names))
	}
	for i, name := range names {
		want := filepath.Join(outDir, "cpuprofile", name+".pprof")
		if profiles[i] != want {
			t.Fatalf("profiles[%d] = %q, want %q (slowest-first order)", i, profiles[i], want)
		}
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("profile %s not written: %v", want, err)
		}
		logPath := filepath.Join(outDir, "cpuprofile", name+".log")
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read %s: %v", logPath, err)
		}
		if !strings.Contains(string(data), name) {
			t.Fatalf("%s log does not name its own test: %q", name, data)
		}
		for _, other := range names {
			if other != name && strings.Contains(string(data), other) {
				t.Fatalf("profile %s observed %s's run: %q", name, other, data)
			}
		}
	}
}

// TestRunCPUProfiles_ReportsPerTestFailures proves a failing re-run is reported
// per test while its selected siblings still produce profiles, and that a test
// beyond -cpuprofile-top is not re-run.
func TestRunCPUProfiles_ReportsPerTestFailures(t *testing.T) {
	fakeGo := writeFakeGo(t)
	outDir := t.TempDir()
	tests := []TestTiming{
		{Test: "TestSlowFails", WallMS: 300},
		{Test: "TestSlowPasses", WallMS: 200},
		{Test: "TestFastNotSelected", WallMS: 100},
	}
	cfg := BatchProfileConfig{
		Root:          testRepoRoot(t),
		GoBin:         fakeGo,
		Package:       "./internal/moduleboundary",
		OutDir:        outDir,
		Timeout:       30 * time.Second,
		Parallel:      1,
		CPUProfileTop: 2,
		Env:           append(os.Environ(), "FAKE_GO_FAIL_NAME=TestSlowFails"),
	}

	profiles, errs := runCPUProfiles(context.Background(), cfg, tests, 2)
	if len(profiles) != 2 {
		t.Fatalf("profiles = %v, want the two selected tests", profiles)
	}
	wantFirst := filepath.Join(outDir, "cpuprofile", "TestSlowFails.pprof")
	if profiles[0] != wantFirst {
		t.Fatalf("profiles[0] = %q, want %q", profiles[0], wantFirst)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "TestSlowFails") || !strings.Contains(errs[0], "cpuprofile") {
		t.Fatalf("errs = %v, want one cpuprofile failure naming TestSlowFails", errs)
	}
	unselected := filepath.Join(outDir, "cpuprofile", "TestFastNotSelected.pprof")
	if _, err := os.Stat(unselected); err == nil {
		t.Fatalf("a test beyond -cpuprofile-top was re-run: %s", unselected)
	}
}
