package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestSaveClaudeEvidenceIfCurrentWaitsForWriter exercises the current-store
// writer against a separate SQLite connection holding the write lock.
func TestSaveClaudeEvidenceIfCurrentWaitsForWriter(t *testing.T) {
	t.Parallel()
	path := storetest.CopyGoldenDB(t)
	db, err := store.Open(path, store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	writer, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(writer, "BEGIN IMMEDIATE", nil); err != nil {
		_ = writer.Close()
		t.Fatalf("take competing writer lock: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), defaults.SQLiteBusyTimeout+2*time.Second)
	result := make(chan error, 1)
	started := make(chan struct{})
	locked, finished := true, false
	defer func() {
		if locked {
			if err := sqlitex.ExecuteTransient(writer, "ROLLBACK", nil); err != nil {
				t.Errorf("release competing writer lock during cleanup: %v", err)
			}
		}
		cancel()
		if !finished {
			select {
			case <-result:
			case <-time.After(defaults.SQLiteBusyTimeout + time.Second):
				t.Error("evidence writer did not finish after cancellation and lock release")
			}
		}
		if err := writer.Close(); err != nil {
			t.Errorf("close competing writer: %v", err)
		}
	}()

	record := ingest.ClaudeTranscriptEvidence{
		SourcePath: "/synthetic/contended-claude.jsonl",
		Scope:      ingest.ClaudeEvidenceScopeRoot,
		Title:      "evidence saved after the writer releases",
		Spawns:     []ingest.ClaudeTeammateIdentity{{Team: "synthetic-team", Name: "synthetic-agent"}},
	}
	go func() {
		close(started)
		result <- store.SaveClaudeEvidenceIfCurrent(ctx, path, []ingest.ClaudeTranscriptEvidence{record}, nil)
	}()
	<-started
	// The timer is the wake source for the bounded lock hold. It gives the
	// real save path an opportunity to encounter the competing writer before
	// the test releases it; no production-only test seam is needed.
	hold := time.NewTimer(250 * time.Millisecond)
	defer hold.Stop()
	select {
	case err := <-result:
		finished = true
		t.Fatalf("evidence write returned before the competing writer released its lock: %v", err)
	case <-hold.C:
	case <-ctx.Done():
		t.Fatalf("waiting while the writer held the lock: %v", ctx.Err())
	}
	// Check again before releasing the lock, in case the timer and completion
	// became ready together.
	select {
	case err := <-result:
		finished = true
		t.Fatalf("evidence write returned before the competing writer released its lock: %v", err)
	default:
	}
	if err := sqlitex.ExecuteTransient(writer, "ROLLBACK", nil); err != nil {
		t.Fatalf("release competing writer lock: %v", err)
	}
	locked = false
	select {
	case err := <-result:
		finished = true
		if err != nil {
			t.Fatalf("evidence write after the competing writer released its lock: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("evidence write did not finish after the writer released its lock: %v", ctx.Err())
	}

	loaded, err := db.LoadClaudeEvidenceFor(t.Context(), []ingest.ResolvedPath{record.SourcePath})
	if err != nil {
		t.Fatalf("read saved evidence: %v", err)
	}
	got, ok := loaded[record.SourcePath]
	if !ok {
		t.Fatalf("saved evidence for %s is missing", record.SourcePath)
	}
	if got.Title != record.Title {
		t.Fatalf("saved title = %q, want %q", got.Title, record.Title)
	}
	if len(got.Spawns) != 1 || got.Spawns[0] != record.Spawns[0] {
		t.Fatalf("saved spawns = %+v, want %+v", got.Spawns, record.Spawns)
	}
	spawns, err := db.ClaudeTeammateSpawnsForPaths(t.Context(), []ingest.ResolvedPath{record.SourcePath})
	if err != nil {
		t.Fatalf("read saved spawn projection: %v", err)
	}
	if got := spawns[record.SourcePath]; len(got) != 1 || got[0] != record.Spawns[0] {
		t.Fatalf("saved spawn projection = %+v, want %+v", got, record.Spawns)
	}
}
