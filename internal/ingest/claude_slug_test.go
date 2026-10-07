package ingest

import (
	"path/filepath"
	"testing"
)

// slugTestPath joins path segments from the unix root exactly the way
// decodeProjectSlug does internally (filepath.Join from "/"), so a
// table-driven case's simulated directory tree and expected path stay
// platform-native instead of asserting a hardcoded POSIX string. On unix
// this reproduces the original literal "/a/b/c" strings unchanged; on
// Windows filepath.Join normalizes "/" to the native separator, so the same
// call yields "\a\b\c" there — matching what the decoder itself produces.
func slugTestPath(segments ...string) string {
	return filepath.Join(append([]string{"/"}, segments...)...)
}

func TestDecodeClaudeSlug(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
		dirs    map[string]bool // simulated directory tree
		want    string
	}{
		{
			name:    "basic decode all segments exist",
			encoded: "-home-user-dev-project",
			dirs: map[string]bool{
				slugTestPath("home"):                           true,
				slugTestPath("home", "user"):                   true,
				slugTestPath("home", "user", "dev"):            true,
				slugTestPath("home", "user", "dev", "project"): true,
			},
			want: slugTestPath("home", "user", "dev", "project"),
		},
		{
			name:    "dash in directory name merges segments",
			encoded: "-home-user-my-project",
			dirs: map[string]bool{
				slugTestPath("home"):                       true,
				slugTestPath("home", "user"):               true,
				slugTestPath("home", "user", "my-project"): true,
			},
			want: slugTestPath("home", "user", "my-project"),
		},
		{
			name:    "multiple dashes in directory name",
			encoded: "-home-user-my-cool-project",
			dirs: map[string]bool{
				slugTestPath("home"):                            true,
				slugTestPath("home", "user"):                    true,
				slugTestPath("home", "user", "my-cool-project"): true,
			},
			want: slugTestPath("home", "user", "my-cool-project"),
		},
		{
			name:    "no matching dirs returns empty",
			encoded: "-nonexistent-path",
			dirs:    map[string]bool{},
			want:    "",
		},
		{
			name:    "empty string returns empty",
			encoded: "",
			dirs:    map[string]bool{},
			want:    "",
		},
		{
			name:    "no leading dash returns empty",
			encoded: "home-user-dev",
			dirs:    map[string]bool{},
			want:    "",
		},
		{
			name:    "single dash returns empty",
			encoded: "-",
			dirs:    map[string]bool{},
			want:    "",
		},
		{
			name:    "partial match returns longest prefix",
			encoded: "-home-user-dev-project",
			dirs: map[string]bool{
				slugTestPath("home"):         true,
				slugTestPath("home", "user"): true,
				// /home/user/dev does not exist — remaining segments are unmatched
			},
			want: slugTestPath("home", "user"),
		},
		{
			name:    "trailing branch name returns repo root",
			encoded: "-home-user-dev-my-repo-feature-branch",
			dirs: map[string]bool{
				slugTestPath("home"):                           true,
				slugTestPath("home", "user"):                   true,
				slugTestPath("home", "user", "dev"):            true,
				slugTestPath("home", "user", "dev", "my-repo"): true,
				// "feature" and "branch" segments don't match dirs
			},
			want: slugTestPath("home", "user", "dev", "my-repo"),
		},
		{
			name:    "worktree suffix after repo root returns repo root",
			encoded: "-home-user-dev-widget-service-feature-worktree",
			dirs: map[string]bool{
				slugTestPath("home"):                                  true,
				slugTestPath("home", "user"):                          true,
				slugTestPath("home", "user", "dev"):                   true,
				slugTestPath("home", "user", "dev", "widget-service"): true,
				// "feature" and "worktree" don't match
			},
			want: slugTestPath("home", "user", "dev", "widget-service"),
		},
		{
			name:    "no segments match at all returns empty",
			encoded: "-nonexistent-path-here",
			dirs:    map[string]bool{},
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dirExists := func(path string) bool {
				return tt.dirs[path]
			}
			got := DecodeClaudeSlug(tt.encoded, dirExists)
			if got != tt.want {
				t.Errorf("DecodeClaudeSlug(%q) = %q, want %q", tt.encoded, got, tt.want)
			}
		})
	}
}

// windowsSlugTestPath joins path segments from a drive root exactly the way
// splitSlugRoot + decodeProjectSlug do internally (drive letter + ":" +
// filepath.Separator, then filepath.Join for every segment after that), so a
// drive-letter case's simulated directory tree and expected path stay
// platform-native. It never asserts a literal "C:\..." or "C:/..." string —
// see splitSlugRoot's doc comment for why the trailing separator matters.
func windowsSlugTestPath(drive string, segments ...string) string {
	root := drive + ":" + string(filepath.Separator)
	return filepath.Join(append([]string{root}, segments...)...)
}
