package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// makeResolveCmd returns a minimal cobra.Command with --session and --session-from-file flags
// registered, suitable for testing resolveSessionIDs.
func makeResolveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "test-resolve",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
	cmd.Flags().String("session", "", "Single session ID")
	cmd.Flags().String("session-from-file", "", "File of session IDs")
	return cmd
}

// executeResolveCmd parses the given args on the test command and calls resolveSessionIDs.
func executeResolveCmd(t *testing.T, args []string) ([]string, error) {
	t.Helper()
	cmd := makeResolveCmd()
	cmd.SetArgs(args)
	// Parse flags (needed to mark Changed flags).
	if err := cmd.ParseFlags(args); err != nil {
		return nil, fmt.Errorf("ParseFlags: %w", err)
	}
	return resolveSessionIDs(cmd)
}

// TestResolveSessionIDs_SessionFlag verifies that --session returns a single-element slice.
func TestResolveSessionIDs_SessionFlag(t *testing.T) {
	t.Parallel()
	ids, err := executeResolveCmd(t, []string{"--session", "99d59925-36bc-424c-a789-8be54d9702ba"})
	if err != nil {
		t.Fatalf("resolveSessionIDs: unexpected error: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 ID, got %d", len(ids))
	}
	if ids[0] != "99d59925-36bc-424c-a789-8be54d9702ba" {
		t.Errorf("ID: expected %q, got %q", "99d59925-36bc-424c-a789-8be54d9702ba", ids[0])
	}
}

// TestResolveSessionIDs_FromFile verifies that --session-from-file reads IDs line by line.
func TestResolveSessionIDs_FromFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ids.txt")
	content := "99d59925-36bc-424c-a789-8be54d9702ba\naaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	ids, err := executeResolveCmd(t, []string{"--session-from-file", path})
	if err != nil {
		t.Fatalf("resolveSessionIDs: unexpected error: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 IDs, got %d: %v", len(ids), ids)
	}
	if ids[0] != "99d59925-36bc-424c-a789-8be54d9702ba" {
		t.Errorf("ids[0]: expected %q, got %q", "99d59925-36bc-424c-a789-8be54d9702ba", ids[0])
	}
	if ids[1] != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("ids[1]: expected %q, got %q", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ids[1])
	}
}

// TestResolveSessionIDs_FromFileSkipsBlankLines verifies that blank and
// whitespace-only lines in the ID file are silently skipped.
func TestResolveSessionIDs_FromFileSkipsBlankLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ids.txt")
	content := "\n  \n99d59925-36bc-424c-a789-8be54d9702ba\n\n   \naaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\n\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	ids, err := executeResolveCmd(t, []string{"--session-from-file", path})
	if err != nil {
		t.Fatalf("resolveSessionIDs: unexpected error: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 IDs (blank lines skipped), got %d: %v", len(ids), ids)
	}
}

// TestResolveSessionIDs_FromFileEmptyFile verifies that an empty file returns an
// empty (but non-error) slice.
func TestResolveSessionIDs_FromFileEmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	ids, err := executeResolveCmd(t, []string{"--session-from-file", path})
	if err != nil {
		t.Fatalf("resolveSessionIDs: unexpected error for empty file: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 IDs for empty file, got %d", len(ids))
	}
}

// TestResolveSessionIDs_BothFlagsError verifies that providing both --session and
// --session-from-file returns an actionable error.
func TestResolveSessionIDs_BothFlagsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ids.txt")
	if err := os.WriteFile(path, []byte("some-id\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := executeResolveCmd(t, []string{
		"--session", "99d59925-36bc-424c-a789-8be54d9702ba",
		"--session-from-file", path,
	})
	if err == nil {
		t.Fatal("resolveSessionIDs: expected error when both flags provided, got nil")
	}
	if !strings.Contains(err.Error(), "both --session and --session-from-file") {
		t.Errorf("error message missing conflict context: %v", err)
	}
}

// TestResolveSessionIDs_NeitherFlagError verifies that providing neither --session nor
// --session-from-file returns an actionable error.
func TestResolveSessionIDs_NeitherFlagError(t *testing.T) {
	t.Parallel()
	_, err := executeResolveCmd(t, []string{})
	if err == nil {
		t.Fatal("resolveSessionIDs: expected error when no flags provided, got nil")
	}
	if !strings.Contains(err.Error(), "neither --session nor --session-from-file") {
		t.Errorf("error message missing guidance: %v", err)
	}
}

// TestResolveSessionIDs_MissingFile verifies that a non-existent --session-from-file
// path returns an actionable error.
func TestResolveSessionIDs_MissingFile(t *testing.T) {
	t.Parallel()
	_, err := executeResolveCmd(t, []string{"--session-from-file", "/nonexistent/path/ids.txt"})
	if err == nil {
		t.Fatal("resolveSessionIDs: expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent") && !strings.Contains(err.Error(), "ids.txt") {
		t.Errorf("error message should reference the missing file path: %v", err)
	}
}

// ---------------------------------------------------------------------------
// resolveOptionalSessionIDs tests
// ---------------------------------------------------------------------------

// makeOptionalResolveCmd returns a minimal cobra.Command with --session and
// --session-from-file flags registered, suitable for testing resolveOptionalSessionIDs.
func makeOptionalResolveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "test-optional-resolve",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
	cmd.Flags().String("session", "", "Single session ID")
	cmd.Flags().String("session-from-file", "", "File of session IDs")
	return cmd
}

// executeOptionalResolveCmd parses the given args on the test command and calls
// resolveOptionalSessionIDs.
func executeOptionalResolveCmd(t *testing.T, args []string) ([]string, error) {
	t.Helper()
	cmd := makeOptionalResolveCmd()
	cmd.SetArgs(args)
	if err := cmd.ParseFlags(args); err != nil {
		return nil, fmt.Errorf("ParseFlags: %w", err)
	}
	return resolveOptionalSessionIDs(cmd)
}

// TestResolveOptionalSessionIDs_NeitherFlag verifies that providing neither flag
// returns (nil, nil) — the "all sessions" signal.
func TestResolveOptionalSessionIDs_NeitherFlag(t *testing.T) {
	t.Parallel()
	ids, err := executeOptionalResolveCmd(t, []string{})
	if err != nil {
		t.Fatalf("resolveOptionalSessionIDs: expected nil error when neither flag set, got: %v", err)
	}
	if ids != nil {
		t.Errorf("resolveOptionalSessionIDs: expected nil slice, got %v", ids)
	}
}

// TestResolveOptionalSessionIDs_SessionFlag verifies that --session returns a
// single-element slice.
func TestResolveOptionalSessionIDs_SessionFlag(t *testing.T) {
	t.Parallel()
	ids, err := executeOptionalResolveCmd(t, []string{"--session", "99d59925-36bc-424c-a789-8be54d9702ba"})
	if err != nil {
		t.Fatalf("resolveOptionalSessionIDs: unexpected error: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 ID, got %d", len(ids))
	}
	if ids[0] != "99d59925-36bc-424c-a789-8be54d9702ba" {
		t.Errorf("ID: expected %q, got %q", "99d59925-36bc-424c-a789-8be54d9702ba", ids[0])
	}
}

// TestResolveOptionalSessionIDs_FromFile verifies that --session-from-file reads
// IDs line by line.
func TestResolveOptionalSessionIDs_FromFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ids.txt")
	content := "99d59925-36bc-424c-a789-8be54d9702ba\naaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	ids, err := executeOptionalResolveCmd(t, []string{"--session-from-file", path})
	if err != nil {
		t.Fatalf("resolveOptionalSessionIDs: unexpected error: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 IDs, got %d: %v", len(ids), ids)
	}
}

// TestResolveOptionalSessionIDs_BothFlagsError verifies that providing both
// --session and --session-from-file returns an error containing
// "both --session and --session-from-file".
func TestResolveOptionalSessionIDs_BothFlagsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ids.txt")
	if err := os.WriteFile(path, []byte("some-id\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := executeOptionalResolveCmd(t, []string{
		"--session", "99d59925-36bc-424c-a789-8be54d9702ba",
		"--session-from-file", path,
	})
	if err == nil {
		t.Fatal("resolveOptionalSessionIDs: expected error when both flags provided, got nil")
	}
	if !strings.Contains(err.Error(), "both --session and --session-from-file") {
		t.Errorf("error message missing conflict context: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ensureOutputDir tests
// ---------------------------------------------------------------------------

// TestEnsureOutputDir_CreatesDirectory verifies that ensureOutputDir creates the
// target directory and any parent directories.
func TestEnsureOutputDir_CreatesDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "a", "b", "c")

	if err := ensureOutputDir(target); err != nil {
		t.Fatalf("ensureOutputDir: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat %q: %v", target, err)
	}
	if !info.IsDir() {
		t.Errorf("%q: expected directory, got file", target)
	}
}

// TestEnsureOutputDir_IdempotentOnExisting verifies that calling ensureOutputDir
// on a pre-existing directory is a no-op (no error).
func TestEnsureOutputDir_IdempotentOnExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := ensureOutputDir(dir); err != nil {
		t.Fatalf("ensureOutputDir on existing dir: %v", err)
	}
}
