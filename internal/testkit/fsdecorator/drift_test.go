// Package fsdecorator_test holds the drift guards that need to see both this
// package and internal/testutil. They live in an external test package because
// internal/testutil now imports internal/testkit/fsdecorator to implement the shared
// GatedFS/BoundedFS; an internal fsdecorator test that imported testutil would
// close an import cycle.
package fsdecorator_test

import (
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/fsdecorator"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// The three shared decorators conform to the frozen contract and to the
// production filesystem interface.
var (
	_ fsdecorator.FileSystem = (*testutil.CountingFS)(nil)
	_ fsdecorator.GatedFS    = (*testutil.GatedFS)(nil)
	_ fsdecorator.BoundedFS  = (*testutil.BoundedFS)(nil)
)

// TestOpVocabularyMatchesTestutil pins the canonical operation set to the one
// CountingFS already uses, so migrating FSOp onto Op cannot silently change the
// vocabulary in either direction.
func TestOpVocabularyMatchesTestutil(t *testing.T) {
	want := make([]string, 0, len(testutil.AllFSOps))
	for _, op := range testutil.AllFSOps {
		want = append(want, string(op))
	}
	got := make([]string, 0, len(fsdecorator.AllOps))
	for _, op := range fsdecorator.AllOps {
		got = append(got, string(op))
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("the operation vocabulary drifted from testutil:\n  got  %v\n  want %v", got, want)
	}
}
