package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
// is root-confined through os.Root: the file is derived from the validated
// session identifier alone, so no captured path can escape the namespace. No
// diagnostic exposes a private filesystem path.
type fileSessionLocker struct {
	root string
}

// NewFileSessionLocker opens the per-session lock namespace rooted at root.
// root is the same owned-artifact root the generation files live under. The
// lock directory itself is created on the first lock, so a run that takes no
// session lock leaves no extra directory in the owned-artifact tree a reader or
// publisher walks.
func NewFileSessionLocker(root string) (SessionLocker, error) {
	if root == "" {
		return nil, fmt.Errorf("store: session lock root is empty in NewFileSessionLocker; readers and activation cannot serialize; configure the owned-artifact root")
	}
	osRoot, err := os.OpenRoot(root)
	if err != nil {
		// The root may not exist yet; create it on the host then confine.
		if mkErr := os.MkdirAll(root, 0o700); mkErr != nil {
			return nil, fmt.Errorf("store: create owned-artifact root in NewFileSessionLocker: %s; no lock was taken; fix filesystem access and retry", sanitizeFSError(mkErr))
		}
		osRoot, err = os.OpenRoot(root)
		if err != nil {
			return nil, fmt.Errorf("store: open owned-artifact root in NewFileSessionLocker: %s; no lock was taken; fix filesystem access and retry", sanitizeFSError(err))
		}
	}
	defer osRoot.Close()
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
		return nil, fmt.Errorf("store: %s session lock in fileSessionLocker for invalid session: %w; no lock was taken; supply a canonical session identifier", label, err)
	}
	if !filepath.IsLocal(string(id)) {
		return nil, fmt.Errorf("store: %s session lock in fileSessionLocker for unconfined session; no lock was taken; supply a canonical session identifier", label)
	}
	osRoot, err := os.OpenRoot(l.root)
	if err != nil {
		return nil, fmt.Errorf("store: open owned-artifact root in fileSessionLocker for %s lock: %s; no lock was taken; fix filesystem access and retry", label, sanitizeFSError(err))
	}
	// The lock file lives at the confined relative path locks/<session>.lock.
	// os.Root refuses a namespace escape before the file is created.
	rel := filepath.Join("locks", string(id)+".lock")
	file, err := osRoot.OpenFile(rel, os.O_CREATE|os.O_RDWR, 0o600)
	_ = osRoot.Close()
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		// The lock namespace is created on first use, so no run pays for a
		// directory it never locks. Create it under confinement and retry the
		// same confined open once; any other error stays a refusal.
		if mkRoot, mkErr := os.OpenRoot(l.root); mkErr == nil {
			_ = mkRoot.MkdirAll("locks", 0o700)
			_ = mkRoot.Close()
			if retryRoot, retryErr := os.OpenRoot(l.root); retryErr == nil {
				file, err = retryRoot.OpenFile(rel, os.O_CREATE|os.O_RDWR, 0o600)
				_ = retryRoot.Close()
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("store: open %s session lock in fileSessionLocker: %s; no lock was taken; fix filesystem access and retry", label, sanitizeFSError(err))
	}
	if err := acquireFlock(ctx, file.Fd(), mode); err != nil {
		_ = file.Close()
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("store: acquire %s session lock in fileSessionLocker for session %s: %w; no lock was taken; retry with a live context after the holder releases", label, id, err)
		}
		return nil, fmt.Errorf("store: acquire %s session lock in fileSessionLocker for session %s: %s; no lock was taken; fix filesystem access and retry", label, id, sanitizeFSError(err))
	}
	release := func() error {
		if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
			_ = file.Close()
			return fmt.Errorf("store: release %s session lock in fileSessionLocker for session %s: %s; the lock was not released and the file description remains open; retry the release after fixing filesystem access", label, id, sanitizeFSError(err))
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("store: close %s session lock in fileSessionLocker for session %s: %s; the advisory lock was released but the file description may leak; retry the release after fixing filesystem access", label, id, sanitizeFSError(err))
		}
		return nil
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
