package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/internal/tui/ingestprogress"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/spf13/cobra"
)

func flagHarvestSearchIndex(t *testing.T, dir string, breakIndex bool) {
	t.Helper()
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	db, err := openPreparedStore(t, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_search_state SET needs_rebuild=1 WHERE id=1`, nil); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if breakIndex {
		if err := sqlitex.ExecuteTransient(conn, `DROP TABLE session_search_fts`, nil); err != nil {
			_ = conn.Close()
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestHarvestSearchRebuildProgressMounted exercises a flagged real store
// through the command's terminal path. The two rebuild lines must be inserted
// above the mounted progress block, not written directly into that block.
func TestHarvestSearchRebuildProgressMounted(t *testing.T) {
	dir := t.TempDir()
	flagHarvestSearchIndex(t, dir, false)
	master, terminal := openTestTerminal(t)
	setTestTerminalSize(t, terminal, 24, 80)
	output := newSignalWriter("rebuild the search index")
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(output, master)
		copied <- err
	}()
	ctx, cancel := context.WithTimeout(testwait.Context(t), 20*time.Second)
	defer cancel()
	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	root.AddCommand(BuildHarvestCommand())
	root.SetContext(ctx)
	root.SetErr(terminal)
	root.SetOut(io.Discard)
	root.SetArgs([]string{"harvest", "--data-dir", dir, "--config-dir", dir,
		"--config", writeTestConfigFile(t, dir), "--output", filepath.Join(dir, "sync")})
	done := make(chan error, 1)
	go func() { done <- root.Execute() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("harvest with a flagged search index: %v\n%s", err, ansi.Strip(output.String()))
		}
	case <-ctx.Done():
		t.Fatalf("harvest did not finish its search rebuild: %v", ctx.Err())
	}
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := testwait.Receive(t, copied, "the completed harvest PTY drained"); err != nil && !errors.Is(err, syscall.EIO) {
		t.Fatalf("read harvest terminal: %v", err)
	}
	raw := output.String()
	insertLine := regexp.MustCompile("\x1b\\[[0-9]+L")
	start := 0
	for _, marker := range []string{
		"store upgrade 1/1: rebuild the search index: started",
		"store upgrade 1/1: rebuild the search index: done in ",
	} {
		index := strings.Index(raw[start:], marker)
		if index < 0 {
			t.Fatalf("harvest did not print %q:\n%s", marker, ansi.Strip(raw))
		}
		end := start + index
		if !insertLine.MatchString(raw[start:end]) {
			t.Fatalf("harvest wrote %q into the live block rather than inserting it above:\n%q", marker, raw[start:end+len(marker)])
		}
		start = end + len(marker)
	}
}

func TestHarvestSearchRebuildProgressPlain(t *testing.T) {
	dir := t.TempDir()
	flagHarvestSearchIndex(t, dir, false)
	output, err := executeHarvestCmd(t, dir, []string{"--output", filepath.Join(dir, "sync")})
	if err != nil {
		t.Fatalf("plain harvest with a flagged search index: %v\n%s", err, output)
	}
	if !strings.Contains(output, "rebuild the search index: started") ||
		!strings.Contains(output, "rebuild the search index: done in ") ||
		strings.Contains(output, "\x1b[") {
		t.Fatalf("plain harvest did not stream both unstyled rebuild lines:\n%q", output)
	}
}

func TestHarvestProgressNoticeWriterKeepsPlainOutput(t *testing.T) {
	var out bytes.Buffer
	renderer := newProgressProgram(&out, ingest.NewProgressState(), nil)
	go renderer.Run(t.Context())
	if _, err := fmt.Fprintln(renderer, "synthetic rebuild notice"); err != nil {
		t.Fatal(err)
	}
	renderer.Finish(ingestprogress.FinalMsg{Outcome: ingestprogress.FinalSucceeded})
	done := make(chan struct{})
	go func() { renderer.Wait(); close(done) }()
	testwait.Receive(t, done, "the plain harvest renderer stopped")
	if got := out.String(); got != "synthetic rebuild notice\n" {
		t.Fatalf("plain notice = %q, want the unmodified line", got)
	}
}

func TestHarvestProgressNoticeWriterAfterRendererExit(t *testing.T) {
	master, terminal := openTestTerminal(t)
	setTestTerminalSize(t, terminal, 24, 80)
	output := newSignalWriter("ctrl+c to cancel")
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(output, master)
		copied <- err
	}()
	ctx, cancel := context.WithTimeout(testwait.Context(t), 8*time.Second)
	defer cancel()
	renderer := newProgressProgram(terminal, ingest.NewProgressState(), nil)
	go renderer.Run(ctx)
	select {
	case <-renderer.programReady:
	case <-ctx.Done():
		t.Fatalf("harvest progress did not start: %v", ctx.Err())
	}
	select {
	case <-output.seen:
	case <-ctx.Done():
		t.Fatalf("harvest progress did not mount: %v", ctx.Err())
	}
	renderer.program.Kill()
	renderer.Wait()
	wrote := make(chan error, 1)
	go func() {
		_, err := fmt.Fprintln(renderer, "synthetic notice after renderer exit")
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("harvest notice blocked after renderer exit: %v", ctx.Err())
	}
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := testwait.Receive(t, copied, "the stopped harvest renderer's PTY drained"); err != nil && !errors.Is(err, syscall.EIO) {
		t.Fatalf("read stopped harvest renderer output: %v", err)
	}
}

// A failed rebuild returns before the harvest pipeline starts. The renderer
// must still release the terminal before Cobra writes the command error.
func TestHarvestSearchRebuildFailureReleasesTerminal(t *testing.T) {
	dir := t.TempDir()
	flagHarvestSearchIndex(t, dir, true)
	master, terminal := openTestTerminal(t)
	setTestTerminalSize(t, terminal, 24, 80)
	output := newSignalWriter("rebuild the search index")
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(output, master)
		copied <- err
	}()
	ctx, cancel := context.WithTimeout(testwait.Context(t), 20*time.Second)
	defer cancel()
	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	root.AddCommand(BuildHarvestCommand())
	root.SetContext(ctx)
	root.SetErr(terminal)
	root.SetOut(io.Discard)
	root.SetArgs([]string{"harvest", "--data-dir", dir, "--config-dir", dir,
		"--config", writeTestConfigFile(t, dir), "--output", filepath.Join(dir, "sync")})
	done := make(chan error, 1)
	go func() { done <- root.Execute() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "heal the flagged search index") {
			t.Fatalf("harvest rebuild failure = %v, want the search-healing error", err)
		}
	case <-ctx.Done():
		t.Fatalf("harvest did not return after the search rebuild failed: %v", ctx.Err())
	}
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := testwait.Receive(t, copied, "the failed harvest PTY drained"); err != nil && !errors.Is(err, syscall.EIO) {
		t.Fatalf("read failed harvest terminal: %v", err)
	}
	raw := output.String()
	start := strings.Index(raw, "rebuild the search index: started")
	if start < 0 {
		t.Fatalf("harvest did not report the started rebuild:\n%q", raw)
	}
	restored := strings.Index(raw[start:], ansi.ShowCursor)
	commandError := strings.Index(raw[start:], "Error: heal the flagged search index")
	if restored < 0 || commandError < restored {
		t.Fatalf("harvest returned without restoring the terminal after the failed rebuild:\n%q", raw)
	}
}
