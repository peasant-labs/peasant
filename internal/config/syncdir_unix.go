//go:build unix

package config

import (
	"fmt"
	"os"
)

// syncSavedConfigDir flushes dir's directory entry so the rename that put path
// in place survives a crash. POSIX requires this separate parent-directory
// fsync; a directory that cannot be opened is skipped rather than failing the
// save, which already succeeded.
func syncSavedConfigDir(dir, path string) error {
	parent, err := os.Open(dir)
	if err != nil {
		return nil
	}
	if syncErr := parent.Sync(); syncErr != nil {
		_ = parent.Close()
		return fmt.Errorf("config save: sync destination directory %q after replacing %q: %w; the new file is present but crash durability is not confirmed; verify the file and retry the save", dir, path, syncErr)
	}
	if closeErr := parent.Close(); closeErr != nil {
		return fmt.Errorf("config save: close destination directory %q after replacing %q: %w; the new file is present; verify it before continuing", dir, path, closeErr)
	}
	return nil
}
