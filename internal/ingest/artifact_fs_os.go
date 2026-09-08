package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var _ DurableFileSystem = (*OSFileSystem)(nil)
var _ ArtifactRoot = (*osArtifactRoot)(nil)
var _ io.Closer = (*artifactFileLock)(nil)

// OpenArtifactRoot pins an existing output directory. os.Root resolves every
// subsequent operation relative to its directory handle, including symlinks.
func (f *OSFileSystem) OpenArtifactRoot(path string) (ArtifactRoot, error) {
	if !artifactLocksSupported {
		return nil, fmt.Errorf("open managed artifact root %q: this platform has no supported advisory-lock implementation; no artifact was changed; use a supported Peasant platform", path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open managed artifact root %q before capture/publication: %w; no recovery was performed; check the configured output directory", path, err)
	}
	return &osArtifactRoot{Root: root}, nil
}

// CreateArtifactRoot is the explicitly mutating bootstrap boundary. It creates
// missing output directories below a pinned existing ancestor, then syncs every
// newly created directory entry before returning the confined output handle.
func (f *OSFileSystem) CreateArtifactRoot(path string) (ArtifactRoot, error) {
	if !artifactLocksSupported {
		return nil, fmt.Errorf("create managed output root: advisory locks are unsupported on this platform; no directory was created")
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || path == string(filepath.Separator) {
		return nil, fmt.Errorf("create managed output root: %q is not a dedicated absolute output directory", path)
	}
	ancestor := filepath.Dir(path)
	var root *os.Root
	var err error
	for {
		root, err = os.OpenRoot(ancestor)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) || filepath.Dir(ancestor) == ancestor {
			return nil, err
		}
		ancestor = filepath.Dir(ancestor)
	}
	defer root.Close()
	relative, err := filepath.Rel(ancestor, path)
	if err != nil || !filepath.IsLocal(relative) {
		return nil, fmt.Errorf("create managed output root: invalid path relative to opened ancestor: %v", err)
	}
	if err := root.MkdirAll(relative, 0700); err != nil {
		return nil, err
	}
	confined := &osArtifactRoot{Root: root}
	parent := "."
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if err := confined.SyncDir(parent); err != nil {
			return nil, err
		}
		parent = filepath.Join(parent, part)
	}
	if err := confined.SyncDir(relative); err != nil {
		return nil, err
	}
	output, err := root.OpenRoot(relative)
	if err != nil {
		return nil, err
	}
	return &osArtifactRoot{Root: output}, nil
}

type osArtifactRoot struct{ *os.Root }

func (r *osArtifactRoot) ReadFile(path string) ([]byte, error) {
	file, err := r.OpenFile(path, os.O_RDONLY|artifactNonblockFlag, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read managed artifact %q: not a regular file; refusing special-file input; restore the expected file before retrying", path)
	}
	return io.ReadAll(file)
}

func (r *osArtifactRoot) CreateFile(path string, data []byte, perm fs.FileMode) error {
	file, err := r.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|artifactNonblockFlag, perm)
	if err != nil {
		return err
	}
	n, writeErr := file.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, file.Close())
}

func (r *osArtifactRoot) ReadDir(path string) ([]fs.DirEntry, error) {
	file, err := r.OpenDirectory(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.ReadDir(-1)
}

func (r *osArtifactRoot) OpenDirectory(path string) (fs.ReadDirFile, error) {
	file, err := r.OpenFile(path, os.O_RDONLY|artifactNonblockFlag, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.IsDir() {
		_ = file.Close()
		return nil, fmt.Errorf("open managed artifact directory %q: expected readable directory: %w", path, errors.Join(err, fs.ErrInvalid))
	}
	return file, nil
}

func (r *osArtifactRoot) SyncFile(path string) error { return r.sync(path, false) }
func (r *osArtifactRoot) SyncDir(path string) error  { return r.sync(path, true) }

func (r *osArtifactRoot) sync(path string, directory bool) error {
	file, err := r.OpenFile(path, os.O_RDONLY|artifactNonblockFlag, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("sync managed path %q: unexpected file type; durability is unconfirmed; restore the expected regular file/directory before retrying", path)
	}
	return file.Sync()
}

func (r *osArtifactRoot) Lock(ctx context.Context, path string, mode ArtifactLockMode, create bool) (io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if mode != ArtifactLockRead && mode != ArtifactLockWrite {
		return nil, fmt.Errorf("lock managed artifact %q: invalid lock mode; no file was changed; select read or write ownership", path)
	}
	flags := os.O_RDONLY | artifactNonblockFlag
	if create {
		flags = os.O_RDWR | os.O_CREATE | artifactNonblockFlag
	}
	file, err := r.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("lock managed artifact %q: coordination path is not a readable regular file (%v); no publication was started; restore the lock directory before retrying", path, err)
	}
	if err := lockArtifactFile(ctx, file, mode); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &artifactFileLock{file: file}, nil
}

type artifactFileLock struct {
	file *os.File
	once sync.Once
	err  error
}

func (l *artifactFileLock) Close() error {
	l.once.Do(func() { l.err = errors.Join(unlockArtifactFile(l.file), l.file.Close()) })
	return l.err
}
