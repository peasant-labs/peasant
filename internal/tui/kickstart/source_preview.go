package kickstart

import (
	"context"
	"fmt"
	"sync"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
)

// DefaultSourceTurnsCacheSize is how many parsed sessions SourceTurns keeps.
// The selection tree previews one row at a time, so a small number covers the
// common move of stepping through a few rows and stepping back. The cache lives
// in memory only, and it goes away with the kickstart process.
const DefaultSourceTurnsCacheSize = 4

// ListingSource records where a discovered session's transcript is, so the
// selection step can read that transcript later without the discovery result.
//
// This function and sourceDiscoveredSession are the two halves of one mapping.
// Keep them together: a new transcript origin must appear in both.
func ListingSource(session ingest.DiscoveredSession) ftue.SessionSource {
	return ftue.SessionSource{
		Path:   string(session.SourcePath),
		Root:   string(session.OriginalRoot),
		Origin: listingSourceOrigin(session.TranscriptOrigin),
	}
}

func listingSourceOrigin(origin ingest.TranscriptOrigin) ftue.SessionSourceOrigin {
	switch origin {
	case ingest.TranscriptOriginOpenCodeLegacySQLite:
		return ftue.SessionSourceOriginOpenCodeLegacySQLite
	case ingest.TranscriptOriginOpenCodeCurrentSQLite:
		return ftue.SessionSourceOriginOpenCodeCurrentSQLite
	default:
		return ftue.SessionSourceOriginFile
	}
}

func ingestTranscriptOrigin(origin ftue.SessionSourceOrigin) (ingest.TranscriptOrigin, error) {
	switch origin {
	case ftue.SessionSourceOriginFile:
		return ingest.TranscriptOriginFile, nil
	case ftue.SessionSourceOriginOpenCodeLegacySQLite:
		return ingest.TranscriptOriginOpenCodeLegacySQLite, nil
	case ftue.SessionSourceOriginOpenCodeCurrentSQLite:
		return ingest.TranscriptOriginOpenCodeCurrentSQLite, nil
	default:
		return ingest.TranscriptOriginFile, origin.Validate()
	}
}

// sourceDiscoveredSession rebuilds the small part of a discovered session that
// a harness indexer needs to parse the transcript: the session identity, the
// harness, the transcript location, and the transcript shape.
func sourceDiscoveredSession(listing ftue.SessionListing) (ingest.DiscoveredSession, error) {
	sessionID, err := ingest.NewSessionID(listing.SessionID)
	if err != nil {
		return ingest.DiscoveredSession{}, fmt.Errorf("session id %q is not valid: %w", listing.SessionID, err)
	}
	origin, err := ingestTranscriptOrigin(listing.Source.Origin)
	if err != nil {
		return ingest.DiscoveredSession{}, err
	}
	return ingest.DiscoveredSession{
		SessionID:        sessionID,
		Harness:          ingest.Harness(listing.Harness),
		SourcePath:       ingest.ResolvedPath(listing.Source.Path),
		OriginalRoot:     ingest.ResolvedPath(listing.Source.Root),
		TranscriptOrigin: origin,
	}, nil
}

// SourceTurns reads the turns of a discovered session directly from the
// transcript its harness wrote.
//
// The selection step needs this because the local store holds only the sessions
// Peasant already imported. A first run holds none of them, and a new session
// appears before the next import. This reader gives every discovered session
// the same preview, whatever the store holds.
//
// It reads the transcript in place through the same harness indexers the ingest
// pipeline uses, and it folds the entries with the same conversion the session
// viewer uses. It writes nothing to disk and nothing to the database. It keeps
// the parsed turns of the last few sessions in memory only.
type SourceTurns struct {
	fs      ingest.FileSystem
	git     ingest.GitResolver
	salt    salt.Salt
	byID    map[string]ftue.SessionListing
	limit   int
	mu      sync.Mutex
	cached  map[string][]ingest.Turn
	recency []string
}

// SourceTurnsOption configures a SourceTurns reader.
type SourceTurnsOption func(*SourceTurns)

// WithSourceTurnsCacheSize sets how many parsed sessions the reader keeps in
// memory. A value of zero or less disables the cache, so every preview parses
// the transcript again.
func WithSourceTurnsCacheSize(limit int) SourceTurnsOption {
	return func(reader *SourceTurns) { reader.limit = limit }
}

// WithSourceTurnsGitResolver sets the git resolver the reader gives the
// materializer that turns a SQLite-discovered session into managed transcript
// bytes. The discovery path resolves a session's project the same way, so the
// preview and discovery agree on a session's project attribution.
func WithSourceTurnsGitResolver(git ingest.GitResolver) SourceTurnsOption {
	return func(reader *SourceTurns) { reader.git = git }
}

// WithSourceTurnsSalt sets the per-installation salt the reader gives the
// materializer. The preview reads a session's transcript, not its project hash,
// so the zero salt is the correct production default; this option exists so a
// test can inject a fixed salt.
func WithSourceTurnsSalt(s salt.Salt) SourceTurnsOption {
	return func(reader *SourceTurns) { reader.salt = s }
}

// NewSourceTurns builds the source reader over the discovery listing.
func NewSourceTurns(fs ingest.FileSystem, sessions []ftue.SessionListing, opts ...SourceTurnsOption) *SourceTurns {
	byID := make(map[string]ftue.SessionListing, len(sessions))
	for _, sess := range sessions {
		if sess.SessionID != "" && !sess.Source.IsZero() {
			byID[sess.SessionID] = sess
		}
	}
	reader := &SourceTurns{
		fs:     fs,
		git:    &ingest.ExecGitResolver{},
		byID:   byID,
		limit:  DefaultSourceTurnsCacheSize,
		cached: make(map[string][]ingest.Turn),
	}
	for _, opt := range opts {
		opt(reader)
	}
	return reader
}

// Turns implements SessionTurnsFunc over the harness transcript.
//
// It returns no turns and no error for a session whose transcript location
// discovery did not report. It returns an error when the transcript is present
// but Peasant cannot read or parse it, because a broken transcript is a real
// failure and the pane must say so.
func (s *SourceTurns) Turns(sessionID string) ([]ingest.Turn, error) {
	listing, ok := s.byID[sessionID]
	if !ok {
		return nil, nil
	}
	if turns, hit := s.lookup(sessionID); hit {
		return turns, nil
	}
	session, err := sourceDiscoveredSession(listing)
	if err != nil {
		return nil, fmt.Errorf("preview the transcript of session %q from its harness source: %w", sessionID, err)
	}
	turns, err := s.readTurns(sessionID, listing, session)
	if err != nil {
		return nil, err
	}
	s.store(sessionID, turns)
	return turns, nil
}

// readTurns produces the turns of one discovered session from its harness
// source. A SQLite-discovered session's SourcePath is the provider database,
// not a transcript file, so reading that path directly would load the whole
// database into memory. The materializer reads only the selected session's rows
// and returns the small managed projection, which the indexer then folds. A
// file-origin session keeps the direct path read.
func (s *SourceTurns) readTurns(sessionID string, listing ftue.SessionListing, session ingest.DiscoveredSession) ([]ingest.Turn, error) {
	switch session.TranscriptOrigin {
	case ingest.TranscriptOriginOpenCodeLegacySQLite, ingest.TranscriptOriginOpenCodeCurrentSQLite:
		return s.materializeTurns(sessionID, listing, session)
	default:
		return s.fileTurns(sessionID, listing, session)
	}
}

// fileTurns reads a file-origin session's transcript directly at its path.
func (s *SourceTurns) fileTurns(sessionID string, listing ftue.SessionListing, session ingest.DiscoveredSession) ([]ingest.Turn, error) {
	// Full content matches what the stored path shows: the session viewer
	// overlays the untruncated bodies over the database preview, so a source
	// preview that kept the database limit would cut turns off mid-word.
	indexer, ok := ingest.NewIndexerRegistry(s.fs, ingest.IndexerRegistryOptions{FullContent: true})[session.Harness]
	if !ok {
		return nil, fmt.Errorf(
			"preview the transcript of session %q from its harness source: harness %q has no transcript reader",
			sessionID, listing.Harness)
	}
	entries, err := indexer.IndexTranscript(context.Background(), session)
	if err != nil {
		return nil, fmt.Errorf("read the harness transcript of session %q at %q: %w", sessionID, listing.Source.Path, err)
	}
	return transcript.EntriesToTurns(entries), nil
}

// materializeTurns reads a SQLite-discovered session through the production
// materializer, then folds the managed projection bytes the same way ingest
// does. It never reads the database file at session.SourcePath directly, so a
// large provider database cannot exhaust memory. A failed materialization
// returns an actionable error the preview pane renders; it never aborts the
// program.
func (s *SourceTurns) materializeTurns(sessionID string, listing ftue.SessionListing, session ingest.DiscoveredSession) ([]ingest.Turn, error) {
	factory, ok := ingest.DefaultAdapterRegistry[session.Harness]
	if !ok {
		return nil, fmt.Errorf(
			"preview the SQLite transcript of session %q from its harness source: harness %q has no discovery adapter",
			sessionID, listing.Harness)
	}
	materializer, ok := factory(s.fs, s.git, s.salt).(ingest.TranscriptMaterializer)
	if !ok {
		return nil, fmt.Errorf(
			"preview the SQLite transcript of session %q from its harness source: harness %q cannot materialize a managed transcript",
			sessionID, listing.Harness)
	}
	_, data, err := materializer.MaterializeTranscript(context.Background(), session)
	if err != nil {
		return nil, fmt.Errorf("materialize the SQLite transcript of session %q for preview: %w", sessionID, err)
	}
	indexer, ok := ingest.NewIndexerRegistry(s.fs, ingest.IndexerRegistryOptions{FullContent: true})[session.Harness]
	if !ok {
		return nil, fmt.Errorf(
			"preview the SQLite transcript of session %q from its harness source: harness %q has no transcript reader",
			sessionID, listing.Harness)
	}
	entries, err := indexer.IndexTranscriptBytes(context.Background(), session, data)
	if err != nil {
		return nil, fmt.Errorf("read the materialized transcript of session %q: %w", sessionID, err)
	}
	return transcript.EntriesToTurns(entries), nil
}

// Previewable reports that discovery recorded a transcript location for the
// session. The preview uses it to tell a session it can still read from a
// session it can say nothing about.
func (s *SourceTurns) Previewable(sessionID string) bool {
	_, ok := s.byID[sessionID]
	return ok
}

func (s *SourceTurns) lookup(sessionID string) ([]ingest.Turn, bool) {
	if s.limit <= 0 {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	turns, ok := s.cached[sessionID]
	return turns, ok
}

// store keeps the parsed turns and drops the oldest entry once the cache is
// full, so a long scan cannot grow the memory of the wizard without a bound.
func (s *SourceTurns) store(sessionID string, turns []ingest.Turn) {
	if s.limit <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cached[sessionID]; !ok {
		s.recency = append(s.recency, sessionID)
	}
	s.cached[sessionID] = turns
	for len(s.recency) > s.limit {
		oldest := s.recency[0]
		s.recency = s.recency[1:]
		delete(s.cached, oldest)
	}
}
