package ingest

import (
	"testing"
)

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
				"/home":                  true,
				"/home/user":             true,
				"/home/user/dev":         true,
				"/home/user/dev/project": true,
			},
			want: "/home/user/dev/project",
		},
		{
			name:    "dash in directory name merges segments",
			encoded: "-home-user-my-project",
			dirs: map[string]bool{
				"/home":                 true,
				"/home/user":            true,
				"/home/user/my-project": true,
			},
			want: "/home/user/my-project",
		},
		{
			name:    "multiple dashes in directory name",
			encoded: "-home-user-my-cool-project",
			dirs: map[string]bool{
				"/home":                      true,
				"/home/user":                 true,
				"/home/user/my-cool-project": true,
			},
			want: "/home/user/my-cool-project",
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
				"/home":      true,
				"/home/user": true,
				// /home/user/dev does not exist — remaining segments are unmatched
			},
			want: "/home/user",
		},
		{
			name:    "trailing branch name returns repo root",
			encoded: "-home-user-dev-my-repo-feature-branch",
			dirs: map[string]bool{
				"/home":                  true,
				"/home/user":             true,
				"/home/user/dev":         true,
				"/home/user/dev/my-repo": true,
				// "feature" and "branch" segments don't match dirs
			},
			want: "/home/user/dev/my-repo",
		},
		{
			name:    "worktree suffix after repo root returns repo root",
			encoded: "-home-user-dev-widget-service-feature-worktree",
			dirs: map[string]bool{
				"/home":                         true,
				"/home/user":                    true,
				"/home/user/dev":                true,
				"/home/user/dev/widget-service": true,
				// "feature" and "worktree" don't match
			},
			want: "/home/user/dev/widget-service",
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
