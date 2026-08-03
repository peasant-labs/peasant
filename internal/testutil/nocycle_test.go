package testutil_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestTestutil_DoesNotReachInternalConfig enforces the invariant the over-claim
// corpus rests on.
//
// internal/config/policy_test.go is `package config`, not `package config_test`,
// and it imports testutil. So the moment anything in testutil's transitive
// closure imports internal/config, that file stops compiling — an import cycle
// through a test file, which is a confusing failure to arrive at cold.
//
// The corpus used to live in its own package that imported nothing, where this
// was structural. Moving it here bought a smaller package count and sold that
// guarantee; this is the guarantee bought back, stated where it can fail. It
// asks the toolchain rather than a hand-maintained list, so a dependency added
// three levels down is caught the same as a direct one.
func TestTestutil_DoesNotReachInternalConfig(t *testing.T) {
	t.Parallel()
	const forbidden = "github.com/peasant-labs/peasant/internal/config"
	output, err := exec.Command("go", "list", "-deps", "github.com/peasant-labs/peasant/internal/testutil").Output()
	if err != nil {
		t.Skipf("go list unavailable in this environment: %v", err)
	}
	for _, dep := range strings.Fields(string(output)) {
		if dep == forbidden {
			t.Fatalf("internal/testutil now reaches %s.\n\n"+
				"internal/config/policy_test.go is an INTERNAL test (package config) and imports testutil for the shared "+
				"over-claim corpus, so this creates an import cycle and that file will not compile. The corpus lived in a "+
				"package that depended on nothing precisely to make this impossible; it was moved here because both "+
				"consumers already imported testutil, and this assertion is what replaced the structural guarantee.\n\n"+
				"Either drop the new dependency, or move the corpus back out to a package that depends on nothing.", forbidden)
		}
	}
}
