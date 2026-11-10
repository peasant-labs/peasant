package testutil_test

import (
	"io/fs"
	"os"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestOpRecorderFSRecordsTheExactPathSetPerOperation pins the recording
// contract: each operation records the path it was given, the whole set is
// readable per operation, a repeated path keeps its multiplicity, and Rename
// and CopyFile are attributed to their destination like CountingFS.
func TestOpRecorderFSRecordsTheExactPathSetPerOperation(t *testing.T) {
	t.Parallel()
	mfs := testutil.NewMemFS()
	recorder := testutil.NewOpRecorderFS(mfs)

	const (
		a = "/root/a.jsonl"
		b = "/root/b/dir"
	)
	if err := mfs.WriteFile(a, []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed %s: %v", a, err)
	}
	recorder.Reset()

	if _, err := recorder.ReadFile(a); err != nil {
		t.Fatalf("ReadFile(%s): %v", a, err)
	}
	if _, err := recorder.ReadFile(a); err != nil {
		t.Fatalf("second ReadFile(%s): %v", a, err)
	}
	if _, err := recorder.ReadDir("/root"); err != nil {
		t.Fatalf("ReadDir(/root): %v", err)
	}
	if _, err := recorder.Stat(a); err != nil {
		t.Fatalf("Stat(%s): %v", a, err)
	}
	if err := recorder.MkdirAll(b, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", b, err)
	}
	if err := recorder.Rename(a, "/root/moved.jsonl"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := recorder.CopyFile("/root/moved.jsonl", "/root/copy.jsonl", 0o644); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}

	if got, want := recorder.Paths(testutil.FSOpReadFile), []string{a}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadFile paths = %v, want %v", got, want)
	}
	if got := recorder.Count(testutil.FSOpReadFile, a); got != 2 {
		t.Fatalf("ReadFile count on %s = %d, want 2", a, got)
	}
	if got, want := recorder.Paths(testutil.FSOpReadDir), []string{"/root"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadDir paths = %v, want %v", got, want)
	}
	if got, want := recorder.Paths(testutil.FSOpStat), []string{a}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Stat paths = %v, want %v", got, want)
	}
	if got, want := recorder.Paths(testutil.FSOpMkdirAll), []string{b}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MkdirAll paths = %v, want %v", got, want)
	}
	if got, want := recorder.Paths(testutil.FSOpRename), []string{"/root/moved.jsonl"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Rename paths = %v, want the destination %v", got, want)
	}
	if got, want := recorder.Paths(testutil.FSOpCopyFile), []string{"/root/copy.jsonl"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CopyFile paths = %v, want the destination %v", got, want)
	}
	if walks := recorder.Total(testutil.FSOpWalkDir); walks != 0 {
		t.Fatalf("WalkDir total = %d, want 0 (no operation walked)", walks)
	}
}

// TestOpRecorderFSDelegatesAndObservesWalk proves the decorator is transparent:
// content still reads back through the wrapper, a walk still reaches every
// file, and the walk is recorded as one operation on its root.
func TestOpRecorderFSDelegatesAndObservesWalk(t *testing.T) {
	t.Parallel()
	mfs := testutil.NewMemFS()
	recorder := testutil.NewOpRecorderFS(mfs)
	for _, path := range []string{"/root/a/x.jsonl", "/root/b/y.jsonl"} {
		if err := mfs.WriteFile(path, []byte(path), 0o644); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
	recorder.Reset()

	var walked []string
	if err := recorder.WalkDir("/root", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			walked = append(walked, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	if len(walked) != 2 {
		t.Fatalf("walked %v, want both seeded files", walked)
	}
	data, err := recorder.ReadFile("/root/a/x.jsonl")
	if err != nil {
		t.Fatalf("ReadFile through the wrapper: %v", err)
	}
	if string(data) != "/root/a/x.jsonl" {
		t.Fatalf("ReadFile through the wrapper = %q, want the seeded bytes", data)
	}
	if got, want := recorder.Paths(testutil.FSOpWalkDir), []string{"/root"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("WalkDir paths = %v, want %v", got, want)
	}
}

// TestOpRecorderFSWrapsARealFilesystem proves the recorder is not bound to
// MemFS: an OpenCode parity case drives a real SQLite file, and the recorder
// must observe the real filesystem's reads.
func TestOpRecorderFSWrapsARealFilesystem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := dir + "/real.jsonl"
	if err := os.WriteFile(path, []byte("real"), 0o600); err != nil {
		t.Fatalf("write the real file: %v", err)
	}
	recorder := testutil.NewOpRecorderFS(&ingestFS{fs: os.DirFS("/")})
	if _, err := recorder.ReadFile(path); err != nil {
		t.Fatalf("ReadFile over the real filesystem: %v", err)
	}
	if got := recorder.Count(testutil.FSOpReadFile, path); got != 1 {
		t.Fatalf("ReadFile count on %s = %d, want 1", path, got)
	}
}

// ingestFS is the minimal real-filesystem FileSystem the recorder test needs;
// it presents the same method set as ingest.FileSystem without importing the
// package under test.
type ingestFS struct {
	fs fs.FS
}

func (f *ingestFS) ReadFile(path string) ([]byte, error)        { return fs.ReadFile(f.fs, trimRoot(path)) }
func (f *ingestFS) WriteFile(string, []byte, os.FileMode) error { return os.ErrPermission }
func (f *ingestFS) MkdirAll(string, os.FileMode) error          { return os.ErrPermission }
func (f *ingestFS) Stat(path string) (os.FileInfo, error)       { return fs.Stat(f.fs, trimRoot(path)) }
func (f *ingestFS) Lstat(path string) (os.FileInfo, error)      { return fs.Stat(f.fs, trimRoot(path)) }
func (f *ingestFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	return fs.WalkDir(f.fs, trimRoot(root), fn)
}
func (f *ingestFS) Rename(string, string) error { return os.ErrPermission }
func (f *ingestFS) ReadDir(path string) ([]os.DirEntry, error) {
	return fs.ReadDir(f.fs, trimRoot(path))
}
func (f *ingestFS) Remove(string) error                        { return os.ErrPermission }
func (f *ingestFS) RemoveAll(string) error                     { return os.ErrPermission }
func (f *ingestFS) CopyFile(string, string, os.FileMode) error { return os.ErrPermission }

func trimRoot(path string) string {
	for len(path) > 1 && path[0] == '/' {
		path = path[1:]
	}
	return path
}
