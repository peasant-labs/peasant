package ingest

// DecodeClaudeSlug decodes a Claude project slug to a real filesystem path.
// Claude encodes paths by replacing "/" with "-" and prepending "-":
//
//	/home/user/dev/project → -home-user-dev-project
//
// The encoding is ambiguous (dashes in directory names vs path separators), so
// this uses a greedy filesystem-based decoder: starting from root, it tries each
// segment and checks if a directory exists. If not, it merges segments with dashes.
//
// Claude often appends branch or worktree names to the encoded project path
// (e.g., -home-user-dev-my-repo-feature-branch). When the trailing segments
// don't correspond to existing directories (worktree cleaned up, branch name),
// the decoder returns the longest prefix that successfully matched rather than
// failing entirely. This ensures the project root is still found even when the
// full slug can't be decoded.
//
// This shares decodeProjectSlug with DecodeCursorSlug; host slugs for peasant-sync
// output are derived separately via DeriveHostSlug in hostslug.go.
//
// dirExists is a callback so callers can use any filesystem abstraction.
// Returns the decoded path or empty string if decoding fails entirely.
func DecodeClaudeSlug(encoded string, dirExists func(string) bool) string {
	matched, _ := decodeProjectSlug(encoded, dirExists, func(s string) []string { return []string{s} })
	return matched
}

// DecodeCursorSlug decodes a Cursor workspace folder name to a real filesystem path.
// Cursor encodes path separators, underscores, and spaces as dashes and does NOT
// prepend a leading "-" (unlike Claude slugs). Callers that hold a raw workspace
// name should prefix "-" before calling, as decodeCursorWorkspace does.
//
// Returns (matchedPath, unmatched) where unmatched holds remaining dash-joined
// segments that could not be resolved — often a project name when the workspace
// slug extends past the repo root.
func DecodeCursorSlug(encoded string, dirExists func(string) bool) (matchedPath, unmatched string) {
	return decodeProjectSlug(encoded, dirExists, cursorSegmentVariants)
}
