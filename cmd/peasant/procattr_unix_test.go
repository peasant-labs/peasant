//go:build unix

package main

import "testing"

// TestBackgroundProcAttrDetachesOnUnix pins the unix process attributes to the
// value the web server detach used before it moved behind the build tag:
// Setsid set, nothing else. #367 requires Linux and macOS process attributes to
// be identical to the pre-extraction values, and this is that assertion.
func TestBackgroundProcAttrDetachesOnUnix(t *testing.T) {
	got := backgroundProcAttr()
	if !got.Setsid {
		t.Fatalf("backgroundProcAttr() = %#v, want Setsid true", got)
	}
	if got.Setpgid || got.Setctty || got.Noctty || got.Foreground || got.Credential != nil {
		t.Fatalf("backgroundProcAttr() = %#v, want only Setsid set", got)
	}
}
