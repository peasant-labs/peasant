//go:build unix

package main

import (
	"fmt"
	"os"
)

// replaceExecutable installs the verified binary written at tempPath as path.
//
// On unix, renaming over a running executable is both legal and atomic: the
// running process keeps the inode of its own open image, and no reader can
// observe a partially written file. Windows cannot do this and carries its own
// implementation in upgrade_replace_windows.go.
func replaceExecutable(tempPath, path string) error {
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename verified binary over %s: %w", path, err)
	}
	return nil
}
