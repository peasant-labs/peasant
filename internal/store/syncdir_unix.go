//go:build unix

package store

import (
	"fmt"
	"os"
)

// fsyncGenerationStagingDir flushes rel's directory entry (opened through the
// confined root) so a preceding rename into it survives a crash. POSIX
// requires this separate parent-directory fsync; unlike the config/auth
// save paths, generation staging opens the directory through *os.Root so the
// fsync target stays confined to the owned store root rather than an
// unconfined os.Open.
func fsyncGenerationStagingDir(root *os.Root, rel string) error {
	dir, err := root.Open(rel)
	if err != nil {
		return fmt.Errorf("open directory to fsync in generation staging: %s; the staged file is not durably recorded; re-run the operation after fixing filesystem access", sanitizeFSError(err))
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("fsync directory in generation staging: %s; the staged file is not durably recorded; re-run the operation after fixing filesystem access", sanitizeFSError(err))
	}
	return nil
}
