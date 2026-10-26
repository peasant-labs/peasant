//go:build windows

package auth

import "golang.org/x/sys/windows"

func tryCredentialLock(fd uintptr) error {
	return windows.LockFileEx(windows.Handle(fd), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
}
func credentialLockBusy(err error) bool { return err == windows.ERROR_LOCK_VIOLATION }
