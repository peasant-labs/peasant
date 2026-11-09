package ingest_test

// A cancelled discovery must stop, and say so. The walk names the files, then
// the shared build performs every stat and read discovery needs, so a cancel
// that arrives after enumeration must surface as context.Canceled instead of
// being swallowed through the rest of the tree.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// strikeCancelingFS cancels a discovery's context at one chosen operation.
type strikeCancelingFS struct {
	*testutil.MemFS
	cancel context.CancelFunc
	fired  bool
	// cancelOnWalk cancels while the walk visits the first transcript. Otherwise
	// it cancels on the first sidecar stat, which the build performs only after
	// enumeration has completed.
	cancelOnWalk bool
}

func (f *strikeCancelingFS) fire() {
	if f.fired {
		return
	}
	f.fired = true
	f.cancel()
}

func (f *strikeCancelingFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	return f.MemFS.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if f.cancelOnWalk && strings.HasSuffix(path, ".jsonl") {
			f.fire()
		}
		return fn(path, d, err)
	})
}

func (f *strikeCancelingFS) Stat(path string) (os.FileInfo, error) {
	if !f.cancelOnWalk && strings.HasSuffix(path, ".meta.json") {
		f.fire()
	}
	return f.MemFS.Stat(path)
}

func TestStrikeDiscoveryHonorsCancellation(t *testing.T) {
	t.Parallel()
	const (
		first  = "20260728T123456.123456789Z-AAAAAAAAAAAAAAAAAAAAAAAAAA"
		second = "20260728T130000.000000001Z-BBBBBBBBBBBBBBBBBBBBBBBBBB"
	)
	mfs := testutil.NewMemFS()
	for _, id := range []string{first, second} {
		path := filepath.Join("/sessions", id)
		if err := mfs.WriteFile(path+".jsonl", []byte(`{"type":"user.message","time":"2026-11-08T09:00:01Z","data":{"sessionId":"`+id+`","text":"fixture turn"}}`+"\n"), 0o600); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
		if err := mfs.WriteFile(path+".meta.json", []byte(`{"sessionId":"`+id+`"}`), 0o600); err != nil {
			t.Fatalf("write sidecar: %v", err)
		}
	}
	root, err := ingest.NewResolvedPath("/sessions")
	if err != nil {
		t.Fatalf("resolve source root: %v", err)
	}
	cfg := ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}}

	for _, testCase := range []struct {
		name         string
		cancelOnWalk bool
	}{
		// The walk itself must stop: a cancelled enumeration cannot be read as
		// sessions that no longer exist.
		{name: "cancel-during-walk", cancelOnWalk: true},
		// The build must stop too. Its first sidecar stat runs only after
		// enumeration finished, so this is the case a build that ignored the
		// context would swallow.
		{name: "cancel-after-enumeration", cancelOnWalk: false},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			canceling := &strikeCancelingFS{MemFS: mfs, cancel: cancel, cancelOnWalk: testCase.cancelOnWalk}
			adapter := ingest.NewStrikeAdapter(canceling, testutil.NoGitResolver(), salt.Salt{})

			sessions, err := adapter.Discover(ctx, cfg)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Discover error = %v, want context.Canceled; a cancel must surface instead of being swallowed by the reads after enumeration", err)
			}
			if len(sessions) != 0 {
				t.Errorf("Discover returned %d sessions with a cancelled context, want none", len(sessions))
			}
		})
	}
}
