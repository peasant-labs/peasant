//go:build unix

package config_test

// posixFileModeSupported reports whether the platform carries POSIX permission
// bits that a saved file's mode can be asserted against.
func posixFileModeSupported() bool { return true }
