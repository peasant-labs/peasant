package ingest

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/peasant-labs/schema"
)

// splitSlugRoot returns the filesystem root a slug decodes from and the
// remaining dash-joined segments. ok is false if encoded matches neither
// shape decodeProjectSlug understands.
//
// A unix slug leads with a dash and decodes from the filesystem root ("/"),
// e.g. -home-user-dev-project for /home/user/dev/project.
//
// A Windows slug leads with a drive letter and two dashes, e.g.
// C--Users-alice-project for C:\Users\alice\project, because the colon and
// the separator each encode as a dash.
//
// The Windows root is built as the drive letter, a colon, and a trailing
// native separator (e.g. "C:\" on windows), not the bare two-character "C:"
// form. filepath.Join special-cases a first element that is exactly two
// characters ending in ':' as a drive-relative reference and does not insert
// a separator after it — Join("C:", "Users") yields "C:Users", a
// drive-relative path, not the absolute "C:\Users" — so appending the
// separator ourselves is required for every joined segment to stay rooted at
// the drive.
func splitSlugRoot(encoded string) (root, rest string, ok bool) {
	if strings.HasPrefix(encoded, "-") {
		return "/", strings.TrimPrefix(encoded, "-"), true
	}
	if len(encoded) >= 3 && isASCIIDriveLetter(encoded[0]) && encoded[1] == '-' && encoded[2] == '-' {
		return string(encoded[0]) + ":" + string(filepath.Separator), encoded[3:], true
	}
	return "", "", false
}

// hasAbsolutePathForm reports whether p is an absolute path in either the POSIX
// or the Windows form, judged by p's own shape rather than by the host OS.
//
// filepath.IsAbs cannot answer this question: it answers for the RUNNING
// platform, and a recording is routinely read on a different OS than the one
// that produced it. filepath.IsAbs("C:\\work") is false on unix and
// filepath.IsAbs("/work") is false on Windows, so either host would reject the
// other's absolute paths as relative.
//
// A bare "C:work" is drive-RELATIVE on Windows and is refused, for the same
// reason splitSlugRoot keeps a separator on the root it returns: a drive letter
// alone does not anchor a path.
func hasAbsolutePathForm(p string) bool {
	// POSIX absolute, and also the forward-slash spelling of a Windows UNC path
	// ("//server/share").
	if strings.HasPrefix(p, "/") {
		return true
	}
	// Windows UNC ("\\server\share").
	if strings.HasPrefix(p, `\\`) {
		return true
	}
	return len(p) >= 3 && isASCIIDriveLetter(p[0]) && p[1] == ':' && (p[2] == '/' || p[2] == '\\')
}

// isASCIIDriveLetter reports whether b is a single-letter Windows drive
// designator (A-Z or a-z).
func isASCIIDriveLetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// decodeProjectSlug is the shared core for greedy filesystem path decoding.
//
// encoded must be a slug shape splitSlugRoot recognizes (unix, leading with
// "-", or Windows, leading with a drive letter and "--"). segmentVariants
// returns all candidate directory names to try for a given dash-separated
// segment (or merged segment).
//
// Returns (matchedPath, unmatched) where unmatched holds the remaining
// dash-joined segments that could not be resolved to an existing directory.
func decodeProjectSlug(encoded string, dirExists func(string) bool, segmentVariants func(string) []string) (matchedPath, unmatched string) {
	root, s, ok := splitSlugRoot(encoded)
	if !ok {
		return "", encoded
	}
	segments := strings.Split(s, "-")

	path := root
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

	if path == root {
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
