package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// SegmentReader is the store surface the transcript segment producer needs.
// It intentionally exposes no write path and no whole-session snapshot: the
// directory is built from one ListEntries pass, and a segment read is one
// bounded ReadSessionEntries page.
type SegmentReader interface {
	ListEntries(context.Context, ingest.SessionID) ([]schema.SessionEntry, error)
	ReadSessionEntries(context.Context, ingest.SessionID, ingest.SessionEntryReadOptions) (ingest.SessionEntryReadPage, error)
	Revision(context.Context) (string, error)
}

// segmentCacheKey names one cut: the session plus the store revision it was cut
// under. A revision change is a different key, and the old key falls out of the
// LRU rather than being served under a newer revision.
type segmentCacheKey string

func newSegmentCacheKey(id ingest.SessionID, revision string) segmentCacheKey {
	return segmentCacheKey(string(id) + "\x00" + revision)
}

// segmentDirectory is the producer's retained artifact: refs and scalars only.
// Turn content is never retained, so a cached directory costs kilobytes even
// for a session whose transcript is tens of megabytes.
type segmentDirectory struct {
	revision   string
	totalTurns int
	refs       []schema.TranscriptSegmentRef
}

type segmentDirectoryEntry struct {
	key       segmentCacheKey
	directory segmentDirectory
}

type segmentBuild struct {
	done      chan struct{}
	directory segmentDirectory
	err       error
}

type turnListBuild struct {
	done  chan struct{}
	turns []ingest.Turn
	err   error
}

// segmentProducer computes one directory per (session, revision) and retains
// the refs in a bounded LRU. Concurrent reads of the same key share one build;
// a build failure is shared by the waiters and not cached.
//
// The window read needs turn identities and roles, not content, so it walks the
// session's turn list on each jump under the same producer and the same
// single-flight discipline. That walk is deliberately not retained: the cache
// keeps refs only, by contract.
type segmentProducer struct {
	reader   SegmentReader
	budget   int
	capacity int

	mu         sync.Mutex
	dirs       []*segmentDirectoryEntry // LRU: oldest first, newest last
	dirBuilds  map[segmentCacheKey]*segmentBuild
	turnBuilds map[segmentCacheKey]*turnListBuild
	builds     int // total completed directory builds; an in-flight test seam
}

func newSegmentProducer(reader SegmentReader) *segmentProducer {
	return &segmentProducer{
		reader:     reader,
		budget:     defaults.TranscriptSegmentBudgetBytes,
		capacity:   defaults.TranscriptSegmentDirectoryCapacity,
		dirBuilds:  make(map[segmentCacheKey]*segmentBuild),
		turnBuilds: make(map[segmentCacheKey]*turnListBuild),
	}
}

// directory returns the cut for the current store revision.
func (p *segmentProducer) directory(ctx context.Context, id ingest.SessionID) (segmentDirectory, error) {
	revision, err := p.reader.Revision(ctx)
	if err != nil {
		return segmentDirectory{}, fmt.Errorf("api.transcriptSegments: read the store revision for session %s: %w; no segment directory was served; restore store access and retry", id, err)
	}
	return p.directoryFor(ctx, id, revision)
}

// directoryFor returns the cut for a revision the caller already read, so the
// directory, the segment range, and a response's revision cannot disagree
// because the store advanced midway through one request.
func (p *segmentProducer) directoryFor(ctx context.Context, id ingest.SessionID, revision string) (segmentDirectory, error) {
	key := newSegmentCacheKey(id, revision)

	p.mu.Lock()
	if entry := p.lookupLocked(key); entry != nil {
		p.touchLocked(entry)
		directory := entry.directory
		p.mu.Unlock()
		return directory, nil
	}
	if build, ok := p.dirBuilds[key]; ok {
		p.mu.Unlock()
		select {
		case <-build.done:
			return build.directory, build.err
		case <-ctx.Done():
			return segmentDirectory{}, ctx.Err()
		}
	}
	build := &segmentBuild{done: make(chan struct{})}
	p.dirBuilds[key] = build
	p.mu.Unlock()

	directory, buildErr := p.buildDirectory(ctx, id, revision)

	p.mu.Lock()
	build.directory, build.err = directory, buildErr
	delete(p.dirBuilds, key)
	if buildErr == nil {
		p.builds++
		p.storeLocked(key, directory)
	}
	p.mu.Unlock()
	close(build.done)
	return directory, buildErr
}

func (p *segmentProducer) buildDirectory(ctx context.Context, id ingest.SessionID, revision string) (segmentDirectory, error) {
	entries, err := p.reader.ListEntries(ctx, id)
	if err != nil {
		return segmentDirectory{}, fmt.Errorf("api.transcriptSegments: list the stored entries of session %s for its segment directory: %w; no directory was served; re-index the session and retry", id, err)
	}
	turns := transcript.EntriesToTurns(entries)
	refs, totalTurns := transcript.CutSegments(turns, p.budget)
	return segmentDirectory{revision: revision, totalTurns: totalTurns, refs: refs}, nil
}

// turns walks the session's turn list for one revision. Concurrent window
// reads share one walk; the result is not retained after its waiters consume
// it, because the producer's cache holds refs only.
func (p *segmentProducer) turns(ctx context.Context, id ingest.SessionID, revision string) ([]ingest.Turn, error) {
	key := newSegmentCacheKey(id, revision)

	p.mu.Lock()
	if build, ok := p.turnBuilds[key]; ok {
		p.mu.Unlock()
		select {
		case <-build.done:
			return build.turns, build.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	build := &turnListBuild{done: make(chan struct{})}
	p.turnBuilds[key] = build
	p.mu.Unlock()

	entries, err := p.reader.ListEntries(ctx, id)
	var turns []ingest.Turn
	if err == nil {
		turns = transcript.EntriesToTurns(entries)
	} else {
		err = fmt.Errorf("api.transcriptSegments: list the stored entries of session %s to resolve a transcript window: %w; no window was served; re-index the session and retry", id, err)
	}

	p.mu.Lock()
	build.turns, build.err = turns, err
	delete(p.turnBuilds, key)
	p.mu.Unlock()
	close(build.done)
	return turns, err
}

func (p *segmentProducer) lookupLocked(key segmentCacheKey) *segmentDirectoryEntry {
	for _, entry := range p.dirs {
		if entry.key == key {
			return entry
		}
	}
	return nil
}

func (p *segmentProducer) touchLocked(entry *segmentDirectoryEntry) {
	for i, candidate := range p.dirs {
		if candidate == entry {
			p.dirs = append(append(p.dirs[:i], p.dirs[i+1:]...), entry)
			return
		}
	}
}

func (p *segmentProducer) storeLocked(key segmentCacheKey, directory segmentDirectory) {
	p.dirs = append(p.dirs, &segmentDirectoryEntry{key: key, directory: directory})
	if p.capacity > 0 && len(p.dirs) > p.capacity {
		p.dirs = p.dirs[len(p.dirs)-p.capacity:]
	}
}

// buildCount reports how many directory builds have completed. Tests use it to
// prove that concurrent reads share one build; production never calls it.
func (p *segmentProducer) buildCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.builds
}

// handleTranscriptSegments serves the directory: the transcript's only global
// index, one ref per segment with the cut's scalars.
func (s *Server) handleTranscriptSegments(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	if s.segments == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "the transcript segment read is unavailable because this server started without a stored-session reader; no directory was served; start peasant web against a store and retry")
		return
	}
	id, ok := transcriptSessionID(w, r)
	if !ok {
		return
	}

	directory, err := s.segments.directory(r.Context(), id)
	if err != nil {
		writeTranscriptReadError(w, "load the transcript segment directory", err)
		return
	}
	if ok := transcriptRevisionMatches(w, r, directory.revision); !ok {
		return
	}

	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("the transcript directory page %q is not a one-based page number; no directory was served; send ?page=1 or omit the parameter", raw))
			return
		}
		page = parsed
	}

	payload := transcriptDirectoryPayload(id, directory, page)
	if err := payload.Validate(); err != nil {
		writeTranscriptReadError(w, "build the transcript segment directory", err)
		return
	}
	writeTranscriptJSON(w, payload)
}

// handleTranscriptSegment serves one segment's whole turns. The directory
// declares the range; the read either returns exactly that range or refuses
// with a 409, because a short segment would silently move every later boundary.
func (s *Server) handleTranscriptSegment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	if s.segments == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "the transcript segment read is unavailable because this server started without a stored-session reader; no segment was served; start peasant web against a store and retry")
		return
	}
	id, ok := transcriptSessionID(w, r)
	if !ok {
		return
	}
	index, indexErr := strconv.Atoi(r.PathValue("index"))
	if indexErr != nil || index < 0 {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("the transcript segment index %q is not a non-negative segment ordinal; no segment was served; use an index from the directory read", r.PathValue("index")))
		return
	}

	revision, refs, err := s.segments.directoryRefs(r.Context(), id)
	if err != nil {
		writeTranscriptReadError(w, "load the transcript segment directory", err)
		return
	}
	if ok := transcriptRevisionMatches(w, r, revision); !ok {
		return
	}
	if index >= len(refs) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("session %s has no transcript segment %d under revision %s; the directory names %d segment(s); read the directory again and choose an existing segment", id, index, revision, len(refs)))
		return
	}
	ref := refs[index]

	page, err := s.segments.reader.ReadSessionEntries(r.Context(), id, ingest.SessionEntryReadOptions{
		Mode:         ingest.SessionEntryReadAvailable,
		FromIndex:    ref.FirstTurn,
		Limit:        defaults.TranscriptSegmentEntryReadLimit,
		SoftMaxBytes: defaults.TranscriptContinuationReadBytes,
	})
	if err != nil {
		writeTranscriptReadError(w, "read the stored entries for transcript segment", err)
		return
	}
	turns := transcript.EntriesToTurns(page.Entries)
	filtered := make([]ingest.Turn, 0, len(turns))
	for _, turn := range turns {
		if turn.Index < ref.FirstTurn || turn.Index > ref.LastTurn {
			continue
		}
		filtered = append(filtered, turn)
	}
	if len(filtered) == 0 || filtered[0].Index != ref.FirstTurn || filtered[len(filtered)-1].Index != ref.LastTurn {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf(
			"the transcript segment read for session %s segment %d declared turns %d-%d under revision %s but the store returned %s; no short segment was served, because a short segment silently moves every boundary after it; run peasant harvest --force --session %s to re-index the session",
			id, index, ref.FirstTurn, ref.LastTurn, revision, transcriptReturnedRange(filtered), id))
		return
	}

	rows := transcript.SegmentTurnRows(filtered, transcript.DefaultServedDocumentBudget())
	byteLength := 0
	for i := range rows {
		byteLength += rows[i].ByteLength
	}
	segment := schema.TranscriptSegment{
		Index:      ref.Index,
		Revision:   revision,
		FirstTurn:  ref.FirstTurn,
		LastTurn:   ref.LastTurn,
		ByteLength: byteLength,
		HasOlder:   ref.Index > 0,
		HasNewer:   ref.Index < len(refs)-1,
		Turns:      rows,
	}
	if err := segment.Validate(); err != nil {
		writeTranscriptReadError(w, "build transcript segment", err)
		return
	}
	if err := schema.ValidateTranscriptSegmentBoundaries(segment, len(refs)); err != nil {
		writeTranscriptReadError(w, "check transcript segment boundaries", err)
		return
	}
	writeTranscriptJSON(w, segment)
}

// handleTranscriptWindow resolves an anchor turn to a contiguous window and
// answers with a descriptor only. Turn content stays in the segment read, so no
// window response can grow with the transcript.
func (s *Server) handleTranscriptWindow(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	if s.segments == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "the transcript window read is unavailable because this server started without a stored-session reader; no window was served; start peasant web against a store and retry")
		return
	}
	id, ok := transcriptSessionID(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	requestedAnchor, anchorErr := strconv.Atoi(query.Get("anchor"))
	if anchorErr != nil || requestedAnchor < 0 {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("the transcript window anchor %q is not a non-negative turn identity; no window was served; send ?anchor=<turn index> from a search hit or a ?turn= deep link", query.Get("anchor")))
		return
	}
	span := 10
	if raw := query.Get("span"); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > 10 {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("the transcript window span %q is outside the accepted range 1..10 assistant turns per side; no window was served; omit span for the design value 10 or send a value in the range", raw))
			return
		}
		span = parsed
	}

	revision, refs, err := s.segments.directoryRefs(r.Context(), id)
	if err != nil {
		writeTranscriptReadError(w, "load the transcript segment directory", err)
		return
	}
	if ok := transcriptRevisionMatches(w, r, revision); !ok {
		return
	}
	turns, err := s.segments.turns(r.Context(), id, revision)
	if err != nil {
		writeTranscriptReadError(w, "walk the stored turns for a transcript window", err)
		return
	}

	window, err := buildTranscriptWindow(schema.SessionID(id), revision, requestedAnchor, span, turns, refs)
	if err != nil {
		var missing *noUserTurnError
		if asNoUserTurn(err, &missing) {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("session %s has no user turn, so a transcript window cannot be centred on turn %d; no window was served; a window requires a user turn", id, requestedAnchor))
			return
		}
		writeTranscriptReadError(w, "resolve a transcript window", err)
		return
	}
	writeTranscriptJSON(w, window)
}

// directoryRefs returns the current revision and its refs in one read, so a
// segment or window request cannot mix a revision with another cut's refs.
func (p *segmentProducer) directoryRefs(ctx context.Context, id ingest.SessionID) (string, []schema.TranscriptSegmentRef, error) {
	directory, err := p.directory(ctx, id)
	if err != nil {
		return "", nil, err
	}
	return directory.revision, directory.refs, nil
}

// transcriptDirectoryPayload assembles the wire directory, applying the
// directory cap and the workspace's one page clamp.
func transcriptDirectoryPayload(id ingest.SessionID, directory segmentDirectory, page int) schema.TranscriptSegmentDirectory {
	payload := schema.TranscriptSegmentDirectory{
		SessionID:     schema.SessionID(id),
		Revision:      directory.revision,
		TotalTurns:    directory.totalTurns,
		TotalSegments: len(directory.refs),
		Segments:      directory.refs,
	}
	if len(directory.refs) <= schema.MaxTranscriptDirectoryRefs {
		return payload
	}
	perPage := schema.MaxTranscriptDirectoryRefs
	page = clampPage(page, len(directory.refs), perPage)
	start := (page - 1) * perPage
	end := start + perPage
	if end > len(directory.refs) {
		end = len(directory.refs)
	}
	payload.Segments = directory.refs[start:end]
	payload.DirectoryPage = &page
	payload.DirectoryPerPage = &perPage
	return payload
}

// transcriptReturnedRange renders the range a short read actually returned.
func transcriptReturnedRange(turns []ingest.Turn) string {
	if len(turns) == 0 {
		return "no turns"
	}
	return fmt.Sprintf("turns %d-%d", turns[0].Index, turns[len(turns)-1].Index)
}

func transcriptSessionID(w http.ResponseWriter, r *http.Request) (ingest.SessionID, bool) {
	raw := r.PathValue("id")
	if raw == "" {
		writeJSONError(w, http.StatusBadRequest, "the transcript request named no session; no transcript read was served; use /api/v1/sessions/{id}/transcript/... with a stored session identifier")
		return "", false
	}
	id, err := ingest.NewSessionID(raw)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("the transcript request named session %q, which is not a valid session identifier; no transcript read was served; use the identifier the session list displays", raw))
		return "", false
	}
	return id, true
}

func transcriptRevisionMatches(w http.ResponseWriter, r *http.Request, current string) bool {
	requested := r.URL.Query().Get("revision")
	if requested == "" || requested == current {
		return true
	}
	writeJSONError(w, http.StatusConflict, fmt.Sprintf("the transcript read was asked for revision %s but the store is at revision %s; no transcript content was served; read the transcript directory again and use its revision", requested, current))
	return false
}

func writeTranscriptReadError(w http.ResponseWriter, operation string, err error) {
	writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("%s failed: %v; no transcript content was served; retry, and if it repeats inspect the Peasant server logs", operation, err), "")
}

func writeTranscriptJSON(w http.ResponseWriter, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("the transcript response could not be encoded: %v; no transcript content was served; retry and inspect the server logs", err))
		return
	}
	_, _ = w.Write(data)
}

// noUserTurnError reports that a window request named a session with no user
// turn to centre a window on.
type noUserTurnError struct {
	session   string
	requested int
}

func (e *noUserTurnError) Error() string {
	return fmt.Sprintf("session %s has no user turn to centre a window on turn %d", e.session, e.requested)
}

func asNoUserTurn(err error, target **noUserTurnError) bool {
	missing, ok := err.(*noUserTurnError)
	if ok {
		*target = missing
	}
	return ok
}

// buildTranscriptWindow resolves the anchor to a user turn, collects up to
// span assistant turns on each side, and describes the contiguous range. It
// never serialises turn content.
func buildTranscriptWindow(id schema.SessionID, revision string, requestedAnchor, span int, turns []ingest.Turn, refs []schema.TranscriptSegmentRef) (schema.TranscriptWindow, error) {
	anchorPos, ok := resolveWindowAnchor(turns, requestedAnchor)
	if !ok {
		return schema.TranscriptWindow{}, &noUserTurnError{session: string(id), requested: requestedAnchor}
	}

	firstPos := anchorPos
	seen := 0
	for pos := anchorPos - 1; pos >= 0; pos-- {
		firstPos = pos
		if turns[pos].Role == schema.RoleAssistant {
			seen++
			if seen == span {
				break
			}
		}
	}
	lastPos := anchorPos
	seen = 0
	for pos := anchorPos + 1; pos < len(turns); pos++ {
		lastPos = pos
		if turns[pos].Role == schema.RoleAssistant {
			seen++
			if seen == span {
				break
			}
		}
	}

	firstTurn := turns[firstPos].Index
	lastTurn := turns[lastPos].Index
	segments, err := coveringSegments(refs, firstTurn, lastTurn)
	if err != nil {
		return schema.TranscriptWindow{}, err
	}

	window := schema.TranscriptWindow{
		SessionID:       id,
		Revision:        revision,
		RequestedAnchor: requestedAnchor,
		AnchorTurn:      turns[anchorPos].Index,
		Span:            span,
		FirstTurn:       firstTurn,
		LastTurn:        lastTurn,
		Segments:        segments,
		HasOlder:        firstPos > 0,
		HasNewer:        lastPos < len(turns)-1,
	}
	if err := window.Validate(); err != nil {
		return schema.TranscriptWindow{}, err
	}
	if err := schema.ValidateTranscriptWindowCoverage(window, refs); err != nil {
		return schema.TranscriptWindow{}, err
	}
	return window, nil
}

// resolveWindowAnchor returns the position of the user turn a window is centred
// on: the anchor itself when it is a user turn, otherwise the nearest lower
// user turn, otherwise the nearest higher one.
func resolveWindowAnchor(turns []ingest.Turn, anchor int) (int, bool) {
	lower := -1
	higher := -1
	for pos := range turns {
		if turns[pos].Role != schema.RoleUser {
			continue
		}
		if turns[pos].Index == anchor {
			return pos, true
		}
		if turns[pos].Index < anchor {
			lower = pos
			continue
		}
		if higher < 0 {
			higher = pos
		}
	}
	if lower >= 0 {
		return lower, true
	}
	if higher >= 0 {
		return higher, true
	}
	return 0, false
}

// coveringSegments returns the ordered contiguous run of segment ordinals that
// covers [firstTurn, lastTurn].
func coveringSegments(refs []schema.TranscriptSegmentRef, firstTurn, lastTurn int) ([]int, error) {
	first := -1
	last := -1
	for i := range refs {
		if refs[i].FirstTurn <= firstTurn && firstTurn <= refs[i].LastTurn {
			first = refs[i].Index
		}
		if refs[i].FirstTurn <= lastTurn && lastTurn <= refs[i].LastTurn {
			last = refs[i].Index
		}
	}
	if first < 0 || last < 0 {
		return nil, fmt.Errorf("the transcript window [%d, %d] is not covered by the segment directory; no window was served; rebuild the directory after the session is re-indexed", firstTurn, lastTurn)
	}
	segments := make([]int, 0, last-first+1)
	for index := first; index <= last; index++ {
		segments = append(segments, index)
	}
	return segments, nil
}
