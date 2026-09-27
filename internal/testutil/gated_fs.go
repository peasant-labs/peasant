package testutil

import (
	"io/fs"
	"os"
	"sync"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testkit/fsdecorator"
)

// GatedFS wraps a MemFS and holds one operation on one path until the test
// releases it. It is the reusable form of the "block a writer mid-install and
// read the torn state" decorator: the held call closes Reached before it
// reaches the inner filesystem, then returns only after Release.
//
// One gate is active at a time. The gate matches on the operation name and
// exactly on the path; Rename and CopyFile are matched against their
// DESTINATION, the installed file a test asks about, as CountingFS counts them.
// An empty Gate.Path matches any path for that operation. A gate is consumed by
// the first matching call, so a second matching call passes through.
type GatedFS struct {
	*MemFS

	mu       sync.Mutex
	gate     *fsdecorator.Gate
	reached  chan struct{}
	release  chan struct{}
	released bool
	consumed bool
}

var (
	_ fsdecorator.GatedFS = (*GatedFS)(nil)
	_ ingest.FileSystem   = (*GatedFS)(nil)
)

// NewGatedFS wraps an existing MemFS.
func NewGatedFS(inner *MemFS) *GatedFS {
	return &GatedFS{MemFS: inner}
}

// Arm installs, or replaces, the active gate.
func (g *GatedFS) Arm(gate fsdecorator.Gate) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gate = &gate
	g.reached = make(chan struct{})
	g.release = make(chan struct{})
	g.released = false
	g.consumed = false
}

// Reached is closed once the armed operation is entered, and is nil when no
// gate is armed.
func (g *GatedFS) Reached() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reached
}

// Release unblocks the held operation. It is idempotent.
func (g *GatedFS) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.release != nil && !g.released {
		g.released = true
		close(g.release)
	}
}

// hold blocks the caller when op on path matches the armed gate, after closing
// the reached channel exactly once.
func (g *GatedFS) hold(op fsdecorator.Op, path string) {
	g.mu.Lock()
	if g.gate == nil || g.consumed || g.gate.Op != op || (g.gate.Path != "" && g.gate.Path != path) {
		g.mu.Unlock()
		return
	}
	g.consumed = true
	reached, release := g.reached, g.release
	close(reached)
	g.mu.Unlock()
	<-release
}

func (g *GatedFS) ReadFile(path string) ([]byte, error) {
	g.hold(fsdecorator.OpReadFile, path)
	return g.MemFS.ReadFile(path)
}

func (g *GatedFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	g.hold(fsdecorator.OpWriteFile, path)
	return g.MemFS.WriteFile(path, data, perm)
}

func (g *GatedFS) MkdirAll(path string, perm os.FileMode) error {
	g.hold(fsdecorator.OpMkdirAll, path)
	return g.MemFS.MkdirAll(path, perm)
}

func (g *GatedFS) Stat(path string) (os.FileInfo, error) {
	g.hold(fsdecorator.OpStat, path)
	return g.MemFS.Stat(path)
}

func (g *GatedFS) Lstat(path string) (os.FileInfo, error) {
	g.hold(fsdecorator.OpLstat, path)
	return g.MemFS.Lstat(path)
}

func (g *GatedFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	g.hold(fsdecorator.OpWalkDir, root)
	return g.MemFS.WalkDir(root, fn)
}

func (g *GatedFS) Rename(oldpath, newpath string) error {
	g.hold(fsdecorator.OpRename, newpath)
	return g.MemFS.Rename(oldpath, newpath)
}

func (g *GatedFS) ReadDir(path string) ([]os.DirEntry, error) {
	g.hold(fsdecorator.OpReadDir, path)
	return g.MemFS.ReadDir(path)
}

func (g *GatedFS) Remove(path string) error {
	g.hold(fsdecorator.OpRemove, path)
	return g.MemFS.Remove(path)
}

func (g *GatedFS) RemoveAll(path string) error {
	g.hold(fsdecorator.OpRemoveAll, path)
	return g.MemFS.RemoveAll(path)
}

func (g *GatedFS) CopyFile(src, dst string, perm os.FileMode) error {
	g.hold(fsdecorator.OpCopyFile, dst)
	return g.MemFS.CopyFile(src, dst, perm)
}
