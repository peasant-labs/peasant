package ftue

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/defaults"
)

const cleanupPeriodDaysKey = "cleanupPeriodDays"

// ClaudeSettingsFile is one strictly parsed Claude settings document bound to
// the exact path that was opened. Config editing uses the same value for its
// initial retention read, later atomic write, and user-facing path reporting.
type ClaudeSettingsFile struct {
	path          string
	exists        bool
	cleanupDays   int
	cleanupDaysOK bool
}

// ClaudeSettingsPath returns the path to ~/.claude/settings.json.
func ClaudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// OpenClaudeSettings resolves the default Claude settings path once and opens
// that exact file for strict reads and atomic writes.
func OpenClaudeSettings() (*ClaudeSettingsFile, error) {
	path, err := ClaudeSettingsPath()
	if err != nil {
		return nil, fmt.Errorf("resolve Claude settings path before opening transcript retention: %w", err)
	}
	return OpenClaudeSettingsAt(path)
}

// OpenClaudeSettingsAt strictly parses one explicit Claude settings path. A
// missing file or cleanupPeriodDays key is valid and reports no current value;
// unreadable, malformed, non-object, and invalid-value documents fail closed.
func OpenClaudeSettingsAt(path string) (*ClaudeSettingsFile, error) {
	if path == "" {
		return nil, fmt.Errorf("open Claude settings: path is empty before reading transcript retention; no settings were changed; pass the resolved settings.json path and retry")
	}

	settings, exists, err := readClaudeSettingsObject(path)
	if err != nil {
		return nil, err
	}
	days, found, err := cleanupDaysFromSettings(path, settings)
	if err != nil {
		return nil, err
	}
	return &ClaudeSettingsFile{
		path:          path,
		exists:        exists,
		cleanupDays:   days,
		cleanupDaysOK: found,
	}, nil
}

// Path returns the exact path this settings document was opened from.
func (f *ClaudeSettingsFile) Path() string {
	if f == nil {
		return ""
	}
	return f.path
}

// CleanupDays reports the strictly parsed cleanupPeriodDays value. Missing
// files and missing keys return (0, false).
func (f *ClaudeSettingsFile) CleanupDays() (int, bool) {
	if f == nil {
		return 0, false
	}
	return f.cleanupDays, f.cleanupDaysOK
}

// WriteCleanupDays strictly rereads the document at Path immediately before
// merging cleanupPeriodDays, then atomically replaces that same destination.
// Valid late unrelated edits are preserved. A malformed/unreadable document or
// a late cleanupPeriodDays change fails before rename, so every returned error
// leaves the destination present when the write began unchanged.
func (f *ClaudeSettingsFile) WriteCleanupDays(days int) error {
	if f == nil || f.path == "" {
		return fmt.Errorf("write Claude transcript retention: settings file is nil or has an empty path before merge; no destination was changed; reopen the resolved settings.json path and retry")
	}
	if days <= 0 {
		return fmt.Errorf("write Claude transcript retention at %q: cleanupPeriodDays=%d is not a positive integer; the destination was not changed; select a supported retention period and retry", f.path, days)
	}

	latest, latestExists, err := readClaudeSettingsObject(f.path)
	if err != nil {
		return refreshClaudeSettingsError(f.path, err)
	}
	latestDays, latestFound, err := cleanupDaysFromSettings(f.path, latest)
	if err != nil {
		return refreshClaudeSettingsError(f.path, err)
	}
	if (f.exists && !latestExists) || latestFound != f.cleanupDaysOK || (latestFound && latestDays != f.cleanupDays) {
		return fmt.Errorf(
			"write Claude transcript retention at %q: cleanupPeriodDays changed outside the config editor from %s to %s.\n"+
				"what: the retention value at the bound Claude settings path drifted after the editor opened.\n"+
				"why: replacing it would overwrite a newer retention decision made by Claude Code or another editor.\n"+
				"where: ftue.ClaudeSettingsFile.WriteCleanupDays drift check.\n"+
				"when: immediately after the strict same-path reread and before merge or atomic rename.\n"+
				"means: the current Claude settings destination was not changed.\n"+
				"fix: reopen peasant config, review the current retention value, and apply the choice again.",
			f.path,
			cleanupDaysState(f.cleanupDays, f.cleanupDaysOK, f.exists),
			cleanupDaysState(latestDays, latestFound, latestExists))
	}

	settings := cloneClaudeSettings(latest)
	rawDays, err := json.Marshal(days)
	if err != nil {
		return fmt.Errorf("marshal cleanupPeriodDays=%d for Claude settings %q before replacement: %w; the destination was not changed; select a valid retention period and retry", days, f.path, err)
	}
	settings[cleanupPeriodDaysKey] = rawDays
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal merged Claude settings for %q before replacement: %w; the destination was not changed; repair the opened JSON document and retry", f.path, err)
	}
	if err := replaceClaudeSettingsAtomic(f.path, append(out, '\n')); err != nil {
		return err
	}
	f.cleanupDays = days
	f.cleanupDaysOK = true
	f.exists = true
	return nil
}

// ReadClaudeCleanupDays is the legacy best-effort reader for
// ~/.claude/settings.json. It returns (0, false) for a missing value or a strict
// open error. New trust boundaries should use OpenClaudeSettings so malformed
// and unreadable documents remain distinguishable from absence.
func ReadClaudeCleanupDays() (int, bool) {
	opened, err := OpenClaudeSettings()
	if err != nil {
		return 0, false
	}
	return opened.CleanupDays()
}

// ReadClaudeCleanupDaysAt is the legacy best-effort reader against an explicit
// path. New trust boundaries should use OpenClaudeSettingsAt.
func ReadClaudeCleanupDaysAt(path string) (int, bool) {
	opened, err := OpenClaudeSettingsAt(path)
	if err != nil {
		return 0, false
	}
	return opened.CleanupDays()
}

// WriteClaudeCleanupDays strictly merges cleanupPeriodDays into
// ~/.claude/settings.json and atomically replaces the file. It preserves all
// other keys and creates the file if it does not exist.
func WriteClaudeCleanupDays(days int) error {
	path, err := ClaudeSettingsPath()
	if err != nil {
		return err
	}
	return WriteClaudeCleanupDaysAt(path, days)
}

// WriteClaudeCleanupDaysAt is WriteClaudeCleanupDays against an explicit settings
// file path. The path is injected so the write can be exercised against a
// temporary file in tests without touching the caller's real ~/.claude
// directory; the production wrapper resolves ClaudeSettingsPath and delegates
// here. The strict parse and atomic merge semantics are identical.
func WriteClaudeCleanupDaysAt(path string, days int) error {
	opened, err := OpenClaudeSettingsAt(path)
	if err != nil {
		return err
	}
	return opened.WriteCleanupDays(days)
}

func readClaudeSettingsObject(path string) (map[string]json.RawMessage, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]json.RawMessage), false, nil
		}
		return nil, false, fmt.Errorf("read Claude settings %q at the strict transcript-retention boundary: %w; no settings were changed; repair the path or permissions and retry", path, err)
	}

	trimmed := bytes.TrimSpace(data)
	if !json.Valid(trimmed) {
		return nil, true, fmt.Errorf("parse Claude settings %q at the strict transcript-retention boundary: JSON is malformed; no settings were changed; repair settings.json and retry", path)
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, true, fmt.Errorf("parse Claude settings %q at the strict transcript-retention boundary: top-level JSON must be an object; no settings were changed; replace the non-object document with a settings object and retry", path)
	}

	var settings map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &settings); err != nil {
		return nil, true, fmt.Errorf("parse Claude settings object %q at the strict transcript-retention boundary: %w; no settings were changed; repair settings.json and retry", path, err)
	}
	if settings == nil {
		settings = make(map[string]json.RawMessage)
	}
	return settings, true, nil
}

func cleanupDaysFromSettings(path string, settings map[string]json.RawMessage) (int, bool, error) {
	raw, found := settings[cleanupPeriodDaysKey]
	if !found {
		return 0, false, nil
	}
	var days int
	if err := json.Unmarshal(raw, &days); err != nil || days <= 0 {
		return 0, false, fmt.Errorf("parse cleanupPeriodDays in Claude settings %q at the strict transcript-retention boundary: value must be a positive integer; no settings were changed; correct or remove the key and retry", path)
	}
	return days, true, nil
}

func refreshClaudeSettingsError(path string, cause error) error {
	return fmt.Errorf(
		"refresh Claude settings at %q before merging transcript retention: %w.\n"+
			"what: the bound Claude settings document could not be strictly reread.\n"+
			"why: the same path became unreadable, malformed, non-object, or carried an invalid cleanupPeriodDays value while the editor was open.\n"+
			"where: ftue.ClaudeSettingsFile.WriteCleanupDays pre-merge refresh.\n"+
			"when: immediately before merging the selected retention value and before atomic rename.\n"+
			"means: the current Claude settings destination was not changed.\n"+
			"fix: repair the current settings.json document, reopen peasant config, and apply the retention choice again.",
		path, cause)
}

func cleanupDaysState(days int, found, exists bool) string {
	if !exists {
		return "document missing"
	}
	if !found {
		return "absent"
	}
	return fmt.Sprintf("%d", days)
}

func cloneClaudeSettings(settings map[string]json.RawMessage) map[string]json.RawMessage {
	cloned := make(map[string]json.RawMessage, len(settings)+1)
	for key, raw := range settings {
		cloned[key] = append(json.RawMessage(nil), raw...)
	}
	return cloned
}

func replaceClaudeSettingsAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, defaults.PrivateDirPerm); err != nil {
		return fmt.Errorf("create Claude settings directory %q before atomically replacing %q: %w; the destination was not changed; repair directory ownership or permissions and retry", dir, path, err)
	}
	tmp, err := os.CreateTemp(dir, ".claude-settings-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file beside Claude settings %q during atomic replacement: %w; the destination was not changed; repair directory permissions or free space and retry", path, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(defaults.PrivateFilePerm); err != nil {
		return fmt.Errorf("set private permissions on temporary Claude settings %q for %q: %w; the destination was not changed; repair filesystem permission support and retry", tmpPath, path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary Claude settings %q for %q: %w; the destination was not changed; free disk space or repair the filesystem and retry", tmpPath, path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary Claude settings %q for %q before replacement: %w; the destination was not changed; repair the filesystem and retry", tmpPath, path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary Claude settings %q for %q before replacement: %w; the destination was not changed; repair the filesystem and retry", tmpPath, path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("atomically replace Claude settings %q from temporary file %q: %w; the destination was not changed; repair the destination path or permissions and retry", path, tmpPath, err)
	}
	committed = true

	// A rename is the commit point. Directory sync is best-effort because a
	// post-rename error could not truthfully promise an unchanged destination.
	if parent, err := os.Open(dir); err == nil {
		_ = parent.Sync()
		_ = parent.Close()
	}
	return nil
}
