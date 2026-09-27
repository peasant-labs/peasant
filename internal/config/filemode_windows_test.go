//go:build windows

package config_test

// posixFileModeSupported reports whether the platform carries POSIX permission
// bits that a saved file's mode can be asserted against. Windows does not: Chmod
// there only toggles the read-only attribute, and Mode().Perm() reports 0666 for
// any writable file whatever mode was requested.
func posixFileModeSupported() bool { return false }
