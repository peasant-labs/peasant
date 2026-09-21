package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite/sqlitex"
)

var _ ingest.PublicationInputReader = (*Store)(nil)

// WithCommittedPublicationInput is the current-generation publication read.
// Capture eligibility, full entries, metadata and the active generation are
// selected in ONE SQLite transaction. The generation supplies derived count and
// graph facts; capture metadata supplies source identity and capture evidence.
// No stored capture, digest or revision is rewritten by this read.
//
// Like WithSessionSnapshot, it holds the shared session lock through the callback
// so activation/cleanup cannot retire blobs during hydration, but returns the
// database connection before invoking it. The callback must finish hydration
// before returning and must not activate a generation or perform network I/O.
// Sessions without a managed generation use the verified legacy capture. A
// broken or unsupported managed generation fails, never falls back to legacy.
func (s *Store) WithCommittedPublicationInput(ctx context.Context, id ingest.SessionID, fn func(ingest.PublicationInputBundle) error) (retErr error) {
	if s.GenerationSnapshotsSupported() {
		release, err := s.sessionLocker.LockShared(ctx, id)
		if err != nil {
			return err
		}
		defer func() {
			if err := release(); retErr == nil {
				retErr = err
			}
		}()
	}
	input, err := s.readCommittedPublicationInput(ctx, id)
	if err != nil {
		return err
	}
	return fn(input)
}

func (s *Store) readCommittedPublicationInput(ctx context.Context, id ingest.SessionID) (input ingest.PublicationInputBundle, err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return input, fmt.Errorf("store: take connection for committed publication %s: %w", id, err)
	}
	defer s.pool.Put(conn)
	end := sqlitex.Save(conn)
	defer end(&err)
	input, err = s.loadPublicationInputOnConn(ctx, conn, id)
	if err != nil || input.Readiness != ingest.PublicationReady {
		return input, err
	}
	active, err := readActiveGenerationOnConn(conn, id)
	if err != nil {
		return input, err
	}
	if active == nil {
		return input, nil
	}
	if !s.GenerationSnapshotsSupported() {
		return input, fmt.Errorf("store: session %s has an active generation but managed content support is not configured; no committed publication can be read; open the store with WithGenerationArtifacts", id)
	}
	snapshot, err := buildReadSnapshotOnConn(conn, id)
	if err != nil {
		return input, err
	}
	if err := snapshot.Validate(); err != nil {
		return input, fmt.Errorf("store: committed publication generation for %s is invalid: %w; no hydration was authorized", id, err)
	}
	// The capture row is evidence of extraction, not the current projection.
	// Compose the public input here so consumers never have to repair these facts.
	meta := &input.Metadata
	if snapshot.Metadata.Stats.InputSubmissionCount == nil {
		meta.Stats.InputSubmissionCount = nil
	} else {
		count := *snapshot.Metadata.Stats.InputSubmissionCount
		meta.Stats.InputSubmissionCount = &count
	}
	if snapshot.Metadata.RootSessionID == nil {
		meta.RootSessionID = nil
	} else {
		root := *snapshot.Metadata.RootSessionID
		meta.RootSessionID = &root
	}
	meta.Purpose = snapshot.Metadata.Purpose
	meta.Relationships = append([]schema.SessionRelationship(nil), snapshot.Metadata.Relationships...)
	input.Generation = &snapshot
	return input, nil
}
