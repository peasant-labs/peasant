//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/minio/minio-go/v7"
)

// TestTranscriptBucketObjectCountMatchesKnownPuts is the focused regression for the
// combined-output counter regression. It provisions ONLY MinIO (no Postgres/village), then:
//
//   - asserts an empty bucket counts EXACTLY 0 — the symmetric guard against the
//     original OVER-count (CI podman 4.9.3 stderr inflated an empty bucket to 8);
//   - PUTs K known objects via the in-process minio-go client and asserts the count
//     equals EXACTLY K — guarding BOTH the over-count AND an always-0 UNDER-count
//     degenerate (wrong bucket / undrained channel) that an empty==0 check alone
//     would falsely pass.
//
// K is DERIVED from the actual successful PutObject results — never hardcoded (the
// hardcoded "8" coincidence is exactly what misled the original diagnosis). The
// count comes from typed ObjectInfo, so it is structurally immune to any stderr a
// container runtime emits; there is no env-dependent RED toggle here (on local
// podman 5.8.2 the OLD mc path was ALSO green — a 5.8.2 "RED demo" would falsely
// pass). The meaningful pre-fix RED proof is the baseline canary run under CI-shape
// podman 4.9.3 (see docs/e2e.md one-command repro).
func TestTranscriptBucketObjectCountMatchesKnownPuts(t *testing.T) {
	reapStaleE2EInfra(t)

	bucket := uniqueName("transcripts")
	endpoint := startEphemeralMinIO(t, bucket)

	// Empty bucket must count EXACTLY 0 — never the stderr-inflated over-count.
	if got := transcriptBucketObjectCount(t, endpoint, bucket); got != 0 {
		t.Fatalf("empty transcript bucket count = %d, want 0 (stderr/over-count regression)", got)
	}

	client, err := newMinioClient(endpoint)
	if err != nil {
		t.Fatalf("build minio client for %s: %v", endpoint, err)
	}

	// Representative transcript-like object layout (hostSlug/sessionId/file). The
	// exact keys do not matter; K is derived from how many PUTs actually succeed.
	keys := []string{
		"host-a/sess-0001/sess-0001--transcript.jsonl",
		"host-a/sess-0001/sess-0001--metadata.json",
		"host-a/sess-0002/sess-0002--transcript.jsonl",
		"host-b/sess-0003/sess-0003--transcript.jsonl",
		"host-b/sess-0003/sess-0003--metadata.json",
	}
	wantK := 0
	for _, key := range keys {
		ctx, cancel := context.WithTimeout(context.Background(), s3OpTimeout)
		body := []byte(fmt.Sprintf("e2e count fixture for %s\n", key))
		_, err := client.PutObject(ctx, bucket, key, bytes.NewReader(body), int64(len(body)),
			minio.PutObjectOptions{ContentType: "application/octet-stream"})
		cancel()
		if err != nil {
			t.Fatalf("put object %q into %s: %v", key, bucket, err)
		}
		wantK++
	}
	if wantK == 0 {
		t.Fatal("no objects were PUT; cannot derive K")
	}

	if got := transcriptBucketObjectCount(t, endpoint, bucket); got != wantK {
		t.Fatalf("transcript bucket count after %d known PUTs = %d, want EXACTLY %d", wantK, got, wantK)
	}
}
