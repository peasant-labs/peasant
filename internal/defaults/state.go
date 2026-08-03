package defaults

import (
	"os"
	"path/filepath"
)

// StateDirPath is a typed XDG state directory path.
type StateDirPath string

func (p StateDirPath) String() string { return string(p) }

type stateDefaults struct {
	DirPath StateDirPath
}

// State holds state-directory defaults, resolved from XDG_STATE_HOME or HOME.
var State = newStateDefaults()

func newStateDefaults() stateDefaults {
	return stateDefaults{
		DirPath: ResolveStateDirPath(),
	}
}

// ResolveStateDirPath returns the state directory path, resolved from the
// environment at call time. This supports t.Setenv in tests where XDG_STATE_HOME
// is set after package init.
func ResolveStateDirPath() StateDirPath {
	if stateHome := os.Getenv(EnvXDGStateHome.String()); stateHome != "" {
		return StateDirPath(filepath.Join(stateHome, string(AppName)))
	}
	if home, err := os.UserHomeDir(); err == nil {
		return StateDirPath(filepath.Join(home, ".local", "state", string(AppName)))
	}
	return StateDirPath(filepath.Join(os.Getenv("HOME"), ".local", "state", string(AppName)))
}

// ResolveStateDirPathWith resolves the state directory, preferring an explicit
// XDG_STATE_HOME override (e.g. from the --state-dir flag) over the process
// environment. An empty override falls back to the environment.
func ResolveStateDirPathWith(xdgStateHomeOverride string) StateDirPath {
	if xdgStateHomeOverride != "" {
		return StateDirPath(filepath.Join(xdgStateHomeOverride, string(AppName)))
	}
	return ResolveStateDirPath()
}
