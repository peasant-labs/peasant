//go:build windows

package auth

// syncSavedCredentialsDir does nothing on Windows. Windows exposes no way to flush a
// directory entry: opening the directory succeeds, but Sync on that handle fails
// with "Access is denied". The rename itself has already completed, so the saved
// file is in place; only the extra POSIX durability barrier is unavailable.
func syncSavedCredentialsDir(_, _ string) error { return nil }
