//go:build unix

package executable

import "syscall"

func lockFork() func() {
	syscall.ForkLock.RLock()
	return syscall.ForkLock.RUnlock
}
