package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite/sqlitex"
)

var _ ingest.SessionIndexStateReader = (*Store)(nil)

// ReadIndexState captures the actual nullable SQL state. A missing row returns
// nil, nil; database access failures return an error. It does not read source
// files or run a parser, and does not turn unknown input into a successful proof.
func (s *Store) ReadIndexState(ctx context.Context, sessionID ingest.SessionID) (_ *ingest.SessionIndexState, err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: capture index state for session %s before parsing: take connection: %w; no replacement was authorized; restore database access and retry", sessionID, err)
	}
	defer s.pool.Put(conn)
	defer sqlitex.Save(conn)(&err)
	return readIndexStateOnConn(conn, sessionID)
}

func validateIndexInputClaim(write ingest.SessionEntryWrite) error {
	if write.ExpectedState != nil && write.ExpectedState.SessionID != write.SessionID {
		return &ingest.StaleIndexWorkError{SessionID: write.SessionID}
	}
	if write.IndexedInputHash == nil {
		return nil
	}
	if !validIndexInputHash(*write.IndexedInputHash) || write.ExpectedState == nil ||
		write.ExpectedState.ArtifactHash == nil || !validIndexInputHash(*write.ExpectedState.ArtifactHash) || write.IndexerVersion < 1 {
		return fmt.Errorf("store: input proof for session %s lacks a valid digest, captured artifact identity or positive producing indexer revision; replacement was refused before changing rows; supply the actual captured SQL state and successfully consumed input, or omit the input proof", write.SessionID)
	}
	return nil
}

func validIndexInputHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, character := range hash {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func sameIndexState(a, b *ingest.SessionIndexState) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.SessionID == b.SessionID && a.Harness == b.Harness && a.IndexerVersion == b.IndexerVersion &&
		a.PublicationCaptureRevision == b.PublicationCaptureRevision &&
		sameIndexValue(a.ArtifactHash, b.ArtifactHash) && sameIndexValue(a.IndexVersion, b.IndexVersion) &&
		sameIndexValue(a.IndexedAt, b.IndexedAt) && sameIndexValue(a.IndexedInputHash, b.IndexedInputHash) &&
		sameIndexValue(a.SessionEntriesHash, b.SessionEntriesHash)
}

func sameIndexValue[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
