package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

// tomlConfig mirrors the FTUE TOML structure for parsing.
// Only fields that the FTUE wizard wrote are captured here; any additional
// sections in the TOML are silently dropped (forward-compatibility).
type tomlConfig struct {
	Village struct {
		Connected bool `toml:"connected"`
	} `toml:"village"`
	Daemon struct {
		ProjectMode string `toml:"project_mode"`
	} `toml:"daemon"`
	Import struct {
		Enabled bool     `toml:"enabled"`
		Method  string   `toml:"method"`
		Sources []string `toml:"sources"`
	} `toml:"import"`
}

// MigrateTOML reads config.toml, converts it to the unified Config struct, writes
// config.yaml, verifies the YAML round-trips cleanly, then removes config.toml.
//
// Safety contract:
//  1. YAML is written and verified before TOML is deleted.
//  2. If the YAML round-trip parse fails, the YAML file is not written and an
//     error is returned. TOML is left intact.
//  3. If the TOML removal fails (e.g. permissions), a warning is printed to
//     stderr but no error is returned — the YAML is already valid.
//
// A notice is printed to stderr on success so users know what happened.
func MigrateTOML(tomlPath, yamlPath string) error {
	// 1. Read TOML.
	tomlData, err := os.ReadFile(tomlPath)
	if err != nil {
		return fmt.Errorf("read TOML: %w", err)
	}

	// 2. Parse TOML into intermediate struct.
	// Unknown sections are silently dropped by the TOML decoder.
	var raw tomlConfig
	if err := toml.Unmarshal(tomlData, &raw); err != nil {
		return fmt.Errorf("parse TOML: %w", err)
	}

	// 3. Map TOML fields to Config via BaseConfig() for safe defaults.
	cfg := BaseConfig()
	cfg.Village.Connected = raw.Village.Connected

	if raw.Daemon.ProjectMode != "" {
		cfg.Daemon.ProjectMode = raw.Daemon.ProjectMode
	}

	if raw.Import.Method != "" {
		cfg.Push.Method = PushMethod(raw.Import.Method)
	}
	if len(raw.Import.Sources) > 0 {
		// Map legacy harness names to canonical bestiary.Harness identifiers.
		mapped := make([]string, len(raw.Import.Sources))
		for i, src := range raw.Import.Sources {
			switch defaults.Harness(src) {
			case defaults.LegacyHarnessClaude:
				mapped[i] = string(defaults.HarnessClaudeCode)
			case defaults.LegacyHarnessGemini:
				mapped[i] = string(defaults.HarnessGeminiCLI)
			default:
				mapped[i] = src
			}
		}
		cfg.Push.Sources = mapped
	}

	// 4. Marshal to YAML.
	yamlData, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal YAML: %w", err)
	}

	// 5. Verify round-trip: parse the generated YAML to ensure validity before
	// writing to disk. This is the safety gate — we don't touch disk until we
	// know the output is valid.
	if _, err := Parse(yamlData); err != nil {
		return fmt.Errorf("verify migrated YAML: %w", err)
	}

	// 6. Write YAML.
	if err := os.WriteFile(yamlPath, yamlData, defaults.PublicFilePerm); err != nil {
		return fmt.Errorf("write YAML: %w", err)
	}

	// 7. Remove TOML only after successful write + verify.
	if err := os.Remove(tomlPath); err != nil {
		// Non-fatal: YAML is already written and verified.
		fmt.Fprintf(os.Stderr, "warning: could not remove %s: %v\n", tomlPath, err)
	}

	fmt.Fprintf(os.Stderr, "Migrated %s → %s\n", tomlPath, yamlPath)
	return nil
}
