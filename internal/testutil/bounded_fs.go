package testutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testkit/fsdecorator"
)

// ErrBoundExceeded is returned when an operation outruns a declared MaxCalls
// bound. It is a sentinel so a test can assert the bound was the reason with
// errors.Is.
var ErrBoundExceeded = errors.New("boundedfs: operation bound exceeded")

// BoundedFS wraps a MemFS and enforces an operation bound, or a read-only
// whole-filesystem bound. It is the reusable form of the "refuse the write" and
// "read at most N bytes" decorators.
//
// One bound is active at a time. It matches on the operation name and exactly on
// the path (an empty Bound.Path matches any path for that operation); Rename and
// CopyFile are matched against their DESTINATION, as CountingFS counts them.
//
// MaxCalls bounds the number of matching calls; the call past the bound fails
// with ErrBoundExceeded. MaxBytes bounds the payload of a matching ReadFile to
// that many bytes; a zero MaxBytes is unlimited. ReadOnly refuses every mutating
// operation with fs.ErrPermission.
type BoundedFS struct {
	*MemFS

	mu       sync.Mutex
	bound    *fsdecorator.Bound
	readOnly bool
	calls    map[fsdecorator.Op]map[string]int64
}

var (
	_ fsdecorator.BoundedFS = (*BoundedFS)(nil)
	_ ingest.FileSystem     = (*BoundedFS)(nil)
)

// NewBoundedFS wraps an existing MemFS.
func NewBoundedFS(inner *MemFS) *BoundedFS {
	return &BoundedFS{MemFS: inner, calls: make(map[fsdecorator.Op]map[string]int64)}
}

// Limit installs, or replaces, the active bound.
func (b *BoundedFS) Limit(bound fsdecorator.Bound) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bound = &bound
}

// ReadOnly refuses every mutating operation.
func (b *BoundedFS) ReadOnly() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.readOnly = true
}

// mutable reports whether op changes the filesystem, which is the set ReadOnly
// refuses.
func mutable(op fsdecorator.Op) bool {
	switch op {
	case fsdecorator.OpWriteFile, fsdecorator.OpMkdirAll, fsdecorator.OpRename,
		fsdecorator.OpRemove, fsdecorator.OpRemoveAll, fsdecorator.OpCopyFile:
		return true
	default:
		return false
	}
}

// guard enforces ReadOnly and the MaxCalls bound for one call.
func (b *BoundedFS) guard(op fsdecorator.Op, path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.readOnly && mutable(op) {
		return &fs.PathError{Op: string(op), Path: path, Err: fs.ErrPermission}
	}
	if b.bound == nil || b.bound.Op != op || (b.bound.Path != "" && b.bound.Path != path) {
		return nil
	}
	if b.calls[op] == nil {
		b.calls[op] = make(map[string]int64)
	}
	b.calls[op][path]++
	if b.bound.MaxCalls > 0 && b.calls[op][path] > b.bound.MaxCalls {
		return fmt.Errorf("%w: %s on %q reached max_calls=%d", ErrBoundExceeded, op, path, b.bound.MaxCalls)
	}
	return nil
}

// capRead truncates a successful ReadFile payload to the matching MaxBytes.
func (b *BoundedFS) capRead(path string, data []byte) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bound == nil || b.bound.Op != fsdecorator.OpReadFile || b.bound.MaxBytes <= 0 {
		return data
	}
	if b.bound.Path != "" && b.bound.Path != path {
		return data
	}
	if int64(len(data)) > b.bound.MaxBytes {
		return data[:b.bound.MaxBytes]
	}
	return data
}

func (b *BoundedFS) ReadFile(path string) ([]byte, error) {
	if err := b.guard(fsdecorator.OpReadFile, path); err != nil {
		return nil, err
	}
	data, err := b.MemFS.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return b.capRead(path, data), nil
}

func (b *BoundedFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	if err := b.guard(fsdecorator.OpWriteFile, path); err != nil {
		return err
	}
	return b.MemFS.WriteFile(path, data, perm)
}

func (b *BoundedFS) MkdirAll(path string, perm os.FileMode) error {
	if err := b.guard(fsdecorator.OpMkdirAll, path); err != nil {
		return err
	}
	return b.MemFS.MkdirAll(path, perm)
}

func (b *BoundedFS) Stat(path string) (os.FileInfo, error) {
	if err := b.guard(fsdecorator.OpStat, path); err != nil {
		return nil, err
	}
	return b.MemFS.Stat(path)
}

func (b *BoundedFS) Lstat(path string) (os.FileInfo, error) {
	if err := b.guard(fsdecorator.OpLstat, path); err != nil {
		return nil, err
	}
	return b.MemFS.Lstat(path)
}

func (b *BoundedFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	if err := b.guard(fsdecorator.OpWalkDir, root); err != nil {
		return err
	}
	return b.MemFS.WalkDir(root, fn)
}

func (b *BoundedFS) Rename(oldpath, newpath string) error {
	if err := b.guard(fsdecorator.OpRename, newpath); err != nil {
		return err
	}
	return b.MemFS.Rename(oldpath, newpath)
}

func (b *BoundedFS) ReadDir(path string) ([]os.DirEntry, error) {
	if err := b.guard(fsdecorator.OpReadDir, path); err != nil {
		return nil, err
	}
	return b.MemFS.ReadDir(path)
}

func (b *BoundedFS) Remove(path string) error {
	if err := b.guard(fsdecorator.OpRemove, path); err != nil {
		return err
	}
	return b.MemFS.Remove(path)
}

func (b *BoundedFS) RemoveAll(path string) error {
	if err := b.guard(fsdecorator.OpRemoveAll, path); err != nil {
		return err
	}
	return b.MemFS.RemoveAll(path)
}

func (b *BoundedFS) CopyFile(src, dst string, perm os.FileMode) error {
	if err := b.guard(fsdecorator.OpCopyFile, dst); err != nil {
		return err
	}
	return b.MemFS.CopyFile(src, dst, perm)
}
