package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
//
// Both reads return a managed generation's entries with their source refs
// (ReadSessionEntries with GenerationRefs set), so the directory and a segment
// fold the turns the session's stored turn counts count.
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

// segmentDirectory is the producer's retained artifact: refs, scalars, one
// entry count per segment, and each turn's identity and role. Turn content is
// never retained. Retained memory is O(turns) for the identity/role list — two
// machine words per turn, not text — plus O(segments) for the refs and entry
// counts. The eight-entry LRU caps the whole cache, so even a 10,000-turn
// session holds tens of kilobytes per directory entry.
type segmentDirectory struct {
	revision    string
	totalTurns  int
	refs        []schema.TranscriptSegmentRef
	entryCounts []int
	// turns is what the window resolver reads: identity and role, never
	// content, so a jump stops at its extents without folding the session again.
	turns []schema.TranscriptWindowTurn
	// totalConversationTurns is nil when the cut failed the self-check; every
	// ref's span is nil with it.
	totalConversationTurns *int
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
	// cut is transcript.CutSegments in production. It is a field because a cut
	// with wrong position data cannot be produced through the store.
	cut    func([]ingest.Turn, int) ([]schema.TranscriptSegmentRef, int)
	logger *slog.Logger

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
		cut:       transcript.CutSegments,
		logger:    slog.Default(),
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

// currentDirectory returns a directory whose revision is still current after
// its build. A build is one whole-session pass, and a write during it advances
// store_state.revision; serving that cut as the current one would mix a
// directory with a newer store. One retry absorbs ordinary churn; a second
// advance is reported rather than guessed.
func (p *segmentProducer) currentDirectory(ctx context.Context, id ingest.SessionID) (segmentDirectory, error) {
	const attempts = 2
	var last string
	for attempt := 0; attempt < attempts; attempt++ {
		directory, err := p.directory(ctx, id)
		if err != nil {
			return segmentDirectory{}, err
		}
		current, err := p.reader.Revision(ctx)
		if err != nil {
			return segmentDirectory{}, fmt.Errorf("api.transcriptSegments: re-read the store revision after building the directory for session %s: %w; no directory was served; restore store access and retry", id, err)
		}
		if current == directory.revision {
			return directory, nil
		}
		last = fmt.Sprintf("%s became %s", directory.revision, current)
	}
	return segmentDirectory{}, fmt.Errorf("api.transcriptSegments: the store revision for session %s advanced twice while building its segment directory (%s); no directory was served; retry when the store is quiet", id, last)
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
	turns, err := p.foldEntries(ctx, id, entries)
	if err != nil {
		return segmentDirectory{}, err
	}
	refs, totalTurns := p.cut(turns, p.budget)
	// A position fault never costs the directory: the position fields are
	// withheld together and the read still answers.
	canonical := ingest.ConversationTurnCount(turns)
	total := &canonical
	if fault := transcript.CheckConversationTurns(refs, canonical); fault != nil {
		p.logger.Error("transcript segments: conversation turn positions withheld",
			"session", string(id), "revision", revision,
			"rule", string(fault.Rule), "segment", fault.Segment, "error", fault.Error())
		for i := range refs {
			refs[i].ConversationTurns = nil
		}
		total = nil
	}
	meta := make([]schema.TranscriptWindowTurn, len(turns))
	for i := range turns {
		meta[i] = schema.TranscriptWindowTurn{Index: turns[i].Index, Role: turns[i].Role}
	}
	return segmentDirectory{
		revision:    revision,
		totalTurns:  totalTurns,
		refs:        refs,
		entryCounts: transcript.SegmentEntryCounts(entries, refs),
		turns:       meta,

		totalConversationTurns: total,
	}, nil
}

// foldEntries uses the same attachment join as full detail. Both directory and
// segment reads must use it: the image metadata contributes to their byte count.
func (p *segmentProducer) foldEntries(ctx context.Context, id ingest.SessionID, entries []schema.SessionEntry) ([]ingest.Turn, error) {
	reader, ok := p.reader.(ingest.ImageFactReader)
	if !ok {
		return ingest.EntriesToTurns(entries), nil
	}
	facts, err := reader.ImageFacts(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("api.transcriptSegments: join image metadata for session %s: %w; no segment was served; restore store access and retry", id, err)
	}
	return ingest.EntriesToTurnsWithImages(entries, facts), nil
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

	directory, err := s.segments.currentDirectory(r.Context(), id)
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

	directory, err := s.segments.currentDirectory(r.Context(), id)
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

	// Limit already bounds this read to the segment's own entries, so raising
	// the soft byte budget for one oversized turn cannot overread past lastTurn.
	// It only lets a turn larger than the continuation budget be served whole.
	softMax := defaults.TranscriptContinuationReadBytes
	if int64(ref.ByteLength)+1 > softMax {
		softMax = int64(ref.ByteLength) + 1
	}
	page, err := s.segments.reader.ReadSessionEntries(r.Context(), id, ingest.SessionEntryReadOptions{
		Mode:         ingest.SessionEntryReadAvailable,
		FromIndex:    ref.FirstTurn,
		Limit:        limit,
		SoftMaxBytes: softMax,
		// The directory was folded from entries that keep their generation
		// source refs; the segment must fold the same entries or it would
		// drop a turn the directory counted.
		GenerationRefs: true,
	})
	if err != nil {
		writeTranscriptReadError(w, "read the stored entries for transcript segment", err)
		return
	}
	turns, err := s.segments.foldEntries(r.Context(), id, page.Entries)
	if err != nil {
		writeTranscriptReadError(w, "join image metadata for transcript segment", err)
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
			"the transcript segment read for session %s segment %d declared %d bytes for turns %d-%d under revision %s but the store returned %d bytes; no partial segment was served, because a turn folded from fewer entries would silently move every boundary after it; read the directory again and retry, and if it repeats the stored transcript is incomplete at this revision",
			id, index, ref.ByteLength, ref.FirstTurn, ref.LastTurn, directory.revision, servedBytes))
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
	encoded, err := json.Marshal(segment)
	if err != nil {
		writeTranscriptReadError(w, "encode transcript segment", err)
		return
	}
	if len(encoded) > defaults.SessionDetailDocumentCapBytes {
		writeTranscriptReadError(w, "bound transcript segment", fmt.Errorf("segment %d exceeds the %d-byte document cap after text was bounded; no segment was served; re-index the session or reduce its unbounded metadata", index, defaults.SessionDetailDocumentCapBytes))
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

	directory, err := s.segments.currentDirectory(r.Context(), id)
	if err != nil {
		writeTranscriptReadError(w, "load the transcript segment directory", err)
		return
	}
	if ok := transcriptRevisionMatches(w, r, directory.revision); !ok {
		return
	}

	// The anchor, extent and covering-segment rule is the contract's, so every
	// server resolves the same window from the same turns.
	window, err := schema.ResolveTranscriptWindow(schema.SessionID(id), directory.revision, requestedAnchor, span, directory.turns, directory.refs)
	if err != nil {
		if errors.Is(err, schema.ErrTranscriptWindowNoUserTurn) {
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
		// The whole-session total goes on every page of refs.
		TotalConversationTurns: directory.totalConversationTurns,
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
