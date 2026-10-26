package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStopWebReadsTheLegacyPIDFile proves the `web stop` fallback reads the
// pre-rename "web:<port>.pid" name, so a server started by a build older than
// the rename can still be stopped after the CLI is upgraded. The reserved port
// has no listener, so stopWeb takes its HTTP-failure fallback; the legacy file
// carries a non-numeric value so the returned error names the file it read.
func TestStopWebReadsTheLegacyPIDFile(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	legacy := legacyPIDFilePath(port)
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatalf("create the state dir: %v", err)
	}
	if err := os.WriteFile(legacy, []byte("not-a-pid"), 0o644); err != nil {
		t.Fatalf("write the legacy PID file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(legacy) })

	stopErr := stopWeb(port)
	if stopErr == nil {
		t.Fatalf("stopWeb(%d) = nil, want an error from the legacy PID file", port)
	}
	if !strings.Contains(stopErr.Error(), legacy) {
		t.Fatalf("stopWeb(%d) error = %q, want it to name the legacy PID file %s", port, stopErr, legacy)
	}
}

// TestStopWebPrefersTheCurrentPIDFile proves the current name wins when both
// files exist: the legacy read is a fallback, not a replacement.
func TestStopWebPrefersTheCurrentPIDFile(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	current := pidFilePath(port)
	legacy := legacyPIDFilePath(port)
	if err := os.MkdirAll(filepath.Dir(current), 0o755); err != nil {
		t.Fatalf("create the state dir: %v", err)
	}
	if err := os.WriteFile(current, []byte("current-not-a-pid"), 0o644); err != nil {
		t.Fatalf("write the current PID file: %v", err)
	}
	if err := os.WriteFile(legacy, []byte("legacy-not-a-pid"), 0o644); err != nil {
		t.Fatalf("write the legacy PID file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(current); _ = os.Remove(legacy) })

	stopErr := stopWeb(port)
	if stopErr == nil {
		t.Fatalf("stopWeb(%d) = nil, want an error from the current PID file", port)
	}
	if !strings.Contains(stopErr.Error(), current) {
		t.Fatalf("stopWeb(%d) error = %q, want it to name the current PID file %s", port, stopErr, current)
	}
}
