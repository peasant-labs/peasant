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

// ExportSession returns full content only when captured retained input and the
// stored coordinates have matching artifact, input and parser evidence.
// managedRoot is the configured ingest output root, never the export destination.
func ExportSession(ctx context.Context, db *store.Store, fs ingest.FileSystem, sessionID string, managedRoots ...string) (*schema.SessionDetailPayload, error) {
	managedRoot := ""
	if len(managedRoots) > 0 {
		managedRoot = managedRoots[0]
	}
	snapshot, err := transcript.ReadSessionContent(ctx, db, fs, managedRoot, sessionID)
	if err != nil {
		return nil, fmt.Errorf("export session %s: %w", sessionID, err)
	}
	if snapshot == nil {
		return nil, fmt.Errorf("export session %s: %w", sessionID, ErrSessionNotFound)
	}
	if snapshot.FullContentError != nil {
		return nil, snapshot.FullContentError
	}
	detail, dbEntries := snapshot.Detail, snapshot.Entries
	sid := schema.SessionID(sessionID)
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
	fullSession.Turns, err = transcript.EntriesToTurnsValidated(dbEntries)
	if err != nil {
		return nil, fmt.Errorf("export.ExportSession: validate indexed observed model evidence after storage read and before export: %w", err)
	}

	// Convert to the standardized detail payload — same as the session viewer.
	payload, err := transcript.SessionToDetailValidated(fullSession)
	if err != nil {
		return nil, fmt.Errorf("export.ExportSession: validate observed model evidence before export: %w", err)
	}
	payload.TurnCount = len(payload.Turns)

	return payload, nil
}
