package testutil

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestPlatformAbsPath_IsAbsOnRunningPlatform(t *testing.T) {
	t.Parallel()
	got := PlatformAbsPath("/test/path/session.jsonl")
	if !filepath.IsAbs(got) {
		t.Fatalf("PlatformAbsPath(%q) = %q, want an absolute path on %s", "/test/path/session.jsonl", got, runtime.GOOS)
	}
}

func TestPlatformAbsPath_UnixInputUnchangedOnUnix(t *testing.T) {
	t.Parallel()
	const posixPath = "/test/path/session.jsonl"
	got := PlatformAbsPath(posixPath)
	if runtime.GOOS == "windows" {
		// On Windows the input gains a volume and native separators, so it is
		// expected to change; assert that transformation instead of the
		// unix no-op, rather than skipping this platform.
		if !filepath.IsAbs(got) || got == posixPath {
			t.Fatalf("PlatformAbsPath(%q) = %q, want a volume-qualified absolute path on %s", posixPath, got, runtime.GOOS)
		}
		return
	}
	if got != posixPath {
		t.Fatalf("PlatformAbsPath(%q) = %q, want unchanged on %s", posixPath, got, runtime.GOOS)
	}
}
