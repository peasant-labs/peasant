package kickstart

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/peasant-labs/peasant/internal/defaults"
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
	fs    ingest.FileSystem
	git   ingest.GitResolver
	salt  salt.Salt
	byID  map[string]ftue.SessionListing
	limit int
	// budget, firstPage and slice bound the preview reads, and they are SHARED
	// by every origin on purpose. The file-origin preview deliberately mirrors
	// the OpenCode preview's bounds rather than choosing its own, by user
	// decision: the two paths do the same thing for the reader, and a second set
	// of numbers would be two things to keep in step for no stated benefit.
	budget int64
	// firstPage bounds the quickly-read leading slice the pane paints before
	// the full bounded read finishes. Zero or less turns the two-step read off,
	// so every preview waits for the full bounded read.
	firstPage int64
	// slice bounds ONE continuation - the chunk a scroll to the bottom of the
	// pane loads. Zero or less turns scrolled loading off, so the preview stops
	// at its first bounded read exactly as it did before.
	slice   int64
	mu      sync.Mutex
	cached  map[string]sourcePreview
	recency []string
}

// sourcePreview is one session read from its harness source: the turns to
// render, and the sentence (empty for most sessions) naming what the read left
// out. They are cached together because a cache hit must reproduce the whole
// pane, notice included.
//
// For a session read one slice at a time it is what has been loaded SO FAR:
// every slice appends its turns here and re-states the sentence, and cursor
// says where the next slice starts. The cache entry therefore grows as the
// reader scrolls, which is the point - they asked to see more of the session -
// while each READ stays one bounded slice.
type sourcePreview struct {
	turns  []ingest.Turn
	notice string
	// cursor continues the session after the turns above it. more reports that
	// there is something to continue to.
	cursor ingest.TranscriptSliceCursor
	more   bool
	// loaded is the RAW transcript bytes a FILE-origin preview has read so far,
	// and every slice re-parses and re-folds all of them.
	//
	// The obvious cheaper thing - parse each slice on its own and append its
	// turns - is wrong twice over. A turn folds from entries that span several
	// LINES: a tool call and the result that answers it are two lines, joined by
	// their call identifier, so a boundary between them would leave the call
	// resultless and drop the output entirely, since an orphan result is
	// suppressed rather than drawn. And every file indexer carries state ACROSS
	// records while it parses - which calls asked the user a question, which
	// parent a record belongs to - so a parse that started mid-file would
	// classify records the whole-file parse classifies differently.
	//
	// Re-reading is what a slice avoids; re-PARSING what is already in memory is
	// cheap by comparison and is what makes a sliced read produce exactly the
	// whole-file read. Holding the bytes costs no more than the whole-file read
	// this replaces, and less until the reader has scrolled the whole way.
	//
	// The SQLite path does not need any of this: a slice there is a whole
	// message with its parts, so its turns are final when they are folded.
	loaded []byte
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

// WithSourceTurnsPreviewBudget sets how many payload bytes of one session the
// reader materializes before it stops and reports the rest as left out. A value
// of zero or less removes the bound, which is only safe for a source whose
// sessions are known to be small.
func WithSourceTurnsPreviewBudget(budgetBytes int64) SourceTurnsOption {
	return func(reader *SourceTurns) { reader.budget = budgetBytes }
}

// WithSourceTurnsFirstPageBudget sets how many payload bytes of one session the
// reader materializes for the quickly-painted first slice. A value of zero or
// less removes the first slice, so a preview shows nothing until the full
// bounded read finishes.
func WithSourceTurnsFirstPageBudget(budgetBytes int64) SourceTurnsOption {
	return func(reader *SourceTurns) { reader.firstPage = budgetBytes }
}

// WithSourceTurnsSliceBudget sets how many payload bytes of one session each
// scrolled continuation materializes. A value of zero or less turns scrolled
// loading off, so the preview stops at its first bounded read.
func WithSourceTurnsSliceBudget(budgetBytes int64) SourceTurnsOption {
	return func(reader *SourceTurns) { reader.slice = budgetBytes }
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
		fs:        fs,
		git:       &ingest.ExecGitResolver{},
		byID:      byID,
		limit:     DefaultSourceTurnsCacheSize,
		budget:    defaults.OpenCodePreviewMaterializeMaxBytes,
		firstPage: defaults.OpenCodePreviewFirstPageMaxBytes,
		slice:     defaults.OpenCodePreviewSliceMaxBytes,
		cached:    make(map[string]sourcePreview),
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
	if preview, hit := s.lookup(sessionID); hit {
		return preview.turns, nil
	}
	session, err := sourceDiscoveredSession(listing)
	if err != nil {
		return nil, fmt.Errorf("preview the transcript of session %q from its harness source: %w", sessionID, err)
	}
	preview, err := s.readTurns(sessionID, listing, session)
	if err != nil {
		return nil, err
	}
	s.store(sessionID, preview)
	return preview.turns, nil
}

// FirstTurns reads a quickly-available LEADING slice of a session's turns and
// reports whether more of the session follows.
//
// It exists because the full bounded read of a very long session takes seconds,
// most of it spent measuring the session so the truncation note can name what
// it left out. This read skips the measurement and stops after the first-page
// budget, so the pane can paint turns while the full read is still running.
//
// A false more means the slice IS the whole session under the preview bound, so
// no further read is needed. A true more means the caller must still call Turns
// to get the result the pane finally shows.
//
// It caches ONLY a result that is already whole. A leading slice is never
// cached, and never becomes the answer a later Turns call returns, because that
// slice stands for less than the session and carries no note saying so.
func (s *SourceTurns) FirstTurns(sessionID string) ([]ingest.Turn, bool, error) {
	listing, ok := s.byID[sessionID]
	if !ok {
		return nil, false, nil
	}
	if preview, hit := s.lookup(sessionID); hit {
		return preview.turns, false, nil
	}
	session, err := sourceDiscoveredSession(listing)
	if err != nil {
		return nil, false, fmt.Errorf("preview the transcript of session %q from its harness source: %w", sessionID, err)
	}
	if slicer, ok := s.fileFirstPageSlicer(session); ok {
		// A line-oriented transcript reads its own leading slice: the first
		// screenful of a hundreds-of-megabyte file, without touching the rest.
		preview, err := s.fileSliceTurns(sessionID, listing, session, slicer, sourcePreview{}, s.firstPage)
		if err != nil {
			return nil, false, err
		}
		if !preview.more {
			// The slice reached the end of the file, so it IS the whole preview
			// and carries no note. Caching it is caching a complete result, and
			// a transcript that fits the first-page bound is read exactly once.
			s.store(sessionID, sourcePreview{turns: preview.turns, loaded: preview.loaded, cursor: preview.cursor})
			return preview.turns, false, nil
		}
		// A leading slice is never cached: it stands for less than the first
		// read the pane finally shows, which is read under the larger bound.
		return preview.turns, true, nil
	}
	firstPage, ok := s.firstPageReader(session)
	if !ok {
		// A source with no first-page read (a reader configured without the
		// bound, or a filesystem that cannot read a range) has one step only:
		// read it whole.
		preview, err := s.readTurns(sessionID, listing, session)
		if err != nil {
			return nil, false, err
		}
		s.store(sessionID, preview)
		return preview.turns, false, nil
	}
	turns, more, err := s.firstPageTurns(sessionID, listing, session, firstPage)
	if err != nil {
		return nil, false, err
	}
	if !more {
		// The slice reached the end of the session, so it is the whole preview
		// and carries no note. Caching it is caching a complete result.
		s.store(sessionID, sourcePreview{turns: turns})
	}
	return turns, more, nil
}

// firstPageReader reports the adapter that can read a leading slice of the
// given session, and false when this session has no two-step SQLite read: a
// file transcript (which has its own leading slice; see fileFirstTurns), a
// harness with no discovery adapter, an adapter that cannot bound a first
// slice, or a reader configured without a first-page budget.
func (s *SourceTurns) firstPageReader(session ingest.DiscoveredSession) (ingest.FirstPageTranscriptMaterializer, bool) {
	if s.firstPage <= 0 {
		return nil, false
	}
	switch session.TranscriptOrigin {
	case ingest.TranscriptOriginOpenCodeLegacySQLite, ingest.TranscriptOriginOpenCodeCurrentSQLite:
	default:
		return nil, false
	}
	factory, ok := ingest.DefaultAdapterRegistry[session.Harness]
	if !ok {
		return nil, false
	}
	firstPage, ok := factory(s.fs, s.git, s.salt).(ingest.FirstPageTranscriptMaterializer)
	return firstPage, ok
}

// fileFirstPageSlicer reports the slicer that can read a leading slice of a
// FILE-origin transcript, and false when this session is not one, the reader
// has no first-page bound, or the filesystem cannot read a byte range.
func (s *SourceTurns) fileFirstPageSlicer(session ingest.DiscoveredSession) (ingest.FileTranscriptSlicer, bool) {
	if s.firstPage <= 0 || s.slice <= 0 {
		return ingest.FileTranscriptSlicer{}, false
	}
	switch session.TranscriptOrigin {
	case ingest.TranscriptOriginOpenCodeLegacySQLite, ingest.TranscriptOriginOpenCodeCurrentSQLite:
		return ingest.FileTranscriptSlicer{}, false
	}
	// Slicing a transcript by lines is only correct for a format that keeps one
	// record per line, and only an indexer that DECLARES that gets sliced.
	indexer, ok := ingest.NewIndexerRegistry(s.fs, ingest.IndexerRegistryOptions{FullContent: true})[session.Harness]
	if !ok {
		return ingest.FileTranscriptSlicer{}, false
	}
	lines, ok := indexer.(ingest.LineOrientedTranscriptIndexer)
	if !ok || !lines.RecordsAreLines() {
		return ingest.FileTranscriptSlicer{}, false
	}
	slicer := ingest.NewFileTranscriptSlicer(s.fs)
	return slicer, slicer.Supported()
}

// firstPageTurns materializes the leading slice and folds it the same way the
// full read folds its own bytes, so the turns the pane paints first are the
// turns it keeps.
func (s *SourceTurns) firstPageTurns(sessionID string, listing ftue.SessionListing, session ingest.DiscoveredSession, firstPage ingest.FirstPageTranscriptMaterializer) ([]ingest.Turn, bool, error) {
	_, data, more, err := firstPage.MaterializeTranscriptFirstPage(context.Background(), session, s.firstPage)
	if err != nil {
		return nil, false, fmt.Errorf("materialize the first page of the SQLite transcript of session %q for preview: %w", sessionID, err)
	}
	indexer, ok := ingest.NewIndexerRegistry(s.fs, ingest.IndexerRegistryOptions{FullContent: true})[session.Harness]
	if !ok {
		return nil, false, fmt.Errorf(
			"preview the SQLite transcript of session %q from its harness source: harness %q has no transcript reader",
			sessionID, listing.Harness)
	}
	entries, err := indexer.IndexTranscriptBytes(context.Background(), session, data)
	if err != nil {
		return nil, false, fmt.Errorf("read the materialized first page of session %q: %w", sessionID, err)
	}
	return transcript.EntriesToTurns(entries), more, nil
}

// Notice implements SessionPreviewNoticeFunc over the harness transcript. It
// reports what the last read of the session left out, so it is meaningful only
// after Turns has read that session; the preview calls it in that order.
func (s *SourceTurns) Notice(sessionID string) string {
	preview, _ := s.lookup(sessionID)
	return preview.notice
}

// readTurns produces the turns of one discovered session from its harness
// source. A SQLite-discovered session's SourcePath is the provider database,
// not a transcript file, so reading that path directly would load the whole
// database into memory. The materializer reads only the selected session's rows
// and returns the small managed projection, which the indexer then folds. A
// file-origin session keeps the direct path read.
func (s *SourceTurns) readTurns(sessionID string, listing ftue.SessionListing, session ingest.DiscoveredSession) (sourcePreview, error) {
	switch session.TranscriptOrigin {
	case ingest.TranscriptOriginOpenCodeLegacySQLite, ingest.TranscriptOriginOpenCodeCurrentSQLite:
		return s.materializeTurns(sessionID, listing, session)
	default:
		return s.fileTurns(sessionID, listing, session)
	}
}

// fileTurns reads a file-origin session's transcript.
//
// A line-oriented transcript reaches hundreds of megabytes on a real machine,
// and reading it whole meant indexing every entry and folding every turn before
// anything appeared in the pane. When the filesystem can read a byte range, the
// preview takes it one bounded slice at a time instead and extends as the
// reader scrolls; otherwise it keeps the whole-file read exactly as before.
func (s *SourceTurns) fileTurns(sessionID string, listing ftue.SessionListing, session ingest.DiscoveredSession) (sourcePreview, error) {
	if slicer, ok := s.fileFirstPageSlicer(session); ok {
		return s.fileSliceTurns(sessionID, listing, session, slicer, sourcePreview{}, s.budget)
	}
	return s.wholeFileTurns(sessionID, listing, session)
}

// fileSliceTurns reads ONE slice of a line-oriented transcript and folds it
// together with everything already loaded. Passing the zero preview reads the
// first slice.
func (s *SourceTurns) fileSliceTurns(sessionID string, listing ftue.SessionListing, session ingest.DiscoveredSession, slicer ingest.FileTranscriptSlicer, loaded sourcePreview, budgetBytes int64) (sourcePreview, error) {
	slice, err := slicer.MaterializeTranscriptSlice(context.Background(), session, budgetBytes, loaded.cursor)
	if err != nil {
		return sourcePreview{}, fmt.Errorf("read the harness transcript of session %q at %q: %w", sessionID, listing.Source.Path, err)
	}
	next := sourcePreview{loaded: loaded.loaded, turns: loaded.turns, cursor: slice.Next, more: slice.More}
	if len(slice.Data) > 0 {
		indexer, ok := ingest.NewIndexerRegistry(s.fs, ingest.IndexerRegistryOptions{FullContent: true})[session.Harness]
		if !ok {
			return sourcePreview{}, fmt.Errorf(
				"preview the transcript of session %q from its harness source: harness %q has no transcript reader",
				sessionID, listing.Harness)
		}
		next.loaded = append(append(make([]byte, 0, len(loaded.loaded)+len(slice.Data)), loaded.loaded...), slice.Data...)
		// Parse and fold the WHOLE accumulated prefix, never one slice. See the
		// comment on sourcePreview.loaded for the two ways a per-slice parse
		// diverges from the read this has to match.
		entries, err := indexer.IndexTranscriptBytes(context.Background(), session, next.loaded)
		if err != nil {
			return sourcePreview{}, fmt.Errorf("read the harness transcript of session %q at %q: %w", sessionID, listing.Source.Path, err)
		}
		next.turns = transcript.EntriesToTurns(entries)
	}
	next.notice = previewSliceNotice(slice.Next, slice.More)
	return next, nil
}

// wholeFileTurns reads a file-origin session's transcript directly at its path.
func (s *SourceTurns) wholeFileTurns(sessionID string, listing ftue.SessionListing, session ingest.DiscoveredSession) (sourcePreview, error) {
	// Full content matches what the stored path shows: the session viewer
	// overlays the untruncated bodies over the database preview, so a source
	// preview that kept the database limit would cut turns off mid-word.
	indexer, ok := ingest.NewIndexerRegistry(s.fs, ingest.IndexerRegistryOptions{FullContent: true})[session.Harness]
	if !ok {
		return sourcePreview{}, fmt.Errorf(
			"preview the transcript of session %q from its harness source: harness %q has no transcript reader",
			sessionID, listing.Harness)
	}
	entries, err := indexer.IndexTranscript(context.Background(), session)
	if err != nil {
		return sourcePreview{}, fmt.Errorf("read the harness transcript of session %q at %q: %w", sessionID, listing.Source.Path, err)
	}
	return sourcePreview{turns: transcript.EntriesToTurns(entries)}, nil
}

// materializeTurns reads a SQLite-discovered session through the production
// materializer, then folds the managed projection bytes the same way ingest
// does. It never reads the database file at session.SourcePath directly, so a
// large provider database cannot exhaust memory. A failed materialization
// returns an actionable error the preview pane renders; it never aborts the
// program.
func (s *SourceTurns) materializeTurns(sessionID string, listing ftue.SessionListing, session ingest.DiscoveredSession) (sourcePreview, error) {
	factory, ok := ingest.DefaultAdapterRegistry[session.Harness]
	if !ok {
		return sourcePreview{}, fmt.Errorf(
			"preview the SQLite transcript of session %q from its harness source: harness %q has no discovery adapter",
			sessionID, listing.Harness)
	}
	adapter := factory(s.fs, s.git, s.salt)
	if resumable, ok := adapter.(ingest.ResumableTranscriptMaterializer); ok && s.slice > 0 {
		// A resumable adapter reads the FIRST slice through the same call every
		// continuation uses, so the preview the pane starts from and the preview
		// it scrolls into are one chain rather than two reads that have to agree.
		return s.sliceTurns(sessionID, listing, session, resumable, sourcePreview{}, s.budget)
	}
	data, truncation, err := s.materializeBytes(sessionID, listing, session, adapter)
	if err != nil {
		return sourcePreview{}, err
	}
	entries, err := s.foldManagedBytes(sessionID, listing, session, data)
	if err != nil {
		return sourcePreview{}, err
	}
	return sourcePreview{turns: entries, notice: previewTruncationNotice(truncation)}, nil
}

// sliceTurns reads ONE slice of a session and appends it to what is already
// loaded. Passing the zero preview reads the first slice.
//
// It never re-reads what an earlier slice already showed: a message belongs to
// exactly one slice, so the turns fold cleanly onto the ones before them.
func (s *SourceTurns) sliceTurns(sessionID string, listing ftue.SessionListing, session ingest.DiscoveredSession, resumable ingest.ResumableTranscriptMaterializer, loaded sourcePreview, budgetBytes int64) (sourcePreview, error) {
	slice, err := resumable.MaterializeTranscriptSlice(context.Background(), session, budgetBytes, loaded.cursor)
	if err != nil {
		return sourcePreview{}, fmt.Errorf("materialize a slice of the SQLite transcript of session %q for preview: %w", sessionID, err)
	}
	next := sourcePreview{turns: loaded.turns, cursor: slice.Next, more: slice.More}
	if len(slice.Data) > 0 {
		turns, err := s.foldManagedBytes(sessionID, listing, session, slice.Data)
		if err != nil {
			return sourcePreview{}, err
		}
		next.turns = append(append([]ingest.Turn{}, loaded.turns...), turns...)
	}
	next.notice = previewSliceNotice(slice.Next, slice.More)
	return next, nil
}

// foldManagedBytes folds one managed projection into turns the same way ingest
// does, so the preview and the imported session read alike.
func (s *SourceTurns) foldManagedBytes(sessionID string, listing ftue.SessionListing, session ingest.DiscoveredSession, data []byte) ([]ingest.Turn, error) {
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

// MoreTurns extends the preview of a session by ONE more slice and returns
// everything loaded so far, plus whether the session continues past it.
//
// It is what a scroll to the bottom of the preview pane asks for. The turns the
// pane retains grow as the reader scrolls, which is what they asked for; each
// READ stays one bounded slice, so no single call can pull an arbitrary amount
// of a 2 GiB session into memory.
//
// It returns the turns unchanged, and false, for a session that has nothing
// more to load: one already read whole, one whose origin cannot be continued,
// or one this reader has not read yet.
func (s *SourceTurns) MoreTurns(sessionID string) ([]ingest.Turn, bool, error) {
	listing, ok := s.byID[sessionID]
	if !ok {
		return nil, false, nil
	}
	loaded, hit := s.lookup(sessionID)
	if !hit || !loaded.more || s.slice <= 0 {
		return loaded.turns, false, nil
	}
	session, err := sourceDiscoveredSession(listing)
	if err != nil {
		return nil, false, fmt.Errorf("preview the transcript of session %q from its harness source: %w", sessionID, err)
	}
	next, err := s.continueTurns(sessionID, listing, session, loaded)
	if err != nil {
		return nil, false, err
	}
	if next == nil {
		return loaded.turns, false, nil
	}
	s.store(sessionID, *next)
	return next.turns, next.more, nil
}

// continueTurns reads one more slice through whichever seam this session's
// origin is served by, and returns nil when the origin has no continuation.
func (s *SourceTurns) continueTurns(sessionID string, listing ftue.SessionListing, session ingest.DiscoveredSession, loaded sourcePreview) (*sourcePreview, error) {
	switch session.TranscriptOrigin {
	case ingest.TranscriptOriginOpenCodeLegacySQLite, ingest.TranscriptOriginOpenCodeCurrentSQLite:
		factory, ok := ingest.DefaultAdapterRegistry[session.Harness]
		if !ok {
			return nil, nil
		}
		resumable, ok := factory(s.fs, s.git, s.salt).(ingest.ResumableTranscriptMaterializer)
		if !ok {
			return nil, nil
		}
		next, err := s.sliceTurns(sessionID, listing, session, resumable, loaded, s.slice)
		if err != nil {
			return nil, err
		}
		return &next, nil
	default:
		slicer, ok := s.fileFirstPageSlicer(session)
		if !ok {
			return nil, nil
		}
		next, err := s.fileSliceTurns(sessionID, listing, session, slicer, loaded, s.slice)
		if err != nil {
			return nil, err
		}
		return &next, nil
	}
}

// HasMore reports whether the preview of a session, as currently loaded, has
// more of that session behind it. It reads only what is already in memory, so
// the pane can ask on its own goroutine.
func (s *SourceTurns) HasMore(sessionID string) bool {
	preview, _ := s.lookup(sessionID)
	return preview.more
}

// materializeBytes produces the managed transcript bytes the preview folds.
// It prefers the bounded materialization, so one very long session cannot pull
// its whole payload into memory; an adapter that cannot bound itself falls back
// to the whole-session read it already supported.
func (s *SourceTurns) materializeBytes(sessionID string, listing ftue.SessionListing, session ingest.DiscoveredSession, adapter ingest.SourceAdapter) ([]byte, ingest.MaterializeTruncation, error) {
	if bounded, ok := adapter.(ingest.BoundedTranscriptMaterializer); ok && s.budget > 0 {
		_, data, truncation, err := bounded.MaterializeTranscriptBounded(context.Background(), session, s.budget)
		if err != nil {
			return nil, ingest.MaterializeTruncation{}, fmt.Errorf("materialize the SQLite transcript of session %q for preview: %w", sessionID, err)
		}
		return data, truncation, nil
	}
	materializer, ok := adapter.(ingest.TranscriptMaterializer)
	if !ok {
		return nil, ingest.MaterializeTruncation{}, fmt.Errorf(
			"preview the SQLite transcript of session %q from its harness source: harness %q cannot materialize a managed transcript",
			sessionID, listing.Harness)
	}
	_, data, err := materializer.MaterializeTranscript(context.Background(), session)
	if err != nil {
		return nil, ingest.MaterializeTruncation{}, fmt.Errorf("materialize the SQLite transcript of session %q for preview: %w", sessionID, err)
	}
	return data, ingest.MaterializeTruncation{}, nil
}

// previewSliceNotice writes the sentence the pane shows above the turns of a
// session it is reading one slice at a time.
//
// It is LIVE: every slice re-states it, so the figures follow what is actually
// on screen instead of describing a bound that was true when the pane first
// painted. It says what is loaded, what the whole session holds, and how to
// load more - because a reader who cannot see why the transcript stops has no
// way to know that scrolling continues it. It keeps the reassurance the fixed
// note carried, since a cut-off transcript otherwise reads as data loss.
//
// A session with nothing more behind it gets no sentence at all: there is
// nothing to explain.
func previewSliceNotice(cursor ingest.TranscriptSliceCursor, more bool) string {
	if !more || cursor.TotalBytes() <= 0 {
		return ""
	}
	return fmt.Sprintf(
		"showing the first %s of this %s session. scroll to the bottom to load more. the full session ingests normally.",
		previewByteSize(cursor.ConsumedBytes()), previewByteSize(cursor.TotalBytes()))
}

// previewTruncationNotice writes the sentence the pane shows above the turns of
// a session it read only in part. It states what is on screen, what the whole
// session holds, that the bound is a preview bound alone, and that ingest still
// takes the whole session, because a reader who sees a cut-off transcript would
// otherwise read it as data loss.
func previewTruncationNotice(truncation ingest.MaterializeTruncation) string {
	if !truncation.Truncated {
		return ""
	}
	return fmt.Sprintf(
		"preview shows the first %s of this %s session (%s of %s %s). the full session ingests normally. this bound applies to the preview only.",
		previewByteSize(truncation.IncludedBytes), previewByteSize(truncation.TotalBytes),
		previewCount(truncation.IncludedRows), previewCount(truncation.TotalRows), truncation.Unit)
}

// previewByteSize renders a byte count the way the note reads it aloud: whole
// mebibytes past a mebibyte, kibibytes below that.
func previewByteSize(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%d MiB", size/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%d KiB", size/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", size)
	}
}

// previewCount groups a row count in thousands so a six-figure count stays
// readable in one glance.
func previewCount(count int64) string {
	digits := strconv.FormatInt(count, 10)
	if len(digits) <= 3 {
		return digits
	}
	var grouped strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		grouped.WriteString(digits[:lead])
	}
	for index := lead; index < len(digits); index += 3 {
		if grouped.Len() > 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteString(digits[index : index+3])
	}
	return grouped.String()
}

// Previewable reports that discovery recorded a transcript location for the
// session. The preview uses it to tell a session it can still read from a
// session it can say nothing about.
func (s *SourceTurns) Previewable(sessionID string) bool {
	_, ok := s.byID[sessionID]
	return ok
}

func (s *SourceTurns) lookup(sessionID string) (sourcePreview, bool) {
	if s.limit <= 0 {
		return sourcePreview{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	preview, ok := s.cached[sessionID]
	return preview, ok
}

// store keeps the parsed turns and drops the oldest entry once the cache is
// full, so a long scan cannot grow the memory of the wizard without a bound.
func (s *SourceTurns) store(sessionID string, preview sourcePreview) {
	if s.limit <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cached[sessionID]; !ok {
		s.recency = append(s.recency, sessionID)
	}
	s.cached[sessionID] = preview
	for len(s.recency) > s.limit {
		oldest := s.recency[0]
		s.recency = s.recency[1:]
		delete(s.cached, oldest)
	}
}
