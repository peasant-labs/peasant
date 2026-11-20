// Package executable creates executable fixtures for tests.
package executable

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write publishes an executable fixture without exposing a partially written file.
// It sets mode exactly, without filtering through the process umask.
//
// On Unix it prevents Go's fork/exec from inheriting its write descriptor.
// O_CLOEXEC alone closes that descriptor only at exec, not at fork, and renaming
// does not change the inode; either alone permits ETXTBSY. See
// https://github.com/golang/go/issues/22315. This protection covers Go's
// os/exec forks, not arbitrary native code that forks outside Go's ForkLock.
func Write(path string, data []byte, mode os.FileMode) (err error) {
	unlock := lockFork()
	defer unlock()
	f, err := os.CreateTemp(filepath.Dir(path), ".executable-*")
	if err != nil {
		return fmt.Errorf("create executable fixture for %s: %w", path, err)
	}
	name := f.Name()
	defer func() {
		if name != "" {
			_ = os.Remove(name)
		}
	}()
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	if err = f.Chmod(mode); err != nil {
		return fmt.Errorf("chmod executable fixture %s: %w", path, err)
	}
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write executable fixture %s: %w", path, err)
	}
	err = f.Close()
	f = nil
	if err != nil {
		return fmt.Errorf("close executable fixture %s: %w", path, err)
	}
	if err = os.Rename(name, path); err != nil {
		return fmt.Errorf("publish executable fixture %s: %w", path, err)
	}
	name = ""
	return nil
}
