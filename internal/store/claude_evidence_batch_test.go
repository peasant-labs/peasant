package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
)

// TestStoredClaudeEvidenceForChunksAtTheReadBatchSize proves the by-path read
// answers more paths than one statement can bind.
//
// The proof is the database's own variable limit, not the path count: the
// connection is lowered to just above one read batch, so an unbounded IN list
// over every path cannot prepare. A batched read still returns every row. If
// the batching is removed, the statement fails at prepare and this test fails
// on the returned error rather than passing on a small fixture.
func TestStoredClaudeEvidenceForChunksAtTheReadBatchSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "evidence-batch.db"), WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pathCount := indexFormatReadBatchSize + 17
	upserts := make([]ingest.ClaudeTranscriptEvidence, 0, pathCount)
	paths := make([]ingest.ResolvedPath, 0, pathCount)
	for index := 0; index < pathCount; index++ {
		path := ingest.ResolvedPath(fmt.Sprintf("/synthetic/evidence/batch-%03d.jsonl", index))
		paths = append(paths, path)
		upserts = append(upserts, ingest.ClaudeTranscriptEvidence{
			SourcePath: path,
			Scope:      ingest.ClaudeEvidenceScopeRoot,
		})
	}
	if err := db.SaveClaudeEvidence(ctx, upserts, nil); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	// One more than a batch but below the full set: a single statement over
	// every path can no longer prepare, while each batch can.
	conn, err := db.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	previous := conn.Limit(sqlite.LimitVariableNumber, int32(indexFormatReadBatchSize+4))
	db.pool.Put(conn)
	t.Cleanup(func() {
		conn, err := db.pool.Take(ctx)
		if err != nil {
			return
		}
		conn.Limit(sqlite.LimitVariableNumber, previous)
		db.pool.Put(conn)
	})

	loaded, err := db.LoadClaudeEvidenceFor(ctx, paths)
	if err != nil {
		t.Fatalf("LoadClaudeEvidenceFor across %d paths with a %d-variable limit: %v; an unbounded IN list cannot prepare here, so this failure means the read stopped batching", pathCount, indexFormatReadBatchSize+4, err)
	}
	if len(loaded) != pathCount {
		t.Fatalf("LoadClaudeEvidenceFor returned %d records for %d paths, want every path across the %d-path batch boundary", len(loaded), pathCount, indexFormatReadBatchSize)
	}
	for _, path := range paths {
		if _, ok := loaded[path]; !ok {
			t.Fatalf("LoadClaudeEvidenceFor omitted %s", path)
		}
	}
}
