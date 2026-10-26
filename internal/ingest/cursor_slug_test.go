package ingest

import (
	_ "embed"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

//go:embed testdata/cursor_workspace_decode.yaml
var cursorWorkspaceDecodeYAML []byte

// TestDecodeCursorWorkspace resolves each fixture workspace against its
// simulated directory tree. A path component that starts with "-" must decode,
// and a decode that stops outside a git work tree must never name a parent
// folder as the project.
func TestDecodeCursorWorkspace(t *testing.T) {
	var fixture struct {
		Required []string `yaml:"requiredNames"`
		Cases    []struct {
			Name        string   `yaml:"name"`
			Workspace   string   `yaml:"workspace"`
			Dirs        []string `yaml:"dirs"`
			ProjectDir  string   `yaml:"projectDir"`
			ProjectName string   `yaml:"projectName"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(cursorWorkspaceDecodeYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		present[c.Name] = true
	}
	for _, name := range fixture.Required {
		if !present[name] {
			t.Fatalf("missing required Cursor workspace decode case %s", name)
		}
	}
	root := slugTestPath("root")
	path := func(p string) string {
		if rest, ok := strings.CutPrefix(p, "<root>/"); ok {
			return filepath.Join(root, rest)
		}
		return slugTestPath(strings.Split(strings.TrimPrefix(p, "/"), "/")...)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dirs := make(map[string]bool, len(c.Dirs))
			for _, dir := range c.Dirs {
				dirs[path(dir)] = true
			}
			adapter := &CursorAdapter{fs: &stubStatFS{dirs: dirs}}
			gotDir, gotName := adapter.decodeCursorWorkspace(root, c.Workspace)
			if gotDir != path(c.ProjectDir) || gotName != c.ProjectName {
				t.Fatalf("decodeCursorWorkspace(%q) = (%q, %q), want (%q, %q)", c.Workspace, gotDir, gotName, path(c.ProjectDir), c.ProjectName)
			}
		})
	}
}

// stubStatFS and stubDirInfo live in fsfault_test.go (Owner B).
