package discovery

import (
	"context"
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/transcript"
)

// ErrPreviewUnavailable reports that discovery recorded no transcript location
// for a session, so there is nothing to read. It is not a read failure: the
// caller can still describe the row.
var ErrPreviewUnavailable = errors.New("discovery recorded no transcript location for this session")

// SourcePreview is the first bounded page of one discovered session, read in
// place from the transcript its harness wrote.
type SourcePreview struct {
	// Turns are the turns the page folded to, in transcript order.
	Turns []ingest.Turn
	// ConsumedBytes and TotalBytes say how much of the session the page took and
	// how large the whole transcript is. TotalBytes is zero when the read could
	// not measure it.
	ConsumedBytes int64
	TotalBytes    int64
	// More reports that the session continues past the page.
	More bool
}

// SourceReader reads the first page of a discovered session for a preview. It
// reads the transcript through the harness indexers ingest uses and folds the
// entries with the conversion the session viewer uses, under the shared preview
// budget. It writes nothing to disk and nothing to the database, and it keeps
// nothing between reads.
type SourceReader struct {
	fs   ingest.FileSystem
	git  ingest.GitResolver
	salt salt.Salt
	byID map[string]SessionListing
	// budget bounds the bytes one read takes from a line-oriented file or a
	// SQLite session.
	budget int64
}

// NewSourceReader builds the reader over the discovery listing. A listing with
// no recorded transcript location is not previewable.
func NewSourceReader(fs ingest.FileSystem, git ingest.GitResolver, listings []SessionListing) *SourceReader {
	byID := make(map[string]SessionListing, len(listings))
	for _, listing := range listings {
		if listing.SessionID != "" && !listing.Source.IsZero() {
			byID[listing.SessionID] = listing
		}
	}
	return &SourceReader{fs: fs, git: git, byID: byID, budget: PreviewFirstPageBudgetBytes}
}

// Previewable reports that discovery recorded a transcript location for the
// session.
func (r *SourceReader) Previewable(sessionID string) bool {
	_, ok := r.byID[sessionID]
	return ok
}

// Read returns the first page of the session. It returns ErrPreviewUnavailable
// for a session discovery recorded no location for, and an actionable error when
// the transcript is present but cannot be read or parsed.
func (r *SourceReader) Read(ctx context.Context, sessionID string) (SourcePreview, error) {
	listing, ok := r.byID[sessionID]
	if !ok {
		return SourcePreview{}, ErrPreviewUnavailable
	}
	session, err := discoveredSessionFor(listing)
	if err != nil {
		return SourcePreview{}, fmt.Errorf("preview session %q from its harness source: %w", sessionID, err)
	}
	indexer, ok := ingest.NewIndexerRegistry(r.fs, ingest.IndexerRegistryOptions{FullContent: true})[session.Harness]
	if !ok {
		return SourcePreview{}, fmt.Errorf(
			"preview session %q from its harness source: harness %q has no transcript reader in internal/discovery.SourceReader; the row cannot be previewed; the session still ingests normally",
			sessionID, listing.Harness)
	}
	switch session.TranscriptOrigin {
	case ingest.TranscriptOriginOpenCodeLegacySQLite, ingest.TranscriptOriginOpenCodeCurrentSQLite:
		return r.readSQLite(ctx, sessionID, session, indexer)
	default:
		return r.readFile(ctx, sessionID, listing, session, indexer)
	}
}

func (r *SourceReader) readFile(ctx context.Context, sessionID string, listing SessionListing, session ingest.DiscoveredSession, indexer ingest.TranscriptIndexer) (SourcePreview, error) {
	if lines, ok := indexer.(ingest.LineOrientedTranscriptIndexer); ok && lines.RecordsAreLines() {
		slicer := ingest.NewFileTranscriptSlicer(r.fs)
		if slicer.Supported() {
			slice, err := slicer.MaterializeTranscriptSlice(ctx, session, r.budget, ingest.TranscriptSliceCursor{})
			if err != nil {
				return SourcePreview{}, fmt.Errorf("read the harness transcript of session %q at %q: %w", sessionID, listing.Source.Path, err)
			}
			return r.fold(ctx, sessionID, session, indexer, slice)
		}
	}
	entries, err := indexer.IndexTranscript(ctx, session)
	if err != nil {
		return SourcePreview{}, fmt.Errorf("read the harness transcript of session %q at %q: %w", sessionID, listing.Source.Path, err)
	}
	return SourcePreview{Turns: transcript.EntriesToTurns(entries)}, nil
}

func (r *SourceReader) readSQLite(ctx context.Context, sessionID string, session ingest.DiscoveredSession, indexer ingest.TranscriptIndexer) (SourcePreview, error) {
	factory, ok := ingest.DefaultAdapterRegistry[session.Harness]
	if !ok {
		return SourcePreview{}, fmt.Errorf("preview the SQLite transcript of session %q: harness %q has no discovery adapter in internal/discovery.SourceReader; the row cannot be previewed; the session still ingests normally", sessionID, session.Harness)
	}
	adapter := factory(r.fs, r.git, r.salt)
	if resumable, ok := adapter.(ingest.ResumableTranscriptMaterializer); ok {
		slice, err := resumable.MaterializeTranscriptSlice(ctx, session, r.budget, ingest.TranscriptSliceCursor{})
		if err != nil {
			return SourcePreview{}, fmt.Errorf("materialize the SQLite transcript of session %q for preview: %w", sessionID, err)
		}
		return r.fold(ctx, sessionID, session, indexer, slice)
	}
	if bounded, ok := adapter.(ingest.BoundedTranscriptMaterializer); ok {
		_, data, truncation, err := bounded.MaterializeTranscriptBounded(ctx, session, r.budget)
		if err != nil {
			return SourcePreview{}, fmt.Errorf("materialize the SQLite transcript of session %q for preview: %w", sessionID, err)
		}
		preview, err := r.foldBytes(ctx, sessionID, session, indexer, data)
		preview.ConsumedBytes, preview.TotalBytes, preview.More = truncation.IncludedBytes, truncation.TotalBytes, truncation.Truncated
		return preview, err
	}
	return SourcePreview{}, fmt.Errorf("preview the SQLite transcript of session %q: harness %q cannot bound a transcript read in internal/discovery.SourceReader; the row cannot be previewed; the session still ingests normally", sessionID, session.Harness)
}

func (r *SourceReader) fold(ctx context.Context, sessionID string, session ingest.DiscoveredSession, indexer ingest.TranscriptIndexer, slice ingest.TranscriptSlice) (SourcePreview, error) {
	preview, err := r.foldBytes(ctx, sessionID, session, indexer, slice.Data)
	if err != nil {
		return SourcePreview{}, err
	}
	preview.More = slice.More
	if slice.More {
		preview.ConsumedBytes, preview.TotalBytes = slice.Next.ConsumedBytes(), slice.Next.TotalBytes()
	}
	return preview, nil
}

func (r *SourceReader) foldBytes(ctx context.Context, sessionID string, session ingest.DiscoveredSession, indexer ingest.TranscriptIndexer, data []byte) (SourcePreview, error) {
	if len(data) == 0 {
		return SourcePreview{}, nil
	}
	entries, err := indexer.IndexTranscriptBytes(ctx, session, data)
	if err != nil {
		return SourcePreview{}, fmt.Errorf("read the transcript of session %q: %w", sessionID, err)
	}
	return SourcePreview{Turns: transcript.EntriesToTurns(entries)}, nil
}

// discoveredSessionFor rebuilds the small part of a discovered session a harness
// indexer needs to parse the transcript: the session identity, the harness, the
// transcript location, and the transcript shape. It is the other half of
// ListingSource.
func discoveredSessionFor(listing SessionListing) (ingest.DiscoveredSession, error) {
	sessionID, err := ingest.NewSessionID(listing.SessionID)
	if err != nil {
		return ingest.DiscoveredSession{}, fmt.Errorf("session id %q is not valid: %w", listing.SessionID, err)
	}
	var origin ingest.TranscriptOrigin
	switch listing.Source.Origin {
	case SessionSourceOriginFile:
		origin = ingest.TranscriptOriginFile
	case SessionSourceOriginOpenCodeLegacySQLite:
		origin = ingest.TranscriptOriginOpenCodeLegacySQLite
	case SessionSourceOriginOpenCodeCurrentSQLite:
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
