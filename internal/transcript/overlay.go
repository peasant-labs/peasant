package transcript

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// BuildContentOverlay re-indexes a session's ORIGINAL source transcript file
// with full (untruncated) content extraction and returns entryIndex → full
// content. It exists because session_entries.content_preview is deliberately
// bounded at defaults.ContentPreviewLimit to keep DB row size sane (see
// truncateString in internal/ingest/utils.go) — this recovers the real turn
// bodies for consumers that must show/export the full transcript rather than
// a preview: the session_detail WS channel (StoreDataProvider.SessionByID)
// and `peasant export sessions` (export.ExportSession) both call this.
//
// Dispatch is keyed by the session's ACTUAL harness, not its SourceFormat.
// This matters because more than one harness can share a SourceFormat (Codex
// and Claude Code both write "jsonl") but need their own format-aware parser
// — using the wrong harness's indexer either produces zero matching entries
// (silently falling back to the truncated preview, the bug this function
// fixes) or misparses the file outright. The harness→indexer map comes from
// ingest.NewIndexerRegistry — the SAME constructor the real ingest pipeline
// wiring (cmd_harvest.go / cmd_kickstart.go) uses, so this can never drift
// into its own hand-copy again.
//
// This is a real cost: a FULL re-parse of the source transcript from disk.
// Callers MUST gate on whether anything actually needs recovering (e.g.
// AnyContentTruncated over the already-fetched DB entries) before calling
// this — BuildContentOverlay itself has no visibility into the DB entries to
// gate on, and unconditionally re-indexing on every session view would be
// the "traded one bug for a worse one" mistake (see AnyContentTruncated).
//
// Returns a nil map (not an error) for harnesses with no full-content
// indexer wired here. Currently: Cursor, whose indexer
// (internal/ingest/cursor.go) has no full-content toggle at all and always
// truncates at defaults.ContentPreviewLimit — recovering its full content
// would need a Cursor indexer change, tracked as a separate follow-up, not
// folded in here. Callers must treat a nil map as "keep existing preview
// content", not as a failure.
func BuildContentOverlay(ctx context.Context, fs ingest.FileSystem, harness defaults.Harness, sourcePath ingest.ResolvedPath, sessionID schema.SessionID) (map[int]string, error) {
	indexer, ok := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{FullContent: true})[ingest.Harness(harness)]
	if !ok {
		return nil, nil
	}

	sid, err := ingest.NewSessionID(string(sessionID))
	if err != nil {
		return nil, fmt.Errorf(
			"transcript.BuildContentOverlay: session ID %q is not a valid ingest.SessionID: %w\n"+
				"What went wrong: the session ID could not be validated before re-indexing its source transcript.\n"+
				"Where: transcript.BuildContentOverlay → ingest.NewSessionID.\n"+
				"Fix: verify the session ID passed in matches the canonical UUID form Peasant assigns.",
			sessionID, err,
		)
	}

	session := ingest.DiscoveredSession{
		SessionID:  sid,
		Harness:    ingest.Harness(harness),
		SourcePath: sourcePath,
	}
	sourceEntries, err := indexer.IndexTranscript(ctx, session)
	if err != nil {
		return nil, fmt.Errorf(
			"transcript.BuildContentOverlay: re-index source transcript for session %q (harness=%s): %w\n"+
				"What went wrong: the harness-specific indexer failed to parse the source transcript file.\n"+
				"Where: transcript.BuildContentOverlay → indexer.IndexTranscript, source path %q.\n"+
				"Why: the file may be missing, moved, or corrupted since ingest.\n"+
				"Fix: verify the source file still exists at the recorded path, or re-run 'peasant ingest' to refresh it. "+
				"The caller should treat this as non-fatal and keep the DB's existing (truncated) content_preview.",
			sessionID, harness, err, sourcePath,
		)
	}

	overlay := make(map[int]string, len(sourceEntries))
	for i := range sourceEntries {
		e := &sourceEntries[i]
		if e.ContentPreview != nil {
			overlay[e.EntryIndex] = *e.ContentPreview
		}
	}
	// depth-0 wrappers with no content of their own inherit their first
	// depth-1 child's content (R6 content migration: depth-0 wrappers have
	// content_preview=NULL because the content moved to depth-1 children).
	for i := range sourceEntries {
		e := &sourceEntries[i]
		if e.Depth == 0 && e.ContentPreview == nil {
			for j := i + 1; j < len(sourceEntries); j++ {
				child := &sourceEntries[j]
				if child.Depth == 0 {
					break // past this entry's children
				}
				if child.ParentIndex != nil && *child.ParentIndex == e.EntryIndex && child.ContentPreview != nil {
					overlay[e.EntryIndex] = *child.ContentPreview
					break
				}
			}
		}
	}
	return overlay, nil
}

// AnyContentTruncated reports whether ANY entry's ContentPreview was cut by
// defaults.ContentPreviewLimit — the gate callers must check before calling
// BuildContentOverlay. BuildContentOverlay does a full re-parse of the
// source transcript from disk; the common case (a normal-sized session,
// nothing hit the limit) has nothing to recover, so it must skip that
// re-parse entirely rather than pay it on every session view. A preview
// whose length is strictly less than the limit was never truncated
// (truncateString only ever produces a preview of EXACTLY maxLen when it
// cuts); reaching exactly the limit is treated as truncated too, since a
// preview that happens to be precisely ContentPreviewLimit chars long
// un-truncated is indistinguishable from one that was cut there, and the
// re-index is cheap insurance in that one-entry-long edge case relative to
// the common all-short-turns case this gate exists to skip.
func AnyContentTruncated(entries []schema.SessionEntry) bool {
	for i := range entries {
		if p := entries[i].ContentPreview; p != nil && len(*p) >= defaults.ContentPreviewLimit {
			return true
		}
	}
	return false
}
