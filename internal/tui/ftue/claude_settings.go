package ftue

import "github.com/peasant-labs/peasant/internal/claudesettings"

// ClaudeSettingsFile is one strictly parsed Claude settings document bound to
// the exact path that was opened.
type ClaudeSettingsFile = claudesettings.File

// ClaudeSettingsPath returns the path to ~/.claude/settings.json.
func ClaudeSettingsPath() (string, error) { return claudesettings.Path() }

// OpenClaudeSettings resolves the default Claude settings path once and opens
// that exact file for strict reads and atomic writes.
func OpenClaudeSettings() (*ClaudeSettingsFile, error) { return claudesettings.Open() }

// OpenClaudeSettingsAt strictly parses one explicit Claude settings path.
func OpenClaudeSettingsAt(path string) (*ClaudeSettingsFile, error) {
	return claudesettings.OpenAt(path)
}

// ReadClaudeCleanupDays is the legacy best-effort reader for
// ~/.claude/settings.json.
func ReadClaudeCleanupDays() (int, bool) { return claudesettings.ReadCleanupDays() }

// ReadClaudeCleanupDaysAt is the legacy best-effort reader against an explicit
// path.
func ReadClaudeCleanupDaysAt(path string) (int, bool) {
	return claudesettings.ReadCleanupDaysAt(path)
}

// WriteClaudeCleanupDays merges cleanupPeriodDays into ~/.claude/settings.json.
func WriteClaudeCleanupDays(days int) error { return claudesettings.WriteCleanupDays(days) }

// WriteClaudeCleanupDaysAt is WriteClaudeCleanupDays against an explicit path.
func WriteClaudeCleanupDaysAt(path string, days int) error {
	return claudesettings.WriteCleanupDaysAt(path, days)
}
