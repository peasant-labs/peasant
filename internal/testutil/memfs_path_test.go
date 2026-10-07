package testutil

import (
	"io/fs"
	"sort"
	"testing"
)

// These tests pin down MemFS's own path handling. MemFS is a slash-based
// virtual filesystem by contract (see the doc comment on MemFS in mocks.go):
// every key is stored and looked up using "/" regardless of host OS. A
// regression here (e.g. reintroducing "path/filepath" — which rewrites
// separators to the native form on Windows — on a MemFS key) silently
// breaks WalkDir/ReadDir/MkdirAll+Stat on Windows while staying green on
// Linux/macOS, which is exactly what happened before this fix. None of the
// assertions below reference os.PathSeparator or filepath.Join, on purpose:
// the expected values are hardcoded with "/" because that is the only
// separator MemFS ever uses, on every platform.

// TestMemFS_WalkDir_NestedRoot walks from a nested (non-root) directory and
// expects every file beneath it to be visited. Before the fix, filepath.Clean
// rewrote the root to a native-separator string on Windows, which no stored
// slash-key would ever match, so WalkDir silently visited zero entries.
func TestMemFS_WalkDir_NestedRoot(t *testing.T) {
	m := NewMemFS()
	files := []string{
		"/data/project/session1.jsonl",
		"/data/project/sub/session2.jsonl",
		"/data/project/sub/deeper/session3.jsonl",
	}
	for _, p := range files {
		if err := m.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("WriteFile %q: %v", p, err)
		}
	}

	var visitedFiles []string
	err := m.WalkDir("/data/project", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			visitedFiles = append(visitedFiles, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	sort.Strings(visitedFiles)
	want := []string{
		"/data/project/session1.jsonl",
		"/data/project/sub/deeper/session3.jsonl",
		"/data/project/sub/session2.jsonl",
	}
	sort.Strings(want)
	if len(visitedFiles) != len(want) {
		t.Fatalf("WalkDir from nested root visited %v, want %v (got %d files, want %d — on a broken build this is 0)",
			visitedFiles, want, len(visitedFiles), len(want))
	}
	for i, w := range want {
		if visitedFiles[i] != w {
			t.Errorf("visitedFiles[%d] = %q, want %q", i, visitedFiles[i], w)
		}
	}
}

// TestMemFS_ReadDir_DirectChildrenOnly asserts ReadDir lists only the
// immediate children of a nested directory — not entries from sibling
// directories, not entries from grandchildren, and not the directory itself.
func TestMemFS_ReadDir_DirectChildrenOnly(t *testing.T) {
	m := NewMemFS()
	for _, p := range []string{
		"/root/nested/a.txt",
		"/root/nested/b.txt",
		"/root/nested/child/c.txt", // grandchild — must NOT appear
		"/root/sibling/d.txt",      // sibling — must NOT appear
	} {
		if err := m.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("WriteFile %q: %v", p, err)
		}
	}

	entries, err := m.ReadDir("/root/nested")
	if err != nil {
		t.Fatalf("ReadDir(/root/nested): %v", err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	want := []string{"a.txt", "b.txt", "child"}
	if len(names) != len(want) {
		t.Fatalf("ReadDir(/root/nested) = %v, want %v (got %d entries, want %d — on a broken build this is 0)",
			names, want, len(names), len(want))
	}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("names[%d] = %q, want %q", i, names[i], w)
		}
	}
}

// TestMemFS_MkdirAll_Stat_NestedRoundTrip proves that a deeply nested
// directory created via MkdirAll is independently discoverable via Stat —
// both for the leaf directory and every intermediate ancestor.
func TestMemFS_MkdirAll_Stat_NestedRoundTrip(t *testing.T) {
	m := NewMemFS()
	if err := m.MkdirAll("/alpha/beta/gamma", 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	for _, dir := range []string{"/alpha", "/alpha/beta", "/alpha/beta/gamma"} {
		info, err := m.Stat(dir)
		if err != nil {
			t.Fatalf("Stat(%q) after MkdirAll: %v (on a broken build the key is unreachable)", dir, err)
		}
		if !info.IsDir() {
			t.Errorf("Stat(%q).IsDir() = false, want true", dir)
		}
	}

	// Root must remain intact and unaffected.
	info, err := m.Stat("/")
	if err != nil {
		t.Fatalf("Stat(/): %v", err)
	}
	if !info.IsDir() {
		t.Errorf("Stat(/).IsDir() = false, want true")
	}
}
