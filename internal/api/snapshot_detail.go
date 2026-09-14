package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// DetailPayload returns the validated detail payload for one stored session.
//
// A session with a committed managed generation is served through the durable
// snapshot boundary: immutable blobs hydrate full arguments/results before
// folding, and the shared lock covers hydration through serialization. Legacy
// sessions without a generation use the preserved SessionByID path. A corrupt
// managed artifact fails the read; it never falls back to truncated content.
//
// DetailPayload applies no discovery or selection scope: selection decides what
// a person is offered, never what an already-stored session opens to. Callers
// that list sessions keep using SessionSummaries; callers that follow a stored
// link use this or SessionSummariesByID.
func (p *StoreDataProvider) DetailPayload(ctx context.Context, id string) (*schema.SessionDetailPayload, error) {
	return DetailPayloadWithReader(ctx, p.store, p.store, id, func(ctx context.Context, id string) (*schema.SessionDetailPayload, error) {
		session, err := p.SessionByID(ctx, id)
		if err != nil {
			return nil, err
		}
		return transcript.SessionToDetailValidated(session)
	})
}

// DetailPayloadWithReader serves one validated detail payload through the
// durable snapshot boundary when the reader supports it, and through the
// preserved legacy callback otherwise. A legacy V1 snapshot explicitly selects
// the legacy path; a session with no stored metadata is reported as the API
// not-found sentinel; any other snapshot failure is returned, never hidden
// behind truncated content.
func DetailPayloadWithReader(ctx context.Context, reader indexformat.SnapshotReader, resolver indexformat.ContentResolver, id string, legacy func(context.Context, string) (*schema.SessionDetailPayload, error)) (*schema.SessionDetailPayload, error) {
	sessionID, err := ingest.NewSessionID(id)
	if err != nil {
		return nil, fmt.Errorf("store adapter: detail payload: %w", err)
	}
	supported, ok := reader.(interface{ GenerationSnapshotsSupported() bool })
	if ok && supported.GenerationSnapshotsSupported() {
		if _, payload, err := transcript.BuildSnapshotDetailBytes(ctx, reader, resolver, sessionID); err == nil {
			return payload, nil
		} else if !errors.Is(err, transcript.ErrLegacySnapshot) {
			if errors.Is(err, indexformat.ErrSnapshotNotFound) {
				return nil, fmt.Errorf("store adapter: detail payload for session %q: %w", id, ErrSessionNotFound)
			}
			return nil, fmt.Errorf("store adapter: detail payload for session %q: %w", id, err)
		}
	}
	detail, err := legacy(ctx, id)
	if err != nil {
		return nil, err
	}
	return detail, nil
}
