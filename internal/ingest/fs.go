package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
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

type sourcePrefixReader interface {
	ReadSourcePrefix(string) ([]byte, error)
}

// fileHeaderReader reads the first bytes of a file without loading the rest of
// it. It is optional: a filesystem that cannot do it is read in full instead,
// which is safe for an in-memory one because it holds only what a caller put
// there. The point of the capability is that a real file of any size can be
// identified from its first bytes alone.
type fileHeaderReader interface {
	ReadFileHeader(path string, limit int) ([]byte, error)
}

var _ fileHeaderReader = (*OSFileSystem)(nil)

var _ sourcePrefixReader = (*OSFileSystem)(nil)

func (f capturedSourceFileSystem) ReadFile(path string) ([]byte, error) {
	if path == f.path {
		return append([]byte(nil), f.data...), nil
	}
	return f.FileSystem.ReadFile(path)
}

func completeJSONLPrefix(data []byte) ([]byte, error) {
	lastComplete := bytes.LastIndexByte(data, '\n')
	records := bytes.Split(data[:lastComplete+1], []byte{'\n'})
	for i, record := range records {
		if len(bytes.TrimSpace(record)) > 0 && !json.Valid(record) {
			return nil, fmt.Errorf("capture JSONL record %d: malformed complete JSON; prior stored snapshot was retained; repair this record and retry ingest", i+1)
		}
	}
	if len(bytes.TrimSpace(data[lastComplete+1:])) == 0 || json.Valid(data[lastComplete+1:]) {
		return data, nil
	}
	return data[:lastComplete+1], nil
}

// ReadSourcePrefix fixes the acquisition boundary at the opened descriptor size.
// Appends after Stat belong to a later ingest, even when the writer stays busy.
func (f *OSFileSystem) ReadSourcePrefix(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, info.Size()))
	if err == nil && int64(len(data)) != info.Size() {
		err = io.ErrUnexpectedEOF
	}
	return data, err
}

// ReadFileHeader returns at most limit bytes from the start of path. A file
// shorter than limit yields what it holds; that is not an error, because a
// caller identifying a format needs whatever prefix exists.
func (f *OSFileSystem) ReadFileHeader(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	header := make([]byte, limit)
	read, err := io.ReadFull(file, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return header[:read], nil
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

// EvalSymlinks supplies canonical candidate ordering to file-backed discovery.
// In-memory filesystems may omit this optional capability when they have no links.
func (f *OSFileSystem) EvalSymlinks(path string) (string, error) {
	return filepath.EvalSymlinks(path)
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

	// No fsync: the write path relies on the database transaction for
	// durability and on rename for atomicity, so a copied debug file left torn
	// by a power loss is re-copied on the next ingest, not fsync'd here.
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("copy %q -> %q: close dst: %w", src, dst, err)
	}

	return nil
}
