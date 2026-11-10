package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/peasant-labs/schema"
)

// TestCapturedSourceSettlesOnlyWhenTheAdapterStampIsCurrent pins the captured
// classifier, not a harvest: identical bytes classify as updated while the
// stored adapter revision is behind this build's target, and unchanged once it
// matches. That status is what decides whether the worker reaches the write
// that stamps the revision; this test does not open a source or read a stamp
// back, and the maintenance write path already asserts the stamp itself.
func TestCapturedSourceSettlesOnlyWhenTheAdapterStampIsCurrent(t *testing.T) {
	sessionID, err := NewSessionID("ses_3cd91f52effeXd3QAJ54jStmp")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := []byte("captured-source-fingerprint")
	ingested := time.Now().Add(-time.Hour).UnixMilli()
	target := GenerationTargets(HarvesterVersionRegistry)[HarnessOpenCode].AdapterVersion

	for _, testCase := range []struct {
		name           string
		storedRevision int
		want           DiffStatus
	}{
		{name: "behind-target-republishes", storedRevision: target - 1, want: DiffUpdated},
		{name: "current-target-settles", storedRevision: target, want: DiffUnchanged},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			outputDir, err := NewResolvedPath(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			pipeline := &Pipeline{
				fs:            &OSFileSystem{},
				config:        PipelineConfig{OutputDir: outputDir},
				locationCache: make(map[SessionID]SessionLocation, 1),
			}
			pipeline.resolvedVersions = GenerationTargets(HarvesterVersionRegistry)
			stored := testCase.storedRevision
			pipeline.locationCache[sessionID] = SessionLocation{
				IngestedMs:              &ingested,
				SchemaVersion:           CurrentSchemaVersion,
				AdapterVersion:          &stored,
				SourceEvidenceSupported: true,
				SourceFingerprint:       fingerprint,
				PublicationReadiness:    PublicationReady,
			}
			session := DiscoveredSession{
				SessionID:        sessionID,
				Harness:          HarnessOpenCode,
				TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite,
				SourceFormat:     SourceFormatJSON,
			}
			captured := &MaterializedTranscript{
				Metadata:          &UnifiedMetadata{SessionID: schema.SessionID(string(sessionID))},
				Data:              []byte("{}\n"),
				SourceFingerprint: fingerprint,
			}

			status, err := pipeline.classifyCapturedSession(context.Background(), session, captured)
			if err != nil {
				t.Fatalf("classify captured source: %v", err)
			}
			if status != testCase.want {
				t.Fatalf("status = %v, want %v", status, testCase.want)
			}
		})
	}
}
