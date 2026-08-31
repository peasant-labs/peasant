package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// TestProgressRenderer_Run_NonTTY verifies that in non-TTY mode (the default
// in test environments, where the writer is a bytes.Buffer rather than *os.File),
// Run() exits cleanly when ctx is cancelled without deadlocking or panicking.
func TestProgressRenderer_Run_NonTTY(t *testing.T) {
	state := ingest.NewProgressState()
	var buf bytes.Buffer
	r := newProgressRenderer(&buf, state, nil)

	ctx, cancel := context.WithCancel(context.Background())

	// Run() is launched in a goroutine (per the usage contract documented in
	// the progressRenderer struct comment). newProgressRenderer pre-increments
	// the WaitGroup so Wait() is safe immediately after go r.Run(ctx).
	go r.Run(ctx)

	// Cancel immediately; in non-TTY mode Run() just reads from ctx.Done().
	cancel()
	r.Wait()

	// In non-TTY mode no bytes are written to the buffer.
	if buf.Len() != 0 {
		t.Errorf("non-TTY renderer wrote %d bytes to buffer, want 0", buf.Len())
	}
}

// TestProgressRenderer_IsTTY_NonFileWriter verifies that non-*os.File writers
// are always detected as non-TTY, preventing ANSI escape sequences from
// being written to non-terminal destinations (e.g. log files, pipes).
func TestProgressRenderer_IsTTY_NonFileWriter(t *testing.T) {
	state := ingest.NewProgressState()
	var buf bytes.Buffer
	r := newProgressRenderer(&buf, state, nil)
	if r.IsTTY() {
		t.Error("IsTTY() = true for bytes.Buffer writer, want false")
	}
}

func TestProgressModelRenderWritesOutput(t *testing.T) {
	state := ingest.NewProgressState()
	// Emit a start event so the stage appears in the snapshot.
	state.Update(ingest.ProgressEvent{
		Kind:  ingest.KindStart,
		Stage: ingest.StageDiscover,
		Total: 5,
	})
	state.Update(ingest.ProgressEvent{
		Kind:  ingest.KindAdvance,
		Stage: ingest.StageDiscover,
		Done:  3,
		Total: 5,
	})

	out := progressModel{state: state, order: ingest.StageOrder}.render()
	if out == "" {
		t.Fatal("render() wrote nothing, want non-empty output")
	}
	// The output should contain the stage name.
	if !strings.Contains(out, ingest.StageDiscover.String()) {
		t.Errorf("render() output %q does not contain stage name %q", out, ingest.StageDiscover.String())
	}
}

func TestProgressRendererUsesGentleFrameRate(t *testing.T) {
	if progressRendererFPS != 24 {
		t.Fatalf("progressRendererFPS = %d, want 24", progressRendererFPS)
	}
}

func TestRenderProgressBarShowsNonZeroProgressBeforeFirstFullCell(t *testing.T) {
	line := renderProgressBar(ingest.StageExtract, ingest.StageProgress{Started: true, Done: 126, Total: 4953})
	if !strings.Contains(line, string(barFill)) {
		t.Fatalf("progress bar should show at least one filled cell for non-zero work; got %q", line)
	}
	if !strings.Contains(line, "126/4953") {
		t.Fatalf("progress bar count should remain precise; got %q", line)
	}
}

// TestProgressRenderer_Run_TTY_StartStop verifies that when isTTY is true the
// tick loop terminates cleanly after ctx is cancelled, without deadlocking.
// The renderer does a final redraw on cancel, so the buffer gets output.
func TestProgressRenderer_Run_TTY_StartStop(t *testing.T) {
	state := ingest.NewProgressState()
	state.Update(ingest.ProgressEvent{
		Kind:  ingest.KindStart,
		Stage: ingest.StageExtract,
		Total: 10,
	})

	var buf bytes.Buffer
	r := newProgressRenderer(&buf, state, nil)
	// Force TTY mode to exercise the tick path.
	r.isTTY = true

	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)

	// Allow a couple of ticks (100 ms each) so the ticker fires at least once.
	time.Sleep(250 * time.Millisecond)
	cancel()
	r.Wait()

	// At least one Bubble Tea render should have occurred before shutdown.
	if buf.Len() == 0 {
		t.Error("TTY renderer wrote 0 bytes after ticks + cancel, want some output")
	}
}
