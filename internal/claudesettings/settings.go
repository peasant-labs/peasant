package claudesettings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/defaults"
)

const cleanupPeriodDaysKey = "cleanupPeriodDays"

// File is one strictly parsed Claude settings document bound to the exact path
// that was opened. Config editing uses the same value for its initial retention
// read, later atomic write, and user-facing path reporting.
type File struct {
	path          string
	exists        bool
	cleanupDays   int
	cleanupDaysOK bool
}

// Path returns the path to ~/.claude/settings.json.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf(
			"resolve Claude settings path.\n"+
				"what: the home directory could not be read.\n"+
				"why: %v.\n"+
				"where: claudesettings.Path.\n"+
				"when: before opening transcript retention.\n"+
				"means: no settings file was read or changed.\n"+
				"fix: repair the home directory environment and retry.",
			err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// Open resolves the default Claude settings path once and opens that exact
// file for strict reads and atomic writes.
func Open() (*File, error) {
	path, err := Path()
	if err != nil {
		return nil, fmt.Errorf("resolve Claude settings path before opening transcript retention: %w", err)
	}
	return OpenAt(path)
}

// OpenAt strictly parses one explicit Claude settings path. A missing file or
// cleanupPeriodDays key is valid and reports no current value. Unreadable,
// malformed, non-object, and invalid-value documents fail closed. Every error
// leaves the destination unchanged.
func OpenAt(path string) (*File, error) {
	if path == "" {
		return nil, fmt.Errorf(
			"open Claude settings.\n" +
				"what: the settings path is empty.\n" +
				"why: a strict read needs the exact settings.json path that will later be replaced.\n" +
				"where: claudesettings.OpenAt.\n" +
				"when: before reading transcript retention.\n" +
				"means: no settings were read or changed.\n" +
				"fix: pass the resolved settings.json path and retry.")
	}

	settings, exists, err := readObject(path)
	if err != nil {
		return nil, err
	}
	days, found, err := cleanupDaysFrom(path, settings)
	if err != nil {
		return nil, err
	}
	return &File{
		path:          path,
		exists:        exists,
		cleanupDays:   days,
		cleanupDaysOK: found,
	}, nil
}

// Path returns the exact path this settings document was opened from.
func (f *File) Path() string {
	if f == nil {
		return ""
	}
	return f.path
}

// CleanupDays reports the strictly parsed cleanupPeriodDays value. Missing
// files and missing keys return (0, false).
func (f *File) CleanupDays() (int, bool) {
	if f == nil {
		return 0, false
	}
	return f.cleanupDays, f.cleanupDaysOK
}

// WriteCleanupDays strictly rereads the document at its path immediately before
// merging cleanupPeriodDays, then atomically replaces that same destination.
// Valid late unrelated edits are preserved. A malformed or unreadable document,
// a late cleanupPeriodDays change, or a failed replacement leaves the
// destination unchanged.
func (f *File) WriteCleanupDays(days int) error {
	if f == nil || f.path == "" {
		return fmt.Errorf(
			"write Claude transcript retention.\n" +
				"what: the settings file is nil or has an empty path.\n" +
				"why: the merge has no destination to reread.\n" +
				"where: claudesettings.File.WriteCleanupDays.\n" +
				"when: before merging cleanupPeriodDays.\n" +
				"means: no destination was changed.\n" +
				"fix: reopen the resolved settings.json path and retry.")
	}
	if days <= 0 {
		return fmt.Errorf(
			"write Claude transcript retention at %q.\n"+
				"what: cleanupPeriodDays=%d is not a positive integer.\n"+
				"why: Claude Code accepts only a positive day count for transcript retention.\n"+
				"where: claudesettings.File.WriteCleanupDays.\n"+
				"when: before rereading or replacing the settings file.\n"+
				"means: the destination was not changed.\n"+
				"fix: select a supported retention period and retry.",
			f.path, days)
	}

	latest, latestExists, err := readObject(f.path)
	if err != nil {
		return refreshError(f.path, err)
	}
	latestDays, latestFound, err := cleanupDaysFrom(f.path, latest)
	if err != nil {
		return refreshError(f.path, err)
	}
	if (f.exists && !latestExists) || latestFound != f.cleanupDaysOK || (latestFound && latestDays != f.cleanupDays) {
		return fmt.Errorf(
			"write Claude transcript retention at %q: cleanupPeriodDays changed outside the config editor from %s to %s.\n"+
				"what: the retention value at the bound Claude settings path drifted after the editor opened.\n"+
				"why: replacing it would overwrite a newer retention decision made by Claude Code or another editor.\n"+
				"where: claudesettings.File.WriteCleanupDays drift check.\n"+
				"when: immediately after the strict same-path reread and before merge or atomic rename.\n"+
				"means: the current Claude settings destination was not changed.\n"+
				"fix: reopen the config editor, review the current retention value, and apply the choice again.",
			f.path,
			cleanupDaysState(f.cleanupDays, f.cleanupDaysOK, f.exists),
			cleanupDaysState(latestDays, latestFound, latestExists))
	}

	settings := cloneSettings(latest)
	rawDays, err := json.Marshal(days)
	if err != nil {
		return fmt.Errorf(
			"marshal cleanupPeriodDays=%d for Claude settings %q.\n"+
				"what: the selected retention value could not be encoded.\n"+
				"why: %v.\n"+
				"where: claudesettings.File.WriteCleanupDays.\n"+
				"when: after the strict reread and before atomic replacement.\n"+
				"means: the destination was not changed.\n"+
				"fix: select a valid retention period and retry.",
			days, f.path, err)
	}
	settings[cleanupPeriodDaysKey] = rawDays
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf(
			"marshal merged Claude settings for %q.\n"+
				"what: the merged settings document could not be encoded.\n"+
				"why: %v.\n"+
				"where: claudesettings.File.WriteCleanupDays.\n"+
				"when: after merging cleanupPeriodDays and before atomic replacement.\n"+
				"means: the destination was not changed.\n"+
				"fix: repair the opened JSON document and retry.",
			f.path, err)
	}
	if err := replaceAtomic(f.path, append(out, '\n')); err != nil {
		return err
	}
	f.cleanupDays = days
	f.cleanupDaysOK = true
	f.exists = true
	return nil
}

// ReadCleanupDays is the legacy best-effort reader for ~/.claude/settings.json.
// It returns (0, false) for a missing value or a strict open error.
func ReadCleanupDays() (int, bool) {
	opened, err := Open()
	if err != nil {
		return 0, false
	}
	return opened.CleanupDays()
}

// ReadCleanupDaysAt is the legacy best-effort reader against an explicit path.
func ReadCleanupDaysAt(path string) (int, bool) {
	opened, err := OpenAt(path)
	if err != nil {
		return 0, false
	}
	return opened.CleanupDays()
}

// WriteCleanupDays merges cleanupPeriodDays into ~/.claude/settings.json and
// atomically replaces the file. It preserves every other key and creates the
// file when it does not exist. A returned error leaves the destination unchanged.
func WriteCleanupDays(days int) error {
	path, err := Path()
	if err != nil {
		return err
	}
	return WriteCleanupDaysAt(path, days)
}

// WriteCleanupDaysAt is WriteCleanupDays against an explicit settings file path.
func WriteCleanupDaysAt(path string, days int) error {
	opened, err := OpenAt(path)
	if err != nil {
		return err
	}
	return opened.WriteCleanupDays(days)
}

func readObject(path string) (map[string]json.RawMessage, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]json.RawMessage), false, nil
		}
		return nil, false, fmt.Errorf(
			"read Claude settings %q.\n"+
				"what: the settings file could not be read.\n"+
				"why: %v.\n"+
				"where: claudesettings.readObject.\n"+
				"when: at the strict transcript-retention boundary.\n"+
				"means: no settings were changed.\n"+
				"fix: repair the path or permissions and retry.",
			path, err)
	}

	trimmed := bytes.TrimSpace(data)
	if !json.Valid(trimmed) {
		return nil, true, fmt.Errorf(
			"parse Claude settings %q.\n"+
				"what: the settings file is not valid JSON.\n"+
				"why: a strict read refuses a malformed document instead of guessing which keys to keep.\n"+
				"where: claudesettings.readObject.\n"+
				"when: at the strict transcript-retention boundary.\n"+
				"means: no settings were changed.\n"+
				"fix: repair settings.json and retry.",
			path)
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, true, fmt.Errorf(
			"parse Claude settings %q.\n"+
				"what: the top-level JSON is not an object.\n"+
				"why: cleanupPeriodDays can only be merged into a settings object.\n"+
				"where: claudesettings.readObject.\n"+
				"when: at the strict transcript-retention boundary.\n"+
				"means: no settings were changed.\n"+
				"fix: replace the non-object document with a settings object and retry.",
			path)
	}

	var settings map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &settings); err != nil {
		return nil, true, fmt.Errorf(
			"parse Claude settings object %q.\n"+
				"what: the settings object could not be decoded.\n"+
				"why: %v.\n"+
				"where: claudesettings.readObject.\n"+
				"when: at the strict transcript-retention boundary.\n"+
				"means: no settings were changed.\n"+
				"fix: repair settings.json and retry.",
			path, err)
	}
	if settings == nil {
		settings = make(map[string]json.RawMessage)
	}
	return settings, true, nil
}

func cleanupDaysFrom(path string, settings map[string]json.RawMessage) (int, bool, error) {
	raw, found := settings[cleanupPeriodDaysKey]
	if !found {
		return 0, false, nil
	}
	var days int
	if err := json.Unmarshal(raw, &days); err != nil || days <= 0 {
		return 0, false, fmt.Errorf(
			"parse cleanupPeriodDays in Claude settings %q.\n"+
				"what: cleanupPeriodDays is not a positive integer.\n"+
				"why: a strict read refuses a non-numeric, zero, or negative retention value.\n"+
				"where: claudesettings.cleanupDaysFrom.\n"+
				"when: at the strict transcript-retention boundary.\n"+
				"means: no settings were changed.\n"+
				"fix: correct or remove the key and retry.",
			path)
	}
	return days, true, nil
}

func refreshError(path string, cause error) error {
	return fmt.Errorf(
		"refresh Claude settings at %q before merging transcript retention: %w.\n"+
			"what: the bound Claude settings document could not be strictly reread.\n"+
			"why: the same path became unreadable, malformed, non-object, or carried an invalid cleanupPeriodDays value while the editor was open.\n"+
			"where: claudesettings.File.WriteCleanupDays pre-merge refresh.\n"+
			"when: immediately before merging the selected retention value and before atomic rename.\n"+
			"means: the current Claude settings destination was not changed.\n"+
			"fix: repair the current settings.json document, reopen the config editor, and apply the retention choice again.",
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

func cloneSettings(settings map[string]json.RawMessage) map[string]json.RawMessage {
	cloned := make(map[string]json.RawMessage, len(settings)+1)
	for key, raw := range settings {
		cloned[key] = append(json.RawMessage(nil), raw...)
	}
	return cloned
}

func replaceAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, defaults.PrivateDirPerm); err != nil {
		return fmt.Errorf(
			"create Claude settings directory %q before atomically replacing %q.\n"+
				"what: the destination directory could not be created.\n"+
				"why: %v.\n"+
				"where: claudesettings.replaceAtomic.\n"+
				"when: before writing the temporary replacement.\n"+
				"means: the destination was not changed.\n"+
				"fix: repair directory ownership or permissions and retry.",
			dir, path, err)
	}
	tmp, err := os.CreateTemp(dir, ".claude-settings-*.json.tmp")
	if err != nil {
		return fmt.Errorf(
			"create temporary file beside Claude settings %q.\n"+
				"what: atomic replacement could not create its temporary file.\n"+
				"why: %v.\n"+
				"where: claudesettings.replaceAtomic.\n"+
				"when: before writing the merged document.\n"+
				"means: the destination was not changed.\n"+
				"fix: repair directory permissions or free space and retry.",
			path, err)
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
		return fmt.Errorf(
			"set private permissions on temporary Claude settings %q for %q.\n"+
				"what: the temporary file could not be made private.\n"+
				"why: %v.\n"+
				"where: claudesettings.replaceAtomic.\n"+
				"when: before writing the merged document.\n"+
				"means: the destination was not changed.\n"+
				"fix: repair filesystem permission support and retry.",
			tmpPath, path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf(
			"write temporary Claude settings %q for %q.\n"+
				"what: the merged document could not be written.\n"+
				"why: %v.\n"+
				"where: claudesettings.replaceAtomic.\n"+
				"when: before replacing the destination.\n"+
				"means: the destination was not changed.\n"+
				"fix: free disk space or repair the filesystem and retry.",
			tmpPath, path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf(
			"sync temporary Claude settings %q for %q.\n"+
				"what: the temporary file could not be flushed.\n"+
				"why: %v.\n"+
				"where: claudesettings.replaceAtomic.\n"+
				"when: before replacing the destination.\n"+
				"means: the destination was not changed.\n"+
				"fix: repair the filesystem and retry.",
			tmpPath, path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf(
			"close temporary Claude settings %q for %q.\n"+
				"what: the temporary file could not be closed.\n"+
				"why: %v.\n"+
				"where: claudesettings.replaceAtomic.\n"+
				"when: before replacing the destination.\n"+
				"means: the destination was not changed.\n"+
				"fix: repair the filesystem and retry.",
			tmpPath, path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf(
			"atomically replace Claude settings %q from temporary file %q.\n"+
				"what: the destination could not be replaced.\n"+
				"why: %v.\n"+
				"where: claudesettings.replaceAtomic.\n"+
				"when: during the atomic rename.\n"+
				"means: the destination was not changed.\n"+
				"fix: repair the destination path or permissions and retry.",
			path, tmpPath, err)
	}
	committed = true
	if parent, err := os.Open(dir); err == nil {
		_ = parent.Sync()
		_ = parent.Close()
	}
	return nil
}
