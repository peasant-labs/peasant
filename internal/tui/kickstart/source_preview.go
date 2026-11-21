package kickstart

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sourcepreview"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
)

// DefaultSourceTurnsCacheSize is how many session captures SourceTurns keeps
// open. The selection tree previews one row at a time, so a small number covers
// the common move of stepping through a few rows and stepping back. Captures
// live in memory only; an evicted one is closed and its loaded prefix cleared.
const DefaultSourceTurnsCacheSize = 4

// ListingSource records where a discovered session's transcript is, so the
// selection step can read that transcript later without the discovery result.
// sourcepreview.DiscoveredSessionFor is its other half.
func ListingSource(session ingest.DiscoveredSession) ftue.SessionSource {
	return discovery.ListingSource(session)
}

// TerminalCaptureOptions are the bounds of one terminal preview capture: the
// shared first and later read sizes, the terminal's own cumulative source cap
// and no turn cap. They are not the web preview's options.
func TerminalCaptureOptions() sourcepreview.CaptureOptions {
	return sourcepreview.CaptureOptions{
		FirstReadBytes: defaults.TranscriptInitialReadBytes,
		SliceReadBytes: defaults.TranscriptContinuationReadBytes,
		SourceCapBytes: defaults.KickstartTerminalPreviewSourceCapBytes,
		TurnCap:        0,
	}
}

// The sentences the pane shows BELOW the loaded turns when a preview ends early
// for a reason the reader can act on. A capture that was cut, or met a record
// too large to read, already carries its own sentence and the pane shows that.
// Moving the cursor off the session and back opens a fresh capture, which is
// the terminal's "read again".
const (
	SourceChangedNotice = "this session's transcript changed after part of it was shown. move to another row and back to read it again from the start."
	LateResultNotice    = "a later part of this session answers a tool call in a turn already shown. move to another row and back to read it again from the start."
	GrownNotice         = "this session has grown since the preview opened. move to another row and back to read the newer part."
)

// SourceTurns previews discovered sessions in place, from the transcript each
// harness wrote, through the shared source preview capture.
//
// The selection step needs this because the local store holds only the sessions
// Peasant already imported. A first run holds none of them, and a new session
// appears before the next import.
//
// It is a small adapter: it fills the preview's first, whole and continued
// reads from one capture per session, keeps the captures of the last few
// sessions open, and turns the capture's end states into sentences. It writes
// nothing to disk and nothing to the database.
type SourceTurns struct {
	fs    ingest.FileSystem
	git   ingest.GitResolver
	salt  salt.Salt
	opts  sourcepreview.CaptureOptions
	limit int

	reader *sourcepreview.Reader

	mu      sync.Mutex
	entries map[string]*sourceCapture
	recency []string
}

// sourceCapture is one session's open capture and what it has handed out.
//
// read serializes the capture's reads. The view fields are guarded by the
// adapter's lock instead, so the UI goroutine can ask what is loaded without
// waiting for a read in flight.
type sourceCapture struct {
	ctx     context.Context
	cancel  context.CancelFunc
	capture atomic.Pointer[sourcepreview.Capture]

	read     sync.Mutex
	opened   bool
	bodyRead bool

	// Guarded by SourceTurns.mu.
	turns     []ingest.Turn
	more      bool
	notice    string
	endNotice string
	// reopen marks a capture that ended in a state only a fresh capture can
	// answer: the next first or whole read opens a new one.
	reopen bool
}

// SourceTurnsOption configures a SourceTurns reader.
type SourceTurnsOption func(*SourceTurns)

// WithSourceTurnsCacheSize sets how many session captures the reader keeps
// open. A value below one keeps one, so a scrolled preview can always continue.
func WithSourceTurnsCacheSize(limit int) SourceTurnsOption {
	return func(reader *SourceTurns) { reader.limit = max(limit, 1) }
}

// WithSourceTurnsGitResolver sets the git resolver the SQLite materializer
// uses, so the preview and discovery attribute a session's project alike.
func WithSourceTurnsGitResolver(git ingest.GitResolver) SourceTurnsOption {
	return func(reader *SourceTurns) { reader.git = git }
}

// WithSourceTurnsSalt sets the per-installation salt the materializer
// receives. The preview reads a transcript, not a project hash, so the zero
// salt is the production default; this option exists so a test can pin one.
func WithSourceTurnsSalt(s salt.Salt) SourceTurnsOption {
	return func(reader *SourceTurns) { reader.salt = s }
}

// WithSourceTurnsCaptureOptions replaces the terminal's capture options. The
// mounted command never passes it; a test or a capture harness uses it to reach
// a bound with a small synthetic transcript.
func WithSourceTurnsCaptureOptions(opts sourcepreview.CaptureOptions) SourceTurnsOption {
	return func(reader *SourceTurns) { reader.opts = opts }
}

// NewSourceTurns builds the source reader over the discovery listing.
func NewSourceTurns(fs ingest.FileSystem, sessions []ftue.SessionListing, opts ...SourceTurnsOption) *SourceTurns {
	reader := &SourceTurns{
		fs:      fs,
		git:     &ingest.ExecGitResolver{},
		opts:    TerminalCaptureOptions(),
		limit:   DefaultSourceTurnsCacheSize,
		entries: make(map[string]*sourceCapture),
	}
	for _, opt := range opts {
		opt(reader)
	}
	reader.reader = sourcepreview.NewReader(fs, sessions,
		sourcepreview.WithGitResolver(reader.git), sourcepreview.WithSalt(reader.salt))
	return reader
}

// CaptureOptions reports the options every capture this reader opens uses.
func (s *SourceTurns) CaptureOptions() sourcepreview.CaptureOptions { return s.opts }

// Previewable reports that discovery recorded a transcript location for the
// session.
func (s *SourceTurns) Previewable(sessionID string) bool {
	return s.reader.Previewable(sessionID)
}

// Turns implements SessionTurnsFunc: it opens the session's capture when none
// is open and reads one continuation after the first read, so a pane that
// paints in one step shows as much as a two-step pane finally does.
//
// It returns no turns and no error for a session whose transcript location
// discovery did not report, or whose transcript holds no readable turns. It
// returns an error when the transcript is present but cannot be read, because a
// broken transcript is a real failure and the pane must say so.
func (s *SourceTurns) Turns(sessionID string) ([]ingest.Turn, error) {
	if !s.Previewable(sessionID) {
		return nil, nil
	}
	entry := s.entryFor(sessionID)
	entry.read.Lock()
	defer entry.read.Unlock()
	if err := s.openLocked(sessionID, entry); err != nil {
		return nil, err
	}
	if !entry.bodyRead {
		entry.bodyRead = true
		if err := s.nextLocked(sessionID, entry); err != nil {
			return nil, err
		}
	}
	turns, _ := s.view(entry)
	return turns, nil
}

// FirstTurns implements SessionFirstTurnsFunc: it opens the session's capture
// and returns its first page, reporting whether the whole read should follow.
// A session whose capture is already open answers from memory.
func (s *SourceTurns) FirstTurns(sessionID string) ([]ingest.Turn, bool, error) {
	if !s.Previewable(sessionID) {
		return nil, false, nil
	}
	entry := s.entryFor(sessionID)
	entry.read.Lock()
	defer entry.read.Unlock()
	if err := s.openLocked(sessionID, entry); err != nil {
		return nil, false, err
	}
	turns, more := s.view(entry)
	follow := more && !entry.bodyRead
	if !follow {
		entry.bodyRead = true
	}
	return turns, follow, nil
}

// MoreTurns implements SessionMoreTurnsFunc: it reads the next page of an open
// capture and returns every turn loaded so far, and whether more remains. A
// session with no open capture, or one that ended, returns what it holds and
// false.
func (s *SourceTurns) MoreTurns(sessionID string) ([]ingest.Turn, bool, error) {
	entry, ok := s.lookup(sessionID)
	if !ok {
		return nil, false, nil
	}
	entry.read.Lock()
	defer entry.read.Unlock()
	entry.bodyRead = true
	if err := s.nextLocked(sessionID, entry); err != nil {
		return nil, false, err
	}
	turns, more := s.view(entry)
	return turns, more, nil
}

// HasMore implements SessionHasMoreFunc. It answers from memory.
func (s *SourceTurns) HasMore(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[sessionID]
	return ok && entry.more && !entry.reopen
}

// Notice implements SessionPreviewNoticeFunc: while the capture can load more,
// it says how much is loaded and how to load more. It reads only memory.
func (s *SourceTurns) Notice(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[sessionID]; ok {
		return entry.notice
	}
	return ""
}

// EndNotice implements SessionPreviewEndNoticeFunc, the sentence below the
// turns: why the preview ended before the session did, or that the session
// grew. It reads only memory.
func (s *SourceTurns) EndNotice(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[sessionID]; ok {
		return entry.endNotice
	}
	return ""
}

// RetainedBytes reports what the open captures hold in memory between reads.
func (s *SourceTurns) RetainedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, entry := range s.entries {
		if capture := entry.capture.Load(); capture != nil {
			total += capture.RetainedBytes()
		}
	}
	return total
}

// entryFor returns the session's entry, replacing one that must be read again,
// and closes the captures the cache bound evicts.
func (s *SourceTurns) entryFor(sessionID string) *sourceCapture {
	var evicted []*sourceCapture
	defer func() {
		for _, entry := range evicted {
			closeEntry(entry)
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[sessionID]; ok && !entry.reopen {
		s.touchLocked(sessionID)
		return entry
	} else if ok {
		evicted = append(evicted, entry)
	}
	ctx, cancel := context.WithCancel(context.Background())
	entry := &sourceCapture{ctx: ctx, cancel: cancel}
	s.entries[sessionID] = entry
	s.touchLocked(sessionID)
	for len(s.recency) > s.limit {
		oldest := s.recency[0]
		s.recency = s.recency[1:]
		evicted = append(evicted, s.entries[oldest])
		delete(s.entries, oldest)
	}
	return entry
}

func (s *SourceTurns) touchLocked(sessionID string) {
	for i, id := range s.recency {
		if id == sessionID {
			s.recency = append(s.recency[:i], s.recency[i+1:]...)
			break
		}
	}
	s.recency = append(s.recency, sessionID)
}

func (s *SourceTurns) lookup(sessionID string) (*sourceCapture, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[sessionID]
	return entry, ok
}

// closeEntry cancels an entry's capture and clears its loaded prefix at once.
// It runs outside the adapter's lock: closing waits for a read in flight.
func closeEntry(entry *sourceCapture) {
	entry.cancel()
	if capture := entry.capture.Load(); capture != nil {
		capture.Close()
	}
}

func (s *SourceTurns) view(entry *sourceCapture) ([]ingest.Turn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return entry.turns, entry.more && !entry.reopen
}

// openLocked opens the entry's capture once. The caller holds entry.read.
func (s *SourceTurns) openLocked(sessionID string, entry *sourceCapture) error {
	if entry.opened {
		return nil
	}
	entry.opened = true
	capture, page, err := s.reader.Open(entry.ctx, sessionID, s.opts)
	if err != nil {
		reason, _ := sourcepreview.ReasonOf(err)
		switch reason {
		case sourcepreview.CloseUnavailable, sourcepreview.CloseNoTurns:
			// Nothing to show: the pane explains an empty or absent transcript
			// itself.
			return nil
		}
		s.markReopen(entry)
		return fmt.Errorf("preview the transcript of this session from its harness source in internal/tui/kickstart: %w", err)
	}
	entry.capture.Store(capture)
	if entry.ctx.Err() != nil {
		// Evicted while it opened: clear what it loaded now.
		capture.Close()
	}
	s.apply(entry, capture, page)
	return nil
}

// nextLocked reads one more page when the capture can load more. The caller
// holds entry.read.
func (s *SourceTurns) nextLocked(sessionID string, entry *sourceCapture) error {
	capture := entry.capture.Load()
	if _, more := s.view(entry); !more || capture == nil {
		return nil
	}
	page, err := capture.Next(entry.ctx)
	if err == nil {
		s.apply(entry, capture, page)
		return nil
	}
	reason, _ := sourcepreview.ReasonOf(err)
	switch reason {
	case sourcepreview.CloseBusy:
		return nil
	case sourcepreview.CloseClosed:
		s.end(entry, "", false)
		return nil
	case sourcepreview.CloseSourceChanged:
		s.end(entry, SourceChangedNotice, true)
		return nil
	case sourcepreview.CloseLateResult:
		s.end(entry, LateResultNotice, true)
		return nil
	}
	s.end(entry, "", true)
	return fmt.Errorf("continue the preview of this session from its harness source in internal/tui/kickstart: %w", err)
}

// apply records a page: its turns join the loaded ones, and its continuation
// decides the sentences above and below them.
func (s *SourceTurns) apply(entry *sourceCapture, capture *sourcepreview.Capture, page sourcepreview.Page) {
	notice := ""
	if page.Continuation == sourcepreview.ContinuationMore {
		consumed, total := capture.Progress()
		notice = discovery.PreviewCutNotice(consumed, total, true, true)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(page.Turns) > 0 {
		entry.turns = append(append(make([]ingest.Turn, 0, len(entry.turns)+len(page.Turns)), entry.turns...), page.Turns...)
	}
	entry.more = page.Continuation == sourcepreview.ContinuationMore
	entry.notice = notice
	switch page.Continuation {
	case sourcepreview.ContinuationCut:
		entry.endNotice = page.Notice
	case sourcepreview.ContinuationEnd:
		if page.Notice == sourcepreview.GrownNotice {
			entry.endNotice, entry.reopen = GrownNotice, true
		}
	}
}

// end records that the capture ended on an error, keeping the turns it
// already handed out.
func (s *SourceTurns) end(entry *sourceCapture, endNotice string, reopen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.more = false
	entry.notice = ""
	entry.endNotice = endNotice
	entry.reopen = entry.reopen || reopen
}

func (s *SourceTurns) markReopen(entry *sourceCapture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.reopen = true
}
