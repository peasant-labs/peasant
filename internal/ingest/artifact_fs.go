package ingest

import (
	"context"
	"io"
	"io/fs"
)

// DurableFileSystem is the optional managed-output capability. Source adapters
// continue using FileSystem; durable publication refuses a missing capability.
// Opening a root does not create it or repair pending publication state.
type DurableFileSystem interface {
	FileSystem
	OpenArtifactRoot(path string) (ArtifactRoot, error)
	CreateArtifactRoot(path string) (ArtifactRoot, error)
}

// ArtifactRoot confines managed publication/recovery to one opened directory.
// CreateFile must not replace an existing path. Locks are persistent coordination
// files, never producer receipts; callers must not delete them after unlocking.
type ArtifactRoot interface {
	ReadFile(path string) ([]byte, error)
	CreateFile(path string, data []byte, perm fs.FileMode) error
	MkdirAll(path string, perm fs.FileMode) error
	Mkdir(path string, perm fs.FileMode) error
	Lstat(path string) (fs.FileInfo, error)
	ReadDir(path string) ([]fs.DirEntry, error)
	OpenDirectory(path string) (fs.ReadDirFile, error)
	Rename(oldPath, newPath string) error
	Remove(path string) error
	SyncFile(path string) error
	SyncDir(path string) error
	Lock(ctx context.Context, path string, mode ArtifactLockMode, create bool) (io.Closer, error)
	Close() error
}

// ArtifactLockMode distinguishes snapshot readers from publication/recovery.
type ArtifactLockMode uint8

const (
	ArtifactLockRead ArtifactLockMode = iota + 1
	ArtifactLockWrite
)
