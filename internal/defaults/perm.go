package defaults

import "os"

// Filesystem permission constants.
const (
	PrivateDirPerm  os.FileMode = 0o700
	PrivateFilePerm os.FileMode = 0o600
	PublicDirPerm   os.FileMode = 0o755
	PublicFilePerm  os.FileMode = 0o644
)
