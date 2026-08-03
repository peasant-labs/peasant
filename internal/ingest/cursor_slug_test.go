package ingest

import (
	"io/fs"
	"os"
	"testing"
	"time"
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
				"/Users":                    true,
				"/Users/foo":                true,
				"/Users/foo/Desktop":        true,
				"/Users/foo/Desktop/myrepo": true,
			},
			wantMatched:   "/Users/foo/Desktop/myrepo",
			wantUnmatched: "",
		},
		{
			name:    "underscore variant: dir has underscores",
			encoded: "-Users-foo-my-project",
			dirs: map[string]bool{
				"/Users":                true,
				"/Users/foo":            true,
				"/Users/foo/my_project": true,
			},
			wantMatched:   "/Users/foo/my_project",
			wantUnmatched: "",
		},
		{
			name:    "space variant: dir has spaces",
			encoded: "-Users-foo-My-Project",
			dirs: map[string]bool{
				"/Users":                true,
				"/Users/foo":            true,
				"/Users/foo/My Project": true,
			},
			wantMatched:   "/Users/foo/My Project",
			wantUnmatched: "",
		},
		{
			name:    "literal dash wins over underscore variant",
			encoded: "-Users-foo-my-project",
			dirs: map[string]bool{
				"/Users":                true,
				"/Users/foo":            true,
				"/Users/foo/my-project": true,
				"/Users/foo/my_project": true,
			},
			wantMatched:   "/Users/foo/my-project",
			wantUnmatched: "",
		},
		{
			name:    "partial match: trailing segments become unmatched",
			encoded: "-Users-foo-Desktop-peasant",
			dirs: map[string]bool{
				"/Users":             true,
				"/Users/foo":         true,
				"/Users/foo/Desktop": true,
				// "peasant" dir does not exist
			},
			wantMatched:   "/Users/foo/Desktop",
			wantUnmatched: "peasant",
		},
		{
			name:    "multiple unmatched trailing segments",
			encoded: "-Users-foo-myrepo-feature-branch",
			dirs: map[string]bool{
				"/Users":            true,
				"/Users/foo":        true,
				"/Users/foo/myrepo": true,
			},
			wantMatched:   "/Users/foo/myrepo",
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
			root:      "/root",
			workspace: "Users-foo-Desktop-myrepo",
			dirs: map[string]bool{
				"/Users":                    true,
				"/Users/foo":                true,
				"/Users/foo/Desktop":        true,
				"/Users/foo/Desktop/myrepo": true,
			},
			wantProjectDir:  "/Users/foo/Desktop/myrepo",
			wantProjectName: "myrepo",
		},
		{
			name:      "partial match: unmatched suffix is project name",
			root:      "/root",
			workspace: "Users-foo-Desktop-peasant",
			dirs: map[string]bool{
				"/Users":             true,
				"/Users/foo":         true,
				"/Users/foo/Desktop": true,
			},
			wantProjectDir:  "/Users/foo/Desktop",
			wantProjectName: "peasant",
		},
		{
			name:            "no match: fallback to joined root path",
			root:            "/root",
			workspace:       "unknown-workspace",
			dirs:            map[string]bool{},
			wantProjectDir:  "/root/unknown-workspace",
			wantProjectName: "unknown-workspace",
		},
		{
			name:      "underscore dir: name from filepath.Base",
			root:      "/root",
			workspace: "Users-foo-my-project",
			dirs: map[string]bool{
				"/Users":                true,
				"/Users/foo":            true,
				"/Users/foo/my_project": true,
			},
			wantProjectDir:  "/Users/foo/my_project",
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

// stubStatFS implements FileSystem with only Stat wired (used by CursorAdapter.dirExists).
var _ FileSystem = (*stubStatFS)(nil)

type stubStatFS struct {
	dirs map[string]bool
}

func (s *stubStatFS) ReadFile(string) ([]byte, error) { panic("stubStatFS: ReadFile") }
func (s *stubStatFS) WriteFile(string, []byte, os.FileMode) error {
	panic("stubStatFS: WriteFile")
}
func (s *stubStatFS) Stat(path string) (os.FileInfo, error) {
	if s.dirs[path] {
		return stubDirInfo{}, nil
	}
	return nil, os.ErrNotExist
}
func (s *stubStatFS) Lstat(path string) (os.FileInfo, error)     { return s.Stat(path) }
func (s *stubStatFS) MkdirAll(string, os.FileMode) error         { panic("stubStatFS: MkdirAll") }
func (s *stubStatFS) ReadDir(string) ([]os.DirEntry, error)      { panic("stubStatFS: ReadDir") }
func (s *stubStatFS) Rename(string, string) error                { panic("stubStatFS: Rename") }
func (s *stubStatFS) Remove(string) error                        { panic("stubStatFS: Remove") }
func (s *stubStatFS) RemoveAll(string) error                     { panic("stubStatFS: RemoveAll") }
func (s *stubStatFS) WalkDir(string, fs.WalkDirFunc) error       { panic("stubStatFS: WalkDir") }
func (s *stubStatFS) CopyFile(string, string, os.FileMode) error { panic("stubStatFS: CopyFile") }

// stubDirInfo is a minimal os.FileInfo that reports IsDir() == true.
type stubDirInfo struct{}

func (stubDirInfo) Name() string       { return "" }
func (stubDirInfo) Size() int64        { return 0 }
func (stubDirInfo) Mode() os.FileMode  { return os.ModeDir | 0755 }
func (stubDirInfo) ModTime() time.Time { return time.Time{} }
func (stubDirInfo) IsDir() bool        { return true }
func (stubDirInfo) Sys() any           { return nil }
