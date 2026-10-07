package defaults_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// TestDefaultClaudePath_ResolvesBelowHome proves that the one supported
// Claude Code session root — defaults.DefaultClaudePath ("~/.claude/projects")
// — expands to a path under the platform's home directory on both unix
// (HOME) and Windows (USERPROFILE).
//
// The "~" expansion itself lives in internal/ingest (NewResolvedPath, in
// types.go), not in internal/defaults — defaults.DefaultClaudePath is just
// the unexpanded string constant. This test therefore calls the real
// ingest.NewResolvedPath rather than re-implementing tilde expansion here, so
// it exercises the actual code path peasant runs, not a stand-in for it. It
// lives in internal/defaults (an external defaults_test file, so importing
// ingest — which imports defaults — creates no cycle) because that is where
// the default value under test is declared, and reuses the existing
// setTestHome(t) helper (testhome_unix_test.go / testhome_windows_test.go,
// same defaults_test package) instead of a third HOME/USERPROFILE stand-in.
//
// The assertion is computed from setTestHome's returned home directory, never
// a hardcoded POSIX string, so it stays meaningful when run on either
// platform (see the Windows verification in the issue: this same test, cross
// -compiled and run as a native .exe, must pass with USERPROFILE substituted
// for HOME).
func TestDefaultClaudePath_ResolvesBelowHome(t *testing.T) {
	home := setTestHome(t)

	resolved, err := ingest.NewResolvedPath(defaults.DefaultClaudePath.String())
	if err != nil {
		t.Fatalf("NewResolvedPath(%q): %v", defaults.DefaultClaudePath, err)
	}
	got := string(resolved)

	want := filepath.Join(home, ".claude", "projects")
	if got != want {
		t.Errorf("DefaultClaudePath resolved to %q, want %q", got, want)
	}

	// Belt-and-suspenders: assert containment under home explicitly (not just
	// exact equality), with a separator boundary so a sibling directory that
	// merely shares "home" as a string prefix can never pass by accident.
	if !strings.HasPrefix(got, home+string(filepath.Separator)) {
		t.Errorf("DefaultClaudePath resolved to %q, want it below home directory %q", got, home)
	}
}
