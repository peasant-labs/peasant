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

// ExportSession hydrates verified full database content and uses the canonical
// EntriesToTurns → SessionToDetail conversion, preserving stored entry anchors.
// The filesystem argument remains for caller compatibility; no source is read.
// Missing sessions return ErrSessionNotFound. Incomplete or corrupt captures
// return an actionable error rather than exporting bounded previews.
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

	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		return nil, err
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
	// Convert to the standardized detail payload — same as the session viewer.
	payload, err := transcript.LoadSessionDetail(ctx, db, fullSession, transcript.DetailLoadOptions{})
	if err != nil {
		return nil, fmt.Errorf("export.ExportSession: load full transcript before export: %w", err)
	}
	payload.TurnCount = len(payload.Turns)

	return payload, nil
}
