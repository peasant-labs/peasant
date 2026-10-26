package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

var _ ingest.PublicationInputReader = (*Store)(nil)

// WithCommittedPublicationInput is the current-generation publication read.
// Capture eligibility, full entries, metadata and the active generation are
// selected in ONE SQLite transaction. The generation supplies derived count and
// graph facts; capture metadata supplies source identity and capture evidence.
// No stored capture, digest or revision is rewritten by this read.
//
// Harmonized content is verified inside that transaction and needs no session
// lock during the callback. File-backed content keeps the shared session lock
// so cleanup cannot retire blobs during hydration. Both paths return the pool
// connection before invoking the callback.
// Sessions without a managed generation use the verified legacy capture. A
// broken or unsupported managed generation fails, never falls back to legacy.
func (s *Store) WithCommittedPublicationInput(ctx context.Context, id ingest.SessionID, fn func(ingest.PublicationInputBundle) error) (retErr error) {
	return s.WithCommittedPublicationInputSelected(ctx, id, ingest.SessionReadSelection{}, fn)
}

// WithCommittedPublicationInputSelected is the explicit-selection form of
// WithCommittedPublicationInput. A zero selection refuses while a latest attempt
// is pending, exactly like the default read. A LastGood
// selection bypasses only that refusal and uploads the last-good authority
// through the unchanged active read; every other publication check still
// applies, so an active preview remains refused.
func (s *Store) WithCommittedPublicationInputSelected(ctx context.Context, id ingest.SessionID, selection ingest.SessionReadSelection, fn func(ingest.PublicationInputBundle) error) (retErr error) {
	if s.GenerationSnapshotsSupported() && !isHarmonizedSession(ctx, s, id) {
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
	input, err := s.readCommittedPublicationInput(ctx, id, selection)
	if err != nil {
		return err
	}
	return fn(input)
}

func (s *Store) readCommittedPublicationInput(ctx context.Context, id ingest.SessionID, selection ingest.SessionReadSelection) (input ingest.PublicationInputBundle, err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return input, fmt.Errorf("store: take connection for committed publication %s: %w", id, err)
	}
	defer s.pool.Put(conn)
	end := sqlitex.Save(conn)
	defer end(&err)
	input, err = s.loadPublicationInputOnConn(ctx, conn, id, selection)
	if err != nil || input.Readiness != ingest.PublicationReady {
		return input, err
	}
	active, err := readActiveGenerationOnConn(conn, id)
	if err != nil {
		return input, err
	}
	if active == nil {
		// No managed generation: the legacy capture is the read authority. Its
		// stored document may predate a parent the session's own evidence now
		// names (a latest attempt carrying started_by evidence with a NULL
		// availability cache is exactly that state), so the bundle parent is the
		// session-level answer when one exists. A nil answer leaves the stored
		// document's parent in place: this read never clears a parent.
		parent, err := logicalParentForSessionOnConn(conn, id)
		if err != nil {
			return input, err
		}
		if parent != nil {
			value := schema.SessionID(*parent)
			input.Metadata.ParentUUID = &value
		}
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
	// The generation document is the authority for its own parent answer. The
	// bundle carries it when the document names one; a nil document parent
	// leaves the stored capture's parent in place, so this read never clears a
	// parent it did not answer.
	if snapshot.Metadata.ParentUUID != nil {
		parent := *snapshot.Metadata.ParentUUID
		meta.ParentUUID = &parent
	}
	input.Generation = &snapshot
	return input, nil
}
