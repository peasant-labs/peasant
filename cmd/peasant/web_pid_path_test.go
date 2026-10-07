package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestPidFilePathIsAValidWindowsComponent guards the backgrounded server's PID
// file name against a character Windows forbids inside a path component. A colon
// there names an NTFS alternate data stream instead of a file, so `web start`
// could not write the PID and `web stop` could not find it. The check runs on
// every platform so the name cannot regress from a unix-only workstation.
func TestPidFilePathIsAValidWindowsComponent(t *testing.T) {
	base := filepath.Base(pidFilePath(8690))

	if want := "web-8690.pid"; base != want {
		t.Errorf("pidFilePath base = %q, want %q", base, want)
	}
	for _, forbidden := range []string{":", "*", "?", `"`, "<", ">", "|"} {
		if strings.Contains(base, forbidden) {
			t.Errorf("pidFilePath base %q contains %q, which Windows forbids in a path component", base, forbidden)
		}
	}
}

// TestPidFilePathSeparatesPortsPerPort keeps two concurrent servers from sharing
// one PID file, which is the reason the port appears in the name at all.
func TestPidFilePathSeparatesPortsPerPort(t *testing.T) {
	if a, b := pidFilePath(8690), pidFilePath(8691); a == b {
		t.Errorf("pidFilePath(8690) and pidFilePath(8691) are both %q; the port must distinguish them", a)
	}
}
