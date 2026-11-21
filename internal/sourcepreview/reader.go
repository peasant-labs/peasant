package sourcepreview

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/salt"
)

// Reader opens captures over the sessions one discovery scan listed. It holds
// only the listing; every capture it opens owns its own state.
type Reader struct {
	fs   ingest.FileSystem
	git  ingest.GitResolver
	salt salt.Salt
	byID map[string]onboarding.SessionListing
}

// ReaderOption configures a Reader.
type ReaderOption func(*Reader)

// WithGitResolver sets the resolver the SQLite materializer uses, so the
// preview and discovery attribute a session's project the same way.
func WithGitResolver(git ingest.GitResolver) ReaderOption {
	return func(r *Reader) { r.git = git }
}

// WithSalt sets the per-installation salt the materializer receives. The
// preview reads a transcript, not a project hash, so the zero salt is the
// production default; this option exists so a test can pin one.
func WithSalt(s salt.Salt) ReaderOption {
	return func(r *Reader) { r.salt = s }
}

// NewReader builds a reader over the discovery listing. A listing with no
// recorded transcript location is not previewable.
func NewReader(fs ingest.FileSystem, listings []onboarding.SessionListing, opts ...ReaderOption) *Reader {
	byID := make(map[string]onboarding.SessionListing, len(listings))
	for _, listing := range listings {
		if listing.SessionID != "" && !listing.Source.IsZero() {
			byID[listing.SessionID] = listing
		}
	}
	reader := &Reader{fs: fs, git: &ingest.ExecGitResolver{}, byID: byID}
	for _, opt := range opts {
		opt(reader)
	}
	return reader
}

// Previewable reports that discovery recorded a transcript location for the
// session.
func (r *Reader) Previewable(sessionID string) bool {
	_, ok := r.byID[sessionID]
	return ok
}

// Open starts a capture of one session and returns its first page.
//
// The capture lives until Close, or until ctx is cancelled: cancellation
// clears everything the capture loaded, at once. A session discovery recorded
// no location for fails with ErrUnavailable; a transcript with no readable
// turns fails with ErrNoTurns. Every failure is a *CaptureError.
func (r *Reader) Open(ctx context.Context, sessionID string, opts CaptureOptions) (*Capture, Page, error) {
	if err := opts.Validate(); err != nil {
		return nil, Page{}, err
	}
	listing, ok := r.byID[sessionID]
	if !ok {
		return nil, Page{}, captureError(CloseUnavailable, nil, "")
	}
	session, err := DiscoveredSessionFor(listing)
	if err != nil {
		return nil, Page{}, captureError(CloseUnreadable, err, "the discovery listing could not be mapped to a transcript source")
	}
	capture, err := newCapture(ctx, r, listing, session, opts)
	if err != nil {
		return nil, Page{}, err
	}
	page, err := capture.first()
	if err != nil {
		capture.Close()
		return nil, Page{}, err
	}
	return capture, page, nil
}

// DiscoveredSessionFor rebuilds the part of a discovered session a harness
// indexer needs to parse the transcript: the session identity, the harness,
// the transcript location and the transcript shape.
//
// It is the one mapping from a listing to an ingest session for a preview.
// discovery.ListingSource is its other half; a new transcript origin must
// appear in both.
func DiscoveredSessionFor(listing onboarding.SessionListing) (ingest.DiscoveredSession, error) {
	sessionID, err := ingest.NewSessionID(listing.SessionID)
	if err != nil {
		return ingest.DiscoveredSession{}, fmt.Errorf("map the discovery listing to a transcript source in sourcepreview.DiscoveredSessionFor: the session id is not valid: %w; the session cannot be previewed; rescan so discovery records a valid id", err)
	}
	var origin ingest.TranscriptOrigin
	switch listing.Source.Origin {
	case onboarding.SessionSourceOriginFile:
		origin = ingest.TranscriptOriginFile
	case onboarding.SessionSourceOriginOpenCodeLegacySQLite:
		origin = ingest.TranscriptOriginOpenCodeLegacySQLite
	case onboarding.SessionSourceOriginOpenCodeCurrentSQLite:
		origin = ingest.TranscriptOriginOpenCodeCurrentSQLite
	default:
		return ingest.DiscoveredSession{}, listing.Source.Origin.Validate()
	}
	return ingest.DiscoveredSession{
		SessionID:        sessionID,
		Harness:          ingest.Harness(listing.Harness),
		SourcePath:       ingest.ResolvedPath(listing.Source.Path),
		OriginalRoot:     ingest.ResolvedPath(listing.Source.Root),
		TranscriptOrigin: origin,
	}, nil
}
