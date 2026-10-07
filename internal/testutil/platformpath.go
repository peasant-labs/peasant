package testutil

import (
	"os"
	"path/filepath"
)

// PlatformAbsPath turns a POSIX-style absolute test path into one that is
// absolute on the running platform. On unix it is returned unchanged. On Windows
// it gains the current volume and native separators, because filepath.IsAbs
// rejects a path with no volume name there and callers such as
// ingest.NewResolvedPath require an absolute path.
//
// It only swaps separators rather than cleaning the result, so a deliberately
// unclean test path keeps whatever shape the caller chose.
func PlatformAbsPath(posixPath string) string {
	volume := filepath.VolumeName(os.TempDir())
	if volume == "" {
		return posixPath
	}
	return volume + filepath.FromSlash(posixPath)
}
