package ingest_test

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestConcurrentPairReadsFailClosed runs a reader against a writer
// installing successive generations transcript-first, metadata-last, the
// production install order. Every read must either see one consistent
// generation or refuse with the checksum error; nothing else may escape,
// and the reader must settle on the latest generation once the writer
// stops. A bounded retry ships only if this test cannot pass without one.
func TestConcurrentPairReadsFailClosed(t *testing.T) {
	fixture := loadPairSnapshotExtFixture(t)
	sid, err := ingest.NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	memfs := testutil.NewMemFS()
	metadataPath := ingest.SessionMetadataPath(testOutputDir, fixture.HostSlug, fixture.SessionID, "")
	transcriptPath := snapshotTranscriptPath(metadataPath, fixture.SessionID)
	const generations = 200
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
	writerDone := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		defer close(writerDone)
		for generation := 1; generation < generations; generation++ {
			metadata := buildPairSnapshotMetadata(t, fixture, transcriptFor(generation))
			if err := memfs.WriteFile(transcriptPath, []byte(transcriptFor(generation)), 0o600); err != nil {
				t.Error(err)
				return
			}
			// Widen the straddle window the production two-rename install
			// leaves between its transcript and metadata commits.
			runtime.Gosched()
			if err := memfs.WriteFile(metadataPath, metadata, 0o600); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	var clean, straddled int
	var other []string
reading:
	for {
		select {
		case <-writerDone:
			break reading
		default:
		}
		artifact, err := ingest.ReadManagedPair(memfs, testOutputDir, metadataPath, sid)
		switch {
		case err == nil:
			clean++
			_ = artifact
		case strings.Contains(err.Error(), "transcript checksum does not match committed metadata"):
			straddled++
		default:
			other = append(other, err.Error())
		}
	}
	writer.Wait()
	if len(other) != 0 {
		t.Fatalf("a concurrent pair read failed with %d unexpected errors, first: %s", len(other), other[0])
	}
	t.Logf("reader saw %d clean and %d straddled reads while the writer installed %d generations; every straddle refused with the checksum error", clean, straddled, generations)
	// Once the writer stops, the pair is stable and every read sees the
	// latest generation.
	for i := 0; i < 5; i++ {
		artifact, err := ingest.ReadManagedPair(memfs, testOutputDir, metadataPath, sid)
		if err != nil {
			t.Fatalf("a settled pair refused: %v", err)
		}
		if string(artifact.Transcript) != transcriptFor(generations-1) {
			t.Fatal("a settled pair did not serve the latest installed generation")
		}
	}
}
