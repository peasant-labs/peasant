package main

import (
	"context"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// The CLI read-router call sites (design §6.2; harmonized content model,
// peasant-labs/peasant#568).
//
// Each function below names one consumer read that the harmonized readers
// collapse onto the one store-level selection shim: navigation (the sessions
// context range and the sessions list previews), publication (the share
// preview), and export (the session transcript). The session-detail read that
// serves the viewer lives in internal/api and collapses in the same step.
//
// Today every seam delegates to the existing store read, so behavior is
// unchanged. A later slice routes them through the shim without touching the
// callers.
func readContextEntries(ctx context.Context, db *store.Store, sid schema.SessionID, fromIndex, toIndex int) ([]schema.SessionEntry, error) {
	return db.ListEntriesRange(ctx, sid, fromIndex, toIndex)
}

func readListPreviews(ctx context.Context, db *store.Store, sessionIDs []string) (map[string]string, error) {
	return db.FirstUserMessageBulk(ctx, sessionIDs)
}

func readPublicationSnapshot(ctx context.Context, reader availableContentReader, id ingest.SessionID) (*store.SessionContentSnapshot, error) {
	return reader.ReadSessionAvailable(ctx, id)
}

func exportSessionTranscript(ctx context.Context, db *store.Store, fs ingest.FileSystem, sessionID string, managedRoots ...string) (*schema.SessionDetailPayload, error) {
	return export.ExportSession(ctx, db, fs, sessionID, managedRoots...)
}
