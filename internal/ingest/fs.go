package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// FileSystem abstracts file operations for testability.
type FileSystem interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
	MkdirAll(path string, perm os.FileMode) error
	Stat(path string) (os.FileInfo, error)
	Lstat(path string) (os.FileInfo, error)
	WalkDir(root string, fn fs.WalkDirFunc) error
	Rename(oldpath, newpath string) error
	ReadDir(path string) ([]os.DirEntry, error)
	Remove(path string) error
	RemoveAll(path string) error
	CopyFile(src, dst string, perm os.FileMode) error
}

type capturedSourceFileSystem struct {
	FileSystem
	path string
	data []byte
}

func (f capturedSourceFileSystem) ReadFile(path string) ([]byte, error) {
	if path == f.path {
		return append([]byte(nil), f.data...), nil
	}
	return f.FileSystem.ReadFile(path)
}

func completeJSONLPrefix(data []byte) []byte {
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return data
	}
	lastComplete := bytes.LastIndexByte(data, '\n')
	if json.Valid(bytes.TrimSpace(data[lastComplete+1:])) {
		return data
	}
	if lastComplete >= 0 {
		return data[:lastComplete+1]
	}
	return nil
}

// OSFileSystem is the production implementation wrapping os.* calls.
type OSFileSystem struct{}

var _ FileSystem = (*OSFileSystem)(nil)

func (f *OSFileSystem) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// Open implements the optional streaming reader Claude discovery uses to avoid
// loading every normal transcript before it can reject file-history sidecars.
func (f *OSFileSystem) Open(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

func (f *OSFileSystem) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

func (f *OSFileSystem) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (f *OSFileSystem) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

func (f *OSFileSystem) Lstat(path string) (os.FileInfo, error) {
	return os.Lstat(path)
}

func (f *OSFileSystem) WalkDir(root string, fn fs.WalkDirFunc) error {
	return filepath.WalkDir(root, fn)
}

func (f *OSFileSystem) Rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

func (f *OSFileSystem) ReadDir(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

func (f *OSFileSystem) Remove(path string) error {
	return os.Remove(path)
}

func (f *OSFileSystem) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

func (f *OSFileSystem) CopyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("copy %q -> %q: open src: %w", src, dst, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("copy %q -> %q: create dst: %w", src, dst, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("copy %q -> %q: copy data: %w", src, dst, err)
	}

	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("copy %q -> %q: sync: %w", src, dst, err)
	}

	if err := out.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("copy %q -> %q: close dst: %w", src, dst, err)
	}

	return nil
}
