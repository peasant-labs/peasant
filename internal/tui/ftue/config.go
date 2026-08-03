package ftue

import (
	"bytes"
	"fmt"
	"os"
	"slices"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/redact"
)

// Config holds the wizard configuration choices.
type Config struct {
	VillageConnected bool
	DaemonMode       string // "opt-in" or "opt-out" — reserved for future daemon feature
	ImportEnabled    bool
	ImportMethod     string         // "all" or "by-source"
	ImportSources    []string       // selected provider names
	RedactionLevel   string         // "minimal", "standard", "maximum"
	License          config.License // default push license ("" = none)
	// Selection holds the selection index built from wizard tree selections.
	// nil preserves the loaded selection; callers represent stop-all with an
	// explicit selected-mode empty allowlist.
	Selection *config.SelectionConfig
	// ExpectedBytes is the exact file observed before final consent. A non-nil
	// value makes SaveTo fail closed if another process edited the configuration.
	ExpectedBytes  []byte
	ExpectedExists bool
	CheckSnapshot  bool
}

// SaveTo applies wizard choices to the loaded configuration and atomically
// replaces the exact path selected by the CLI.
func (c *Config) SaveTo(path string, loaded *config.Config) error {
	if c.CheckSnapshot {
		current, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("check kickstart configuration drift at %s before final consent save: %w; no onboarding side effect started; review the file and rerun 'peasant kickstart --config %s'", path, err, path)
		}
		if os.IsNotExist(err) && c.ExpectedExists {
			return fmt.Errorf("kickstart configuration at %s was removed after it was reviewed; no onboarding side effect started; rerun 'peasant kickstart --config %s' to review the current state", path, path)
		}
		if err == nil && !c.ExpectedExists {
			return fmt.Errorf("kickstart configuration appeared at %s after an absent file was reviewed; no onboarding side effect started; review the new file and rerun 'peasant kickstart --config %s'", path, path)
		}
		if !bytes.Equal(current, c.ExpectedBytes) {
			return fmt.Errorf("kickstart configuration changed at %s after it was reviewed; no onboarding side effect started; review the new file and rerun 'peasant kickstart --config %s'", path, path)
		}
	}
	// The caller's configuration is merged into, never mutated: it is still the
	// wizard's in-memory view after a save, and a wizard restart re-reads it.
	cfg := config.BaseConfig()
	if loaded != nil {
		merged := *loaded
		cfg = &merged
	}
	cfg.Village.Connected = c.VillageConnected
	cfg.Daemon.ProjectMode = c.DaemonMode
	cfg.Push.Method = config.PushMethod(c.ImportMethod)
	cfg.Push.Sources = c.ImportSources
	for _, harness := range defaults.AllHarnesses {
		if source, ok := cfg.Sources.Provider(harness); ok {
			source.Enabled = slices.Contains(c.ImportSources, harness.String())
		}
	}
	if c.RedactionLevel != "" {
		cfg.Redaction.Level = redact.RedactionLevel(c.RedactionLevel)
	}
	if c.License != "" {
		cfg.Push.License = c.License
	}
	if c.Selection != nil {
		cfg.Selection = *c.Selection
	}

	return config.SaveAtomic(path, cfg)
}
