//go:build windows

package defaults_test

import "testing"

// setTestHome points the platform's home-directory lookup at a fixed path and
// returns it. os.UserHomeDir reads USERPROFILE on Windows and ignores HOME, and
// the path must carry a volume name to count as absolute there.
func setTestHome(t *testing.T) string {
	t.Helper()
	home := `C:\home\example`
	t.Setenv("USERPROFILE", home)
	return home
}
