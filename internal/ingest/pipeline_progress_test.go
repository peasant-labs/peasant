package ingest

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/salt"
)

func TestPipelineDiffProgressAdvancesBeforeSlowSecondSession(t *testing.T) {
	progress := NewProgressState()
	progress.Update(ProgressEvent{Kind: KindStart, Stage: StageDiff, Total: 2})
	secondReadStarted := make(chan struct{})
	releaseSecondRead := make(chan struct{})
	filesystem := &blockingReadDirFS{
		secondReadStarted: secondReadStarted,
		releaseSecondRead: releaseSecondRead,
	}
	pipeline := &Pipeline{
		fs: filesystem,
		config: PipelineConfig{
			OutputDir: ResolvedPath("/out"),
		},
	}
	sessions := []DiscoveredSession{
		{SessionID: "session-one", Harness: HarnessClaudeCode},
		{SessionID: "session-two", Harness: HarnessClaudeCode},
	}

	done := make(chan struct{})
	go func() {
		pipeline.diff(sessions, progress)
		close(done)
	}()

	select {
	case <-secondReadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("DIFF did not reach the controlled second-session metadata lookup")
	}

	if got := progress.Snapshot()[StageDiff].Done; got != 1 {
		t.Fatalf("DIFF progress while second session is blocked = %d, want 1", got)
	}
	close(releaseSecondRead)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("DIFF did not finish after releasing the blocked metadata lookup")
	}
}

func TestPipelineFilterProgressDoesNotEndBeforeSlowFilterReturns(t *testing.T) {
	progress := NewProgressState()
	secondFilterStarted := make(chan struct{})
	releaseSecondFilter := make(chan struct{})
	var filterCalls atomic.Int64
	sessions := []DiscoveredSession{
		{SessionID: "session-one", Harness: HarnessClaudeCode},
		{SessionID: "session-two", Harness: HarnessClaudeCode},
	}
	pipeline := &Pipeline{
		fs: emptyProgressFS{},
		adapters: map[Harness]AdapterFactory{
			HarnessClaudeCode: func(FileSystem, GitResolver, salt.Salt) SourceAdapter {
				return progressAdapter{sessions: sessions}
			},
		},
		config: PipelineConfig{
			Sources: map[Harness]SourceConfig{
				HarnessClaudeCode: {Enabled: true},
			},
			OutputDir: ResolvedPath("/out"),
			DryRun:    true,
			Progress:  progress,
			SessionFilter: func(DiscoveredSession) bool {
				if filterCalls.Add(1) == 2 {
					close(secondFilterStarted)
					<-releaseSecondFilter
				}
				return false
			},
		},
	}

	done := make(chan error, 1)
	go func() {
		_, err := pipeline.Run(context.Background())
		done <- err
	}()

	select {
	case <-secondFilterStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("FILTER did not reach the controlled second-session callback")
	}

	filterProgress := progress.Snapshot()[StageFilter]
	if filterProgress.Done != 1 {
		t.Fatalf("FILTER progress while second session is blocked = %d, want 1", filterProgress.Done)
	}
	if filterProgress.Ended {
		t.Fatal("FILTER progress ended before the blocked filter callback returned")
	}
	close(releaseSecondFilter)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pipeline run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline did not finish after releasing the blocked filter callback")
	}
}

type progressAdapter struct {
	sessions []DiscoveredSession
}

func (adapter progressAdapter) Harness() Harness { return HarnessClaudeCode }

func (adapter progressAdapter) Discover(context.Context, SourceConfig) ([]DiscoveredSession, error) {
	return adapter.sessions, nil
}

func (adapter progressAdapter) ExtractMetadata(context.Context, DiscoveredSession) (*UnifiedMetadata, error) {
	return nil, errors.New("progressAdapter: ExtractMetadata should not run in this dry-run test")
}

type emptyProgressFS struct{}

func (emptyProgressFS) ReadFile(string) ([]byte, error) { return nil, os.ErrNotExist }

func (emptyProgressFS) WriteFile(string, []byte, os.FileMode) error { return os.ErrPermission }

func (emptyProgressFS) MkdirAll(string, os.FileMode) error { return os.ErrPermission }

func (emptyProgressFS) Stat(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

func (emptyProgressFS) Lstat(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

func (emptyProgressFS) WalkDir(string, fs.WalkDirFunc) error { return os.ErrNotExist }

func (emptyProgressFS) Rename(string, string) error { return os.ErrPermission }

func (emptyProgressFS) ReadDir(string) ([]os.DirEntry, error) { return nil, os.ErrNotExist }

func (emptyProgressFS) Remove(string) error { return os.ErrPermission }

func (emptyProgressFS) RemoveAll(string) error { return os.ErrPermission }

func (emptyProgressFS) CopyFile(string, string, os.FileMode) error { return os.ErrPermission }

type blockingReadDirFS struct {
	emptyProgressFS
	readDirCalls      atomic.Int64
	secondReadStarted chan struct{}
	releaseSecondRead chan struct{}
}

func (filesystem *blockingReadDirFS) ReadDir(string) ([]os.DirEntry, error) {
	if filesystem.readDirCalls.Add(1) == 2 {
		close(filesystem.secondReadStarted)
		<-filesystem.releaseSecondRead
	}
	return nil, os.ErrNotExist
}
