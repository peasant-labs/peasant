package kickstart

import "github.com/peasant-labs/peasant/internal/tui/ftue"

// RetentionWriter persists the Claude Code transcript-retention preference
// (cleanupPeriodDays in ~/.claude/settings.json). It is the one seam the
// kickstart program's retention step depends on, injected so a test can point it
// at a temporary file and assert the write happens AFTER the config save (the
// legacy ordering) without touching a real home directory.
type RetentionWriter interface {
	// WriteCleanupDays merges cleanupPeriodDays=days into the settings file,
	// preserving every other key and creating the file when absent.
	WriteCleanupDays(days int) error
}

// RetentionWriterFunc adapts a plain function to RetentionWriter.
type RetentionWriterFunc func(days int) error

// WriteCleanupDays calls the wrapped function.
func (f RetentionWriterFunc) WriteCleanupDays(days int) error { return f(days) }

var _ RetentionWriter = RetentionWriterFunc(nil)

// DefaultRetentionWriter writes to the real ~/.claude/settings.json via the
// existing ftue retention writer, unchanged in behavior. The mounted program
// injects this; tests inject a temp-path func over ftue.WriteClaudeCleanupDaysAt.
func DefaultRetentionWriter() RetentionWriter {
	return RetentionWriterFunc(ftue.WriteClaudeCleanupDays)
}

// FileRetentionWriter writes cleanupPeriodDays to an explicit settings path,
// reusing the exact merge/create semantics of the production writer. It exists so
// the retention step can be exercised against a temporary file.
func FileRetentionWriter(path string) RetentionWriter {
	return RetentionWriterFunc(func(days int) error {
		return ftue.WriteClaudeCleanupDaysAt(path, days)
	})
}
