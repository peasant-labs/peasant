package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteFileAtomicWritesNewFile pins the success path: bytes land at the
// target with the requested content and no temp files remain beside it.
func TestWriteFileAtomicWritesNewFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "session.json")
	if err := writeFileAtomic(target, []byte(`{"turns":1}`), 0644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"turns":1}` {
		t.Fatalf("target = %q, want committed bytes", got)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".export-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temp files remain after success: %v", leftovers)
	}
}

// TestWriteFileAtomicReplacesTarget pins overwrite: a pre-existing target is
// replaced whole, never appended to or left half-written.
func TestWriteFileAtomicReplacesTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "session.json")
	if err := os.WriteFile(target, []byte("prior bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("new bytes"), 0644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new bytes" {
		t.Fatalf("target = %q, want replaced bytes", got)
	}
}

// TestWriteFileAtomicFailureLeavesTargetUntouched pins the failure path: when
// the write cannot proceed, the pre-existing target keeps its exact bytes and
// no temp files remain. The read-only directory refuses temp creation; the
// parent-is-a-file case refuses the same way even for a privileged caller.
func TestWriteFileAtomicFailureLeavesTargetUntouched(t *testing.T) {
	t.Parallel()
	t.Run("unwritable directory", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions; the parent-is-a-file subtest covers the failure path")
		}
		dir := t.TempDir()
		target := filepath.Join(dir, "session.json")
		if err := os.WriteFile(target, []byte("prior bytes"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0755) })
		if err := writeFileAtomic(target, []byte("new bytes"), 0644); err == nil {
			t.Fatal("writeFileAtomic into an unwritable directory succeeded; want refusal")
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "prior bytes" {
			t.Fatalf("failed write changed the target: %q", got)
		}
		leftovers, err := filepath.Glob(filepath.Join(dir, ".export-*.tmp"))
		if err != nil {
			t.Fatal(err)
		}
		if len(leftovers) != 0 {
			t.Fatalf("temp files remain after failure: %v", leftovers)
		}
	})
	t.Run("parent is a file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("blocker bytes"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := writeFileAtomic(filepath.Join(blocker, "out.json"), []byte("new bytes"), 0644); err == nil {
			t.Fatal("writeFileAtomic under a file parent succeeded; want refusal")
		}
		got, err := os.ReadFile(blocker)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "blocker bytes" {
			t.Fatalf("failed write changed the blocker: %q", got)
		}
	})
}

// TestExportSessionsFailureLeavesTargetUntouchedAndStdoutSilent drives the
// real sessions command against a session that cannot export and asserts the
// failure boundary: no target file appears and stdout carries no export line.
// The warning goes to stderr and the command reports the failure.
func TestExportSessionsFailureLeavesTargetUntouchedAndStdoutSilent(t *testing.T) {
	outDir := t.TempDir()
	cmd := buildExportSessionsCommand()
	if err := cmd.Flags().Set("session", "00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("output-dir", outDir); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})
	// Silence cobra's own error/usage echo: the test asserts what the command
	// itself writes, not the framework's error rendering.
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("export of an absent session succeeded; want failure")
	}
	if strings.Contains(stdout.String(), "exported ") {
		t.Fatalf("failed export wrote an export line to stdout: %q", stdout.String())
	}
	matches, err := filepath.Glob(filepath.Join(outDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("failed export created target files: %v", matches)
	}
	if !strings.Contains(stderr.String(), "warning:") {
		t.Fatalf("failed export reported nothing on stderr: %q", stderr.String())
	}
}
