package ingest

import (
	"path/filepath"
	"testing"
)

func TestDecodeCursorSlug(t *testing.T) {
	tests := []struct {
		name          string
		encoded       string
		dirs          map[string]bool
		wantMatched   string
		wantUnmatched string
	}{
		{
			name:    "basic decode all segments exist",
			encoded: "-Users-foo-Desktop-myrepo",
			dirs: map[string]bool{
				slugTestPath("Users"):                             true,
				slugTestPath("Users", "foo"):                      true,
				slugTestPath("Users", "foo", "Desktop"):           true,
				slugTestPath("Users", "foo", "Desktop", "myrepo"): true,
			},
			wantMatched:   slugTestPath("Users", "foo", "Desktop", "myrepo"),
			wantUnmatched: "",
		},
		{
			name:    "underscore variant: dir has underscores",
			encoded: "-Users-foo-my-project",
			dirs: map[string]bool{
				slugTestPath("Users"):                      true,
				slugTestPath("Users", "foo"):               true,
				slugTestPath("Users", "foo", "my_project"): true,
			},
			wantMatched:   slugTestPath("Users", "foo", "my_project"),
			wantUnmatched: "",
		},
		{
			name:    "space variant: dir has spaces",
			encoded: "-Users-foo-My-Project",
			dirs: map[string]bool{
				slugTestPath("Users"):                      true,
				slugTestPath("Users", "foo"):               true,
				slugTestPath("Users", "foo", "My Project"): true,
			},
			wantMatched:   slugTestPath("Users", "foo", "My Project"),
			wantUnmatched: "",
		},
		{
			name:    "literal dash wins over underscore variant",
			encoded: "-Users-foo-my-project",
			dirs: map[string]bool{
				slugTestPath("Users"):                      true,
				slugTestPath("Users", "foo"):               true,
				slugTestPath("Users", "foo", "my-project"): true,
				slugTestPath("Users", "foo", "my_project"): true,
			},
			wantMatched:   slugTestPath("Users", "foo", "my-project"),
			wantUnmatched: "",
		},
		{
			name:    "partial match: trailing segments become unmatched",
			encoded: "-Users-foo-Desktop-peasant",
			dirs: map[string]bool{
				slugTestPath("Users"):                   true,
				slugTestPath("Users", "foo"):            true,
				slugTestPath("Users", "foo", "Desktop"): true,
				// "peasant" dir does not exist
			},
			wantMatched:   slugTestPath("Users", "foo", "Desktop"),
			wantUnmatched: "peasant",
		},
		{
			name:    "multiple unmatched trailing segments",
			encoded: "-Users-foo-myrepo-feature-branch",
			dirs: map[string]bool{
				slugTestPath("Users"):                  true,
				slugTestPath("Users", "foo"):           true,
				slugTestPath("Users", "foo", "myrepo"): true,
			},
			wantMatched:   slugTestPath("Users", "foo", "myrepo"),
			wantUnmatched: "feature-branch",
		},
		{
			name:          "no segments match returns empty matched and full unmatched",
			encoded:       "-nonexistent-path",
			dirs:          map[string]bool{},
			wantMatched:   "",
			wantUnmatched: "nonexistent-path",
		},
		{
			name:          "empty string returns empty",
			encoded:       "",
			dirs:          map[string]bool{},
			wantMatched:   "",
			wantUnmatched: "",
		},
		{
			name:          "no leading dash returns empty matched with original unmatched",
			encoded:       "Users-foo",
			dirs:          map[string]bool{},
			wantMatched:   "",
			wantUnmatched: "Users-foo",
		},
		{
			name:    "windows drive-letter workspace decodes from the drive root",
			encoded: "C--Users-alice-project",
			dirs: map[string]bool{
				windowsSlugTestPath("C", "Users"):                     true,
				windowsSlugTestPath("C", "Users", "alice"):            true,
				windowsSlugTestPath("C", "Users", "alice", "project"): true,
			},
			wantMatched:   windowsSlugTestPath("C", "Users", "alice", "project"),
			wantUnmatched: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dirExists := func(path string) bool { return tt.dirs[path] }
			gotMatched, gotUnmatched := DecodeCursorSlug(tt.encoded, dirExists)
			if gotMatched != tt.wantMatched {
				t.Errorf("matched: got %q, want %q", gotMatched, tt.wantMatched)
			}
			if gotUnmatched != tt.wantUnmatched {
				t.Errorf("unmatched: got %q, want %q", gotUnmatched, tt.wantUnmatched)
			}
		})
	}
}

func TestCursorSegmentVariants(t *testing.T) {
	tests := []struct {
		segment string
		want    []string
	}{
		// No dashes → only one variant.
		{"foo", []string{"foo"}},
		// No dashes → only one variant (identity for all three replacements).
		{"nodashes", []string{"nodashes"}},
		// Dashes → three distinct variants.
		{"my-project", []string{"my-project", "my_project", "my project"}},
		{"no-dashes-here", []string{"no-dashes-here", "no_dashes_here", "no dashes here"}},
	}
	for _, tt := range tests {
		t.Run(tt.segment, func(t *testing.T) {
			got := cursorSegmentVariants(tt.segment)
			if len(got) != len(tt.want) {
				t.Fatalf("len: got %d (%v), want %d (%v)", len(got), got, len(tt.want), tt.want)
			}
			for i, v := range got {
				if v != tt.want[i] {
					t.Errorf("variants[%d]: got %q, want %q", i, v, tt.want[i])
				}
			}
		})
	}
}

func TestDecodeCursorWorkspace(t *testing.T) {
	tests := []struct {
		name            string
		root            string
		workspace       string
		dirs            map[string]bool
		wantProjectDir  string
		wantProjectName string
	}{
		{
			name:      "full match: project name from filepath.Base",
			root:      slugTestPath("root"),
			workspace: "Users-foo-Desktop-myrepo",
			dirs: map[string]bool{
				slugTestPath("Users"):                             true,
				slugTestPath("Users", "foo"):                      true,
				slugTestPath("Users", "foo", "Desktop"):           true,
				slugTestPath("Users", "foo", "Desktop", "myrepo"): true,
			},
			wantProjectDir:  slugTestPath("Users", "foo", "Desktop", "myrepo"),
			wantProjectName: "myrepo",
		},
		{
			name:      "partial match: unmatched suffix is project name",
			root:      slugTestPath("root"),
			workspace: "Users-foo-Desktop-peasant",
			dirs: map[string]bool{
				slugTestPath("Users"):                   true,
				slugTestPath("Users", "foo"):            true,
				slugTestPath("Users", "foo", "Desktop"): true,
			},
			wantProjectDir:  slugTestPath("Users", "foo", "Desktop"),
			wantProjectName: "peasant",
		},
		{
			name:            "no match: fallback to joined root path",
			root:            slugTestPath("root"),
			workspace:       "unknown-workspace",
			dirs:            map[string]bool{},
			wantProjectDir:  filepath.Join(slugTestPath("root"), "unknown-workspace"),
			wantProjectName: "unknown-workspace",
		},
		{
			name:      "underscore dir: name from filepath.Base",
			root:      slugTestPath("root"),
			workspace: "Users-foo-my-project",
			dirs: map[string]bool{
				slugTestPath("Users"):                      true,
				slugTestPath("Users", "foo"):               true,
				slugTestPath("Users", "foo", "my_project"): true,
			},
			wantProjectDir:  slugTestPath("Users", "foo", "my_project"),
			wantProjectName: "my_project",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := &CursorAdapter{
				fs: &stubStatFS{dirs: tt.dirs},
			}
			gotDir, gotName := adapter.decodeCursorWorkspace(tt.root, tt.workspace)
			if gotDir != tt.wantProjectDir {
				t.Errorf("projectDir: got %q, want %q", gotDir, tt.wantProjectDir)
			}
			if gotName != tt.wantProjectName {
				t.Errorf("projectName: got %q, want %q", gotName, tt.wantProjectName)
			}
		})
	}
}

// stubStatFS and stubDirInfo live in fsfault_test.go (Owner B).
