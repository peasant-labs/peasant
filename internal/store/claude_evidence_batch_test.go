package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// TestStoredClaudeEvidenceForChunksAtTheReadBatchSize proves the by-path read
// answers more paths than one statement can bind. P3 hands it every stale
// origin path at once, so a single unbounded IN list would either hit SQLite's
// variable limit or build a statement sized by the store. The read must return
// every requested row across chunk boundaries.
func TestStoredClaudeEvidenceForChunksAtTheReadBatchSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "evidence-batch.db"), WithPoolSize(2))
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

	loaded, err := db.LoadClaudeEvidenceFor(ctx, paths)
	if err != nil {
		t.Fatalf("LoadClaudeEvidenceFor across %d paths: %v", pathCount, err)
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
