package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
)

// harvestRendererHeader is the first line the mounted harvest renderer draws.
const harvestRendererHeader = "local import progress"

// TestOpenRunStoreWithMountedRendererWritesNothing pins the guard that keeps a
// schema-upgrade plan, prompt or progress line off a terminal a renderer owns:
// with the renderer mounted, a store that is behind is an error and the open
// writes no byte and starts no upgrade. Each write the upgrade makes goes
// straight to the terminal, and the renderer's next redraw erases it.
func TestOpenRunStoreWithMountedRendererWritesNothing(t *testing.T) {
	t.Run("behind store fails closed and writes nothing", func(t *testing.T) {
		stubTerminal(t, true)
		dir := t.TempDir()
		dbPath := seedPolicyStore(t, dir, "real-quick")
		before := policyFileHash(t, dbPath)
		started := 0
		previous := upgradeStore
		upgradeStore = func(context.Context, string, store.UpgradeOptions) (store.UpgradeReport, error) {
			started++
			return store.UpgradeReport{}, nil
		}
		t.Cleanup(func() { upgradeStore = previous })
		cmd, stderr := policyTestCmd(t, dir, false, "y\n")

		db, err := openRunStore(cmd, false, t.TempDir(), ingest.DefaultWriteConfig(1), storeUpgradeProgressMounted)
		if err == nil {
			_ = db.Close()
			t.Fatal("openRunStore on a behind store with the renderer mounted opened it; want the refusal")
		}
		var behind *storeUpgradeBehindWhileMountedError
		if !errors.As(err, &behind) {
			t.Fatalf("openRunStore error = %v; want the behind-while-mounted error", err)
		}
		if stderr.Len() != 0 {
			t.Errorf("openRunStore wrote to the terminal while the renderer was mounted:\n%s", stderr.String())
		}
		if started != 0 {
			t.Errorf("the upgrade started %d time(s) while the renderer was mounted, want none", started)
		}
		if got := policyFileHash(t, dbPath); got != before {
			t.Error("the refused open modified the database file")
		}
		for _, part := range []string{"what:", "why:", "where:", "when:", "meaning:", "fix:"} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("the error misses %q:\n%s", part, err)
			}
		}
	})

	t.Run("current store opens and writes nothing", func(t *testing.T) {
		stubTerminal(t, true)
		dir := t.TempDir()
		seedPolicyStore(t, dir, "current")
		cmd, stderr := policyTestCmd(t, dir, false, "")

		db, err := openRunStore(cmd, false, t.TempDir(), ingest.DefaultWriteConfig(1), storeUpgradeProgressMounted)
		if err != nil {
			t.Fatalf("openRunStore on a current store: %v", err)
		}
		_ = db.Close()
		if stderr.Len() != 0 {
			t.Errorf("a current store produced terminal output:\n%s", stderr.String())
		}
	})
}

// TestHarvestAsksBeforeTheRendererMounts runs the real harvest command on a
// terminal against a store with a long upgrade step, the case where the
// upgrade asks first. The prompt must reach the person before the harvest
// renderer draws anything: a renderer that is already mounted redraws over a
// prompt and holds the terminal's input, so the answer could not be given.
func TestHarvestAsksBeforeTheRendererMounts(t *testing.T) {
	const childEnabled = "1"
	if os.Getenv("PEASANT_TEST_HARVEST_UPGRADE_CHILD") == childEnabled {
		dir := os.Getenv("PEASANT_TEST_HARVEST_UPGRADE_DIR")
		root := newTestRoot()
		root.AddCommand(BuildHarvestCommand())
		root.SetArgs([]string{"harvest", "--data-dir", dir, "--config-dir", dir, "--state-dir", dir, "--config", writeTestConfigFile(t, dir), "--output", filepath.Join(dir, "out")})
		if err := root.Execute(); err != nil {
			println(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	dir := t.TempDir()
	dbPath := seedPolicyStore(t, dir, "real-v75-long")
	master, terminal := openTestTerminal(t)
	setTestTerminalSize(t, terminal, 24, 80)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHarvestAsksBeforeTheRendererMounts$")
	child.Env = append(os.Environ(), "PEASANT_TEST_HARVEST_UPGRADE_CHILD="+childEnabled, "PEASANT_TEST_HARVEST_UPGRADE_DIR="+dir)
	child.Stdin, child.Stdout, child.Stderr = terminal, terminal, terminal
	output := newSignalWriter("upgrade the store now?")
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
		t.Fatalf("harvest exited before it asked: %v\n%s", err, ansi.Strip(output.String()))
	case <-ctx.Done():
		t.Fatalf("harvest did not ask: %v\n%s", ctx.Err(), ansi.Strip(output.String()))
	}
	asked := ansi.Strip(output.String())
	if strings.Contains(asked, harvestRendererHeader) {
		t.Fatalf("the harvest renderer drew before the upgrade prompt was answered; its redraw would erase the prompt:\n%s", asked)
	}
	if !strings.Contains(asked, "needs an upgrade") {
		t.Errorf("the plan was not shown before the prompt:\n%s", asked)
	}
	if _, err := master.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("harvest failed after the upgrade was approved: %v\n%s", err, ansi.Strip(output.String()))
		}
	case <-ctx.Done():
		t.Fatalf("harvest did not finish: %v\n%s", ctx.Err(), ansi.Strip(output.String()))
	}
	_ = terminal.Close()
	if err := <-drained; err != nil && !errors.Is(err, syscall.EIO) {
		t.Fatal(err)
	}
	if version, err := store.SchemaVersionAt(dbPath); err != nil || version != store.CurrentSchemaVersion() {
		t.Errorf("store version = %d, err = %v; want the build schema %d after the approved upgrade", version, err, store.CurrentSchemaVersion())
	}
}
