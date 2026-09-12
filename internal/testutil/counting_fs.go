package testutil

import (
	"io/fs"
	"os"
	"strings"
	"sync"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// FSOp names one FileSystem operation CountingFS counts. It is a closed set:
// a test asks for a count by operation, and an operation this type does not
// name is one the filesystem cannot have counted.
type FSOp string

const (
	FSOpReadFile  FSOp = "ReadFile"
	FSOpWriteFile FSOp = "WriteFile"
	FSOpMkdirAll  FSOp = "MkdirAll"
	FSOpStat      FSOp = "Stat"
	FSOpLstat     FSOp = "Lstat"
	FSOpWalkDir   FSOp = "WalkDir"
	FSOpRename    FSOp = "Rename"
	FSOpReadDir   FSOp = "ReadDir"
	FSOpRemove    FSOp = "Remove"
	FSOpRemoveAll FSOp = "RemoveAll"
	FSOpCopyFile  FSOp = "CopyFile"
)

// AllFSOps lists every counted operation, so a fixture that names one can be
// checked against the closed set.
var AllFSOps = []FSOp{FSOpReadFile, FSOpWriteFile, FSOpMkdirAll, FSOpStat, FSOpLstat, FSOpWalkDir, FSOpRename, FSOpReadDir, FSOpRemove, FSOpRemoveAll, FSOpCopyFile}

// NewFSOp converts a fixture name into an operation, refusing anything outside
// the closed set instead of counting nothing under an unknown name.
func NewFSOp(name string) (FSOp, bool) {
	for _, op := range AllFSOps {
		if string(op) == name {
			return op, true
		}
	}
	return "", false
}

type fsFault struct {
	op   FSOp
	path string
}

// CountingFS wraps MemFS and counts every FileSystem operation by path. A
// Rename or CopyFile is counted against its DESTINATION, because that is the
// installed file a test asks about; every other operation is counted against
// the path it was given. ReadFile also records the bytes it returned, so a
// test can prove that a run read no transcript at all rather than only that
// it opened none.
//
// A test can also plant a fault: the next call of one operation on one path
// fails with the planted error, which is how an install-order fixture proves
// what a failed rename leaves behind.
type CountingFS struct {
	*MemFS

	mu     sync.Mutex
	counts map[FSOp]map[string]int
	bytes  map[string]int64
	faults map[fsFault]error
}

var _ ingest.FileSystem = (*CountingFS)(nil)

// NewCountingFS wraps an existing MemFS.
func NewCountingFS(inner *MemFS) *CountingFS {
	return &CountingFS{MemFS: inner, counts: make(map[FSOp]map[string]int), bytes: make(map[string]int64), faults: make(map[fsFault]error)}
}

func (c *CountingFS) record(op FSOp, path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[op] == nil {
		c.counts[op] = make(map[string]int)
	}
	c.counts[op][path]++
	if err, planted := c.faults[fsFault{op: op, path: path}]; planted {
		return err
	}
	return nil
}

// Fail plants an error for every later call of op on path until ClearFaults.
func (c *CountingFS) Fail(op FSOp, path string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.faults[fsFault{op: op, path: path}] = err
}

// ClearFaults removes every planted fault.
func (c *CountingFS) ClearFaults() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.faults = make(map[fsFault]error)
}

// Count returns how often op ran on exactly path.
func (c *CountingFS) Count(op FSOp, path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[op][path]
}

// CountUnder returns how often op ran on prefix itself or on any path below it.
func (c *CountingFS) CountUnder(op FSOp, prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for path, count := range c.counts[op] {
		if pathUnder(path, prefix) {
			total += count
		}
	}
	return total
}

// CountSuffixUnder returns how often op ran on a path below prefix whose base
// name ends in suffix, which is how a test counts the reads of every retained
// transcript or metadata file in a tree.
func (c *CountingFS) CountSuffixUnder(op FSOp, prefix, suffix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for path, count := range c.counts[op] {
		if pathUnder(path, prefix) && strings.HasSuffix(path, suffix) {
			total += count
		}
	}
	return total
}

// Total returns how often op ran on any path.
func (c *CountingFS) Total(op FSOp) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, count := range c.counts[op] {
		total += count
	}
	return total
}

// BytesReadUnder returns how many bytes ReadFile returned for paths below
// prefix whose base name ends in suffix; an empty suffix matches every path.
func (c *CountingFS) BytesReadUnder(prefix, suffix string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total int64
	for path, n := range c.bytes {
		if pathUnder(path, prefix) && strings.HasSuffix(path, suffix) {
			total += n
		}
	}
	return total
}

func pathUnder(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/")
}

// ReadCount returns how often the test read the content of one path.
func (c *CountingFS) ReadCount(path string) int { return c.Count(FSOpReadFile, path) }

// TotalReads returns how often the test read the content of any path.
func (c *CountingFS) TotalReads() int { return c.Total(FSOpReadFile) }

// ResetCounts clears every count and byte tally, so a test can measure one
// run on its own. Planted faults stay planted.
func (c *CountingFS) ResetCounts() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts = make(map[FSOp]map[string]int)
	c.bytes = make(map[string]int64)
}

func (c *CountingFS) ReadFile(path string) ([]byte, error) {
	if err := c.record(FSOpReadFile, path); err != nil {
		return nil, err
	}
	data, err := c.MemFS.ReadFile(path)
	if err == nil {
		c.mu.Lock()
		c.bytes[path] += int64(len(data))
		c.mu.Unlock()
	}
	return data, err
}

func (c *CountingFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	if err := c.record(FSOpWriteFile, path); err != nil {
		return err
	}
	return c.MemFS.WriteFile(path, data, perm)
}

func (c *CountingFS) MkdirAll(path string, perm os.FileMode) error {
	if err := c.record(FSOpMkdirAll, path); err != nil {
		return err
	}
	return c.MemFS.MkdirAll(path, perm)
}

func (c *CountingFS) Stat(path string) (os.FileInfo, error) {
	if err := c.record(FSOpStat, path); err != nil {
		return nil, err
	}
	return c.MemFS.Stat(path)
}

func (c *CountingFS) Lstat(path string) (os.FileInfo, error) {
	if err := c.record(FSOpLstat, path); err != nil {
		return nil, err
	}
	return c.MemFS.Lstat(path)
}

func (c *CountingFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	if err := c.record(FSOpWalkDir, root); err != nil {
		return err
	}
	return c.MemFS.WalkDir(root, fn)
}

func (c *CountingFS) Rename(oldpath, newpath string) error {
	if err := c.record(FSOpRename, newpath); err != nil {
		return err
	}
	return c.MemFS.Rename(oldpath, newpath)
}

func (c *CountingFS) ReadDir(path string) ([]os.DirEntry, error) {
	if err := c.record(FSOpReadDir, path); err != nil {
		return nil, err
	}
	return c.MemFS.ReadDir(path)
}

func (c *CountingFS) Remove(path string) error {
	if err := c.record(FSOpRemove, path); err != nil {
		return err
	}
	return c.MemFS.Remove(path)
}

func (c *CountingFS) RemoveAll(path string) error {
	if err := c.record(FSOpRemoveAll, path); err != nil {
		return err
	}
	return c.MemFS.RemoveAll(path)
}

func (c *CountingFS) CopyFile(src, dst string, perm os.FileMode) error {
	if err := c.record(FSOpCopyFile, dst); err != nil {
		return err
	}
	return c.MemFS.CopyFile(src, dst, perm)
}
