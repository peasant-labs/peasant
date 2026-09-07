//go:build linux || darwin

package ingest

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

const artifactLocksSupported = true
const artifactNonblockFlag = syscall.O_NONBLOCK

func lockArtifactFile(ctx context.Context, file *os.File, mode ArtifactLockMode) error {
	operation := syscall.LOCK_SH | syscall.LOCK_NB
	if mode == ArtifactLockWrite {
		operation = syscall.LOCK_EX | syscall.LOCK_NB
	}
	retry := time.NewTicker(10 * time.Millisecond)
	defer retry.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := syscall.Flock(int(file.Fd()), operation)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-retry.C:
		}
	}
}

func unlockArtifactFile(file *os.File) error { return syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }
