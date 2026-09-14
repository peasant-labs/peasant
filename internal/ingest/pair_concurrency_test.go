package ingest_test

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// gatedPairFS pauses a writer at the production install's transcript-first /
// metadata-last window: once the transcript write has landed it signals
// reachedWindow and waits for releaseWindow before returning, so the metadata
// rename cannot land until the test has read the torn pair in between.
type gatedPairFS struct {
	*testutil.MemFS
	transcriptPath string
	reachedWindow  chan struct{}
	releaseWindow  chan struct{}
	signalOnce     sync.Once
}

var _ ingest.FileSystem = (*gatedPairFS)(nil)

func (filesystem *gatedPairFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	if err := filesystem.MemFS.WriteFile(path, data, perm); err != nil {
		return err
	}
	if path == filesystem.transcriptPath {
		filesystem.signalOnce.Do(func() { close(filesystem.reachedWindow) })
		<-filesystem.releaseWindow
	}
	return nil
}

// TestConcurrentPairReadsFailClosed holds a writer inside the production
// install's transcript-first/metadata-last window and requires the production
// reader to attempt exactly that window: the pair must refuse with the checksum
// error while the transcript is the next generation and the metadata is still
// the previous one. Once the metadata write is released the reader must settle
// on the latest generation. A bounded retry ships only if this test cannot pass
// without one.
func TestConcurrentPairReadsFailClosed(t *testing.T) {
	fixture := loadPairSnapshotExtFixture(t)
	sid, err := ingest.NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	memfs := testutil.NewMemFS()
	metadataPath := ingest.SessionMetadataPath(testOutputDir, fixture.HostSlug, fixture.SessionID, "")
	transcriptPath := snapshotTranscriptPath(metadataPath, fixture.SessionID)
	transcriptFor := func(generation int) string {
		return fmt.Sprintf("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"concurrent generation %d\"}}\n", generation)
	}
	// Seed the first generation so the reader never meets missing files,
	// which would answer a different question than the straddle.
	if err := memfs.WriteFile(metadataPath, buildPairSnapshotMetadata(t, fixture, transcriptFor(0)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := memfs.WriteFile(transcriptPath, []byte(transcriptFor(0)), 0o600); err != nil {
		t.Fatal(err)
	}
	second := buildPairSnapshotMetadata(t, fixture, transcriptFor(1))
	third := buildPairSnapshotMetadata(t, fixture, transcriptFor(2))
	gated := &gatedPairFS{
		MemFS:          memfs,
		transcriptPath: transcriptPath,
		reachedWindow:  make(chan struct{}),
		releaseWindow:  make(chan struct{}),
	}
	writerErr := make(chan error, 1)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		var writeErr error
		// Install generation 1 transcript-first. The write returns only after
		// the test releases the window, so the metadata rename below cannot
		// land before the torn read.
		if writeErr = gated.WriteFile(transcriptPath, []byte(transcriptFor(1)), 0o600); writeErr == nil {
			if writeErr = gated.WriteFile(metadataPath, second, 0o600); writeErr == nil {
				// Then install a later generation normally, so the settled
				// pair has moved past the straddled one.
				if writeErr = gated.WriteFile(transcriptPath, []byte(transcriptFor(2)), 0o600); writeErr == nil {
					writeErr = gated.WriteFile(metadataPath, third, 0o600)
				}
			}
		}
		writerErr <- writeErr
	}()

	<-gated.reachedWindow
	// The transcript is generation 1 while the metadata still names generation
	// 0. The production reader must attempt this window and refuse it.
	_, readErr := ingest.ReadManagedPair(gated, testOutputDir, metadataPath, sid)
	if readErr == nil || !strings.Contains(readErr.Error(), "transcript checksum does not match committed metadata") {
		close(gated.releaseWindow)
		<-writerDone
		t.Fatalf("the production reader did not refuse the straddled pair it was asked to read: %v", readErr)
	}
	close(gated.releaseWindow)
	<-writerDone
	if err := <-writerErr; err != nil {
		t.Fatal(err)
	}
	// Once the writer stops, the pair is stable and every read sees the
	// latest generation.
	for i := 0; i < 5; i++ {
		artifact, err := ingest.ReadManagedPair(gated, testOutputDir, metadataPath, sid)
		if err != nil {
			t.Fatalf("a settled pair refused: %v", err)
		}
		if string(artifact.Transcript) != transcriptFor(2) {
			t.Fatal("a settled pair did not serve the latest installed generation")
		}
	}
}
