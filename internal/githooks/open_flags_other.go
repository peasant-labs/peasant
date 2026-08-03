//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package githooks

// Platforms without POSIX FIFO open semantics need no non-blocking flag.
const readTargetNonblockFlag = 0

// Platforms without POSIX symlink open semantics cannot express the refusal as
// an open flag. The mode check on the opened object still applies, so a slot
// that is not a regular file is still refused; only the narrow swap-to-symlink
// race stays open there.
const readSlotNoFollowFlag = 0
