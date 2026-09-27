//go:build unix

package auth

import (
	"fmt"
	"os"
)

// syncSavedCredentialsDir flushes dir's directory entry so the rename that put
// path in place survives a crash. POSIX requires this separate parent-directory
// fsync; a directory that cannot be opened is skipped rather than failing the
// save, which already succeeded.
func syncSavedCredentialsDir(dir, path string) error {
	parent, err := os.Open(dir)
	if err != nil {
		return nil
	}
	if syncErr := parent.Sync(); syncErr != nil {
		_ = parent.Close()
		return fmt.Errorf("save credentials: sync directory %q after replacing %q: %w; the new file is present but crash durability is not confirmed; verify it and retry", dir, path, syncErr)
	}
	if closeErr := parent.Close(); closeErr != nil {
		return fmt.Errorf("save credentials: close directory %q after replacing %q: %w; the new file is present; verify it before continuing", dir, path, closeErr)
	}
	return nil
}
