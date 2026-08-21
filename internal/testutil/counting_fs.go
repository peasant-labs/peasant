package testutil

import (
	"os"
	"sync"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// CountingFS wraps MemFS and counts how often a test reads the CONTENT of a
// file. Discovery caches let a second run answer from the store instead of the
// file, and the read count is how a test proves that the second run did not
// touch the transcripts again. Directory walks and stat calls are not counted:
// discovery must still walk and stat to find the files.
type CountingFS struct {
	*MemFS

	mu     sync.Mutex
	counts map[string]int
}

var _ ingest.FileSystem = (*CountingFS)(nil)

// NewCountingFS wraps an existing MemFS.
func NewCountingFS(inner *MemFS) *CountingFS {
	return &CountingFS{MemFS: inner, counts: make(map[string]int)}
}

// ReadFile counts the read and passes it to the wrapped filesystem.
func (c *CountingFS) ReadFile(path string) ([]byte, error) {
	c.mu.Lock()
	c.counts[path]++
	c.mu.Unlock()
	return c.MemFS.ReadFile(path)
}

// ReadCount returns how often the test read one path.
func (c *CountingFS) ReadCount(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[path]
}

// TotalReads returns how often the test read any path.
func (c *CountingFS) TotalReads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, count := range c.counts {
		total += count
	}
	return total
}

// ResetCounts clears every count, so a test can measure one run on its own.
func (c *CountingFS) ResetCounts() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts = make(map[string]int)
}

// WriteFile writes through to the wrapped filesystem. Writes are not counted.
func (c *CountingFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	return c.MemFS.WriteFile(path, data, perm)
}
