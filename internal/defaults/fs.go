package defaults

// PathSeparator is the canonical path separator for output paths.
const PathSeparator = "/"

// Filesystem operation names used in os.PathError.
const (
	FSOpOpen    = "open"
	FSOpStat    = "stat"
	FSOpRename  = "rename"
	FSOpReadDir = "readdir"
	FSOpRemove  = "remove"
)

// SignalBufferSize is the buffer size for OS signal channels.
const SignalBufferSize = 1
