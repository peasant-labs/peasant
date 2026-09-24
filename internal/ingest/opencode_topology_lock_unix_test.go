//go:build linux || darwin

package ingest_test

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const openCodeBuildTopologyOwnershipSupported = true

// acquireOpenCodeBuildTopologyOwnership holds a non-blocking exclusive flock on
// the open source directory. Closing the descriptor, or the process exiting,
// releases it, so a killed run never leaves ownership behind.
func acquireOpenCodeBuildTopologyOwnership(sourceDirectory string) (release func() error, err error) {
	file, err := os.Open(sourceDirectory)
	if err != nil {
		return nil, fmt.Errorf("acquire exclusive build-topology startup ownership: open the source directory %q: %w; no sweep or case creation was performed; make sure the ingest source directory exists and is readable, then retry", sourceDirectory, err)
	}
	defer func() {
		if err == nil {
			return
		}
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close the source directory descriptor for %q after the failed ownership attempt: %w; whether it was released is unknown; end this test process, then retry", sourceDirectory, closeErr))
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("acquire exclusive build-topology startup ownership: inspect the source directory %q: %w; no sweep or case creation was performed; make sure the ingest source directory is readable, then retry", sourceDirectory, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("acquire exclusive build-topology startup ownership: %q is a %v, not a directory; no sweep or case creation was performed; run the guard from the ingest package directory", sourceDirectory, info.Mode().Type())
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("acquire exclusive build-topology startup ownership of %q: exclusive startup ownership is busy because another test run holds this directory; no sweep or case creation was performed; stop the overlapping run or run tests in a separate worktree, then retry", sourceDirectory)
		}
		return nil, fmt.Errorf("acquire exclusive build-topology startup ownership: lock the source directory %q: %w; no sweep or case creation was performed; run the guard on a local filesystem that supports flock, then retry", sourceDirectory, err)
	}
	return newOpenCodeBuildTopologyRelease(file, sourceDirectory), nil
}
