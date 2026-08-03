package defaults

import (
	"os"
	"path/filepath"
)

// AppID is a typed application identifier.
type AppID string

func (a AppID) String() string { return string(a) }

// AppName is the application identifier used in paths and display.
const AppName AppID = "peasant"

// ConfigFileName is a typed config file name.
type ConfigFileName string

func (f ConfigFileName) String() string { return string(f) }

// ConfigDirName is a typed config directory name component.
type ConfigDirName string

func (d ConfigDirName) String() string { return string(d) }

// ConfigDirPath is a typed full config directory path.
type ConfigDirPath string

func (p ConfigDirPath) String() string { return string(p) }

// ConfigFilePath is a typed full config file path.
type ConfigFilePath string

func (p ConfigFilePath) String() string { return string(p) }

type configDefaults struct {
	FileName ConfigFileName
	DirName  ConfigDirName
	DirPath  ConfigDirPath
	FilePath ConfigFilePath
}

// Config holds config-related default values.
// Constant fields are set at init; computed fields (DirPath, FilePath) are
// resolved once from the runtime environment (XDG_CONFIG_HOME, HOME).
var Config = newConfigDefaults()

func newConfigDefaults() configDefaults {
	dirName := ConfigDirName(AppName)
	fileName := ConfigFileName("config.yaml")
	dirPath := resolveConfigDirPath(dirName)
	filePath := ConfigFilePath(filepath.Join(string(dirPath), string(fileName)))
	return configDefaults{
		FileName: fileName,
		DirName:  dirName,
		DirPath:  dirPath,
		FilePath: filePath,
	}
}

// ResolveConfigDirPath returns the config directory path, resolved from the
// current environment. Use this instead of Config.DirPath when the environment
// may have changed after init (e.g. in tests with t.Setenv).
func ResolveConfigDirPath() ConfigDirPath {
	return resolveConfigDirPath(ConfigDirName(AppName))
}

func resolveConfigDirPath(dirName ConfigDirName) ConfigDirPath {
	if configHome := os.Getenv(EnvXDGConfigHome.String()); configHome != "" {
		return ConfigDirPath(filepath.Join(configHome, string(dirName)))
	}
	if home, err := os.UserHomeDir(); err == nil {
		return ConfigDirPath(filepath.Join(home, ".config", string(dirName)))
	}
	return ConfigDirPath(filepath.Join(os.Getenv("HOME"), ".config", string(dirName)))
}

// ResolveConfigFilePath returns the config file path, resolved from the
// current environment. Use this instead of Config.FilePath when the environment
// may have changed after init (e.g. in tests with t.Setenv).
func ResolveConfigFilePath() ConfigFilePath {
	dirPath := ResolveConfigDirPath()
	return ConfigFilePath(filepath.Join(string(dirPath), string(Config.FileName)))
}

// ResolveConfigDirPathWith resolves the config directory, preferring an explicit
// XDG_CONFIG_HOME override (e.g. from the --config-dir flag) over the process
// environment. The parallel-safe counterpart to ResolveConfigDirPath. An empty
// override falls back to the environment.
func ResolveConfigDirPathWith(xdgConfigHomeOverride string) ConfigDirPath {
	if xdgConfigHomeOverride != "" {
		return ConfigDirPath(filepath.Join(xdgConfigHomeOverride, string(ConfigDirName(AppName))))
	}
	return ResolveConfigDirPath()
}

// ResolveConfigFilePathWith returns the config file path under the resolved
// config directory, preferring the given XDG_CONFIG_HOME override.
func ResolveConfigFilePathWith(xdgConfigHomeOverride string) ConfigFilePath {
	return ConfigFilePath(filepath.Join(string(ResolveConfigDirPathWith(xdgConfigHomeOverride)), string(Config.FileName)))
}
