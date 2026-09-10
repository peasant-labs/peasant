package transcript

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// PRIOR-VERSION, DEPRECATION CANDIDATE. Database content is authoritative.
// This overlay has no production caller: full detail, export and publication
// read verified content from SQLite, which does not depend on a retained file
// still existing or still parsing. It is retained, not deleted, because no
// replacement has been ratified for a caller that genuinely has only a file.
//
// BuildContentOverlay is a legacy source-only compatibility helper. It cannot
// establish capture completeness or stable stored entry coordinates. Full
// detail, export and publication use verified database content instead.
func BuildContentOverlay(ctx context.Context, fs ingest.FileSystem, harness defaults.Harness, sourcePath ingest.ResolvedPath, sessionID schema.SessionID) (map[int]string, error) {
	entries, err := fullContentEntries(ctx, fs, harness, sourcePath, sessionID)
	if err != nil {
		return nil, err
	}
	return contentOverlayFromEntries(entries), nil
}

func fullContentEntries(ctx context.Context, fs ingest.FileSystem, harness defaults.Harness, sourcePath ingest.ResolvedPath, sessionID schema.SessionID) ([]schema.SessionEntry, error) {
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

	return sourceEntries, nil
}

// contentOverlayFromEntries maps entry_index to the full content preview of
// each re-indexed entry. A depth-0 wrapper with no content of its own inherits
// its first depth-1 child's content (R6 content migration: depth-0 wrappers
// have content_preview=NULL because the content moved to depth-1 children).
// The child scan stops at the next depth-0 entry, so a depth-0 entry that
// carries a ParentIndex (a harness message graph, see ingest.Turn.ParentIndex)
// is never adopted as a child: children are found by Depth, not by ParentIndex.
func contentOverlayFromEntries(sourceEntries []schema.SessionEntry) map[int]string {
	overlay := make(map[int]string, len(sourceEntries))
	for i := range sourceEntries {
		e := &sourceEntries[i]
		if e.ContentPreview != nil {
			overlay[e.EntryIndex] = *e.ContentPreview
		}
	}
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
	return overlay
}

// AnyContentTruncated conservatively detects potentially bounded legacy text.
// At the limit, an exact-size value and a cut value are indistinguishable.
// Database full readers use capture integrity, not this heuristic.
func AnyContentTruncated(entries []schema.SessionEntry) bool {
	for i := range entries {
		for _, p := range []*string{entries[i].ContentPreview, entries[i].ToolInput, entries[i].ToolOutput} {
			if p != nil && len(*p) >= defaults.ContentPreviewLimit {
				return true
			}
		}
	}
	return false
}
