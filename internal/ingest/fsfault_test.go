package ingest

// Owner B of the shared test filesystem decorators.
//
// internal/testutil imports internal/ingest, so a white-box `package ingest`
// test cannot import internal/testutil: that is an import cycle. The decorators
// that need this package's unexported internals therefore live here, beside the
// white-box tests that use them. The shared contract they implement is declared
// in internal/testkit/fsdecorator, a standard-library-only leaf package both owners
// import. See TESTING.md, "Two owners, no import cycle", and
// internal/testkit/fsdecorator/testdata/decorator_classification.yaml for the
// per-decorator classification and owner.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing/fstest"
	"time"
)

// --- counting capability ---------------------------------------------------

// cancellingLookupFS cancels the run from inside a managed-layout lookup. That
// reproduces a cancellation arriving while the pool is already classifying,
// without depending on timing between goroutines.
type cancellingLookupFS struct {
	FileSystem
	lookups     atomic.Int64
	cancelAfter int64
	cancel      context.CancelFunc
}

func (filesystem *cancellingLookupFS) note() {
	if filesystem.lookups.Add(1) == filesystem.cancelAfter {
		filesystem.cancel()
	}
}

func (filesystem *cancellingLookupFS) Stat(path string) (os.FileInfo, error) {
	filesystem.note()
	return filesystem.FileSystem.Stat(path)
}

func (filesystem *cancellingLookupFS) ReadDir(path string) ([]os.DirEntry, error) {
	filesystem.note()
	return filesystem.FileSystem.ReadDir(path)
}

// cancelNestedDiffFS returns a host/session layout for every directory read, so
// a lookup that misses the flat metadata path walks into the nested subagents
// layout. The first Stat under "/subagents/session-two/" cancels the run from
// inside that walk. Counters are atomic because the classifier pool calls the
// filesystem from several goroutines at once.
type cancelNestedDiffFS struct {
	emptyProgressFS
	cancel      context.CancelFunc
	reads       atomic.Int64
	stats       atomic.Int64
	canceled    atomic.Bool
	afterCancel atomic.Int64
}

var _ FileSystem = (*cancelNestedDiffFS)(nil)

func (f *cancelNestedDiffFS) ReadDir(string) ([]os.DirEntry, error) {
	f.reads.Add(1)
	if f.canceled.Load() {
		f.afterCancel.Add(1)
	}
	return fs.ReadDir(fstest.MapFS{
		"parent":  &fstest.MapFile{Mode: fs.ModeDir},
		"sibling": &fstest.MapFile{Mode: fs.ModeDir},
	}, ".")
}

func (f *cancelNestedDiffFS) Stat(path string) (os.FileInfo, error) {
	f.stats.Add(1)
	if f.canceled.Load() {
		f.afterCancel.Add(1)
	}
	if f.cancel != nil && strings.Contains(path, "/subagents/session-two/") && f.canceled.CompareAndSwap(false, true) {
		f.cancel()
	}
	return nil, os.ErrNotExist
}

// countingReadFS records disk reads of one pair's two halves, so a case can
// tell a lone metadata read (the mixed snapshot) from one validated read of
// both halves.
type countingReadFS struct {
	FileSystem
	metadataPath    string
	transcriptPath  string
	metadataReads   int
	transcriptReads int
}

var _ FileSystem = (*countingReadFS)(nil)

func (filesystem *countingReadFS) ReadFile(path string) ([]byte, error) {
	switch path {
	case filesystem.metadataPath:
		filesystem.metadataReads++
	case filesystem.transcriptPath:
		filesystem.transcriptReads++
	}
	return filesystem.FileSystem.ReadFile(path)
}

// countingCancelFS records every filesystem read the lookup performs and can
// cancel the run from inside one of them, which is how a case reproduces a
// cancellation that arrives mid-walk without depending on timing.
type countingCancelFS struct {
	FileSystem
	calls       int
	cancelAfter int
	cancel      context.CancelFunc
}

func (filesystem *countingCancelFS) note() {
	filesystem.calls++
	if filesystem.cancelAfter > 0 && filesystem.calls == filesystem.cancelAfter {
		filesystem.cancel()
	}
}

func (filesystem *countingCancelFS) Stat(path string) (os.FileInfo, error) {
	filesystem.note()
	return filesystem.FileSystem.Stat(path)
}

func (filesystem *countingCancelFS) ReadDir(path string) ([]os.DirEntry, error) {
	filesystem.note()
	return filesystem.FileSystem.ReadDir(path)
}

func (filesystem *countingCancelFS) ReadFile(path string) ([]byte, error) {
	filesystem.note()
	return filesystem.FileSystem.ReadFile(path)
}

// --- gated capability ------------------------------------------------------

// emptyProgressFS is an empty read-only filesystem: every read misses and every
// mutation is refused. It is the base for the decorators that hold or script a
// single metadata lookup.
type emptyProgressFS struct{}

func (emptyProgressFS) ReadFile(string) ([]byte, error)             { return nil, os.ErrNotExist }
func (emptyProgressFS) WriteFile(string, []byte, os.FileMode) error { return os.ErrPermission }
func (emptyProgressFS) MkdirAll(string, os.FileMode) error          { return os.ErrPermission }
func (emptyProgressFS) Stat(string) (os.FileInfo, error)            { return nil, os.ErrNotExist }
func (emptyProgressFS) Lstat(string) (os.FileInfo, error)           { return nil, os.ErrNotExist }
func (emptyProgressFS) WalkDir(string, fs.WalkDirFunc) error        { return os.ErrNotExist }
func (emptyProgressFS) Rename(string, string) error                 { return os.ErrPermission }
func (emptyProgressFS) ReadDir(string) ([]os.DirEntry, error)       { return nil, os.ErrNotExist }
func (emptyProgressFS) Remove(string) error                         { return os.ErrPermission }
func (emptyProgressFS) RemoveAll(string) error                      { return os.ErrPermission }
func (emptyProgressFS) CopyFile(string, string, os.FileMode) error  { return os.ErrPermission }

type blockingReadDirFS struct {
	emptyProgressFS
	blocked           atomic.Bool
	secondReadStarted chan struct{}
	releaseSecondRead chan struct{}
}

func (filesystem *blockingReadDirFS) ReadDir(string) ([]os.DirEntry, error) {
	return fs.ReadDir(fstest.MapFS{"host": &fstest.MapFile{Mode: fs.ModeDir}}, ".")
}

// Block the metadata lookup of the SECOND session, named by its path. A call
// ordinal cannot name it: one session's lookup probes both the flat and the
// nested layout, so the number of probes per session is incidental.
func (filesystem *blockingReadDirFS) Stat(path string) (os.FileInfo, error) {
	if strings.Contains(path, "session-two") && filesystem.blocked.CompareAndSwap(false, true) {
		close(filesystem.secondReadStarted)
		<-filesystem.releaseSecondRead
	}
	return nil, os.ErrNotExist
}

// --- scripted and stub decorators (capability none) ------------------------

// swapAfterTranscriptReadFS serves the first generation until the first
// transcript read completes, then installs the second generation. The first
// transcript read of a fallback run is the fallback's own pair read, so the
// swap lands after the fallback capture and before the index capture, the
// way a concurrent writer landing between the two reads does.
type swapAfterTranscriptReadFS struct {
	FileSystem
	mu             sync.Mutex
	transcriptPath string
	metadataPath   string
	nextMetadata   []byte
	nextTranscript []byte
	hookFired      bool
}

var _ FileSystem = (*swapAfterTranscriptReadFS)(nil)

func (filesystem *swapAfterTranscriptReadFS) ReadFile(path string) ([]byte, error) {
	data, err := filesystem.FileSystem.ReadFile(path)
	if err == nil && path == filesystem.transcriptPath {
		filesystem.mu.Lock()
		defer filesystem.mu.Unlock()
		if !filesystem.hookFired {
			if err := filesystem.FileSystem.WriteFile(filesystem.metadataPath, filesystem.nextMetadata, 0o600); err != nil {
				return nil, err
			}
			if err := filesystem.FileSystem.WriteFile(filesystem.transcriptPath, filesystem.nextTranscript, 0o600); err != nil {
				return nil, err
			}
			filesystem.hookFired = true
		}
		return data, nil
	}
	return data, err
}

func (filesystem *swapAfterTranscriptReadFS) fired() bool {
	filesystem.mu.Lock()
	defer filesystem.mu.Unlock()
	return filesystem.hookFired
}

// stubStatFS implements FileSystem with only Stat wired (used by
// CursorAdapter.dirExists).
var _ FileSystem = (*stubStatFS)(nil)

type stubStatFS struct {
	dirs map[string]bool
}

func (s *stubStatFS) ReadFile(string) ([]byte, error) { panic("stubStatFS: ReadFile") }
func (s *stubStatFS) WriteFile(string, []byte, os.FileMode) error {
	panic("stubStatFS: WriteFile")
}
func (s *stubStatFS) Stat(path string) (os.FileInfo, error) {
	if s.dirs[path] {
		return stubDirInfo{}, nil
	}
	return nil, os.ErrNotExist
}
func (s *stubStatFS) Lstat(path string) (os.FileInfo, error)     { return s.Stat(path) }
func (s *stubStatFS) MkdirAll(string, os.FileMode) error         { panic("stubStatFS: MkdirAll") }
func (s *stubStatFS) ReadDir(string) ([]os.DirEntry, error)      { panic("stubStatFS: ReadDir") }
func (s *stubStatFS) Rename(string, string) error                { panic("stubStatFS: Rename") }
func (s *stubStatFS) Remove(string) error                        { panic("stubStatFS: Remove") }
func (s *stubStatFS) RemoveAll(string) error                     { panic("stubStatFS: RemoveAll") }
func (s *stubStatFS) WalkDir(string, fs.WalkDirFunc) error       { panic("stubStatFS: WalkDir") }
func (s *stubStatFS) CopyFile(string, string, os.FileMode) error { panic("stubStatFS: CopyFile") }

// stubDirInfo is a minimal os.FileInfo that reports IsDir() == true.
type stubDirInfo struct{}

func (stubDirInfo) Name() string       { return "" }
func (stubDirInfo) Size() int64        { return 0 }
func (stubDirInfo) Mode() os.FileMode  { return os.ModeDir | 0755 }
func (stubDirInfo) ModTime() time.Time { return time.Time{} }
func (stubDirInfo) IsDir() bool        { return true }
func (stubDirInfo) Sys() any           { return nil }

// Observe the existing retained fixture at the filesystem boundary, before any
// installed file changes, rather than merely checking eventual index success.
type preparedRetainedFS struct {
	*OSFileSystem
	store *serialIndexStore
	sid   SessionID
}

var _ FileSystem = (*preparedRetainedFS)(nil)

func (f *preparedRetainedFS) Rename(src, dst string) error {
	state, err := f.store.ReadIndexState(context.Background(), f.sid)
	if err != nil {
		return err
	}
	if state.IndexedInputHash != nil {
		return fmt.Errorf("retained publication renamed a file before preparation")
	}
	return f.OSFileSystem.Rename(src, dst)
}

type openCodeSQLiteFreshnessFS struct {
	FileSystem
	databasePath string
	testCase     openCodeSQLiteFreshnessCase
	statPaths    []string
}

func (filesystem *openCodeSQLiteFreshnessFS) Stat(path string) (os.FileInfo, error) {
	filesystem.statPaths = append(filesystem.statPaths, path)
	switch path {
	case filesystem.databasePath:
		return openCodeSQLiteFreshnessInfo{modified: time.UnixMilli(filesystem.testCase.DatabaseMTimeMs), size: 4096}, nil
	case filesystem.databasePath + "-wal":
		switch filesystem.testCase.WALState {
		case openCodeSQLiteWALPresent:
			return openCodeSQLiteFreshnessInfo{modified: time.UnixMilli(filesystem.testCase.WALMTimeMs), size: filesystem.testCase.WALSizeBytes}, nil
		case openCodeSQLiteWALMissing:
			return nil, fs.ErrNotExist
		case openCodeSQLiteWALError:
			return nil, errors.New("synthetic WAL stat denial")
		}
	case filesystem.databasePath + "-shm":
		return openCodeSQLiteFreshnessInfo{modified: time.UnixMilli(filesystem.testCase.SHMMTimeMs), size: 4096}, nil
	}
	return nil, fs.ErrNotExist
}

type openCodeSQLiteFreshnessInfo struct {
	modified time.Time
	size     int64
}

func (info openCodeSQLiteFreshnessInfo) Name() string       { return "synthetic" }
func (info openCodeSQLiteFreshnessInfo) Size() int64        { return info.size }
func (info openCodeSQLiteFreshnessInfo) Mode() os.FileMode  { return 0o600 }
func (info openCodeSQLiteFreshnessInfo) ModTime() time.Time { return info.modified }
func (info openCodeSQLiteFreshnessInfo) IsDir() bool        { return false }
func (info openCodeSQLiteFreshnessInfo) Sys() any           { return nil }
