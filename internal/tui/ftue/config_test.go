package ftue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/redact"
	"gopkg.in/yaml.v3"
)

// providerSources builds a []string from Provider constants for use in
// ImportSources (which is []string, not []Provider).
func providerSources(providers ...defaults.Harness) []string {
	out := make([]string, len(providers))
	for i, p := range providers {
		out[i] = p.String()
	}
	return out
}

func TestConfigSave_WritesYAML(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &Config{
		VillageConnected: true,
		DaemonMode:       "opt-in",
		ImportEnabled:    true,
		ImportMethod:     string(config.PushMethodBySource),
		ImportSources:    providerSources(defaults.HarnessClaudeCode, defaults.HarnessOpenCode),
	}

	if err := cfg.SaveTo(defaults.ResolveConfigFilePath().String(), config.BaseConfig()); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// File must be config.yaml (not config.toml).
	yamlPath := filepath.Join(defaults.ResolveConfigDirPath().String(), "config.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}

	// Verify the YAML is parseable by the unified config parser.
	parsed, err := config.Parse(data)
	if err != nil {
		t.Fatalf("config.Parse output of Save(): %v", err)
	}

	if !parsed.Village.Connected {
		t.Errorf("Village.Connected = false, want true")
	}
	if parsed.Daemon.ProjectMode != "opt-in" {
		t.Errorf("Daemon.ProjectMode = %q, want %q", parsed.Daemon.ProjectMode, "opt-in")
	}
	if parsed.Push.Method != config.PushMethodBySource {
		t.Errorf("Push.Method = %q, want %q", parsed.Push.Method, config.PushMethodBySource)
	}
	if len(parsed.Push.Sources) != 2 {
		t.Errorf("Push.Sources len = %d, want 2", len(parsed.Push.Sources))
	}
}

func TestConfigSave_PersistsEnablementForEveryConfiguredHarness(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	selected := providerSources(defaults.HarnessOpenCode, defaults.HarnessStrike)
	cfg := &Config{
		DaemonMode:    "opt-in",
		ImportMethod:  string(config.PushMethodBySource),
		ImportSources: selected,
	}
	if err := cfg.SaveTo(defaults.ResolveConfigFilePath().String(), config.BaseConfig()); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	path := filepath.Join(defaults.ResolveConfigDirPath().String(), "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	parsed, err := config.Parse(data)
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}

	selectedSet := make(map[string]bool, len(selected))
	for _, harness := range selected {
		selectedSet[harness] = true
	}
	for _, harness := range defaults.AllHarnesses {
		source, ok := parsed.Sources.Provider(harness)
		if !ok {
			continue
		}
		wantEnabled := selectedSet[harness.String()]
		if source.Enabled != wantEnabled {
			t.Errorf("source %q enabled = %v, want %v from the saved provider selection", harness, source.Enabled, wantEnabled)
		}
	}
}

func TestConfigSave_AllFields(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &Config{
		VillageConnected: true,
		DaemonMode:       "opt-in",
		ImportEnabled:    true,
		ImportMethod:     string(config.PushMethodBySource),
		ImportSources:    providerSources(defaults.HarnessClaudeCode, defaults.HarnessOpenCode),
	}

	if err := cfg.SaveTo(defaults.ResolveConfigFilePath().String(), config.BaseConfig()); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	path := filepath.Join(defaults.ResolveConfigDirPath().String(), "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	content := string(data)

	// Verify key YAML fields are present (not TOML syntax).
	checks := []struct {
		label    string
		expected string
	}{
		{"village section", "village:"},
		{"village connected", "connected: true"},
		{"daemon section", "daemon:"},
		{"daemon projectMode", "projectMode: opt-in"},
		{"push section", "push:"},
		{"push method", "method: by-source"},
		{"push sources", "sources:"},
		{"version", "version:"},
	}
	for _, c := range checks {
		if !strings.Contains(content, c.expected) {
			t.Errorf("%s: expected %q in config, got:\n%s", c.label, c.expected, content)
		}
	}

	// Verify TOML syntax is NOT present.
	tomlChecks := []string{
		"[village]",
		"[daemon]",
		"[import]",
		"connected = ",
		"project_mode = ",
	}
	for _, bad := range tomlChecks {
		if strings.Contains(content, bad) {
			t.Errorf("TOML syntax found in YAML output: %q\ncontent:\n%s", bad, content)
		}
	}
}

func TestConfigSave_ImportDisabled(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &Config{
		VillageConnected: false,
		DaemonMode:       "opt-out",
		ImportEnabled:    false,
		ImportMethod:     string(config.PushMethodAll),
		ImportSources:    nil,
	}

	if err := cfg.SaveTo(defaults.ResolveConfigFilePath().String(), config.BaseConfig()); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	path := filepath.Join(defaults.ResolveConfigDirPath().String(), "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal YAML: %v", err)
	}

	village, ok := raw["village"].(map[string]interface{})
	if !ok {
		t.Fatal("village section missing")
	}
	if village["connected"] != false {
		t.Errorf("village.connected = %v, want false", village["connected"])
	}

	daemon, ok := raw["daemon"].(map[string]interface{})
	if !ok {
		t.Fatal("daemon section missing")
	}
	if daemon["projectMode"] != "opt-out" {
		t.Errorf("daemon.projectMode = %v, want opt-out", daemon["projectMode"])
	}
}

func TestConfigSave_MultipleSources(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &Config{
		VillageConnected: true,
		DaemonMode:       "opt-in",
		ImportEnabled:    true,
		ImportMethod:     string(config.PushMethodBySource),
		ImportSources:    providerSources(defaults.HarnessClaudeCode, defaults.HarnessGeminiCLI, defaults.HarnessCodex, defaults.HarnessOpenCode),
	}

	if err := cfg.SaveTo(defaults.ResolveConfigFilePath().String(), config.BaseConfig()); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	path := filepath.Join(defaults.ResolveConfigDirPath().String(), "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}

	parsed, err := config.Parse(data)
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}

	if len(parsed.Push.Sources) != 4 {
		t.Errorf("Push.Sources len = %d, want 4; sources = %v", len(parsed.Push.Sources), parsed.Push.Sources)
	}
}

func TestConfigSave_CreatesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	cfg := &Config{
		VillageConnected: false,
		DaemonMode:       "opt-in",
		ImportEnabled:    false,
		ImportMethod:     string(config.PushMethodAll),
	}

	if err := cfg.SaveTo(defaults.ResolveConfigFilePath().String(), config.BaseConfig()); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	dir := filepath.Join(tmpDir, defaults.AppName.String())
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("config directory not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected config path to be a directory")
	}

	path := filepath.Join(dir, "config.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config.yaml not created: %v", err)
	}

	// Ensure config.toml is NOT created.
	tomlPath := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(tomlPath); err == nil {
		t.Error("config.toml should not exist — Save() should write YAML only")
	}
}

func TestConfigSave_EmptySources(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &Config{
		VillageConnected: true,
		DaemonMode:       "opt-in",
		ImportEnabled:    true,
		ImportMethod:     string(config.PushMethodAll),
		ImportSources:    []string{},
	}

	if err := cfg.SaveTo(defaults.ResolveConfigFilePath().String(), config.BaseConfig()); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	path := filepath.Join(defaults.ResolveConfigDirPath().String(), "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}

	parsed, err := config.Parse(data)
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}

	// Method=all with no sources is valid.
	if parsed.Push.Method != config.PushMethodAll {
		t.Errorf("Push.Method = %q, want %q", parsed.Push.Method, config.PushMethodAll)
	}
}

func TestConfigSave_RedactionLevel(t *testing.T) {
	tests := []struct {
		name  string
		level string
		want  redact.RedactionLevel
	}{
		{"minimal", string(redact.Minimal), redact.Minimal},
		{"standard", string(redact.Standard), redact.Standard},
		{"maximum", string(redact.Maximum), redact.Maximum},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())

			cfg := &Config{
				DaemonMode:     "opt-in",
				RedactionLevel: tt.level,
			}
			if err := cfg.SaveTo(defaults.ResolveConfigFilePath().String(), config.BaseConfig()); err != nil {
				t.Fatalf("Save() error: %v", err)
			}

			path := filepath.Join(defaults.ResolveConfigDirPath().String(), "config.yaml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read config file: %v", err)
			}

			parsed, err := config.Parse(data)
			if err != nil {
				t.Fatalf("config.Parse: %v", err)
			}

			if parsed.Redaction.Level != tt.want {
				t.Errorf("Redaction.Level = %q, want %q", parsed.Redaction.Level, tt.want)
			}
		})
	}
}

// TestConfigSave_License proves the kickstart license choice persists into
// push.license and round-trips through the real config parser. The empty case
// (no license chosen) must leave push.license unset, never imposing a default.
func TestConfigSave_License(t *testing.T) {
	tests := []struct {
		name    string
		license config.License
		want    config.License
	}{
		{"cc0", config.LicenseCC0, config.LicenseCC0},
		{"cc-by", config.LicenseCCBY, config.LicenseCCBY},
		{"cc-by-sa", config.LicenseCCBYSA, config.LicenseCCBYSA},
		{"none", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())

			cfg := &Config{DaemonMode: "opt-in", License: tt.license}
			if err := cfg.SaveTo(defaults.ResolveConfigFilePath().String(), config.BaseConfig()); err != nil {
				t.Fatalf("Save() error: %v", err)
			}

			path := filepath.Join(defaults.ResolveConfigDirPath().String(), "config.yaml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read config file: %v", err)
			}
			parsed, err := config.Parse(data)
			if err != nil {
				t.Fatalf("config.Parse: %v", err)
			}
			if parsed.Push.License != tt.want {
				t.Errorf("Push.License = %q, want %q", parsed.Push.License, tt.want)
			}
		})
	}
}

func TestConfigSave_EmptyRedactionLevel_DefaultsToStandard(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &Config{
		DaemonMode:     "opt-in",
		RedactionLevel: "", // empty — should fall back to BaseConfig default
	}
	if err := cfg.SaveTo(defaults.ResolveConfigFilePath().String(), config.BaseConfig()); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	path := filepath.Join(defaults.ResolveConfigDirPath().String(), "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}

	parsed, err := config.Parse(data)
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}

	if parsed.Redaction.Level != redact.Standard {
		t.Errorf("Redaction.Level = %q, want %q (default)", parsed.Redaction.Level, redact.Standard)
	}
}
