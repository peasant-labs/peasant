package transcript

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// DetailLoadOptions bounds each read, not the final wire payload allocation.
type DetailLoadOptions struct {
	SoftMaxBytes int64
}

// LoadEntriesForDetail materializes full entries without consulting provider or
// retained files. The mandatory reader owns snapshot consistency and integrity.
func LoadEntriesForDetail(ctx context.Context, r ingest.FullSessionEntryReader, id ingest.SessionID, opts DetailLoadOptions) ([]schema.SessionEntry, ingest.SessionContentCapture, error) {
	var entries []schema.SessionEntry
	var capture ingest.SessionContentCapture
	fail := func(err error) ([]schema.SessionEntry, ingest.SessionContentCapture, error) {
		return nil, capture, fmt.Errorf("transcript full-content load for session %q failed before detail/export/publication: %w; no complete transcript is available; run 'peasant harvest index --force' with retained artifacts to rebuild the capture, then retry", id, err)
	}
	if opts.SoftMaxBytes < 0 {
		return fail(fmt.Errorf("negative page budget; use zero defaults or a positive override"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	var err error
	entries, capture, err = r.LoadFullSessionEntries(ctx, id, opts.SoftMaxBytes)
	if err != nil {
		return fail(err)
	}
	// A capture that is incomplete ONLY because oversized source records were
	// omitted still holds every entry, with a placeholder standing in each
	// omitted record's place, and the store proves it with the same full-capture
	// hash. It loads here like a complete one; every other incompleteness is
	// refused exactly as before.
	if !store.PublishableWithOmissions(capture) || capture.FullCaptureSHA256 == "" {
		return fail(fmt.Errorf("database capture is incomplete; bounded previews cannot substitute for full content"))
	}
	if capture.SessionID != id || len(entries) != capture.EntryCount {
		return fail(fmt.Errorf("database capture identity or entry count does not match hydrated entries"))
	}
	return entries, capture, nil
}

// LoadSessionDetail uses the canonical conversion path after full hydration.
func LoadSessionDetail(ctx context.Context, r ingest.FullSessionEntryReader, session *ingest.Session, opts DetailLoadOptions) (*schema.SessionDetailPayload, error) {
	entries, _, err := LoadEntriesForDetail(ctx, r, session.ID, opts)
	if err != nil {
		return nil, err
	}
	copy := *session
	projection, err := EntriesToProjectionValidated(entries, ProjectionOptions{Harness: session.Harness})
	if err != nil {
		return nil, err
	}
	return SessionToDetailValidatedWithProjection(&copy, projection)
}
