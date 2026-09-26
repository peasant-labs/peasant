// Package fsdecorator declares the shared contract for the test filesystem
// decorators used across internal/testutil and internal/ingest.
//
// # Why this package exists, and where the declaration lives
//
// The suite has one family of test filesystem decorators that wrap an
// ingest.FileSystem to inject a fault, count an operation, hold an operation, or
// bound it. They are used from two owners:
//
//   - internal/testutil owns the non-white-box decorators, and already
//     imports internal/ingest.
//   - a white-box test file in package ingest owns the rest, and needs the
//     unexported internals of the package under test.
//
// internal/testutil imports internal/ingest, so a white-box `package ingest`
// test cannot import internal/testutil: that is an import cycle (ingest test ->
// testutil -> ingest). The shared contract therefore CANNOT live in
// internal/testutil.
//
// This package is the single declaration site. It imports only the standard
// library, so both owners can import it without a cycle: the white-box
// `package ingest` test imports fsdecorator, which imports nothing from ingest,
// and internal/testutil imports both.
//
// # Go interfaces are structural
//
// A decorator does not have to import this package to satisfy it, and a
// consumer does not have to know the concrete type. A decorator that provides
// the FileSystem method set plus the capability methods satisfies GatedFS or
// BoundedFS structurally, and a value held as GatedFS is assignable to
// ingest.FileSystem because their method sets are identical. The contract test
// pins FileSystem to ingest.FileSystem, so the two cannot drift apart silently.
//
// # The three capabilities
//
//   - CountingFS (already in internal/testutil): a path-keyed fault and count.
//   - GatedFS: a blocking gate that holds one operation on one path until the
//     test releases it.
//   - BoundedFS: a bound on one operation, including a read-only filesystem.
//
// classify.go carries the schema that records, per decorator type, which
// capability it needs and which owner implements it.
package fsdecorator

import (
	"io/fs"
	"os"
)

// FileSystem is the filesystem method set a decorator provides. It is identical
// to ingest.FileSystem; the contract test asserts the two method sets match, so
// a change to either one fails the build's tests until they are reconciled.
//
// It is declared here rather than imported because a white-box `package ingest`
// test must be able to import this package without an import cycle.
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

// Op names one filesystem operation a decorator keys on. It is a closed set: a
// decorator that wants to act on an operation this type does not name cannot,
// and a fixture that names an unknown operation is rejected rather than treated
// as a no-op.
type Op string

const (
	OpReadFile  Op = "ReadFile"
	OpWriteFile Op = "WriteFile"
	OpMkdirAll  Op = "MkdirAll"
	OpStat      Op = "Stat"
	OpLstat     Op = "Lstat"
	OpWalkDir   Op = "WalkDir"
	OpRename    Op = "Rename"
	OpReadDir   Op = "ReadDir"
	OpRemove    Op = "Remove"
	OpRemoveAll Op = "RemoveAll"
	OpCopyFile  Op = "CopyFile"
)

// AllOps lists every operation in the closed set.
var AllOps = []Op{
	OpReadFile, OpWriteFile, OpMkdirAll, OpStat, OpLstat, OpWalkDir,
	OpRename, OpReadDir, OpRemove, OpRemoveAll, OpCopyFile,
}

// NewOp converts a fixture or configuration name into an operation, refusing
// anything outside the closed set.
func NewOp(name string) (Op, bool) {
	for _, op := range AllOps {
		if string(op) == name {
			return op, true
		}
	}
	return "", false
}

// Valid reports whether o is a member of the closed set.
func (o Op) Valid() bool {
	_, ok := NewOp(string(o))
	return ok
}

// Gate names one operation on one path that a GatedFS holds. A held operation
// signals Reached before it reaches the inner filesystem and returns only after
// Release.
type Gate struct {
	Op   Op     `yaml:"op"`
	Path string `yaml:"path"`
}

// GatedFS is a FileSystem whose next matching operation is held until Release.
// It is the reusable form of the "block one writer mid-install and read the torn
// state" decorator.
type GatedFS interface {
	FileSystem
	// Arm installs (or replaces) the active gate. The next call of g.Op on
	// g.Path blocks before it reaches the inner filesystem.
	Arm(g Gate)
	// Reached is closed once the armed operation is entered. It is nil when no
	// gate is armed.
	Reached() <-chan struct{}
	// Release unblocks the held operation. It is idempotent.
	Release()
}

// Bound names one operation on one path that a BoundedFS limits or refuses.
// MaxCalls and MaxBytes are both optional; a zero MaxBytes on a read operation
// means unlimited bytes.
type Bound struct {
	Op       Op     `yaml:"op"`
	Path     string `yaml:"path"`
	MaxCalls int64  `yaml:"max_calls"`
	MaxBytes int64  `yaml:"max_bytes"`
}

// BoundedFS is a FileSystem that enforces an operation bound. ReadOnly is the
// whole-filesystem case of a bound (every mutating operation is refused).
type BoundedFS interface {
	FileSystem
	// Limit installs (or replaces) the bound for b.Op on b.Path.
	Limit(b Bound)
	// ReadOnly refuses every mutating operation: WriteFile, MkdirAll, Rename,
	// Remove, RemoveAll, CopyFile.
	ReadOnly()
}
