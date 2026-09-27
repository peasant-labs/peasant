package ftue

import (
	"os"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// TestMain pins the config home for the whole test binary. Production resolves
// the kickstart config path from XDG_CONFIG_HOME (defaults.ResolveConfigFilePath);
// pointing it at a private temp directory once here keeps every
// ResolveConfigFilePath() read hermetic without a per-test t.Setenv, which would
// forbid t.Parallel in those tests. Tests that must isolate the exact written
// path still inject it through the ResolveConfigFilePathWith seam.
func TestMain(m *testing.M) {
	configHome, err := os.MkdirTemp("", "ftue-xdg-*")
	if err != nil {
		panic("ftue TestMain: create config home: " + err.Error())
	}
	if err := os.Setenv(defaults.EnvXDGConfigHome.String(), configHome); err != nil {
		panic("ftue TestMain: set " + defaults.EnvXDGConfigHome.String() + ": " + err.Error())
	}
	code := m.Run()
	if err := os.RemoveAll(configHome); err != nil {
		os.Stderr.WriteString("ftue TestMain: remove config home: " + err.Error() + "\n")
	}
	os.Exit(code)
}
