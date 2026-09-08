package testutil

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// FullContentWriter is the production atomic capture-write boundary.
type FullContentWriter interface {
	IndexSessionEntryBatch(context.Context, []ingest.SessionEntryWrite) []ingest.SessionEntryWriteResult
}

// WriteFullEntries seeds synthetic complete source entries, never bounded previews.
func WriteFullEntries(ctx context.Context, db FullContentWriter, id ingest.SessionID, entries []schema.SessionEntry) error {
	results := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Entries: entries, RequireFullContent: true,
		ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureComplete,
			SourceAuthority: ingest.ContentSourceNewIngest, CaptureRevision: "synthetic-complete-source"},
	}})
	if len(results) != 1 {
		return fmt.Errorf("seed full content: expected one atomic write result")
	}
	return results[0].Err
}
