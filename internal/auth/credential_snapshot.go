package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

const credentialLockTimeout = 30 * time.Second

// ErrSignedOut means no valid selected credentials exist. Callers must not
// fall back to another account's private cache or initiate login implicitly.
var ErrSignedOut = errors.New("selected peasant online account is signed out; sign in to inspect private collective observations, or use unfiltered local transcript inventory without credentials")

// ErrCredentialsChanged refuses installation of a response obtained with an
// earlier credential generation, even after switching back to the same user.
var ErrCredentialsChanged = errors.New("selected peasant online credentials changed during the operation; uncommitted observations were discarded; retry explicitly with the currently selected account")

// CredentialSnapshot pins a concrete config directory and credential generation.
// Capture is local only; it does not verify the remote authenticated identity.
type CredentialSnapshot struct {
	configHome  string
	credentials Credentials
	digest      [sha256.Size]byte
	valid       bool
}

// Credentials returns a copy; callers cannot mutate the guarded snapshot.
func (s CredentialSnapshot) Credentials() Credentials { return s.credentials }

// CaptureCredentials reads selected credentials without login or network work.
func CaptureCredentials(ctx context.Context, configHome string) (CredentialSnapshot, error) {
	// Pin the resolved absolute profile, not a mutable process environment.
	dir, err := filepath.Abs(resolveConfigDirWith(configHome))
	if err != nil {
		return CredentialSnapshot{}, fmt.Errorf("capture selected credential directory: %w; no observations were read; correct the config directory and retry", err)
	}
	// The resolver expects XDG's parent; locking/reads below use a pinned path
	// directly so profile selection cannot be resolved a second time.
	var snapshot CredentialSnapshot
	err = withCredentialDirectoryLock(ctx, dir, func() error {
		raw, err := os.ReadFile(filepath.Join(dir, defaults.CredentialsFile.String()))
		if errors.Is(err, os.ErrNotExist) {
			return ErrSignedOut
		}
		if err != nil {
			return fmt.Errorf("capture credentials: read selected profile: %w; no observations were read; fix config access and retry", err)
		}
		var credentials Credentials
		if err := json.Unmarshal(raw, &credentials); err != nil {
			return fmt.Errorf("capture credentials: decode selected profile: %w; no observations were read; sign in again to repair the credential file", err)
		}
		if !credentials.IsValid() {
			return ErrSignedOut
		}
		snapshot = CredentialSnapshot{configHome: dir, credentials: credentials, digest: sha256.Sum256(raw), valid: true}
		return nil
	})
	return snapshot, err
}

// WithCurrent checks generation and holds the cross-process credential lock
// THROUGH commit. The callback must contain only a short local transaction,
// never network I/O. Lock ordering is credential lock, then Store write lane.
func (s CredentialSnapshot) WithCurrent(ctx context.Context, commit func() error) error {
	if !s.valid {
		return ErrSignedOut
	}
	if commit == nil {
		return fmt.Errorf("commit observations: missing transaction callback; no observations were installed; supply the Store transaction")
	}
	return withCredentialDirectoryLock(ctx, s.configHome, func() error {
		raw, err := os.ReadFile(filepath.Join(s.configHome, defaults.CredentialsFile.String()))
		if errors.Is(err, os.ErrNotExist) {
			return ErrCredentialsChanged
		}
		if err != nil {
			return fmt.Errorf("commit observations: re-read selected credentials: %w; no observations were installed; fix config access and retry", err)
		}
		if sha256.Sum256(raw) != s.digest {
			return ErrCredentialsChanged
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return commit()
	})
}

func withCredentialLock(ctx context.Context, configHome string, action func() error) error {
	return withCredentialDirectoryLock(ctx, resolveConfigDirWith(configHome), action)
}

func withCredentialDirectoryLock(ctx context.Context, dir string, action func() error) error {
	ctx, cancel := context.WithTimeout(ctx, credentialLockTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("lock selected credentials: create config directory: %w; no credential or observation changes were made; repair config access and retry", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, ".credentials.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("lock selected credentials: open lock file: %w; no changes were made; repair config access and retry", err)
	}
	defer f.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = tryCredentialLock(f.Fd())
		if err == nil {
			break
		}
		if !credentialLockBusy(err) {
			return fmt.Errorf("lock selected credentials: acquire OS lock: %w; no changes were made; use a filesystem supporting advisory locks and retry", err)
		}
		timer := time.NewTimer(2 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	// Close releases the OS lock, including on callback panic. Never unlink the
	// lock file: waiters must always address the same inode across processes.
	return action()
}
