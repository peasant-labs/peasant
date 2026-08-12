package export

import (
	"context"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// ExportSession reads a session's transcript from the store and source file,
// using DB entries (session_entries) as the source of truth for turn structure
// and indices, and the source transcript for full content extraction.
//
// Flow:
//  1. Look up source_path and source_format from the store.
//  2. Re-index the source transcript to build a contentMap (entryIndex → fullContent).
//     contentMap keys come from a fresh re-index. If the source file has changed since
//     ingest, keys may not match all DB entry indices. Fallback to ContentPreview ensures
//     graceful degradation.
//  3. Read DB entries via store.ListEntries — these carry the canonical entry_index values
//     that annotations reference.
//  4. Look up session metadata for the envelope.
//  5. Convert to SessionDetailPayload via SessionToDetail (same path as the session viewer).
//  6. TurnCount = len(payload.Turns) — always correct regardless of DB metadata.
//  7. Return the SessionDetailPayload with full content overlaid from contentMap.
//
// Returns ErrSessionNotFound when the session ID does not exist in the store.
// Returns an actionable error when the source file is missing or unreadable.
func ExportSession(ctx context.Context, db *store.Store, fs ingest.FileSystem, sessionID string) (*schema.SessionDetailPayload, error) {
	// Step 1: Look up source info.
	info, err := db.SessionSourceInfo(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf(
			"export.ExportSession: query source info for session %q: %w\n"+
				"What went wrong: database query for session source info failed.\n"+
				"Where: export.ExportSession → store.SessionSourceInfo.\n"+
				"Fix: verify the database is accessible and not corrupted.",
			sessionID, err,
		)
	}
	if info == nil {
		return nil, fmt.Errorf(
			"export.ExportSession: session %q: %w\n"+
				"What went wrong: no session with this ID exists in the store.\n"+
				"Where: export.ExportSession → store.SessionSourceInfo returned nil.\n"+
				"Fix: run 'peasant ingest' to discover sessions, or verify the session ID is correct.",
			sessionID, ErrSessionNotFound,
		)
	}

	// Step 2: List DB entries first — the standard ListEntries → EntriesToTurns
	// → SessionToDetail path (same as the session viewer), AND the input to
	// the truncation gate below.
	sid := schema.SessionID(sessionID)
	dbEntries, err := db.ListEntries(ctx, sid)
	if err != nil {
		return nil, fmt.Errorf(
			"export.ExportSession: list entries for session %q: %w\n"+
				"What went wrong: database query for session entries failed.\n"+
				"Where: export.ExportSession → store.ListEntries.\n"+
				"Fix: verify the database is accessible and not corrupted.",
			sessionID, err,
		)
	}

	// Step 3: Build contentMap by re-indexing the source transcript with full
	// content, dispatched by the session's ACTUAL harness (not source format —
	// see transcript.BuildContentOverlay's doc comment: Codex and Claude Code
	// both write "jsonl", so a format-only dispatch silently mis-indexes one
	// of them). contentMap maps entry_index → full content string; used ONLY
	// for content overlay below, never for structure or indices.
	//
	// GATED on transcript.AnyContentTruncated: BuildContentOverlay re-parses
	// the ENTIRE source transcript from disk, which is real cost this
	// function would otherwise pay on every export regardless of session
	// size. The common case — nothing in this session hit the preview limit
	// — has nothing to recover, so it skips the re-parse entirely.
	var contentMap map[int]string
	if transcript.AnyContentTruncated(dbEntries) {
		contentMap, err = transcript.BuildContentOverlay(ctx, fs, defaults.Harness(info.Harness), ingest.ResolvedPath(info.SourcePath), sid)
		if err != nil {
			return nil, fmt.Errorf("export.ExportSession: %w", err)
		}
	}

	detail, err := db.SessionDetailByID(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf(
			"export.ExportSession: query session detail for %q: %w\n"+
				"What went wrong: database query for session detail failed.\n"+
				"Where: export.ExportSession → store.SessionDetailByID.\n"+
				"Fix: verify the database is accessible and not corrupted.",
			sessionID, err,
		)
	}

	// Build an ingest.Session so we can call SessionToDetail.
	fullSession := &ingest.Session{ID: sid}
	if detail != nil {
		fullSession.Harness = defaults.Harness(detail.ModelHarness)
		fullSession.Model = detail.ModelID
		fullSession.Project = detail.ProjectName
		fullSession.ProjectPath = detail.ProjectPath
		fullSession.StartTime = time.UnixMilli(detail.StartMs)
		if detail.EndMs > 0 {
			fullSession.EndTime = time.UnixMilli(detail.EndMs)
		}
		fullSession.Metadata.TotalTokens = detail.TokensTotal
		fullSession.Metadata.TurnCount = detail.TurnCount
		if detail.StartMs > 0 && detail.EndMs > 0 {
			fullSession.Metadata.Duration = time.Duration(detail.EndMs-detail.StartMs) * time.Millisecond
		}
		if detail.GitBranch != nil {
			fullSession.GitBranch = *detail.GitBranch
		}
		if detail.GitRemote != nil {
			fullSession.GitRemote = *detail.GitRemote
		}
		fullSession.PushedAt = detail.PushedAt
	}
	fullSession.Turns = transcript.EntriesToTurns(dbEntries)

	// Convert to the standardized detail payload — same as the session viewer.
	payload, err := transcript.SessionToDetailValidated(fullSession)
	if err != nil {
		return nil, fmt.Errorf("export.ExportSession: validate observed model evidence before export: %w", err)
	}
	payload.TurnCount = len(payload.Turns)

	// Overlay full content from source re-index where available.
	// The DB stores truncated ContentPreview; the source has full text.
	for i := range payload.Turns {
		if content, ok := contentMap[payload.Turns[i].Index]; ok {
			payload.Turns[i].Content = content
		}
		// Turns not in contentMap keep their existing content (from DB ContentPreview
		// via EntriesToTurns). Turns with no content at all (e.g. tool_use entries)
		// are expected — they have no text body.
	}

	return payload, nil
}
