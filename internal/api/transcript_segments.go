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
// It intentionally exposes no write path: the directory is built from the same
// Available-mode content the segment read serves, whole, and a segment read is
// one bounded ReadSessionEntries page.
//
// LoadAvailableSessionEntries, not ListEntries, is the directory's source: the
// latter always serves the 2 KB preview projection, so a directory measured
// from it would declare a byte length the segment read can never match.
type SegmentReader interface {
	LoadAvailableSessionEntries(context.Context, ingest.SessionID) ([]schema.SessionEntry, error)
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

// segmentTurnMeta is the identity and role of one turn. A window read needs
// both to resolve an anchor and count assistant turns, and neither is turn
// content. Retaining this small metadata is what lets a jump stop at its
// extents without folding the whole session again.
type segmentTurnMeta struct {
	index int
	role  schema.Role
}

// segmentDirectory is the producer's retained artifact: refs, scalars, one
// entry count per segment, and each turn's identity and role. Turn content is
// never retained, so a cached directory costs kilobytes even for a session
// whose transcript is tens of megabytes.
type segmentDirectory struct {
	revision    string
	totalTurns  int
	refs        []schema.TranscriptSegmentRef
	entryCounts []int
	turns       []segmentTurnMeta
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

// segmentProducer computes one directory per (session, revision) and retains
// the refs in a bounded LRU. Concurrent reads of the same key share one build;
// a build failure is shared by the waiters and not cached.
//
// A pruned session needs no callback: deleting the sessions row advances
// store_state.revision through the store's AFTER DELETE trigger, so the next
// read uses a new key and the old refs fall out of the LRU. The server process
// does not share memory with `peasant prune`.
type segmentProducer struct {
	reader   SegmentReader
	budget   int
	capacity int

	mu        sync.Mutex
	dirs      []*segmentDirectoryEntry // LRU: oldest first, newest last
	dirBuilds map[segmentCacheKey]*segmentBuild
	builds    int // total completed directory builds; an in-flight test seam
}

func newSegmentProducer(reader SegmentReader) *segmentProducer {
	return &segmentProducer{
		reader:    reader,
		budget:    defaults.TranscriptSegmentBudgetBytes,
		capacity:  defaults.TranscriptSegmentDirectoryCapacity,
		dirBuilds: make(map[segmentCacheKey]*segmentBuild),
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
	entries, err := p.reader.LoadAvailableSessionEntries(ctx, id)
	if err != nil {
		return segmentDirectory{}, fmt.Errorf("api.transcriptSegments: load the available stored entries of session %s for its segment directory: %w; no directory was served; re-index the session and retry", id, err)
	}
	turns := transcript.EntriesToTurns(entries)
	refs, totalTurns := transcript.CutSegments(turns, p.budget)
	meta := make([]segmentTurnMeta, len(turns))
	for i := range turns {
		meta[i] = segmentTurnMeta{index: turns[i].Index, role: turns[i].Role}
	}
	return segmentDirectory{
		revision:    revision,
		totalTurns:  totalTurns,
		refs:        refs,
		entryCounts: transcript.SegmentEntryCounts(entries, refs),
		turns:       meta,
	}, nil
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

	directory, err := s.segments.directory(r.Context(), id)
	if err != nil {
		writeTranscriptReadError(w, "load the transcript segment directory", err)
		return
	}
	if ok := transcriptRevisionMatches(w, r, directory.revision); !ok {
		return
	}
	if index >= len(directory.refs) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("session %s has no transcript segment %d under revision %s; the directory names %d segment(s); read the directory again and choose an existing segment", id, index, directory.revision, len(directory.refs)))
		return
	}
	ref := directory.refs[index]
	if index >= len(directory.entryCounts) || directory.entryCounts[index] <= 0 {
		writeTranscriptReadError(w, "read the stored entries for transcript segment", fmt.Errorf("the directory has no entry count for segment %d; no segment was served; rebuild the directory", index))
		return
	}
	// The count is the segment's own entries, so the read stops at lastTurn
	// instead of consuming the continuation budget past it.
	limit := directory.entryCounts[index]

	page, err := s.segments.reader.ReadSessionEntries(r.Context(), id, ingest.SessionEntryReadOptions{
		Mode:         ingest.SessionEntryReadAvailable,
		FromIndex:    ref.FirstTurn,
		Limit:        limit,
		SoftMaxBytes: defaults.TranscriptContinuationReadBytes,
	})
	if err != nil {
		writeTranscriptReadError(w, "read the stored entries for transcript segment", err)
		return
	}
	current, revErr := s.segments.reader.Revision(r.Context())
	if revErr != nil {
		writeTranscriptReadError(w, "re-check the store revision for transcript segment", revErr)
		return
	}
	if current != directory.revision {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("the transcript segment read began under revision %s but the store advanced to revision %s while reading; no segment was served; read the transcript directory again and use its revision", directory.revision, current))
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
		writeJSONError(w, http.StatusConflict, transcriptShortSegmentMessage(id, index, ref, directory.revision, transcriptReturnedRange(filtered)))
		return
	}
	servedBytes := 0
	for i := range filtered {
		servedBytes += transcript.FoldedTurnByteLength(filtered[i])
	}
	if servedBytes != ref.ByteLength {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf(
			"the transcript segment read for session %s segment %d declared %d bytes for turns %d-%d under revision %s but the store returned %d bytes; no short segment was served, because a soft store-budget stop inside a turn would silently move every boundary after it; run peasant harvest --force --session %s to re-index the session",
			id, index, ref.ByteLength, ref.FirstTurn, ref.LastTurn, directory.revision, servedBytes, id))
		return
	}

	rows := transcript.SegmentTurnRows(filtered, transcript.DefaultServedDocumentBudget())
	byteLength := 0
	for i := range rows {
		byteLength += rows[i].ByteLength
	}
	segment := schema.TranscriptSegment{
		Index:      ref.Index,
		Revision:   directory.revision,
		FirstTurn:  ref.FirstTurn,
		LastTurn:   ref.LastTurn,
		ByteLength: byteLength,
		HasOlder:   ref.Index > 0,
		HasNewer:   ref.Index < len(directory.refs)-1,
		Turns:      rows,
	}
	if err := segment.Validate(); err != nil {
		writeTranscriptReadError(w, "build transcript segment", err)
		return
	}
	if err := schema.ValidateTranscriptSegmentBoundaries(segment, len(directory.refs)); err != nil {
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
	span := defaults.TranscriptWindowSpanDefault
	if raw := query.Get("span"); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < defaults.TranscriptWindowSpanMinimum || parsed > defaults.TranscriptWindowSpanMaximum {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("the transcript window span %q is outside the accepted range %d..%d assistant turns per side; no window was served; omit span for the design value %d or send a value in the range", raw, defaults.TranscriptWindowSpanMinimum, defaults.TranscriptWindowSpanMaximum, defaults.TranscriptWindowSpanDefault))
			return
		}
		span = parsed
	}

	directory, err := s.segments.directory(r.Context(), id)
	if err != nil {
		writeTranscriptReadError(w, "load the transcript segment directory", err)
		return
	}
	if ok := transcriptRevisionMatches(w, r, directory.revision); !ok {
		return
	}

	window, err := buildTranscriptWindow(schema.SessionID(id), directory.revision, requestedAnchor, span, directory.turns, directory.refs)
	if err != nil {
		var missing *noUserTurnError
		if asNoUserTurn(err, &missing) {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("session %s has no user turn, so a transcript window cannot be centred on turn %d; no window was served; a window requires a user turn", id, requestedAnchor))
			return
		}
		writeTranscriptReadError(w, "resolve a transcript window", err)
		return
	}
	current, revErr := s.segments.reader.Revision(r.Context())
	if revErr != nil {
		writeTranscriptReadError(w, "re-check the store revision for transcript window", revErr)
		return
	}
	if current != directory.revision {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("the transcript window was resolved under revision %s but the store advanced to revision %s before it was served; no window was served; read the transcript directory again and use its revision", directory.revision, current))
		return
	}
	writeTranscriptJSON(w, window)
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

func transcriptShortSegmentMessage(id ingest.SessionID, index int, ref schema.TranscriptSegmentRef, revision, returned string) string {
	return fmt.Sprintf(
		"the transcript segment read for session %s segment %d declared turns %d-%d under revision %s but the store returned %s; no short segment was served, because a short segment silently moves every boundary after it; run peasant harvest --force --session %s to re-index the session",
		id, index, ref.FirstTurn, ref.LastTurn, revision, returned, id)
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
func buildTranscriptWindow(id schema.SessionID, revision string, requestedAnchor, span int, turns []segmentTurnMeta, refs []schema.TranscriptSegmentRef) (schema.TranscriptWindow, error) {
	anchorPos, ok := resolveWindowAnchor(turns, requestedAnchor)
	if !ok {
		return schema.TranscriptWindow{}, &noUserTurnError{session: string(id), requested: requestedAnchor}
	}

	firstPos := anchorPos
	seen := 0
	for pos := anchorPos - 1; pos >= 0; pos-- {
		firstPos = pos
		if turns[pos].role == schema.RoleAssistant {
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
		if turns[pos].role == schema.RoleAssistant {
			seen++
			if seen == span {
				break
			}
		}
	}

	firstTurn := turns[firstPos].index
	lastTurn := turns[lastPos].index
	segments, err := coveringSegments(refs, firstTurn, lastTurn)
	if err != nil {
		return schema.TranscriptWindow{}, err
	}

	window := schema.TranscriptWindow{
		SessionID:       id,
		Revision:        revision,
		RequestedAnchor: requestedAnchor,
		AnchorTurn:      turns[anchorPos].index,
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
func resolveWindowAnchor(turns []segmentTurnMeta, anchor int) (int, bool) {
	lower := -1
	higher := -1
	for pos := range turns {
		if turns[pos].role != schema.RoleUser {
			continue
		}
		if turns[pos].index == anchor {
			return pos, true
		}
		if turns[pos].index < anchor {
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
