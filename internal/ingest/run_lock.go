package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/peasant-labs/peasant/internal/filelock"
)

// runLockPoll is how long one lock attempt waits before Run checks its
// context again.
const runLockPoll = 250 * time.Millisecond

// RunLockFileName is the lock file a writing run takes in the data directory
// when the caller passes WithRunLock.
const RunLockFileName = "harvest.lock"

// WithRunLock makes Run hold an exclusive advisory lock on path for a writing
// run, across processes. Two harvests started together on one data dir
// otherwise raced each other's index replacement: one refused parser results
// and the counts of both did not add up. The CLI and the web server pass a
// path in the data directory; a pipeline without this option takes no lock,
// which keeps in-memory test filesystems lock-free.
func WithRunLock(path string) PipelineOption {
	return func(p *Pipeline) { p.runLockPath = path }
}

// RunLockWaitNotice is the reason a waiting run reports through its
// ProgressState, so a full-screen view such as kickstart can say why it is
// not moving. It fits an 80-column terminal with the view's indent.
const RunLockWaitNotice = "waiting: another harvest is running on this data dir; ctrl+c cancels"

// ErrHarvestRunning is Run's error when WithRunLockNoWait is set and another
// harvest holds the data dir's lock.
var ErrHarvestRunning = errors.New("another harvest is running on this data dir")

// WithRunLockNoWait makes Run fail with ErrHarvestRunning instead of waiting
// for a held WithRunLock lock. It does not fail instantly: Run makes one lock
// attempt, which polls for up to runLockPoll (250ms), and fails when the lock
// is still held after it. The web server uses it: a
// request cannot show a waiting state or be cancelled while it waits, and a
// wedged CLI harvest would block it indefinitely.
func WithRunLockNoWait() PipelineOption {
	return func(p *Pipeline) { p.runLockNoWait = true }
}

// lockRun takes the WithRunLock lock, waiting for a running holder with one
// warning and honoring cancellation. A dry run writes nothing and takes none.
func (p *Pipeline) lockRun(ctx context.Context) (func(), error) {
	if p.config.DryRun || p.runLockPath == "" {
		return func() {}, nil
	}
	path := p.runLockPath
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("pipeline start: create the data directory for the harvest lock: %w", err)
	}
	waiting := false
	if prog := p.config.Progress; prog != nil {
		defer func() {
			if waiting {
				prog.SetWaiting("")
			}
		}()
	}
	for {
		release, err := filelock.Acquire(path, time.Now().Add(runLockPoll))
		if err == nil {
			return func() { _ = release() }, nil
		}
		if !filelock.IsDeadline(err) {
			return nil, fmt.Errorf("pipeline start: take the harvest lock: %w", err)
		}
		if p.runLockNoWait {
			return nil, fmt.Errorf("pipeline start: %w; retry when it finishes", ErrHarvestRunning)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("pipeline start: canceled while waiting for another harvest on this data dir: %w", ctxErr)
		}
		if !waiting {
			waiting = true
			if p.config.Progress != nil {
				p.config.Progress.SetWaiting(RunLockWaitNotice)
			}
			slog.Warn("harvest: another harvest is running on this data dir; waiting for it to finish")
		}
	}
}
