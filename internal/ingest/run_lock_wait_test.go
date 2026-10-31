package ingest_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/filelock"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestRunLockWaitIsVisibleInProgress proves a run that waits for another
// harvest's lock says so through its ProgressState, which the kickstart
// screens render. The slog warning alone is hidden behind kickstart's
// full-screen TUI, so without this the screen sat on an empty progress list.
func TestRunLockWaitIsVisibleInProgress(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), ingest.RunLockFileName)
	release, err := filelock.Acquire(lockPath, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	prog := ingest.NewProgressState()
	p, err := newTestPipeline(testutil.NewMemFS(), nil, ingest.DefaultAdapterRegistry, ingest.PipelineConfig{OutputDir: "/out", Progress: prog}, ingest.WithRunLock(lockPath))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := p.Run(ctx); done <- err }()
	deadline := time.Now().Add(10 * time.Second)
	for prog.Waiting() == "" {
		if time.Now().After(deadline) {
			cancel()
			_ = release()
			t.Fatal("the run never reported that it is waiting for another harvest")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := prog.Waiting(); !strings.Contains(got, "another harvest is running") {
		t.Errorf("waiting notice = %q, want it to say another harvest is running", got)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run after cancel = %v, want context.Canceled", err)
	}
	_ = release()
	if got := prog.Waiting(); got != "" {
		t.Errorf("waiting notice after the run stopped = %q, want it cleared", got)
	}
}
