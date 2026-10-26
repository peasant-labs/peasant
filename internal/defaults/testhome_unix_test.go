//go:build unix

package defaults_test

import "testing"

// setTestHome points the platform's home-directory lookup at a fixed path and
// returns it. os.UserHomeDir reads HOME on unix.
func setTestHome(t *testing.T) string {
	t.Helper()
	home := "/home/example"
	t.Setenv("HOME", home)
	return home
}
