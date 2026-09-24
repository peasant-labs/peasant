package api_test

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// The session-list, grouped-sync, and child-reference construction sites all
// normalize a stored Unix-millisecond instant to UTC so the wire carries a
// Z-suffixed datetime the pinned client accepts. time.UnixMilli returns a value
// in the process local zone, so the bug those sites guard against is only
// observable when the local zone is NOT UTC. CI runs in UTC, where a dropped
// normalization would leave every regression green.
//
// forcedNonUTCLocalZoneEnv, when present in the environment, tells TestMain to
// install a fixed non-UTC local zone before the tests run. It is a fixed offset
// rather than a named zone so it does not depend on a tzdata database being
// present: a missing named zone silently falls back to UTC and would make the
// guard vacuous. The install itself lives in TestMain (package api), which runs
// once for the whole test binary; this file only provides the re-exec entry
// point for external (package api_test) timestamp tests. The internal
// (package api) timestamp tests use the identical helper in
// nonutc_local_zone_testhelper_test.go. The two definitions must keep the same
// environment variable name and offset semantics.
const forcedNonUTCLocalZoneEnv = "PEASANT_API_FORCE_NON_UTC_LOCAL"

// RunInForcedNonUTCLocalZone runs testName in a child process whose local zone
// is a fixed non-UTC offset, so a timestamp that is not explicitly normalized to
// UTC serializes with that offset and fails the caller's wire assertions.
//
// It returns true in the PARENT process, after the child has run: the caller
// must then return immediately, having delegated its assertions to the child. It
// returns false in the CHILD process, where the caller runs its body under the
// forced zone.
//
// The child fails closed: if TestMain did not install a non-UTC local zone (for
// instance because the environment variable did not propagate), the local offset
// is zero and the helper aborts rather than assert against UTC, which would be
// the very condition the test exists to rule out.
func RunInForcedNonUTCLocalZone(t *testing.T, testName string) (isParent bool) {
	t.Helper()
	if os.Getenv(forcedNonUTCLocalZoneEnv) == "1" {
		if _, offset := time.Now().Zone(); offset == 0 {
			t.Fatalf("forced non-UTC local-zone guard for %s: process local offset is 0 (UTC), so TestMain did not install the fixed zone; the timestamp assertions would be vacuous. Reproduce with: %s=1 go test -race ./internal/api -run '^%s$' -count=1",
				testName, forcedNonUTCLocalZoneEnv, testName)
		}
		return false
	}

	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$", "-test.v")
	cmd.Env = append(os.Environ(), forcedNonUTCLocalZoneEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("forced non-UTC local-zone child run of %s failed: %v\nThis child re-runs the test with time.Local set to a non-UTC fixed offset so a dropped UTC normalization is visible. Reproduce with: %s=1 go test -race ./internal/api -run '^%s$' -count=1\n--- child output ---\n%s",
			testName, err, forcedNonUTCLocalZoneEnv, testName, out)
	}
	return true
}
