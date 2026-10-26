package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// TestPipeline_moveSessionFiles_PrunesBelowDebugDirectory exercises the
// containment checks fixed for issue #375: files below the session's debug
// directory must be discovered and moved, including a file nested two levels
// below debug/, on both Linux and Windows. Before the fix, filepath.Rel's
// native-separator result (backslash on Windows) never matched the hardcoded
// forward-slash "debug/" prefix, so:
//   - a flat file directly under debug/ failed the file-level prefix check
//     (pipeline.go:3581, now compared against filepath.ToSlash(rel)), and
//   - a nested subdirectory under debug/ was pruned by the directory-level
//     check (pipeline.go:3576, now compared against filepath.ToSlash(rel))
//     before WalkDir ever descended into it, so a file below that
//     subdirectory was never even visited.
//
// This uses the real filesystem (OSFileSystem) via t.TempDir(), so it
// exercises whatever separator the running platform's filepath.Rel actually
// produces rather than asserting a hardcoded POSIX string.
func TestPipeline_moveSessionFiles_PrunesBelowDebugDirectory(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	sessionID := "testsession"
	debug := defaults.DirDebug.String()

	files := map[string]string{
		filepath.Join(src, sessionID+"--meta.json"):     "session metadata",
		filepath.Join(src, debug, "note.log"):           "flat debug artifact",
		filepath.Join(src, debug, "archive", "old.log"): "nested debug artifact",
		filepath.Join(src, "unrelated", "skip.txt"):     "not part of this session",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	p := &Pipeline{fs: &OSFileSystem{}}
	if err := p.moveSessionFiles(src, dst, sessionID); err != nil {
		t.Fatalf("moveSessionFiles: %v", err)
	}

	// The session's own top-level artifact and everything below debug/ -
	// flat or nested - must have been moved to dst.
	wantMoved := []string{
		filepath.Join(dst, sessionID+"--meta.json"),
		filepath.Join(dst, debug, "note.log"),
		filepath.Join(dst, debug, "archive", "old.log"),
	}
	for _, path := range wantMoved {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("expected moved file %s: %v", path, err)
			continue
		}
		want := files[srcCounterpart(path, src, dst)]
		if string(data) != want {
			t.Errorf("moved file %s content = %q, want %q", path, data, want)
		}
	}

	// The corresponding source files must be gone (Rename, not copy).
	for _, path := range wantMoved {
		srcPath := srcCounterpart(path, src, dst)
		if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
			t.Errorf("expected source file %s to be moved away, stat err = %v", srcPath, err)
		}
	}

	// Unrelated content outside debug/ and outside the session's own prefix
	// must never be touched: still present at its original location, absent
	// at the destination.
	unrelatedSrc := filepath.Join(src, "unrelated", "skip.txt")
	if _, err := os.Stat(unrelatedSrc); err != nil {
		t.Errorf("unrelated file should remain at %s: %v", unrelatedSrc, err)
	}
	unrelatedDst := filepath.Join(dst, "unrelated", "skip.txt")
	if _, err := os.Stat(unrelatedDst); !os.IsNotExist(err) {
		t.Errorf("unrelated file should not have been moved to %s, stat err = %v", unrelatedDst, err)
	}
}

// srcCounterpart maps a moved destination path back to its original source
// path, for the paired before/after assertions above.
func srcCounterpart(dstPath, src, dst string) string {
	rel, err := filepath.Rel(dst, dstPath)
	if err != nil {
		return dstPath
	}
	return filepath.Join(src, rel)
}

// TestPipeline_replaceSessionDir_PrunesObsoleteDebugFiles exercises the prune
// walk's containment check, the sibling of the one fixed in moveSessionFiles for
// issue #375 and missed when that one was corrected. A parent-owned file below
// debug/ that the replacement output no longer produces must be pruned, on both
// Linux and Windows.
//
// Before the fix, filepath.Rel's native-separator result (backslash on Windows)
// never matched the hardcoded forward-slash "debug/" prefix, so every file below
// debug/ took the not-parent-owned branch and was skipped instead of being
// considered stale. The failure is silent accumulation rather than an error:
// obsolete debug artifacts stayed in the session directory forever.
//
// Like its sibling this uses the real filesystem (OSFileSystem) via t.TempDir(),
// so the separator is whatever the running platform's filepath.Rel produces
// rather than a hardcoded POSIX string. It therefore passes on Linux with or
// without the fix, and only distinguishes them on Windows.
func TestPipeline_replaceSessionDir_PrunesObsoleteDebugFiles(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	sessionID := "testsession"
	metadataName := sessionID + "--meta.json"
	debug := defaults.DirDebug.String()

	// The staged replacement output: metadata plus the one debug artifact this
	// run still produces.
	staged := map[string]string{
		filepath.Join(src, metadataName):      "new session metadata",
		filepath.Join(src, debug, "keep.log"): "still produced",
	}
	// Already installed: the same two paths, plus obsolete debug artifacts (one
	// flat, one nested) that the replacement output no longer contains.
	installed := map[string]string{
		filepath.Join(dst, metadataName):               "old session metadata",
		filepath.Join(dst, debug, "keep.log"):          "superseded",
		filepath.Join(dst, debug, "obsolete.log"):      "no longer produced",
		filepath.Join(dst, debug, "old", "nested.log"): "no longer produced either",
	}
	for _, set := range []map[string]string{staged, installed} {
		for path, content := range set {
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
			}
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatalf("WriteFile(%s): %v", path, err)
			}
		}
	}

	p := &Pipeline{fs: &OSFileSystem{}}
	if err := p.replaceSessionDir(context.Background(), src, dst, sessionID, metadataName, nil); err != nil {
		t.Fatalf("replaceSessionDir: %v", err)
	}

	for _, gone := range []string{
		filepath.Join(dst, debug, "obsolete.log"),
		filepath.Join(dst, debug, "old", "nested.log"),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("obsolete parent-owned debug file %s survived the prune (stat error = %v)", gone, err)
		}
	}

	// The replacement output must still be installed, with its new contents.
	for path, want := range map[string]string{
		filepath.Join(dst, metadataName):      "new session metadata",
		filepath.Join(dst, debug, "keep.log"): "still produced",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read installed file %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}
