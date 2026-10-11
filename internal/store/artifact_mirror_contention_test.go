package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestArtifactMirrorWaitsForWriter exercises the production record path with
// another pooled connection holding SQLite's writer lock. A deferred read
// transaction cannot upgrade here, even with the configured busy timeout.
func TestArtifactMirrorWaitsForWriter(t *testing.T) {
	t.Parallel()
	input := loadArtifactMirrorFixtures(t)
	db := openTestStore(t)
	entry := makeStoreEntry(t, input.SessionID, input.ProjectHash, input.HostSlug, defaults.HarnessOpenCode, input.StartedAt, 100, 50)
	artifact := mirrorTestArtifact(t, entry.Metadata, input.Transcript)
	ctx, cancel := context.WithTimeout(t.Context(), defaults.SQLiteBusyTimeout)
	defer cancel()
	writer := takeConn(t, db.Pool())
	defer db.Pool().Put(writer)
	if err := sqlitex.ExecuteTransient(writer, "BEGIN IMMEDIATE", nil); err != nil {
		t.Fatal(err)
	}
	// The timer is the explicit wake source for this bounded lock hold, not
	// a scheduler assumption: success must follow the writer's release.
	released := make(chan error, 1)
	go func() {
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		released <- sqlitex.ExecuteTransient(writer, "ROLLBACK", nil)
	}()
	results := db.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: artifact}})
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Err != nil || !results[0].Mirrored {
		t.Fatalf("record failed while another connection held the writer: %+v", results)
	}
	state, err := db.ReadIndexState(ctx, entry.Metadata.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.ArtifactHash == nil || *state.ArtifactHash != artifact.ArtifactHash {
		t.Fatalf("record did not persist the saved artifact identity: %+v", state)
	}
}
