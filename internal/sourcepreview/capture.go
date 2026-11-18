package sourcepreview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"os"
	"sync"
	"sync/atomic"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// originKind is how a capture reads its source. Closed set.
type originKind uint8

const (
	// originFileRanged reads a line-oriented file one range at a time and
	// re-parses the whole loaded prefix after each read.
	originFileRanged originKind = iota + 1
	// originFileWhole reads a file once, whole: a format that is not one
	// record per line, or a filesystem that cannot read a range.
	originFileWhole
	// originSQLiteResumable reads an OpenCode session one bounded slice of
	// whole messages at a time.
	originSQLiteResumable
	// originSQLiteBounded reads an OpenCode session once, bounded.
	originSQLiteBounded
)

// emittedTurn is what a capture remembers of a turn it handed out: enough to
// prove a later re-parse did not change it, and nothing of its content.
type emittedTurn struct {
	index  int
	digest [sha256.Size]byte
	// pending names the tool calls the turn was handed out without results.
	pending map[string]struct{}
}

// Capture is one preview of one session. It holds what the preview has loaded
// so far and hands out each sealed turn exactly once: a turn it has handed out
// never changes. When the source or a re-parse would change a turn already
// handed out, the capture ends instead of re-sending it.
//
// A Capture serializes its own operations. Close, or cancellation of the
// context Open was given, clears everything it loaded at once.
type Capture struct {
	reader  *Reader
	listing onboarding.SessionListing
	session ingest.DiscoveredSession
	opts    CaptureOptions
	kind    originKind
	indexer ingest.TranscriptIndexer
	slicer  ingest.FileTranscriptSlicer
	ranged  ingest.RangeReadableFileSystem
	sqlite  ingest.ResumableTranscriptMaterializer
	bounded ingest.BoundedTranscriptMaterializer
	marked  ingest.ChangeMarkedTranscriptMaterializer

	// parent is the context Open was given; its cancellation closes the
	// capture. ctx is the capture's own, cancelled by Close.
	parent    context.Context
	ctx       context.Context
	cancel    context.CancelFunc
	stopWatch atomic.Pointer[func() bool]

	mu       sync.Mutex
	closed   atomic.Bool
	retained atomic.Int64

	// Source state.
	started    bool
	cursor     ingest.TranscriptSliceCursor
	sourceMore bool
	loaded     []byte
	identity   os.FileInfo
	marker     ingest.TranscriptChangeMarker
	maxEntry   int

	// Emitted state.
	emitted          []emittedTurn
	retainedBoundary int
	partial          bool
	firstTurn        *ingest.Turn
	lastTurn         *ingest.Turn
	terminal         *Page
}

func newCapture(ctx context.Context, reader *Reader, listing onboarding.SessionListing, session ingest.DiscoveredSession, opts CaptureOptions) (*Capture, error) {
	indexer, ok := ingest.NewIndexerRegistry(reader.fs, ingest.IndexerRegistryOptions{FullContent: true})[session.Harness]
	if !ok {
		return nil, captureError(CloseUnreadable, nil, "this harness has no transcript reader in sourcepreview.Reader.Open")
	}
	c := &Capture{reader: reader, listing: listing, session: session, opts: opts, indexer: indexer, maxEntry: -1}
	switch session.TranscriptOrigin {
	case ingest.TranscriptOriginOpenCodeLegacySQLite, ingest.TranscriptOriginOpenCodeCurrentSQLite:
		factory, ok := ingest.DefaultAdapterRegistry[session.Harness]
		if !ok {
			return nil, captureError(CloseUnreadable, nil, "this harness has no SQLite adapter in sourcepreview.Reader.Open")
		}
		adapter := factory(reader.fs, reader.git, reader.salt)
		if resumable, ok := adapter.(ingest.ResumableTranscriptMaterializer); ok {
			c.kind, c.sqlite = originSQLiteResumable, resumable
			c.marked, _ = adapter.(ingest.ChangeMarkedTranscriptMaterializer)
		} else if bounded, ok := adapter.(ingest.BoundedTranscriptMaterializer); ok {
			c.kind, c.bounded = originSQLiteBounded, bounded
		} else {
			return nil, captureError(CloseUnreadable, nil, "this harness cannot bound a SQLite transcript read in sourcepreview.Reader.Open")
		}
	default:
		c.kind = originFileWhole
		if lines, ok := indexer.(ingest.LineOrientedTranscriptIndexer); ok && lines.RecordsAreLines() {
			slicer := ingest.NewFileTranscriptSlicer(reader.fs)
			if ranged, ok := reader.fs.(ingest.RangeReadableFileSystem); ok && slicer.Supported() {
				c.kind, c.slicer, c.ranged = originFileRanged, slicer, ranged
			}
		}
	}
	c.parent = ctx
	c.ctx, c.cancel = context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, c.Close)
	c.stopWatch.Store(&stop)
	return c, nil
}

// Next returns the sealed turns not yet handed out. It reads the source as
// often as it needs to find at least one, so a page with no turns means the
// capture ended. It never returns a turn twice.
//
// It fails with ErrBusy while another call runs, with ErrClosed after Close,
// and with a *CaptureError naming why the capture ended otherwise; after any
// such error except ErrBusy the capture is closed.
func (c *Capture) Next(ctx context.Context) (Page, error) {
	if !c.mu.TryLock() {
		return Page{}, captureError(CloseBusy, nil, "")
	}
	defer c.mu.Unlock()
	if c.parent.Err() != nil && !c.closed.Swap(true) {
		// The owner's context ended. Close it here rather than wait for the
		// watcher, so nothing is read after cancellation.
		c.cancel()
		c.unwatch()
	}
	if c.closed.Load() {
		// A Close racing this call may not have cleared yet; clearing is
		// idempotent, so do it here and answer with nothing retained.
		c.clearLocked()
		return Page{}, captureError(CloseClosed, nil, "")
	}
	if c.terminal != nil {
		page := *c.terminal
		page.Turns, page.NativeMetadata, page.RetainedUnknown = nil, nil, nil
		return page, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	page, err := c.step(ctx)
	return c.settle(page, err)
}

// first reads the first page. Open calls it before handing the capture out.
func (c *Capture) first() (Page, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	page, err := c.step(c.ctx)
	if err == nil && len(c.emitted) == 0 && page.Continuation == ContinuationEnd {
		err = captureError(CloseNoTurns, nil, "")
	}
	return c.settle(page, err)
}

// settle records a terminal page and clears everything after a failure.
func (c *Capture) settle(page Page, err error) (Page, error) {
	if err == nil && c.closed.Load() {
		err = captureError(CloseClosed, nil, "")
	}
	if err != nil {
		var captureErr *CaptureError
		if !errors.As(err, &captureErr) {
			err = captureError(CloseUnreadable, err, "reading the transcript")
		}
		c.clearLocked()
		c.closed.Store(true)
		c.cancel()
		c.unwatch()
		return Page{}, err
	}
	if page.Continuation != ContinuationMore {
		terminal := page
		c.terminal = &terminal
		// The capture ended: nothing it loaded is needed to answer again.
		c.loaded = nil
	}
	c.retained.Store(c.retainedLocked())
	return page, nil
}

// Close ends the capture and clears everything it loaded. It is safe to call
// more than once and from any goroutine.
func (c *Capture) Close() {
	if c.closed.Swap(true) {
		return
	}
	c.cancel()
	c.unwatch()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
}

// unwatch stops watching the owner's context, so a closed capture is not kept
// reachable by it.
func (c *Capture) unwatch() {
	if stop := c.stopWatch.Swap(nil); stop != nil {
		(*stop)()
	}
}

// RetainedBytes reports what the capture holds in memory between reads: the
// loaded prefix and the digests of the turns it handed out. It is zero once the
// capture is closed.
func (c *Capture) RetainedBytes() int64 { return c.retained.Load() }

func (c *Capture) clearLocked() {
	c.loaded = nil
	c.emitted = nil
	c.identity = nil
	c.firstTurn, c.lastTurn = nil, nil
	c.retained.Store(0)
}

func (c *Capture) retainedLocked() int64 {
	perTurn := int64(sha256.Size + 16)
	return int64(cap(c.loaded)) + perTurn*int64(len(c.emitted))
}

// step performs one or more reads until it has a page to hand out.
func (c *Capture) step(ctx context.Context) (Page, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Page{}, captureError(CloseClosed, err, "")
		}
		var page Page
		var err error
		switch c.kind {
		case originFileRanged:
			page, err = c.stepFileRanged(ctx)
		case originFileWhole:
			page, err = c.stepFileWhole(ctx)
		case originSQLiteResumable:
			page, err = c.stepSQLiteResumable(ctx)
		case originSQLiteBounded:
			page, err = c.stepSQLiteBounded(ctx)
		}
		if err != nil {
			return Page{}, err
		}
		if len(page.Turns) > 0 || page.Continuation != ContinuationMore {
			return page, nil
		}
	}
}

// readBudget is the next read's size and the record ceiling under the source
// cap. exhausted reports that the cap allows no further read.
func (c *Capture) readBudget(consumed int64) (budget, ceiling int64, exhausted bool) {
	budget = c.opts.SliceReadBytes
	if !c.started {
		budget = c.opts.FirstReadBytes
	}
	ceiling = defaults.MaxJSONLRecordBytes
	if c.opts.SourceCapBytes > 0 {
		remaining := c.opts.SourceCapBytes - consumed
		if remaining <= 0 {
			return 0, 0, true
		}
		budget = min(budget, remaining)
		ceiling = min(ceiling, remaining)
	}
	return budget, ceiling, false
}

func (c *Capture) stepFileRanged(ctx context.Context) (Page, error) {
	path := string(c.session.SourcePath)
	cut, cutNotice := CutNone, ""
	budget, ceiling, exhausted := c.readBudget(int64(len(c.loaded)))
	if exhausted {
		cut, cutNotice = CutSourceBudget, sourceBudgetNotice(int64(len(c.loaded)), c.cursor.TotalBytes(), c.opts.SourceCapBytes)
	} else {
		if c.started {
			if err := c.verifyFile(path); err != nil {
				return Page{}, err
			}
		} else if info, err := c.reader.fs.Stat(path); err == nil {
			c.identity = info
		}
		if err := ctx.Err(); err != nil {
			return Page{}, captureError(CloseClosed, err, "")
		}
		// An in-flight range read cannot be interrupted; discard its result
		// after close instead of adding it to the prefix or parsing it.
		slice, err := c.slicer.MaterializeTranscriptSliceWithin(ctx, c.session, budget, c.cursor, ingest.TranscriptSliceLimits{RecordCeilingBytes: ceiling})
		if err := ctx.Err(); err != nil {
			return Page{}, captureError(CloseClosed, err, "")
		}
		var oversized *ingest.RecordExceedsBudgetError
		switch {
		case errors.As(err, &oversized):
			cut, cutNotice = c.classifyOversizedLine(path, oversized)
		case err != nil:
			return Page{}, captureError(CloseUnreadable, err, "reading %d bytes after byte %d", budget, len(c.loaded))
		default:
			c.started = true
			c.cursor, c.sourceMore = slice.Next, slice.More
			if len(slice.Data) > 0 {
				grown := make([]byte, 0, len(c.loaded)+len(slice.Data))
				c.loaded = append(append(grown, c.loaded...), slice.Data...)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Page{}, captureError(CloseClosed, err, "")
	}
	entries, err := c.parse(ctx, c.loaded)
	if err != nil {
		return Page{}, err
	}
	final := !c.sourceMore || cut != CutNone
	page, err := c.fold(entries, final, true)
	if err != nil {
		return Page{}, err
	}
	switch {
	case page.Continuation == ContinuationCut:
	case cut != CutNone:
		page.Continuation, page.CutReason, page.Notice = ContinuationCut, cut, cutNotice
	case !c.sourceMore:
		page.Continuation = ContinuationEnd
		if info, err := c.reader.fs.Stat(path); err == nil && info.Size() > c.cursor.TotalBytes() {
			page.Notice = GrownNotice
		}
	}
	return page, nil
}

// recordLimit is the largest single record any read of this capture may hold:
// the whole source cap, and never more than ingest reads.
func (c *Capture) recordLimit() int64 {
	limit := int64(defaults.MaxJSONLRecordBytes)
	if c.opts.SourceCapBytes > 0 {
		limit = min(limit, c.opts.SourceCapBytes)
	}
	return limit
}

// classifyRecord decides why a refused record ends the capture. A record that
// would fit a fresh preview only crossed what is left of the source cap: that
// is the source budget. A record larger than any preview may hold is an
// oversized record, and the notice names it.
func (c *Capture) classifyRecord(refused *ingest.RecordExceedsBudgetError, size int64) (CutReason, string) {
	limit := c.recordLimit()
	if size <= limit {
		return CutSourceBudget, sourceBudgetNotice(c.consumed(), c.cursor.TotalBytes(), c.opts.SourceCapBytes)
	}
	named := *refused
	named.AtLeastBytes, named.CeilingBytes = size, limit
	return CutOversizedRecord, oversizedNotice(&named)
}

// classifyOversizedLine measures a refused line without holding it, so the
// capture can tell a line that only crossed the rest of the source cap from one
// larger than any preview may hold.
func (c *Capture) classifyOversizedLine(path string, refused *ingest.RecordExceedsBudgetError) (CutReason, string) {
	size := refused.AtLeastBytes
	limit := c.recordLimit()
	if refused.CeilingBytes < limit {
		size = c.measureLine(path, c.cursor.FileOffset(), limit+1)
	}
	return c.classifyRecord(refused, size)
}

// measureLine returns the length of the line starting at offset, reading one
// chunk at a time and keeping none of it, and stops counting at most.
func (c *Capture) measureLine(path string, offset, most int64) int64 {
	var length int64
	for length < most {
		chunk, err := c.ranged.ReadFileRange(path, offset+length, min(measureChunkBytes, most-length))
		if err != nil || len(chunk) == 0 {
			return length
		}
		if cut := bytes.IndexByte(chunk, '\n'); cut >= 0 {
			return length + int64(cut) + 1
		}
		length += int64(len(chunk))
	}
	return length
}

// measureChunkBytes is how much measureLine reads at a time.
const measureChunkBytes int64 = 1 << 20

func (c *Capture) consumed() int64 {
	if c.kind == originFileRanged {
		return int64(len(c.loaded))
	}
	return c.cursor.ConsumedBytes()
}

func (c *Capture) stepFileWhole(ctx context.Context) (Page, error) {
	path := string(c.session.SourcePath)
	if c.opts.SourceCapBytes > 0 {
		info, err := c.reader.fs.Stat(path)
		if err != nil {
			return Page{}, captureError(CloseUnreadable, err, "measuring the transcript before a whole read")
		}
		if info.Size() > c.opts.SourceCapBytes {
			c.started = true
			return Page{Session: c.header(nil), Continuation: ContinuationCut, CutReason: CutOriginNotResumable,
				Notice: notResumableNotice(0, info.Size())}, nil
		}
	}
	entries, err := c.indexWhole(ctx)
	if err != nil {
		return Page{}, captureError(CloseUnreadable, err, "reading the whole transcript")
	}
	c.started = true
	page, err := c.fold(entries, true, true)
	if err != nil {
		return Page{}, err
	}
	if page.Continuation != ContinuationCut {
		page.Continuation = ContinuationEnd
	}
	return page, nil
}

func (c *Capture) stepSQLiteResumable(ctx context.Context) (Page, error) {
	if c.started && c.sourceMore && c.marked == nil {
		return Page{Session: c.header(nil), Continuation: ContinuationCut, CutReason: CutOriginNotResumable,
			Notice: notResumableNotice(c.cursor.ConsumedBytes(), c.cursor.TotalBytes())}, nil
	}
	budget, ceiling, exhausted := c.readBudget(c.cursor.ConsumedBytes())
	if exhausted {
		return Page{Session: c.header(nil), Continuation: ContinuationCut, CutReason: CutSourceBudget,
			Notice: sourceBudgetNotice(c.cursor.ConsumedBytes(), c.cursor.TotalBytes(), c.opts.SourceCapBytes)}, nil
	}
	if c.opts.SourceCapBytes == 0 {
		// OpenCode has no per-record size limit; an unbounded capture takes
		// every row whole, as ingest does.
		ceiling = 0
	}
	if c.started && c.marked != nil {
		marker, supported, err := c.marked.TranscriptChangeMarker(ctx, c.session, c.cursor)
		if err != nil {
			return Page{}, captureError(CloseUnreadable, err, "summarizing the rows already read")
		}
		if !supported || marker != c.marker {
			return Page{}, captureError(CloseSourceChanged, nil, "the %d rows already read changed before the next read", c.marker.Rows)
		}
	}
	var slice ingest.TranscriptSlice
	var err error
	if limited, ok := c.sqlite.(ingest.LimitedResumableTranscriptMaterializer); ok {
		slice, err = limited.MaterializeTranscriptSliceWithin(ctx, c.session, budget, c.cursor, ingest.TranscriptSliceLimits{RecordCeilingBytes: ceiling})
	} else {
		slice, err = c.sqlite.MaterializeTranscriptSlice(ctx, c.session, budget, c.cursor)
	}
	var oversized *ingest.RecordExceedsBudgetError
	if errors.As(err, &oversized) {
		cut, notice := c.classifyRecord(oversized, oversized.AtLeastBytes)
		return Page{Session: c.header(nil), Continuation: ContinuationCut, CutReason: cut, Notice: notice}, nil
	}
	if err != nil {
		return Page{}, captureError(CloseUnreadable, err, "reading a slice of %d bytes after %d rows", budget, c.cursor.ConsumedRows())
	}
	c.started = true
	c.cursor, c.sourceMore = slice.Next, slice.More
	if c.marked != nil && c.sourceMore {
		marker, supported, err := c.marked.TranscriptChangeMarker(ctx, c.session, c.cursor)
		if err != nil {
			return Page{}, captureError(CloseUnreadable, err, "summarizing the rows just read")
		}
		if !supported {
			c.marked = nil
		}
		c.marker = marker
	}
	entries, err := c.parse(ctx, slice.Data)
	if err != nil {
		return Page{}, err
	}
	c.rebase(entries)
	page, err := c.fold(entries, true, false)
	if err != nil {
		return Page{}, err
	}
	if page.Continuation == ContinuationCut {
		return page, nil
	}
	switch {
	case !c.sourceMore:
		page.Continuation = ContinuationEnd
	case c.marked == nil:
		page.Continuation, page.CutReason = ContinuationCut, CutOriginNotResumable
		page.Notice = notResumableNotice(c.cursor.ConsumedBytes(), c.cursor.TotalBytes())
	}
	return page, nil
}

func (c *Capture) stepSQLiteBounded(ctx context.Context) (Page, error) {
	budget, _, exhausted := c.readBudget(0)
	if exhausted {
		budget = c.opts.FirstReadBytes
	}
	_, data, truncation, err := c.bounded.MaterializeTranscriptBounded(ctx, c.session, budget)
	if err != nil {
		return Page{}, captureError(CloseUnreadable, err, "reading one bounded slice of %d bytes", budget)
	}
	c.started = true
	entries, err := c.parse(ctx, data)
	if err != nil {
		return Page{}, err
	}
	page, err := c.fold(entries, true, false)
	if err != nil || page.Continuation == ContinuationCut {
		return page, err
	}
	page.Continuation = ContinuationEnd
	if truncation.Truncated {
		page.Continuation, page.CutReason = ContinuationCut, CutOriginNotResumable
		page.Notice = notResumableNotice(truncation.IncludedBytes, truncation.TotalBytes)
	}
	return page, nil
}

func (c *Capture) parse(ctx context.Context, data []byte) ([]schema.SessionEntry, error) {
	if len(data) == 0 {
		return nil, nil
	}
	entries, err := c.indexBytes(ctx, data)
	if err != nil {
		if ctx.Err() != nil {
			return nil, captureError(CloseClosed, err, "")
		}
		return nil, captureError(CloseUnreadable, err, "parsing %d loaded bytes", len(data))
	}
	return entries, nil
}

// indexBytes parses through the harness's authoritative capture entry point,
// the one that keeps retained unknown records, so the preview shows what an
// import would keep. A transcript that entry point refuses (a malformed record
// it will not certify) is read through the tolerant compatibility parser
// instead, and the capture is marked partial.
func (c *Capture) indexBytes(ctx context.Context, data []byte) ([]schema.SessionEntry, error) {
	if authoritative, ok := c.indexer.(ingest.AuthoritativeTranscriptIndexer); ok {
		result, err := authoritative.IndexTranscriptBytesForCapture(ctx, c.session, data)
		if err == nil {
			return result.Entries, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		c.partial = true
	}
	return c.indexer.IndexTranscriptBytes(ctx, c.session, data)
}

// indexWhole is indexBytes for a transcript read whole at its path.
func (c *Capture) indexWhole(ctx context.Context) ([]schema.SessionEntry, error) {
	if authoritative, ok := c.indexer.(ingest.AuthoritativeTranscriptIndexer); ok {
		result, err := authoritative.IndexTranscriptForCapture(ctx, c.session)
		if err == nil {
			return result.Entries, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		c.partial = true
	}
	return c.indexer.IndexTranscript(ctx, c.session)
}

// rebase moves a SQLite slice's entries past every entry of earlier slices.
// Each slice is folded on its own, and a fold that numbers its entries from
// zero would otherwise give two turns one identity.
func (c *Capture) rebase(entries []schema.SessionEntry) {
	if len(entries) == 0 {
		return
	}
	lowest := entries[0].EntryIndex
	for _, entry := range entries {
		lowest = min(lowest, entry.EntryIndex)
	}
	if shift := c.maxEntry + 1 - lowest; c.maxEntry >= 0 && shift > 0 {
		for i := range entries {
			entries[i].EntryIndex += shift
			if entries[i].ParentIndex != nil {
				parent := *entries[i].ParentIndex + shift
				entries[i].ParentIndex = &parent
			}
		}
	}
	for _, entry := range entries {
		c.maxEntry = max(c.maxEntry, entry.EntryIndex)
	}
}

// fold projects the entries, proves the turns already handed out are
// unchanged, and builds the page of newly sealed turns.
//
// prefix reports that entries are the whole loaded prefix (a file re-parse), so
// the turns already handed out are expected at its head. Otherwise the entries
// are a SQLite slice that holds only new turns. final reports that nothing
// later can change the turns: the read reached its end or the capture is cut.
func (c *Capture) fold(entries []schema.SessionEntry, final, prefix bool) (Page, error) {
	projection, err := transcript.EntriesToProjectionValidated(entries, transcript.ProjectionOptions{Harness: c.session.Harness, BoundedPreview: true})
	if err != nil {
		return Page{}, captureError(CloseUnreadable, err, "projecting %d entries", len(entries))
	}
	if projection.Diagnostics != nil && projection.Diagnostics.Partial {
		c.partial = true
	}
	results := resultIDs(entries)
	turns := projection.Turns
	start := 0
	if prefix {
		if err := c.verifyEmitted(turns, results); err != nil {
			return Page{}, err
		}
		start = len(c.emitted)
	} else if len(turns) > 0 && len(c.emitted) > 0 && turns[0].Index <= c.emitted[len(c.emitted)-1].index {
		return Page{}, captureError(CloseSourceChanged, nil, "a slice began at turn %d, not after the %d turns already shown", turns[0].Index, len(c.emitted))
	}
	end := c.sealedEnd(turns, start, results, final)
	cut := CutNone
	if c.opts.TurnCap > 0 {
		// The cap cuts when it stops this page short, or when it is reached
		// with more of the session still to come.
		remaining := c.opts.TurnCap - len(c.emitted)
		moreToCome := end < len(turns) || !final
		if end-start > remaining || (end-start == remaining && moreToCome) {
			end, cut = start+remaining, CutTurnBudget
		}
	}
	page := Page{Turns: append([]ingest.Turn(nil), turns[start:end]...), Continuation: ContinuationMore}
	for i := range page.Turns {
		c.emitted = append(c.emitted, emittedTurn{index: page.Turns[i].Index, digest: turnDigest(page.Turns[i]), pending: pendingCalls(page.Turns[i], results)})
		if c.firstTurn == nil {
			first := page.Turns[i]
			c.firstTurn = &first
		}
		last := page.Turns[i]
		c.lastTurn = &last
	}
	boundary := math.MaxInt
	if end < len(turns) {
		boundary = turns[end].Index
	}
	retained, err := c.retainedRecords(entries, boundary)
	if err != nil {
		return Page{}, err
	}
	page.RetainedUnknown = retained
	page.NativeMetadata = attachedMetadata(projection.NativeMetadata, page.Turns)
	page.Session = c.header(projection.NativeMetadata)
	if cut != CutNone {
		page.Continuation, page.CutReason, page.Notice = ContinuationCut, cut, turnBudgetNotice(c.opts.TurnCap)
	}
	return page, nil
}

// verifyEmitted proves every turn already handed out is still at the head of
// the re-parse, unchanged.
func (c *Capture) verifyEmitted(turns []ingest.Turn, results map[string]struct{}) error {
	if len(turns) < len(c.emitted) {
		return captureError(CloseSourceChanged, nil, "the re-read holds %d turns, fewer than the %d already shown", len(turns), len(c.emitted))
	}
	for i, shown := range c.emitted {
		turn := turns[i]
		for _, call := range turn.ToolCalls {
			if _, waiting := shown.pending[call.ID]; waiting {
				if _, answered := results[call.ID]; answered {
					return captureError(CloseLateResult, nil, "a result arrived for a call shown without one, in turn %d of %d shown", i+1, len(c.emitted))
				}
			}
		}
		if turn.Index != shown.index || turnDigest(turn) != shown.digest {
			return captureError(CloseSourceChanged, nil, "turn %d of %d shown read differently on the next read", i+1, len(c.emitted))
		}
	}
	return nil
}

// sealedEnd returns the end of the run of turns from start that can be handed
// out now. A turn is held back while one of its tool calls waits for a result,
// and the last parsed turn is held back while the read can still continue it.
// Past the holdback bound the oldest held turns go out with their calls
// pending, so an unanswered call cannot stall the preview.
func (c *Capture) sealedEnd(turns []ingest.Turn, start int, results map[string]struct{}, final bool) int {
	if final {
		return len(turns)
	}
	limit := len(turns) - 1
	end := start
	for end < limit {
		if len(pendingCalls(turns[end], results)) == 0 {
			end++
			continue
		}
		held := 0
		for i := end; i < len(turns); i++ {
			held += transcript.FoldedTurnByteLength(turns[i])
		}
		if held <= c.opts.holdback() {
			break
		}
		end++
	}
	return max(end, start)
}

// retainedRecords projects the retained records carried by entries at or after
// the last boundary and before the new one, one entry at a time so each record
// keeps the entry index that positions it.
func (c *Capture) retainedRecords(entries []schema.SessionEntry, boundary int) ([]RetainedRecord, error) {
	var records []RetainedRecord
	for _, entry := range entries {
		if entry.EntryIndex < c.retainedBoundary || entry.EntryIndex >= boundary {
			continue
		}
		// A record rides either an evidence-only carrier row or the
		// conversational row it was found inside.
		if found, err := ingest.RetainedUnknownOf(entry); err != nil || len(found) == 0 {
			continue
		}
		projected, err := ingest.ProjectRetainedUnknown([]schema.SessionEntry{entry}, c.session.Harness)
		if errors.Is(err, ingest.ErrUnknownPositionUnavailable) {
			c.partial = true
			continue
		}
		if err != nil {
			return nil, captureError(CloseUnreadable, err, "projecting the retained record at entry %d", entry.EntryIndex)
		}
		for _, record := range projected {
			records = append(records, RetainedRecord{Record: record, EntryIndex: entry.EntryIndex})
		}
	}
	if len(records) > 0 {
		c.partial = true
	}
	if boundary > c.retainedBoundary {
		c.retainedBoundary = boundary
	}
	return records, nil
}

// header is the session as far as loaded: identity, the listing's context,
// the time span of the turns handed out, session-level metadata and the
// cumulative diagnostics.
func (c *Capture) header(metadata []schema.NativeMetadataRecord) ingest.Session {
	session := ingest.Session{
		ID:          c.session.SessionID,
		Harness:     c.session.Harness,
		Project:     c.listing.ProjectName,
		GitBranch:   c.listing.Branch,
		GitRemote:   c.listing.GitRemote,
		ProjectPath: c.listing.WorkingDir,
	}
	if c.firstTurn != nil {
		session.StartTime = c.firstTurn.Timestamp
		session.EndTime = c.lastTurn.Timestamp
	}
	for _, record := range metadata {
		if record.Attachment == nil {
			session.NativeMetadata = append(session.NativeMetadata, record)
		}
	}
	if c.partial {
		session.Diagnostics = &schema.InterpretationDiagnostics{Partial: true}
	}
	return session
}

func attachedMetadata(records []schema.NativeMetadataRecord, turns []ingest.Turn) []schema.NativeMetadataRecord {
	if len(records) == 0 || len(turns) == 0 {
		return nil
	}
	indexes := make(map[int]struct{}, len(turns))
	for _, turn := range turns {
		indexes[turn.Index] = struct{}{}
	}
	var attached []schema.NativeMetadataRecord
	for _, record := range records {
		if record.Attachment == nil || record.Attachment.TurnIndex == nil {
			continue
		}
		if _, ok := indexes[*record.Attachment.TurnIndex]; ok {
			attached = append(attached, record)
		}
	}
	return attached
}

// resultIDs is the set of tool call identifiers that have a result entry.
func resultIDs(entries []schema.SessionEntry) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, entry := range entries {
		if entry.EntryType == schema.EntryTypeToolResult && entry.ToolCallID != nil {
			ids[*entry.ToolCallID] = struct{}{}
		}
	}
	return ids
}

// pendingCalls names the turn's tool calls that have no result yet.
func pendingCalls(turn ingest.Turn, results map[string]struct{}) map[string]struct{} {
	var pending map[string]struct{}
	for _, call := range turn.ToolCalls {
		if _, ok := results[call.ID]; ok {
			continue
		}
		if pending == nil {
			pending = make(map[string]struct{})
		}
		pending[call.ID] = struct{}{}
	}
	return pending
}
