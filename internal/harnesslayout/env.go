package harnesslayout

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Env carries the host values that root templates resolve against.
type Env struct {
	GOOS OS
	// Home replaces {home}.
	Home string
	// ConfigDir replaces {config}: $XDG_CONFIG_HOME or ~/.config on Linux,
	// ~/Library/Application Support on macOS, %APPDATA% on Windows.
	ConfigDir string
}

// HostEnv reads the running host.
func HostEnv() (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Env{}, err
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return Env{}, err
	}
	return Env{GOOS: OS(runtime.GOOS), Home: home, ConfigDir: config}, nil
}

// Resolve returns the operating system paths of the layout's roots that apply
// to e.GOOS, in declaration order.
func (e Env) Resolve(layout Layout) []string {
	var out []string
	for _, root := range layout.Roots {
		if len(root.OS) > 0 && !slices.Contains(root.OS, e.GOOS) {
			continue
		}
		path := strings.NewReplacer("{home}", e.Home, "{config}", e.ConfigDir).Replace(root.Path)
		out = append(out, filepath.FromSlash(path))
	}
	return out
}
