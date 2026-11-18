package sourcepreview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/schema"
)

type heldRangeFS struct {
	*ingest.OSFileSystem
	reached chan struct{}
	release chan struct{}
}

var _ ingest.FileSystem = (*heldRangeFS)(nil)
var _ ingest.RangeReadableFileSystem = (*heldRangeFS)(nil)

func (fs *heldRangeFS) ReadFileRange(path string, offset, length int64) ([]byte, error) {
	close(fs.reached)
	<-fs.release
	return fs.OSFileSystem.ReadFileRange(path, offset, length)
}

type countingIndexer struct {
	ingest.TranscriptIndexer
	calls atomic.Int32
}

var _ ingest.TranscriptIndexer = (*countingIndexer)(nil)

func (i *countingIndexer) IndexTranscriptBytes(context.Context, ingest.DiscoveredSession, []byte) ([]schema.SessionEntry, error) {
	i.calls.Add(1)
	return nil, nil
}

func TestCanceledRangeReadDoesNotParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"user\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fs := &heldRangeFS{OSFileSystem: &ingest.OSFileSystem{}, reached: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(fs.release) })
	indexer := &countingIndexer{}
	capture := &Capture{
		reader:  &Reader{fs: fs},
		session: ingest.DiscoveredSession{SourcePath: ingest.ResolvedPath(path), TranscriptOrigin: ingest.TranscriptOriginFile},
		opts:    CaptureOptions{FirstReadBytes: 1024},
		slicer:  ingest.NewFileTranscriptSlicer(fs),
		ranged:  fs,
		indexer: indexer,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	wait := testwait.Context(t)
	go func() {
		_, err := capture.stepFileRanged(ctx)
		result <- err
	}()
	select {
	case <-fs.reached:
	case <-wait.Done():
		t.Fatal("range read did not start")
	}
	cancel()
	release.Do(func() { close(fs.release) })
	select {
	case err := <-result:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("read after cancellation returned %v, want closed", err)
		}
	case <-wait.Done():
		t.Fatal("range read did not finish")
	}
	if indexer.calls.Load() != 0 || len(capture.loaded) != 0 {
		t.Fatal("a cancelled range read was retained or parsed")
	}
}
