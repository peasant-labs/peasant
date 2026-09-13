package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/peasant-labs/schema"
	"golang.org/x/sys/unix"
)

// SessionLocker acquires ONE per-session OS advisory lock from the shared
// owned-artifact lock namespace. Readers take the shared lock and hold it for
// the entire snapshot callback; activation and cleanup take the exclusive lock.
// The lock is process-wide and cross-process: the same underlying file
// description semantics that serialize two Peasant processes serialize two
// goroutines that open the file independently.
type SessionLocker interface {
	// LockShared waits for a shared session lock and returns its release.
	LockShared(context.Context, schema.SessionID) (func() error, error)
	// LockExclusive waits for an exclusive session lock and returns its release.
	LockExclusive(context.Context, schema.SessionID) (func() error, error)
}

// fileSessionLocker is the production lock implementation. Its lock directory
// is root-confined: the file is derived from the validated session identifier
// alone, so no captured path can escape the namespace.
type fileSessionLocker struct {
	root string
}

// NewFileSessionLocker opens the per-session lock namespace rooted at root.
// root is the same owned-artifact root the generation files live under.
func NewFileSessionLocker(root string) (SessionLocker, error) {
	if root == "" {
		return nil, fmt.Errorf("store: session lock root is empty; readers and activation cannot serialize; configure the owned-artifact root")
	}
	if err := os.MkdirAll(filepath.Join(root, "locks"), 0o700); err != nil {
		return nil, fmt.Errorf("store: create session lock directory under %s: %w; no lock was taken; fix filesystem access and retry", root, err)
	}
	return &fileSessionLocker{root: root}, nil
}

func (l *fileSessionLocker) LockShared(ctx context.Context, id schema.SessionID) (func() error, error) {
	return l.lock(ctx, id, unix.LOCK_SH, "shared")
}

func (l *fileSessionLocker) LockExclusive(ctx context.Context, id schema.SessionID) (func() error, error) {
	return l.lock(ctx, id, unix.LOCK_EX, "exclusive")
}

func (l *fileSessionLocker) lock(ctx context.Context, id schema.SessionID, mode int, label string) (func() error, error) {
	if _, err := schema.NewSessionID(string(id)); err != nil {
		return nil, fmt.Errorf("store: %s session lock for %q: %w; no lock was taken; supply a canonical session identifier", label, id, err)
	}
	if !filepath.IsLocal(string(id)) {
		return nil, fmt.Errorf("store: %s session lock for %q is not a confined name; no lock was taken; supply a canonical session identifier", label, id)
	}
	lockPath := filepath.Join(l.root, "locks", string(id)+".lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: open %s session lock %s: %w; no lock was taken; fix filesystem access and retry", label, lockPath, err)
	}
	if err := acquireFlock(ctx, file.Fd(), mode); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("store: acquire %s session lock for %s: %w; no lock was taken", label, id, err)
	}
	release := func() error {
		if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
			_ = file.Close()
			return fmt.Errorf("store: release %s session lock for %s: %w", label, id, err)
		}
		return file.Close()
	}
	return release, nil
}

// acquireFlock retries a non-blocking flock so a cancelled context is honored
// instead of blocking forever on a held exclusive lock.
func acquireFlock(ctx context.Context, fd uintptr, mode int) error {
	for {
		err := unix.Flock(int(fd), mode|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}
