package ingest

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/peasant-labs/schema"
)

// decodeProjectSlug is the shared core for greedy filesystem path decoding.
//
// encoded must start with "-". segmentVariants returns all candidate directory
// names to try for a given dash-separated segment (or merged segment).
//
// Returns (matchedPath, unmatched) where unmatched holds the remaining
// dash-joined segments that could not be resolved to an existing directory.
func decodeProjectSlug(encoded string, dirExists func(string) bool, segmentVariants func(string) []string) (matchedPath, unmatched string) {
	if encoded == "" || !strings.HasPrefix(encoded, "-") {
		return "", encoded
	}
	s := strings.TrimPrefix(encoded, "-")
	segments := strings.Split(s, "-")

	path := "/"
	i := 0
	for i < len(segments) {
		// Try single segment (all variants).
		if found := firstExisting(path, segmentVariants(segments[i]), dirExists); found != "" {
			path = found
			i++
			continue
		}

		// Try merging with subsequent segments (dash was a literal in the dir
		// name, or an underscore/space encoded as dash by Cursor).
		merged := segments[i]
		foundMerge := false
		for j := i + 1; j < len(segments); j++ {
			merged += "-" + segments[j]
			if found := firstExisting(path, segmentVariants(merged), dirExists); found != "" {
				path = found
				i = j + 1
				foundMerge = true
				break
			}
		}
		if !foundMerge {
			// Remaining segments don't match — likely a branch or worktree name
			// appended to the slug. Return longest prefix matched so far.
			break
		}
	}

	if path == "/" {
		return "", strings.Join(segments, "-")
	}
	return path, strings.Join(segments[i:], "-")
}

// firstExisting returns filepath.Join(base, v) for the first v in variants
// where dirExists reports true, or "" if none match.
func firstExisting(base string, variants []string, dirExists func(string) bool) string {
	for _, v := range variants {
		if candidate := filepath.Join(base, v); dirExists(candidate) {
			return candidate
		}
	}
	return ""
}

// parseIndexTimestamp extracts milliseconds from a JSON timestamp field
// that can be an integer (unix ms) or an ISO 8601 string.
func parseIndexTimestamp(raw json.RawMessage) *int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	// Try integer first.
	var ms int64
	if json.Unmarshal(raw, &ms) == nil && ms > 0 {
		return &ms
	}

	// Try string — could be ISO 8601 or a string-encoded integer (e.g. "1708531200000").
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		// Try a string-encoded integer first.
		if parsed, err := strconv.ParseInt(s, 10, 64); err == nil && parsed > 0 {
			return &parsed
		}
		// Fall through to ISO 8601 parsing.
		ms = parseTimestampMillis(s)
		if ms > 0 {
			return &ms
		}
	}

	return nil
}

// isSystemInjectedContent reports whether content is system-injected:
//   - entirely a <system-reminder>...</system-reminder> block
//   - a <task-notification>...</task-notification> block, possibly with trailing
//     harness text (e.g. "Read the output file to retrieve the result: /tmp/…")
//     appended after the closing tag by Claude Code
//   - entirely a <command-name>/X</command-name> block where X is a BuiltinCommand
//   - a skill body injection starting with "Base directory for this skill:"
//
// Only the trimmed content is examined; surrounding whitespace is ignored.
// For <system-reminder>, the trimmed string must start AND end with the tags
// (no trailing user text). For <task-notification>, only the opening tag and
// presence of the closing tag are required — Claude Code sometimes appends
// harness-generated text after the closing tag that is not user content.
func isSystemInjectedContent(content string) bool {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, TagSystemReminder) && strings.HasSuffix(trimmed, TagSystemReminderClose) {
		return true
	}
	// <task-notification> blocks: Claude Code occasionally appends trailing harness
	// text after the closing tag. Any entry that starts with the opening tag and
	// contains the closing tag is considered fully system-injected.
	if strings.HasPrefix(trimmed, TagTaskNotification) && strings.Contains(trimmed, TagTaskNotificationClose) {
		return true
	}
	if strings.HasPrefix(trimmed, PrefixSkillBody) {
		return true
	}
	if strings.HasPrefix(trimmed, TagLocalCommand) {
		return true
	}
	if strings.HasPrefix(trimmed, TagTeammateMessage) {
		return true
	}
	if strings.HasPrefix(trimmed, TagErrorReport) ||
		strings.HasPrefix(trimmed, TagBashStdout) ||
		strings.HasPrefix(trimmed, TagBashInput) ||
		strings.HasPrefix(trimmed, TagFeedback) {
		return true
	}
	if strings.HasPrefix(trimmed, CompactionPrefix) {
		return true
	}
	if trimmed == ExactToolLoaded {
		return true
	}
	name, ok := extractCommandName(trimmed)
	return ok && schema.IsClaudeBuiltinCommand(name)
}

// parseSkillInvocation extracts a skill invocation from content.
// Returns (name, args, true) if content contains <command-name>/X</command-name>
// where X is NOT a BuiltinCommand. name is returned with the leading slash (e.g. "/aura:epoch").
// args is extracted from <command-args>Y</command-args> if present; empty string otherwise.
// Returns ("", "", false) if content is not a skill invocation.
func parseSkillInvocation(content string) (name string, args string, ok bool) {
	trimmed := strings.TrimSpace(content)
	rawName, found := extractCommandName(trimmed)
	if !found {
		return "", "", false
	}
	// Only skill invocations — builtin commands are handled by isSystemInjectedContent.
	if schema.IsClaudeBuiltinCommand(rawName) {
		return "", "", false
	}
	// Ensure the name has a slash prefix.
	if !strings.HasPrefix(rawName, "/") {
		rawName = "/" + rawName
	}
	// Extract optional args.
	argsVal := extractCommandArgs(trimmed)
	return rawName, argsVal, true
}

// extractCommandName extracts the command name from a <command-name>/X</command-name> block.
// Returns (name, true) on success; ("", false) if the pattern is absent.
// The leading slash is stripped so callers can compare directly against
// BuiltinCommand values (e.g. "exit", not "/exit").
func extractCommandName(s string) (string, bool) {
	const open = "<command-name>"
	const close = "</command-name>"
	_, after, found := strings.Cut(s, open)
	if !found {
		return "", false
	}
	raw, _, found := strings.Cut(after, close)
	if !found {
		return "", false
	}
	name := strings.TrimSpace(raw)
	// Strip leading slash for the returned name so callers can compare against
	// BuiltinCommand values (which do not carry a slash).
	if strings.HasPrefix(name, "/") {
		return name[1:], true
	}
	return name, true
}

// extractCommandArgs extracts the args text from a <command-args>Y</command-args> block.
// Returns empty string if the block is absent.
func extractCommandArgs(s string) string {
	const open = "<command-args>"
	const close = "</command-args>"
	_, after, found := strings.Cut(s, open)
	if !found {
		return ""
	}
	raw, _, found := strings.Cut(after, close)
	if !found {
		return ""
	}
	return strings.TrimSpace(raw)
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
