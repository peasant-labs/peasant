package kickstart

import (
	"context"
	"fmt"
	"sync"

	"github.com/peasant-labs/schema"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/transcript"
)

// StoredEntryReader is the narrow local-store seam the paged stored preview
// reads through: how far a session's recorded entries go, and one index range
// of them.
//
// It is an interface rather than the store itself so the preview depends on
// the two reads it makes and nothing else, and so a test can drive the paging
// without a database.
type StoredEntryReader interface {
	// MaxEntryIndex reports the highest entry index the store holds for the
	// session, or -1 when it holds none.
	MaxEntryIndex(ctx context.Context, sessionID schema.SessionID) (int, error)
	// ListEntriesRange returns the session's entries whose index falls in
	// [fromIndex, toIndex], both ends included, ordered by index.
	ListEntriesRange(ctx context.Context, sessionID schema.SessionID, fromIndex, toIndex int) ([]schema.SessionEntry, error)
}

// StoredContentOverlayFunc recovers the untruncated body of every turn of one
// session, keyed by entry index, by re-reading the transcript its harness
// wrote.
//
// It exists because the store keeps each entry's body only up to
// defaults.ContentPreviewLimit, so turns read straight out of the store stop
// mid-word on any long message. It is the same recovery the session viewer
// makes, through the same builder.
//
// It is best-effort by contract, exactly as the viewer's is: a session whose
// source transcript has moved, been deleted, or cannot be parsed returns no
// overlay and no error, and its turns keep the bodies the store holds.
type StoredContentOverlayFunc func(sessionID string) (map[int]string, error)

// DefaultStoredTurnsCacheSize is how many stored sessions StoredTurns keeps
// loaded. It matches DefaultSourceTurnsCacheSize: the two readers serve the
// same pane, one row at a time, and a reader that kept a different number of
// sessions would make the pane behave differently depending on which one
// answered.
const DefaultStoredTurnsCacheSize = DefaultSourceTurnsCacheSize

// storedPreview is one session read from the local store: the entries loaded so
// far, the turns they fold to, where the next slice starts, and the sentence
// naming what is on screen.
//
// It keeps the ENTRIES, not only the turns, because every slice re-folds the
// whole accumulated prefix. See foldLoaded for why that is the only correct
// fold at a slice seam.
type storedPreview struct {
	entries []schema.SessionEntry
	turns   []ingest.Turn
	notice  string
	// next is the entry index the following slice starts at. total is how many
	// entry indices the session spans, and more reports that next has not
	// passed it yet.
	next  int
	total int
	more  bool
	// overlay holds the untruncated bodies, and overlaid records that the
	// recovery has already been attempted, so a session whose source cannot be
	// read does not re-attempt it on every slice.
	overlay  map[int]string
	overlaid bool
}

// StoredTurns reads the turns of an already-imported session out of the local
// store ONE INDEX RANGE AT A TIME.
//
// The preview needs this because a real recorded session reaches tens of
// thousands of entries: the longest session in a personal store of 10,454 held
// 42,731 of them. Reading all of them to draw a preview took about 1.7 seconds
// and produced 2,138 turns, of which the pane then drew its standing bound of
// 200 and said the rest was not shown - so the reader waited seconds for a
// transcript that stopped and could not be scrolled any further. This reader
// takes the first 2,000 entries in about 5 milliseconds instead and extends the
// preview as the reader scrolls, which is what the harness-source paths beside
// it already do.
//
// It writes nothing. It holds the loaded prefix of the last few sessions in
// memory only, and it goes away with the kickstart process.
type StoredTurns struct {
	reader  StoredEntryReader
	overlay StoredContentOverlayFunc
	// firstPage bounds the quickly-read leading slice; slice bounds every read
	// the pane keeps. Zero or less in either turns that step off: no first page
	// means the pane waits for the first kept read, and no slice bound means
	// there is no paging left to do.
	firstPage int
	slice     int
	limit     int

	mu      sync.Mutex
	cached  map[string]storedPreview
	recency []string
}

// StoredTurnsOption configures a StoredTurns reader.
type StoredTurnsOption func(*StoredTurns)

// WithStoredTurnsContentOverlay supplies the untruncated-body recovery. Without
// it the preview shows the bodies the store holds, which stop at
// defaults.ContentPreviewLimit.
func WithStoredTurnsContentOverlay(overlay StoredContentOverlayFunc) StoredTurnsOption {
	return func(reader *StoredTurns) { reader.overlay = overlay }
}

// WithStoredTurnsFirstPageEntries sets how many entries the quickly-painted
// leading slice reads. A value of zero or less removes that slice, so the pane
// shows nothing until the first kept read finishes.
func WithStoredTurnsFirstPageEntries(entries int) StoredTurnsOption {
	return func(reader *StoredTurns) { reader.firstPage = entries }
}

// WithStoredTurnsSliceEntries sets how many entries each kept read covers. A
// value of zero or less turns paging off, so the preview reads the session
// whole.
func WithStoredTurnsSliceEntries(entries int) StoredTurnsOption {
	return func(reader *StoredTurns) { reader.slice = entries }
}

// WithStoredTurnsCacheSize sets how many stored sessions the reader keeps
// loaded. A value of zero or less disables the cache, which also disables
// scrolled continuation: a continuation extends what the previous read left,
// and nothing is left to extend.
func WithStoredTurnsCacheSize(limit int) StoredTurnsOption {
	return func(reader *StoredTurns) { reader.limit = limit }
}

// NewStoredTurns builds the paged stored reader over a local-store seam.
func NewStoredTurns(reader StoredEntryReader, opts ...StoredTurnsOption) *StoredTurns {
	stored := &StoredTurns{
		reader:    reader,
		firstPage: defaults.StoredPreviewFirstPageEntries,
		slice:     defaults.StoredPreviewSliceEntries,
		limit:     DefaultStoredTurnsCacheSize,
		cached:    make(map[string]storedPreview),
	}
	for _, opt := range opts {
		opt(stored)
	}
	return stored
}

// FirstTurns implements SessionFirstTurnsFunc over the local store: it reads
// the leading slice of a session and reports whether more of it follows.
//
// A true more means the pane paints this slice and then asks Turns for the read
// it keeps. A false more means the slice IS the whole session, and that read is
// the only one: a short session is therefore loaded exactly as it was before
// this reader existed, in one step.
func (s *StoredTurns) FirstTurns(sessionID string) ([]ingest.Turn, bool, error) {
	if loaded, hit := s.lookup(sessionID); hit {
		return loaded.turns, false, nil
	}
	if s.firstPage <= 0 {
		turns, err := s.Turns(sessionID)
		return turns, false, err
	}
	preview, err := s.read(sessionID, storedPreview{}, s.firstPage)
	if err != nil {
		return nil, false, err
	}
	if preview.more {
		// A leading slice is never kept. It stands for less of the session than
		// the read the pane settles on, and it carries none of the untruncated
		// bodies that read recovers.
		return preview.turns, true, nil
	}
	final, err := s.finish(sessionID, preview)
	if err != nil {
		return nil, false, err
	}
	s.store(sessionID, final)
	return final.turns, false, nil
}

// Turns implements SessionTurnsFunc over the local store. It is the first read
// the pane KEEPS, so it is bounded by the slice size rather than the smaller
// first page, and it carries the untruncated turn bodies.
func (s *StoredTurns) Turns(sessionID string) ([]ingest.Turn, error) {
	if loaded, hit := s.lookup(sessionID); hit {
		return loaded.turns, nil
	}
	preview, err := s.read(sessionID, storedPreview{}, s.sliceEntries())
	if err != nil {
		return nil, err
	}
	final, err := s.finish(sessionID, preview)
	if err != nil {
		return nil, err
	}
	s.store(sessionID, final)
	return final.turns, nil
}

// MoreTurns implements SessionMoreTurnsFunc over the local store: it extends
// the loaded preview by one more slice and returns EVERYTHING loaded so far, so
// the pane replaces its body with the same transcript plus more below it.
//
// A session with nothing more behind it, or one this reader has not loaded, is
// returned unchanged.
func (s *StoredTurns) MoreTurns(sessionID string) ([]ingest.Turn, bool, error) {
	loaded, hit := s.lookup(sessionID)
	if !hit || !loaded.more || s.slice <= 0 {
		return loaded.turns, false, nil
	}
	preview, err := s.read(sessionID, loaded, s.sliceEntries())
	if err != nil {
		return nil, false, err
	}
	final, err := s.finish(sessionID, preview)
	if err != nil {
		return nil, false, err
	}
	s.store(sessionID, final)
	return final.turns, final.more, nil
}

// HasMore implements SessionHasMoreFunc. It answers from what is already
// loaded and reads nothing, so the pane can ask on its own goroutine.
func (s *StoredTurns) HasMore(sessionID string) bool {
	loaded, _ := s.lookup(sessionID)
	return loaded.more
}

// Loaded reports whether this reader is the one holding the given session's
// preview. The mounted preview reads one session from the store and another
// from its harness source, and it asks this to know which of the two a
// continuation, a notice, or a scroll belongs to.
func (s *StoredTurns) Loaded(sessionID string) bool {
	_, hit := s.lookup(sessionID)
	return hit
}

// Notice implements SessionPreviewNoticeFunc over the local store: the sentence
// naming how much of the session the turns on screen stand for. It is
// meaningful only after a read, and the preview asks in that order.
func (s *StoredTurns) Notice(sessionID string) string {
	loaded, _ := s.lookup(sessionID)
	return loaded.notice
}

// sliceEntries is the bound on a read the pane keeps. A reader configured
// without one reads every remaining entry in a single range.
func (s *StoredTurns) sliceEntries() int {
	if s.slice <= 0 {
		return 0
	}
	return s.slice
}

// read takes ONE index range of a session's entries, starting where the given
// preview left off, and folds the result together with everything already
// loaded. Passing the zero preview reads from the start of the session.
//
// A bound of zero or less reads every remaining entry at once.
func (s *StoredTurns) read(sessionID string, loaded storedPreview, bound int) (storedPreview, error) {
	ctx := context.Background()
	sid := schema.SessionID(sessionID)
	total := loaded.total
	if len(loaded.entries) == 0 {
		maxIndex, err := s.reader.MaxEntryIndex(ctx, sid)
		if err != nil {
			return storedPreview{}, fmt.Errorf("read how far the recorded session %q goes in the local store: %w", sessionID, err)
		}
		total = maxIndex + 1
	}
	if total <= 0 {
		return storedPreview{}, nil
	}
	last := total - 1
	if bound > 0 && loaded.next+bound-1 < last {
		last = loaded.next + bound - 1
	}
	entries, err := s.reader.ListEntriesRange(ctx, sid, loaded.next, last)
	if err != nil {
		return storedPreview{}, fmt.Errorf("read entries %d to %d of recorded session %q from the local store: %w",
			loaded.next, last, sessionID, err)
	}
	next := storedPreview{
		entries:  append(append(make([]schema.SessionEntry, 0, len(loaded.entries)+len(entries)), loaded.entries...), entries...),
		turns:    loaded.turns,
		next:     last + 1,
		total:    total,
		more:     last < total-1,
		overlay:  loaded.overlay,
		overlaid: loaded.overlaid,
	}
	turns, err := foldLoaded(sessionID, next.entries)
	if err != nil {
		return storedPreview{}, err
	}
	next.turns = turns
	return next, nil
}

// foldLoaded folds the WHOLE accumulated prefix, never one slice on its own.
//
// A turn folds from consecutive entries, and a tool call joins the result that
// answers it by call identifier - a result that a harness records under a LATER
// message of its own, so it can land in a later slice than its call. Folding
// each slice alone would leave that call resultless and drop its output, since
// an orphan result is suppressed rather than drawn. Re-folding the prefix
// instead joins the pair as soon as the slice carrying the result arrives, so
// the paged read produces exactly what one whole read produces.
//
// Re-reading is what a slice avoids; re-FOLDING entries already in memory is
// cheap by comparison - about 14 milliseconds for the 42,731 entries of the
// longest session in a real store, against about 1.7 seconds to read them.
//
// It folds through the same validating conversion the session viewer uses, so a
// session whose recorded model evidence is invalid is reported here rather than
// drawn as if it were sound.
func foldLoaded(sessionID string, entries []schema.SessionEntry) ([]ingest.Turn, error) {
	turns, err := transcript.EntriesToTurnsValidated(entries)
	if err != nil {
		return nil, fmt.Errorf("fold the recorded entries of session %q into turns: %w", sessionID, err)
	}
	return turns, nil
}

// finish completes a read the pane is going to KEEP: it recovers the
// untruncated turn bodies and writes the sentence describing what is loaded.
//
// The recovery is done here, and never on a leading slice, because it costs a
// whole re-read of the harness transcript - about 1.5 seconds for a 99 MiB
// session - and the leading slice exists precisely to put turns on screen
// before any such cost is paid. A short session, whose leading slice IS the
// whole session, is finished on that slice, so it keeps the single-step
// behavior it had before this reader existed.
func (s *StoredTurns) finish(sessionID string, preview storedPreview) (storedPreview, error) {
	if s.overlay != nil && !preview.overlaid && transcript.AnyContentTruncated(preview.entries) {
		overlay, err := s.overlay(sessionID)
		if err != nil {
			return storedPreview{}, err
		}
		preview.overlay = overlay
		preview.overlaid = true
	}
	for index := range preview.turns {
		if content, ok := preview.overlay[preview.turns[index].Index]; ok {
			preview.turns[index].Content = content
		}
	}
	preview.notice = storedPreviewNotice(len(preview.turns), preview.next, preview.total, preview.more)
	return preview, nil
}

// storedPreviewNotice writes the sentence the pane shows above the turns of a
// session it is reading one slice at a time.
//
// It is LIVE: every kept read re-states it, so the figures follow what is on
// screen rather than describing the first read forever. It says how many turns
// are drawn, roughly how far into the session they reach, and how to load more,
// because a reader who cannot see why the transcript stops has no way to know
// that scrolling continues it. It ends with the same reassurance the harness
// paths give, since a transcript that stops otherwise reads as data loss.
//
// The share of the session is stated in RECORDED ENTRIES and hedged with
// "about", not given as a turn total, because the number of turns a session
// folds to is knowable only by folding all of it - which is the read this whole
// path exists to avoid. A stated total that required the slow read to be
// correct would be either a guess or a lie.
//
// A session with nothing more behind it gets no sentence at all: there is
// nothing left to explain.
func storedPreviewNotice(turns, loadedEntries, totalEntries int, more bool) string {
	if !more || turns <= 0 || totalEntries <= 0 {
		return ""
	}
	share := loadedEntries * 100 / totalEntries
	if share < 1 {
		share = 1
	}
	if share > 99 {
		share = 99
	}
	return fmt.Sprintf(
		"showing the first %s turns of this session, about %d%% of what it recorded. scroll to the bottom to load more. the full session ingests normally.",
		previewCount(int64(turns)), share)
}

func (s *StoredTurns) lookup(sessionID string) (storedPreview, bool) {
	if s.limit <= 0 {
		return storedPreview{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	preview, ok := s.cached[sessionID]
	return preview, ok
}

// store keeps the loaded prefix and drops the oldest session once the cache is
// full, so stepping through a long list cannot grow the memory of the wizard
// without a bound.
func (s *StoredTurns) store(sessionID string, preview storedPreview) {
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
