package ingest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestPipelineArenaWrapsManyTimesAndRecordsEverySession drives the production
// ingest path with a staging arena sized to a few transcripts. Equal-sized
// transcripts and an arena that is not a whole multiple of the transcript size
// make the ring wrap repeatedly, so this is the end-to-end check that a wrap
// releases its capacity: the run completes and every session is recorded.
//
// The transcript bodies carry nothing to redact, so the staged payload is the
// planted file byte-for-byte and the wrap arithmetic is exact.
func TestPipelineArenaWrapsManyTimesAndRecordsEverySession(t *testing.T) {
	const sessionCount = 32
	const fillerBytes = 400

	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// One transcript body, reused for every session so the staged payloads are
	// all the same size.
	body := fmt.Sprintf(
		`{"type":"user","message":{"role":"user","content":%q},"timestamp":"2024-02-19T00:00:00Z"}`+"\n",
		strings.Repeat("a", fillerBytes),
	)

	sessions := make([]ingest.DiscoveredSession, 0, sessionCount)
	metadata := make(map[ingest.SessionID]*ingest.UnifiedMetadata, sessionCount)
	for i := 0; i < sessionCount; i++ {
		sessionID := fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)
		sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, sessionID)
		if err := mfs.WriteFile(sourcePath, []byte(body), 0644); err != nil {
			t.Fatalf("WriteFile(%q): %v", sourcePath, err)
		}
		session := makeDiscoveredSession(t, sessionID, sourcePath, time.Now().Add(-time.Hour))
		sessions = append(sessions, session)
		metadata[session.SessionID] = makeMinimalMeta(t, sessionID)
	}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(sessions, metadata),
	}
	cfg := makePipelineConfig(testOutputDir)

	// 3.5 transcripts: one transcript always fits, but a copy eventually
	// straddles the slab end, forcing a wrap every few copies.
	payloadBytes := int64(len(body))
	arenaBytes := 3*payloadBytes + payloadBytes/2

	pipeline, err := newTestPipeline(mfs, git, adapters, cfg, ingest.WithArenaSizeBytes(arenaBytes))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	// Run with a bound: on the unfixed accounting the ring loses capacity at
	// every wrap and the producers eventually wait forever, so the test must
	// fail rather than hang.
	type runOutcome struct {
		result *ingest.PipelineResult
		err    error
	}
	done := make(chan runOutcome, 1)
	go func() {
		result, err := pipeline.Run(context.Background())
		done <- runOutcome{result: result, err: err}
	}()

	var outcome runOutcome
	select {
	case outcome = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("ingest did not finish; the staging arena likely lost capacity at a slab wrap")
	}
	if outcome.err != nil {
		t.Fatalf("Run: %v", outcome.err)
	}
	result := outcome.result
	if result.Summary.Errors != 0 {
		for _, sr := range result.Sessions {
			if sr.Error != nil {
				t.Logf("session %s error: %v", sr.SessionID, sr.Error)
			}
		}
		t.Fatalf("Summary.Errors = %d, want 0", result.Summary.Errors)
	}
	if len(result.Sessions) != sessionCount {
		t.Fatalf("Sessions len = %d, want %d", len(result.Sessions), sessionCount)
	}

	// Every session's transcript was recorded at the planted size, which is the
	// precondition the wrap arithmetic above relies on.
	for _, session := range sessions {
		base := expectedOutputBase(testOutputDir, session.SessionID.String())
		transcriptPath := fmt.Sprintf("%s/%s--transcript.jsonl", base, session.SessionID)
		written, err := mfs.ReadFile(transcriptPath)
		if err != nil {
			t.Fatalf("session %s transcript not recorded at %q: %v", session.SessionID, transcriptPath, err)
		}
		if int64(len(written)) != payloadBytes {
			t.Fatalf("session %s recorded %d transcript bytes, want %d (equal-sized payloads are the wrap precondition)", session.SessionID, len(written), payloadBytes)
		}
	}
}
