package export

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// ExportSnapshotPayload builds one export payload through the durable
// snapshot boundary. It is the export half of the single payload-construction
// boundary shared with detail reads and publication packaging: the snapshot's
// shared lock covers hydration through final serialization, and the caller
// writes the returned payload after the lock is released.
func ExportSnapshotPayload(ctx context.Context, reader indexformat.SnapshotReader, resolver indexformat.ContentResolver, sessionID schema.SessionID) (*schema.SessionDetailPayload, error) {
	_, payload, err := transcript.BuildSnapshotDetailBytes(ctx, reader, resolver, sessionID)
	return payload, err
}

// ExportSession reads verified full database content and context in one snapshot.
// Filesystem and managed-root arguments remain for caller compatibility only.
//
// A session with a committed managed generation is exported through the durable
// snapshot boundary: immutable blobs hydrate full arguments/results before
// folding, and the shared lock covers hydration through serialization. Legacy
// sessions without a generation use the preserved content-capture path. A
// corrupt managed artifact fails the export; it never falls back to truncated
// content.
func ExportSession(ctx context.Context, db *store.Store, fs ingest.FileSystem, sessionID string, managedRoots ...string) (*schema.SessionDetailPayload, error) {
	if db.GenerationSnapshotsSupported() {
		sid, err := ingest.NewSessionID(sessionID)
		if err != nil {
			return nil, fmt.Errorf("export session %s: %w", sessionID, err)
		}
		if payload, err := ExportSnapshotPayload(ctx, db, db, sid); err == nil {
			return payload, nil
		} else if !errors.Is(err, transcript.ErrLegacySnapshot) {
			return nil, fmt.Errorf("export session %s: %w", sessionID, err)
		}
	}
	snapshot, err := db.ReadSessionContent(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("export session %s: %w", sessionID, err)
	}
	if snapshot == nil {
		return nil, fmt.Errorf("export session %s: %w", sessionID, ErrSessionNotFound)
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
	// Convert to the standardized detail payload — same as the session viewer.
	projection, err := transcript.EntriesToProjectionValidated(dbEntries, transcript.ProjectionOptions{Harness: fullSession.Harness})
	if err != nil {
		return nil, fmt.Errorf("export session %s: validate captured entries: %w", sessionID, err)
	}
	payload, err := transcript.SessionToDetailValidatedWithProjection(fullSession, projection)
	if err != nil {
		return nil, fmt.Errorf("export.ExportSession: load full transcript before export: %w", err)
	}
	payload.TurnCount = len(payload.Turns)

	return payload, nil
}
