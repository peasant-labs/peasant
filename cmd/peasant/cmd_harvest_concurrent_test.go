package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// gatedWalkFS pauses the first walk of root until release is closed and
// counts every walk of root.
type gatedWalkFS struct {
	*ingest.OSFileSystem
	root    string
	walks   atomic.Int32
	paused  chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *gatedWalkFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	if root == f.root {
		if f.walks.Add(1) == 1 {
			f.once.Do(func() { close(f.paused) })
			<-f.release
		}
	}
	return f.OSFileSystem.WalkDir(root, fn)
}

// waitingWarning is a slog handler that closes seen when the run lock logs
// that it is waiting for another harvest: the wake source for the negative
// assertion below.
type waitingWarning struct {
	slog.Handler
	seen chan struct{}
	once sync.Once
}

func (h *waitingWarning) Handle(ctx context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "another harvest is running") {
		h.once.Do(func() { close(h.seen) })
	}
	return h.Handler.Handle(ctx, r)
}

// TestConcurrentHarvestsOnOneDataDirRunOneAtATime proves a second harvest on
// the same data dir waits for the first instead of racing it.
//
// Not parallel: it swaps the process-wide slog default to observe the second
// harvest's "waiting" warning, which no parallel test may share.
func TestConcurrentHarvestsOnOneDataDirRunOneAtATime(t *testing.T) {
	// Wrap a fresh handler: wrapping slog.Default().Handler() deadlocks
	// (the default handler writes through log, which calls back into slog).
	warned := &waitingWarning{Handler: slog.NewTextHandler(io.Discard, nil), seen: make(chan struct{})}
	previous := slog.Default()
	slog.SetDefault(slog.New(warned))
	t.Cleanup(func() { slog.SetDefault(previous) })
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "projects")
	for i := 1; i <= 3; i++ {
		sid := fmt.Sprintf("eeeeeeee-0000-4000-8000-00000000000%d", i)
		line := `{"type":"user","sessionId":"` + sid + `","uuid":"u1","cwd":"/tmp/qa-lock","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"
		if err := os.MkdirAll(filepath.Join(src, "-tmp-qa-lock"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "-tmp-qa-lock", sid+".jsonl"), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf("version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths:\n      - %s\n  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\noutput:\n  basePath: %s\n", src, filepath.Join(dir, "out"))
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	gate := &gatedWalkFS{OSFileSystem: &ingest.OSFileSystem{}, root: src, paused: make(chan struct{}), release: make(chan struct{})}
	run := func() (string, error) {
		root := newTestRoot()
		root.AddCommand(buildHarvestCommand(gate))
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"harvest", "--data-dir", dir, "--config-dir", dir, "--config", configPath})
		err := root.Execute()
		return out.String(), err
	}
	type outcome struct {
		out string
		err error
	}
	first, second := make(chan outcome, 1), make(chan outcome, 1)
	go func() { o, e := run(); first <- outcome{o, e} }()
	select {
	case <-gate.paused:
	case <-time.After(30 * time.Second):
		t.Fatal("first harvest never reached discovery")
	}
	go func() { o, e := run(); second <- outcome{o, e} }()
	// Wake when the second harvest reports it is waiting on the lock: it
	// has reached the point where, without the lock, it would walk.
	select {
	case <-warned.seen:
	case <-time.After(30 * time.Second):
		close(gate.release)
		t.Fatal("second harvest never reported waiting for the first")
	}
	if walks := gate.walks.Load(); walks != 1 {
		close(gate.release)
		<-first
		<-second
		t.Fatalf("second harvest started discovery (%d walks) while the first was still running", walks)
	}
	close(gate.release)
	a, b := <-first, <-second
	if a.err != nil || b.err != nil {
		t.Fatalf("harvests failed: first=%v second=%v\n%s\n%s", a.err, b.err, a.out, b.out)
	}
	if !strings.Contains(a.out, "3     active") || !strings.Contains(b.out, "3     unchanged") {
		t.Fatalf("want the first run to store the 3 (just-written, so active) sessions and the second to find them unchanged:\n--- first\n%s\n--- second\n%s", a.out, b.out)
	}
}
