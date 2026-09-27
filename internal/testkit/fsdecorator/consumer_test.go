// Package fsdecorator_test is an external consumer of the decorator contract:
// it implements and uses the capability interfaces with only exported names,
// and proves a decorated filesystem is assignable to ingest.FileSystem.
package fsdecorator_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testkit/fsdecorator"
)

// consumerGate is a decorator built outside the contract package: it embeds the
// mirrored base and adds the three control methods.
type consumerGate struct{ fsdecorator.FileSystem }

func (consumerGate) Arm(fsdecorator.Gate)     {}
func (consumerGate) Reached() <-chan struct{} { return nil }
func (consumerGate) Release()                 {}

var (
	_ fsdecorator.GatedFS   = consumerGate{}
	_ fsdecorator.BoundedFS = consumerBounded{}
	// A decorator held as the capability interface is an ingest.FileSystem.
	_ ingest.FileSystem = fsdecorator.GatedFS(consumerGate{})
)

type consumerBounded struct{ fsdecorator.FileSystem }

func (consumerBounded) Limit(fsdecorator.Bound) {}
func (consumerBounded) ReadOnly()               {}

func driveGated(fs fsdecorator.GatedFS) {
	fs.Arm(fsdecorator.Gate{Op: fsdecorator.OpWriteFile, Path: "/tmp/example"})
	fs.Release()
}

func driveBounded(fs fsdecorator.BoundedFS) {
	fs.Limit(fsdecorator.Bound{Op: fsdecorator.OpReadFile, Path: "/tmp/example", MaxBytes: 64})
	fs.ReadOnly()
}

func TestConsumer_DecoratorContractCompilesAndRuns(t *testing.T) {
	driveGated(consumerGate{})
	driveBounded(consumerBounded{})
	if _, ok := fsdecorator.NewOp("ReadFile"); !ok {
		t.Fatal("ReadFile must be in the operation closed set")
	}
	if !fsdecorator.OpReadFile.Valid() {
		t.Fatal("OpReadFile must be valid")
	}
}
