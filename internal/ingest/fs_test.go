package ingest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newOSFS() *OSFileSystem { return &OSFileSystem{} }

func TestOSFileSystem_WriteReadFile(t *testing.T) {
	fsys := newOSFS()
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")

	want := []byte("hello, world\n")
	if err := fsys.WriteFile(path, want, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("ReadFile = %q, want %q", got, want)
	}
}

func TestOSFileSystem_MkdirAll(t *testing.T) {
	fsys := newOSFS()
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c")

	if err := fsys.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	info, err := os.Stat(nested)
	if err != nil {
		t.Fatalf("os.Stat after MkdirAll: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("expected directory at %q", nested)
	}
}

func TestOSFileSystem_Stat(t *testing.T) {
	fsys := newOSFS()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	data := []byte{0x01, 0x02, 0x03}

	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("setup WriteFile: %v", err)
	}

	info, err := fsys.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != int64(len(data)) {
		t.Errorf("Stat.Size = %d, want %d", info.Size(), len(data))
	}
	if info.IsDir() {
		t.Errorf("Stat.IsDir = true, want false")
	}
}

func TestOSFileSystem_Stat_Dir(t *testing.T) {
	fsys := newOSFS()
	dir := t.TempDir()

	info, err := fsys.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(dir): %v", err)
	}
	if !info.IsDir() {
		t.Errorf("Stat(dir).IsDir = false, want true")
	}
}

func TestOSFileSystem_Rename(t *testing.T) {
	fsys := newOSFS()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")

	content := []byte("rename me")
	if err := os.WriteFile(src, content, 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := fsys.Rename(src, dst); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src still exists after Rename")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("dst content = %q, want %q", got, content)
	}
}

func TestOSFileSystem_CopyFile(t *testing.T) {
	fsys := newOSFS()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")

	content := []byte("copy me")
	if err := os.WriteFile(src, content, 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := fsys.CopyFile(src, dst, 0600); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("dst content = %q, want %q", got, content)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("Stat dst: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("dst perm = %o, want %o", info.Mode().Perm(), 0600)
	}
}

func TestOSFileSystem_Remove(t *testing.T) {
	fsys := newOSFS()
	dir := t.TempDir()
	path := filepath.Join(dir, "todelete.txt")

	if err := os.WriteFile(path, []byte("bye"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := fsys.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still exists after Remove")
	}
}

func TestOSFileSystem_RemoveAll(t *testing.T) {
	fsys := newOSFS()
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "nested"), 0755); err != nil {
		t.Fatalf("setup MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "file.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("setup WriteFile: %v", err)
	}

	if err := fsys.RemoveAll(sub); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Errorf("directory still exists after RemoveAll")
	}
}

func TestOSFileSystem_WalkDir(t *testing.T) {
	fsys := newOSFS()
	dir := t.TempDir()

	// create: dir/a.txt, dir/sub/b.txt
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("b"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var visited []string
	err := fsys.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// record relative path
		rel, _ := filepath.Rel(dir, path)
		visited = append(visited, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	want := []string{".", "a.txt", "sub", filepath.Join("sub", "b.txt")}
	if len(visited) != len(want) {
		t.Fatalf("WalkDir visited %v, want %v", visited, want)
	}
	for i, w := range want {
		if visited[i] != w {
			t.Errorf("visited[%d] = %q, want %q", i, visited[i], w)
		}
	}
}

func TestOSFileSystem_ReadDir(t *testing.T) {
	fsys := newOSFS()
	dir := t.TempDir()

	names := []string{"z.txt", "a.txt", "m.txt"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	entries, err := fsys.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("ReadDir returned %d entries, want 3", len(entries))
	}
	// os.ReadDir returns sorted entries
	want := []string{"a.txt", "m.txt", "z.txt"}
	for i, e := range entries {
		if e.Name() != want[i] {
			t.Errorf("entries[%d].Name() = %q, want %q", i, e.Name(), want[i])
		}
	}
}

func TestOSFileSystem_ReadFile_NotExist(t *testing.T) {
	fsys := newOSFS()
	dir := t.TempDir()
	_, err := fsys.ReadFile(filepath.Join(dir, "nonexistent.txt"))
	if err == nil {
		t.Fatalf("ReadFile on nonexistent path should return error")
	}
	if !strings.Contains(err.Error(), "nonexistent.txt") {
		t.Errorf("error should mention file name, got: %v", err)
	}
}
