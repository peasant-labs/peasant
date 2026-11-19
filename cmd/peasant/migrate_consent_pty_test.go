package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestMigrateConsentAfterUpgradeMounted(t *testing.T) {
	const childEnabled = "1"
	if os.Getenv("PEASANT_TEST_MIGRATE_CHILD") == childEnabled {
		root := newTestRoot()
		root.AddCommand(BuildMigrateCommand())
		dir := os.Getenv("PEASANT_TEST_MIGRATE_DIR")
		root.SetArgs([]string{"--data-dir", dir, "--config-dir", dir, "--state-dir", dir, "migrate"})
		if err := root.Execute(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	dir := t.TempDir()
	seedPolicyStore(t, dir, "real-quick")
	master, terminal := openTestTerminal(t)
	setTestTerminalSize(t, terminal, 24, 80)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMigrateConsentAfterUpgradeMounted$")
	child.Env = append(os.Environ(), "PEASANT_TEST_MIGRATE_CHILD="+childEnabled, "PEASANT_TEST_MIGRATE_DIR="+dir)
	child.Stdin, child.Stdout, child.Stderr = terminal, terminal, terminal
	output := newSignalWriter("convert file-backed sessions in place?")
	drained := make(chan error, 1)
	go func() { _, err := io.Copy(output, master); drained <- err }()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-output.seen:
	case err := <-done:
		t.Fatalf("migrate exited before prompt: %v\n%s", err, output.String())
	case <-ctx.Done():
		t.Fatalf("migrate did not prompt: %v\n%s", ctx.Err(), output.String())
	}
	if _, err := master.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("migrate failed: %v\n%s", err, output.String())
		}
	case <-ctx.Done():
		t.Fatalf("migrate did not finish: %v\n%s", ctx.Err(), output.String())
	}
	_ = terminal.Close()
	if err := <-drained; err != nil && !errors.Is(err, syscall.EIO) {
		t.Fatal(err)
	}
	text := ansi.Strip(output.String())
	if !strings.Contains(text, "upgrading the local store") || !strings.Contains(text, "phase ") || strings.Contains(text, "aborted") {
		t.Fatalf("upgrade-to-conversion handoff failed:\n%s", text)
	}
}
