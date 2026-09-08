package testutil

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// Memory artifact operations exercise the same publisher protocol as OS roots.
// Sync is a type/existence check only; real durability is proved by OS fixtures.
var _ ingest.DurableFileSystem = (*MemFS)(nil)
var _ ingest.ArtifactRoot = (*memoryArtifactRoot)(nil)

func (m *MemFS) OpenArtifactRoot(path string) (ingest.ArtifactRoot, error) {
	info, err := m.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("memory artifact root is not a directory")
	}
	return &memoryArtifactRoot{fs: m, base: filepath.Clean(path)}, nil
}

func (m *MemFS) CreateArtifactRoot(path string) (ingest.ArtifactRoot, error) {
	if err := m.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	return m.OpenArtifactRoot(path)
}

type memoryArtifactRoot struct {
	fs     *MemFS
	base   string
	closed atomic.Bool
}

func (r *memoryArtifactRoot) path(path string) (string, error) {
	if r.closed.Load() {
		return "", fs.ErrClosed
	}
	if !filepath.IsLocal(path) {
		return "", fs.ErrInvalid
	}
	return filepath.Join(r.base, path), nil
}
func (r *memoryArtifactRoot) Close() error { r.closed.Store(true); return nil }
func (r *memoryArtifactRoot) ReadFile(path string) ([]byte, error) {
	path, err := r.path(path)
	if err != nil {
		return nil, err
	}
	return r.fs.ReadFile(path)
}
func (r *memoryArtifactRoot) ReadDir(path string) ([]fs.DirEntry, error) {
	path, err := r.path(path)
	if err != nil {
		return nil, err
	}
	return r.fs.ReadDir(path)
}
func (r *memoryArtifactRoot) Lstat(path string) (fs.FileInfo, error) {
	path, err := r.path(path)
	if err != nil {
		return nil, err
	}
	return r.fs.Lstat(path)
}
func (r *memoryArtifactRoot) MkdirAll(path string, mode fs.FileMode) error {
	path, err := r.path(path)
	if err != nil {
		return err
	}
	return r.fs.MkdirAll(path, mode)
}
func (r *memoryArtifactRoot) Remove(path string) error {
	path, err := r.path(path)
	if err != nil {
		return err
	}
	return r.fs.Remove(path)
}
func (r *memoryArtifactRoot) Mkdir(path string, _ fs.FileMode) error {
	path, err := r.path(path)
	if err != nil {
		return err
	}
	r.fs.mu.Lock()
	defer r.fs.mu.Unlock()
	if r.fs.Dirs[path] {
		return fs.ErrExist
	}
	if _, ok := r.fs.Files[path]; ok {
		return fs.ErrExist
	}
	if !r.fs.Dirs[filepath.Dir(path)] {
		return fs.ErrNotExist
	}
	r.fs.Dirs[path] = true
	return nil
}
func (r *memoryArtifactRoot) CreateFile(path string, data []byte, _ fs.FileMode) error {
	path, err := r.path(path)
	if err != nil {
		return err
	}
	r.fs.mu.Lock()
	defer r.fs.mu.Unlock()
	if _, ok := r.fs.Files[path]; ok || r.fs.Dirs[path] {
		return fs.ErrExist
	}
	if !r.fs.Dirs[filepath.Dir(path)] {
		return fs.ErrNotExist
	}
	r.fs.Files[path] = append([]byte(nil), data...)
	r.fs.ModTimes[path] = time.Now()
	return nil
}
func (r *memoryArtifactRoot) SyncFile(path string) error {
	info, err := r.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fs.ErrInvalid
	}
	return nil
}
func (r *memoryArtifactRoot) SyncDir(path string) error {
	info, err := r.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fs.ErrInvalid
	}
	return nil
}
func (r *memoryArtifactRoot) Rename(from, to string) error {
	from, err := r.path(from)
	if err != nil {
		return err
	}
	to, err = r.path(to)
	if err != nil {
		return err
	}
	r.fs.mu.Lock()
	defer r.fs.mu.Unlock()
	if data, ok := r.fs.Files[from]; ok {
		if r.fs.Dirs[to] {
			return fs.ErrExist
		}
		if !r.fs.Dirs[filepath.Dir(to)] {
			return fs.ErrNotExist
		}
		r.fs.Files[to], r.fs.ModTimes[to] = data, r.fs.ModTimes[from]
		delete(r.fs.Files, from)
		delete(r.fs.ModTimes, from)
		return nil
	}
	if !r.fs.Dirs[from] {
		return fs.ErrNotExist
	}
	if strings.HasPrefix(to, from+string(filepath.Separator)) {
		return fs.ErrInvalid
	}
	if _, ok := r.fs.Files[to]; ok {
		return fs.ErrExist
	}
	for path := range r.fs.Files {
		if strings.HasPrefix(path, to+string(filepath.Separator)) {
			return fs.ErrExist
		}
	}
	for path := range r.fs.Dirs {
		if path != to && strings.HasPrefix(path, to+string(filepath.Separator)) {
			return fs.ErrExist
		}
	}
	files := make(map[string][]byte)
	directories := make([]string, 0)
	for path, data := range r.fs.Files {
		if strings.HasPrefix(path, from+string(filepath.Separator)) {
			files[path] = data
		}
	}
	for path := range r.fs.Dirs {
		if path == from || strings.HasPrefix(path, from+string(filepath.Separator)) {
			directories = append(directories, path)
		}
	}
	for path, data := range files {
		destination := to + strings.TrimPrefix(path, from)
		r.fs.Files[destination], r.fs.ModTimes[destination] = data, r.fs.ModTimes[path]
		delete(r.fs.Files, path)
		delete(r.fs.ModTimes, path)
	}
	for _, path := range directories {
		delete(r.fs.Dirs, path)
	}
	for _, path := range directories {
		r.fs.Dirs[to+strings.TrimPrefix(path, from)] = true
	}
	return nil
}

type memoryArtifactLock struct {
	release chan struct{}
	once    sync.Once
}

var _ io.Closer = (*memoryArtifactLock)(nil)

func (l *memoryArtifactLock) Close() error { l.once.Do(func() { l.release <- struct{}{} }); return nil }
func (r *memoryArtifactRoot) Lock(ctx context.Context, path string, mode ingest.ArtifactLockMode, create bool) (io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if mode != ingest.ArtifactLockRead && mode != ingest.ArtifactLockWrite {
		return nil, fs.ErrInvalid
	}
	path, err := r.path(path)
	if err != nil {
		return nil, err
	}
	r.fs.mu.Lock()
	if _, ok := r.fs.Files[path]; !ok {
		if !create {
			r.fs.mu.Unlock()
			return nil, &os.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
		}
		r.fs.Files[path] = nil
	}
	if r.fs.artifactLocks == nil {
		r.fs.artifactLocks = make(map[string]chan struct{})
	}
	lock := r.fs.artifactLocks[path]
	if lock == nil {
		lock = make(chan struct{}, 1)
		lock <- struct{}{}
		r.fs.artifactLocks[path] = lock
	}
	r.fs.mu.Unlock()
	// Readers serialize in this in-memory dependency. OS fixtures separately
	// prove actual shared/exclusive descriptor and cross-process behavior.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-lock:
		return &memoryArtifactLock{release: lock}, nil
	}
}
