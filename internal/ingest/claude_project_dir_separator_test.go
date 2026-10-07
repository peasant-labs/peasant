package ingest

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/salt"
)

// alwaysDirFS is a minimal FileSystem stub whose Stat reports every path as an
// existing directory. It exists purely to drive DecodeClaudeSlug's greedy
// decoder to a deterministic, always-successful result (each dash-split
// segment is consumed one at a time, never merged), so these tests isolate
// exactly the thing decodeClaudeProjectDir controls — where the "encoded"
// substring boundary falls inside cwd — without depending on any real
// filesystem or on testutil.MemFS (which is a slash-only virtual filesystem by
// contract and would misinterpret a Windows-shaped backslash cwd as a single
// opaque path segment; see internal/testutil/mocks.go).
type alwaysDirFS struct{}

func (alwaysDirFS) ReadFile(string) ([]byte, error)             { return nil, os.ErrNotExist }
func (alwaysDirFS) WriteFile(string, []byte, os.FileMode) error { return os.ErrNotExist }
func (alwaysDirFS) MkdirAll(string, os.FileMode) error          { return nil }
func (alwaysDirFS) WalkDir(string, fs.WalkDirFunc) error        { return nil }
func (alwaysDirFS) Rename(string, string) error                 { return os.ErrNotExist }
func (alwaysDirFS) ReadDir(string) ([]os.DirEntry, error)       { return nil, os.ErrNotExist }
func (alwaysDirFS) Remove(string) error                         { return os.ErrNotExist }
func (alwaysDirFS) RemoveAll(string) error                      { return os.ErrNotExist }
func (alwaysDirFS) CopyFile(string, string, os.FileMode) error  { return os.ErrNotExist }
func (f alwaysDirFS) Lstat(path string) (os.FileInfo, error)    { return f.Stat(path) }
func (alwaysDirFS) Stat(string) (os.FileInfo, error) {
	return alwaysDirInfo{}, nil
}

type alwaysDirInfo struct{}

func (alwaysDirInfo) Name() string       { return "" }
func (alwaysDirInfo) Size() int64        { return 0 }
func (alwaysDirInfo) Mode() os.FileMode  { return os.ModeDir }
func (alwaysDirInfo) ModTime() time.Time { return time.Time{} }
func (alwaysDirInfo) IsDir() bool        { return true }
func (alwaysDirInfo) Sys() any           { return nil }

// TestDecodeClaudeProjectDir_UnixShaped_AlwaysDecodes proves that a
// unix-shaped ("/") cwd decodes on every platform, including a Windows GOOS
// build — filepath.ToSlash is only ever asked to rewrite backslashes, so a
// cwd that already uses "/" throughout passes through unchanged regardless of
// build target, and detection must keep working for it (e.g. WSL/Git-Bash
// style paths reaching a Windows-built binary).
//
// alwaysDirFS makes DecodeClaudeSlug's greedy decoder always succeed by
// consuming exactly one dash-split segment per directory level, so the
// resulting decoded path is a direct, verifiable readout of where
// decodeClaudeProjectDir cut "encoded" out of cwd — which is exactly the
// segment boundary decodeClaudeProjectDir relies on. filepath.FromSlash(want)
// is used for comparison
// (never a literal separator) so the same assertion is meaningful on every
// platform this test binary is built for.
func TestDecodeClaudeProjectDir_UnixShaped_AlwaysDecodes(t *testing.T) {
	a := NewClaudeAdapter(alwaysDirFS{}, nil, salt.Salt{})

	const slug = "-home-test-myproject"
	want := filepath.FromSlash("/home/test/myproject")

	tests := []struct {
		name string
		cwd  string
	}{
		{
			name: "unix-shaped cwd, project dir itself",
			cwd:  "/home/test/.claude/projects/" + slug,
		},
		{
			name: "unix-shaped cwd, JSONL parent with trailing component",
			cwd:  "/home/test/.claude/projects/" + slug + "/extra-subdir",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.decodeClaudeProjectDir(tt.cwd)
			if got != want {
				t.Fatalf("decodeClaudeProjectDir(%q) = %q, want %q", tt.cwd, got, want)
			}
		})
	}
}

// TestDecodeClaudeProjectDir_WindowsShaped_DecodesOnWindows proves the actual
// bug fix: a Windows-shaped ("\") cwd — e.g.
// "C:\Users\alice\.claude\projects\-home-test-myproject", exactly the shape
// described in the issue — must decode when this test runs AS A WINDOWS
// BUILD, where filepath.ToSlash genuinely rewrites "\" to "/" before the
// segment match. Off Windows, a literal backslash is not a path separator at
// all (it is just another character in the string on unix), so
// filepath.ToSlash is a documented no-op there and the segment is never
// found — that is not a real-world case for a non-Windows build (a unix cwd
// never contains backslashes as separators) and this test asserts that
// documented, byte-identical-to-before-the-fix non-match explicitly rather
// than skipping silently.
func TestDecodeClaudeProjectDir_WindowsShaped_DecodesOnWindows(t *testing.T) {
	a := NewClaudeAdapter(alwaysDirFS{}, nil, salt.Salt{})

	const slug = "-home-test-myproject"

	tests := []string{
		`C:\Users\alice\.claude\projects\` + slug,
		`C:\Users\alice\.claude\projects\` + slug + `\extra-subdir`,
	}

	if runtime.GOOS == "windows" {
		want := filepath.FromSlash("/home/test/myproject")
		for _, cwd := range tests {
			t.Run(cwd, func(t *testing.T) {
				got := a.decodeClaudeProjectDir(cwd)
				if got != want {
					t.Fatalf("decodeClaudeProjectDir(%q) = %q, want %q", cwd, got, want)
				}
			})
		}
		return
	}

	// Not a Windows build: document the (correct, byte-identical-to-unfixed)
	// non-match instead of asserting nothing.
	for _, cwd := range tests {
		t.Run(cwd, func(t *testing.T) {
			if got := a.decodeClaudeProjectDir(cwd); got != "" {
				t.Fatalf("decodeClaudeProjectDir(%q) = %q, want empty on non-Windows GOOS %s", cwd, got, runtime.GOOS)
			}
		})
	}
}

// TestDecodeClaudeProjectDir_NoSegment_ReturnsEmpty is a control: a cwd that
// never contains ".claude/projects" (in either separator form) must not
// decode to anything, on either shape of path.
func TestDecodeClaudeProjectDir_NoSegment_ReturnsEmpty(t *testing.T) {
	a := NewClaudeAdapter(alwaysDirFS{}, nil, salt.Salt{})

	for _, cwd := range []string{
		"/home/test/dev/myproject",
		`C:\Users\alice\dev\myproject`,
	} {
		if got := a.decodeClaudeProjectDir(cwd); got != "" {
			t.Errorf("decodeClaudeProjectDir(%q) = %q, want empty (no .claude/projects segment)", cwd, got)
		}
	}
}
