package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// ModuleRoot walks up from the test's working directory to the directory holding
// go.mod, so a test can address files in OTHER packages by their repository path.
//
// A test that has to reason about more than its own package - a codegen freshness
// gate, or a guard over source text in several packages at once - cannot use
// relative paths without encoding how deep it happens to sit. Resolving the root
// once keeps that knowledge out of every such test.
//
// It is NOT a de-duplication, and an earlier version of this comment claimed it
// was. The walk-up-to-go.mod idiom appears in several places and none of them
// were absorbed into this: the two cmd/gen-* mains cannot call it, because it
// takes a *testing.T and a production main must not import testing, and the
// pre-existing copies in internal/e2e and internal/redactmock were left where
// they are. It serves the tests that call it, which today is two.
//
// It fails the test rather than returning an error: a test that cannot locate the
// module cannot have proved whatever it was about to check, and continuing would
// report a pass over zero files.
func ModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("locate the module root: reading the working directory failed: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("locate the module root: walked up from the test working directory to %q without finding a go.mod. "+
				"A test that addresses files by repository path cannot run outside the module, so nothing it was about to "+
				"check has been proved. Run it from within the peasant module.", dir)
		}
		dir = parent
	}
}
