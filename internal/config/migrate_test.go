package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// --- TestMigrateTOML_RoundTrip ---

// TestMigrateTOML_RoundTrip verifies that a well-formed FTUE TOML migrates
// to a valid YAML file with the expected field values.
func TestMigrateTOML_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	yamlPath := filepath.Join(dir, "config.yaml")

	tomlContent := `
[village]
connected = true

[daemon]
project_mode = "opt-in"

[import]
enabled = true
method = "by-source"
sources = ["claude", "opencode"]
`
	if err := os.WriteFile(tomlPath, []byte(tomlContent), 0644); err != nil {
		t.Fatalf("setup: write TOML: %v", err)
	}

	if err := MigrateTOML(tomlPath, yamlPath); err != nil {
		t.Fatalf("MigrateTOML: unexpected error: %v", err)
	}

	// YAML file should exist.
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read migrated YAML: %v", err)
	}

	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse migrated YAML: %v", err)
	}

	if !cfg.Village.Connected {
		t.Errorf("Village.Connected = false, want true")
	}
	if cfg.Daemon.ProjectMode != "opt-in" {
		t.Errorf("Daemon.ProjectMode = %q, want %q", cfg.Daemon.ProjectMode, "opt-in")
	}
	if cfg.Push.Method != PushMethodBySource {
		t.Errorf("Push.Method = %q, want %q", cfg.Push.Method, PushMethodBySource)
	}
	if len(cfg.Push.Sources) != 2 {
		t.Errorf("Push.Sources len = %d, want 2", len(cfg.Push.Sources))
	}
}

// --- TestMigrateTOML_VerifyBeforeDelete ---

// TestMigrateTOML_VerifyBeforeDelete verifies that TOML is removed only after
// YAML is successfully written and verified.
func TestMigrateTOML_VerifyBeforeDelete(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	yamlPath := filepath.Join(dir, "config.yaml")

	tomlContent := `
[village]
connected = false

[daemon]
project_mode = "opt-out"

[import]
enabled = false
method = "all"
sources = []
`
	if err := os.WriteFile(tomlPath, []byte(tomlContent), 0644); err != nil {
		t.Fatalf("setup: write TOML: %v", err)
	}

	if err := MigrateTOML(tomlPath, yamlPath); err != nil {
		t.Fatalf("MigrateTOML: unexpected error: %v", err)
	}

	// TOML should be removed after successful migration.
	if _, err := os.Stat(tomlPath); err == nil {
		t.Errorf("TOML file still exists after migration: %s", tomlPath)
	}

	// YAML should exist.
	if _, err := os.Stat(yamlPath); err != nil {
		t.Errorf("YAML file does not exist after migration: %v", err)
	}
}

// --- TestMigrateTOML_UnrecognizedFields ---

// TestMigrateTOML_UnrecognizedFields verifies that unknown TOML sections are
// silently dropped and migration succeeds.
func TestMigrateTOML_UnrecognizedFields(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	yamlPath := filepath.Join(dir, "config.yaml")

	// Include a [custom] section that is not in tomlConfig struct.
	tomlContent := `
[village]
connected = true

[daemon]
project_mode = "opt-in"

[import]
enabled = true
method = "all"
sources = []

[custom]
some_flag = true
another_key = "value"
`
	if err := os.WriteFile(tomlPath, []byte(tomlContent), 0644); err != nil {
		t.Fatalf("setup: write TOML: %v", err)
	}

	// Migration must succeed despite the unknown [custom] section.
	if err := MigrateTOML(tomlPath, yamlPath); err != nil {
		t.Fatalf("MigrateTOML with unrecognized section: unexpected error: %v", err)
	}

	// YAML should be valid.
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read migrated YAML: %v", err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("Parse migrated YAML: unexpected error: %v", err)
	}
}

// --- TestMigrateTOML_InvalidTOML ---

// TestMigrateTOML_InvalidTOML verifies that malformed TOML returns an error
// and does not create a YAML file.
func TestMigrateTOML_InvalidTOML(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	yamlPath := filepath.Join(dir, "config.yaml")

	// Write intentionally malformed TOML.
	if err := os.WriteFile(tomlPath, []byte("not valid toml [[["), 0644); err != nil {
		t.Fatalf("setup: write TOML: %v", err)
	}

	err := MigrateTOML(tomlPath, yamlPath)
	if err == nil {
		t.Fatal("MigrateTOML with invalid TOML: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "parse TOML") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "parse TOML")
	}

	// YAML should not have been created.
	if _, statErr := os.Stat(yamlPath); statErr == nil {
		t.Errorf("YAML file should not exist after failed migration")
	}
}

// --- TestMigrateTOML_FieldMapping ---

// TestMigrateTOML_FieldMapping verifies that each TOML field is mapped to the
// correct Config field.
func TestMigrateTOML_FieldMapping(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	yamlPath := filepath.Join(dir, "config.yaml")

	tomlContent := `
[village]
connected = true

[daemon]
project_mode = "opt-out"

[import]
enabled = true
method = "individual"
sources = ["claude"]
`
	if err := os.WriteFile(tomlPath, []byte(tomlContent), 0644); err != nil {
		t.Fatalf("setup: write TOML: %v", err)
	}

	if err := MigrateTOML(tomlPath, yamlPath); err != nil {
		t.Fatalf("MigrateTOML: unexpected error: %v", err)
	}

	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read migrated YAML: %v", err)
	}

	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse migrated YAML: %v", err)
	}

	if !cfg.Village.Connected {
		t.Errorf("Village.Connected = false, want true")
	}
	if cfg.Daemon.ProjectMode != "opt-out" {
		t.Errorf("Daemon.ProjectMode = %q, want %q", cfg.Daemon.ProjectMode, "opt-out")
	}
	if cfg.Push.Method != PushMethodIndividual {
		t.Errorf("Push.Method = %q, want %q", cfg.Push.Method, PushMethodIndividual)
	}
	if len(cfg.Push.Sources) != 1 || cfg.Push.Sources[0] != defaults.HarnessClaudeCode.String() {
		t.Errorf("Push.Sources = %v, want [%s]", cfg.Push.Sources, defaults.HarnessClaudeCode)
	}
}

// --- TestMigrateTOML_DefaultsPreserved ---

// TestMigrateTOML_DefaultsPreserved verifies that safe defaults are applied
// for any TOML fields that are absent or empty.
func TestMigrateTOML_DefaultsPreserved(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	yamlPath := filepath.Join(dir, "config.yaml")

	// Minimal TOML with only village section.
	tomlContent := `
[village]
connected = false
`
	if err := os.WriteFile(tomlPath, []byte(tomlContent), 0644); err != nil {
		t.Fatalf("setup: write TOML: %v", err)
	}

	if err := MigrateTOML(tomlPath, yamlPath); err != nil {
		t.Fatalf("MigrateTOML: unexpected error: %v", err)
	}

	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read migrated YAML: %v", err)
	}

	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse migrated YAML: %v", err)
	}

	// Push defaults should be applied.
	if cfg.Push.Method != PushMethodAll {
		t.Errorf("Push.Method = %q, want default %q", cfg.Push.Method, PushMethodAll)
	}
	if cfg.Push.Visibility != VisibilityPrivate {
		t.Errorf("Push.Visibility = %q, want default %q", cfg.Push.Visibility, VisibilityPrivate)
	}
	// Daemon default.
	if cfg.Daemon.ProjectMode != "opt-in" {
		t.Errorf("Daemon.ProjectMode = %q, want default %q", cfg.Daemon.ProjectMode, "opt-in")
	}
}
