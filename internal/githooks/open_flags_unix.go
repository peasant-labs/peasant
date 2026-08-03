//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package githooks

import "syscall"

// readTargetNonblockFlag prevents inspection from waiting on a symlinked FIFO
// before it can reject the opened object as non-regular.
const readTargetNonblockFlag = syscall.O_NONBLOCK

// readSlotNoFollowFlag refuses to open a symlink at all, so the re-check that
// runs immediately before a write or a delete can never reach through one.
//
// It is one flag rather than a separate Lstat because the two are not
// equivalent here: an Lstat followed by an open leaves a window in which the
// slot can BECOME a symlink between the two calls, and closing exactly that
// kind of window is the entire reason the re-check exists. open(2) resolves the
// question atomically, failing with ELOOP.
const readSlotNoFollowFlag = syscall.O_NOFOLLOW
