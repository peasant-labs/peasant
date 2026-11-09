package testutil

import (
	"io/fs"
	"os"
	"sort"
	"sync"

	"github.com/peasant-labs/peasant/internal/testkit/fsdecorator"
)

// OpRecorderFS wraps any FileSystem and records the exact paths every
// operation touched, with multiplicity. It is the recording decorator of the
// fsdecorator contract (capability "recording").
//
// It differs from CountingFS in both directions that matter to a parity test:
// it can wrap a real-filesystem implementation, not only MemFS, so an OpenCode
// case can drive a real SQLite file; and it exposes the whole recorded path set
// per operation, so a fixture can assert set equality and prove the absence of
// a WalkDir rather than checking a single count.
//
// Writes are recorded too. A source-root assertion filters by path, so an
// output-root write cannot hide a source-root read, and an unexpected write
// under the source root is visible rather than silently excluded.
type OpRecorderFS struct {
	inner fsdecorator.FileSystem

	mu     sync.Mutex
	counts map[FSOp]map[string]int
}

// NewOpRecorderFS wraps inner and returns a recorder over it.
func NewOpRecorderFS(inner fsdecorator.FileSystem) *OpRecorderFS {
	return &OpRecorderFS{inner: inner, counts: make(map[FSOp]map[string]int)}
}

// record counts one operation on one path. The caller holds no lock.
func (r *OpRecorderFS) record(op FSOp, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts[op] == nil {
		r.counts[op] = make(map[string]int)
	}
	r.counts[op][path]++
}

// Paths returns the sorted, de-duplicated paths recorded for op.
func (r *OpRecorderFS) Paths(op FSOp) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.counts[op]))
	for path := range r.counts[op] {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// Count returns how many times op ran on exactly path.
func (r *OpRecorderFS) Count(op FSOp, path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[op][path]
}

// Total returns how many times op ran on any path.
func (r *OpRecorderFS) Total(op FSOp) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, count := range r.counts[op] {
		total += count
	}
	return total
}

// Counts returns a copy of every recorded op's path counts. The copy keeps the
// recorder safe to read while a run is still writing.
func (r *OpRecorderFS) Counts() map[FSOp]map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[FSOp]map[string]int, len(r.counts))
	for op, paths := range r.counts {
		copied := make(map[string]int, len(paths))
		for path, count := range paths {
			copied[path] = count
		}
		out[op] = copied
	}
	return out
}

// Reset drops every recorded operation. A parity case resets between its seed
// phase and the run it asserts, so the seed's reads cannot be mistaken for the
// run's.
func (r *OpRecorderFS) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts = make(map[FSOp]map[string]int)
}

func (r *OpRecorderFS) ReadFile(path string) ([]byte, error) {
	r.record(FSOpReadFile, path)
	return r.inner.ReadFile(path)
}

func (r *OpRecorderFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	r.record(FSOpWriteFile, path)
	return r.inner.WriteFile(path, data, perm)
}

func (r *OpRecorderFS) MkdirAll(path string, perm os.FileMode) error {
	r.record(FSOpMkdirAll, path)
	return r.inner.MkdirAll(path, perm)
}

func (r *OpRecorderFS) Stat(path string) (os.FileInfo, error) {
	r.record(FSOpStat, path)
	return r.inner.Stat(path)
}

func (r *OpRecorderFS) Lstat(path string) (os.FileInfo, error) {
	r.record(FSOpLstat, path)
	return r.inner.Lstat(path)
}

func (r *OpRecorderFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	r.record(FSOpWalkDir, root)
	return r.inner.WalkDir(root, fn)
}

func (r *OpRecorderFS) Rename(oldpath, newpath string) error {
	r.record(FSOpRename, newpath)
	return r.inner.Rename(oldpath, newpath)
}

func (r *OpRecorderFS) ReadDir(path string) ([]os.DirEntry, error) {
	r.record(FSOpReadDir, path)
	return r.inner.ReadDir(path)
}

func (r *OpRecorderFS) Remove(path string) error {
	r.record(FSOpRemove, path)
	return r.inner.Remove(path)
}

func (r *OpRecorderFS) RemoveAll(path string) error {
	r.record(FSOpRemoveAll, path)
	return r.inner.RemoveAll(path)
}

func (r *OpRecorderFS) CopyFile(src, dst string, perm os.FileMode) error {
	r.record(FSOpCopyFile, dst)
	return r.inner.CopyFile(src, dst, perm)
}
