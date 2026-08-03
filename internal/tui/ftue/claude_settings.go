package ftue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ClaudeSettingsPath returns the path to ~/.claude/settings.json.
func ClaudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// ReadClaudeCleanupDays reads the cleanupPeriodDays value from ~/.claude/settings.json.
// Returns the value and true if found, or (0, false) if the file doesn't exist
// or the key is not set (meaning Claude Code uses its default of 30 days).
func ReadClaudeCleanupDays() (int, bool) {
	path, err := ClaudeSettingsPath()
	if err != nil {
		return 0, false
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return 0, false
	}

	val, ok := settings["cleanupPeriodDays"]
	if !ok {
		return 0, false
	}

	// JSON numbers decode as float64.
	switch v := val.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

// WriteClaudeCleanupDays updates the cleanupPeriodDays value in ~/.claude/settings.json.
// It preserves all other keys in the file. Creates the file if it doesn't exist.
func WriteClaudeCleanupDays(days int) error {
	path, err := ClaudeSettingsPath()
	if err != nil {
		return err
	}

	settings := make(map[string]any)

	// Read existing settings if present.
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &settings) // best-effort parse
	}

	settings["cleanupPeriodDays"] = days

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}

	// Ensure directory exists.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}

	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}
