//go:build unix

package auth

import "golang.org/x/sys/unix"

func tryCredentialLock(fd uintptr) error { return unix.Flock(int(fd), unix.LOCK_EX|unix.LOCK_NB) }
func credentialLockBusy(err error) bool  { return err == unix.EWOULDBLOCK || err == unix.EAGAIN }
