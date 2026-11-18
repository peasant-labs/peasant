package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/sourcepreview"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

type previewRegistry struct {
	mu       sync.Mutex
	sessions *kickstartSessions
	captures map[*Conn]*webPreview
	worker   chan struct{}
	revision atomic.Uint64
	newID    func() (string, error)
}

var errPreviewReasonUnmapped = errors.New("preview reason has no wire mapping")

func previewCutReason(reason sourcepreview.CutReason) (schema.LocalKickstartPreviewCutReason, bool) {
	switch reason {
	case sourcepreview.CutSourceBudget:
		return schema.LocalKickstartPreviewCutSourceBudget, true
	case sourcepreview.CutSerializedBudget:
		return schema.LocalKickstartPreviewCutSerializedBudget, true
	case sourcepreview.CutTurnBudget:
		return schema.LocalKickstartPreviewCutTurnBudget, true
	case sourcepreview.CutOversizedRecord:
		return schema.LocalKickstartPreviewCutOversizedRecord, true
	case sourcepreview.CutOriginNotResumable:
		return schema.LocalKickstartPreviewCutOriginNotResumable, true
	default:
		return "", false
	}
}

func previewCloseReason(reason sourcepreview.CloseReason) (schema.LocalKickstartPreviewCloseReason, bool) {
	switch reason {
	case sourcepreview.CloseSourceChanged:
		return schema.LocalKickstartPreviewSourceChanged, true
	case sourcepreview.CloseLateResult:
		return schema.LocalKickstartPreviewLateResult, true
	case sourcepreview.CloseUnavailable, sourcepreview.CloseClosed:
		return schema.LocalKickstartPreviewUnavailable, true
	case sourcepreview.CloseNoTurns:
		return schema.LocalKickstartPreviewNoTurns, true
	case sourcepreview.CloseUnreadable:
		return schema.LocalKickstartPreviewUnreadable, true
	case sourcepreview.CloseBusy:
		return schema.LocalKickstartPreviewOutOfOrder, true
	default:
		return "", false
	}
}

type webPreview struct {
	outMu     sync.Mutex
	conn      *Conn
	sessionID string
	id        string
	requestID string
	revision  string
	ctx       context.Context
	cancel    context.CancelFunc
	stopIdle  func() bool
	last      time.Time
	busy      bool
	opened    bool
	seq       uint32
	capture   *sourcepreview.Capture
	segmenter *sourcepreview.Segmenter
	page      sourcepreview.Page
}

func newPreviewRegistry(sessions *kickstartSessions) *previewRegistry {
	return &previewRegistry{sessions: sessions, captures: make(map[*Conn]*webPreview), worker: make(chan struct{}, defaults.KickstartWebPreviewParseWorkers), newID: newPreviewCaptureID}
}

func newPreviewCaptureID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(id[:]), nil
}

func (h *Hub) previewDispatch(ctx context.Context, conn *Conn, request *schema.LocalKickstartPreviewRequest) {
	h.kickstartMu.RLock()
	registry := h.previews
	h.kickstartMu.RUnlock()
	if registry == nil || request == nil || request.Validate() != nil {
		conn.Send(ServerMessage{Type: MsgError, Message: "preview request is invalid or unavailable; reconnect and open the preview again"})
		return
	}
	registry.dispatch(ctx, conn, *request)
}

func (h *Hub) previewConnectionClosed(conn *Conn) {
	h.kickstartMu.RLock()
	registry := h.previews
	h.kickstartMu.RUnlock()
	if registry != nil {
		registry.remove(conn, schema.LocalKickstartPreviewClosedByClient, false)
	}
}

func (r *previewRegistry) dispatch(ctx context.Context, conn *Conn, req schema.LocalKickstartPreviewRequest) {
	if req.Op == schema.LocalKickstartPreviewOpen {
		r.open(ctx, conn, req)
		return
	}
	r.mu.Lock()
	p := r.captures[conn]
	if p == nil || p.id != req.CaptureID || p.sessionID != req.SessionID {
		r.mu.Unlock()
		r.refuse(ctx, conn, req, schema.LocalKickstartPreviewUnavailable, schema.LocalKickstartErrorPreviewCaptureNotFound, "this preview is no longer available; open it again")
		return
	}
	if req.Op == schema.LocalKickstartPreviewClose {
		r.removeLocked(p, schema.LocalKickstartPreviewClosedByClient, true)
		r.mu.Unlock()
		return
	}
	if p.busy || req.After != p.seq {
		r.removeLocked(p, schema.LocalKickstartPreviewOutOfOrder, true)
		r.mu.Unlock()
		return
	}
	p.busy = true
	if p.stopIdle != nil {
		p.stopIdle()
	}
	r.mu.Unlock()
	go r.advance(p)
}

func (r *previewRegistry) open(ctx context.Context, conn *Conn, req schema.LocalKickstartPreviewRequest) {
	if !conn.IsSubscribed(TopicKickstart) {
		r.refuse(ctx, conn, req, schema.LocalKickstartPreviewUnavailable, "", "subscribe to kickstart before opening a preview")
		return
	}
	target, err := r.sessions.previewTarget(req.SessionID, req.NodeID)
	if err != nil {
		reason, code := schema.LocalKickstartPreviewRowNotFound, schema.LocalKickstartErrorPreviewRowNotFound
		var ke *kickstartError
		if errors.As(err, &ke) && ke.code == schema.LocalKickstartErrorSessionNotFound {
			reason, code = schema.LocalKickstartPreviewSessionClosed, schema.LocalKickstartErrorSessionNotFound
		}
		r.refuse(ctx, conn, req, reason, code, "the setup session or row is unavailable; reload the tree and try again")
		return
	}
	if target.node.Kind != onboarding.NodeKindSession {
		r.refuse(ctx, conn, req, schema.LocalKickstartPreviewNotSessionRow, "", "this row is not a session; use the repository preview instead")
		return
	}
	id, err := r.newID()
	if err != nil {
		r.refuse(ctx, conn, req, schema.LocalKickstartPreviewUnavailable, "", "the preview could not start securely; try again")
		return
	}
	captureCtx, cancel := context.WithCancel(ctx)
	p := &webPreview{conn: conn, sessionID: req.SessionID, requestID: req.RequestID, id: id, revision: strconv.FormatUint(r.revision.Add(1), 10), ctx: captureCtx, cancel: cancel, busy: true, last: r.sessions.env.now()}
	r.sessions.mu.Lock()
	session, liveErr := r.sessions.liveLocked(req.SessionID)
	if liveErr != nil || session.scanGen != target.scanGen {
		r.sessions.mu.Unlock()
		cancel()
		r.refuse(ctx, conn, req, schema.LocalKickstartPreviewRescanned, "", "the setup changed while the preview opened; reload the tree and try again")
		return
	}
	r.mu.Lock()
	existing := r.captures[conn]
	if existing == nil && len(r.captures) >= defaults.KickstartWebPreviewLiveCaptures {
		var oldest *webPreview
		for _, candidate := range r.captures {
			if !candidate.busy && (oldest == nil || candidate.last.Before(oldest.last)) {
				oldest = candidate
			}
		}
		if oldest == nil {
			r.mu.Unlock()
			r.sessions.mu.Unlock()
			cancel()
			r.refuse(ctx, conn, req, schema.LocalKickstartPreviewCapacity, schema.LocalKickstartErrorPreviewCapacity, "both previews are reading; wait and try again")
			return
		}
		r.removeLocked(oldest, schema.LocalKickstartPreviewEvicted, true)
	}
	var previous <-chan struct{}
	if existing != nil {
		previous = r.removeLocked(existing, schema.LocalKickstartPreviewReplaced, true)
	}
	r.captures[conn] = p
	r.mu.Unlock()
	r.sessions.mu.Unlock()
	sessionID := target.node.SessionID
	if sessionID == "" {
		sessionID = target.node.ID
	}
	go func() {
		if previous != nil {
			select {
			case <-previous:
			case <-p.ctx.Done():
				return
			}
		}
		r.start(p, target.listings, sessionID)
	}()
}

func (r *previewRegistry) start(p *webPreview, listings []onboarding.SessionListing, sessionID string) {
	select {
	case r.worker <- struct{}{}:
	case <-p.ctx.Done():
		return
	}
	reader := sourcepreview.NewReader(r.sessions.env.previewFS, listings)
	capture, page, err := reader.Open(p.ctx, sessionID, sourcepreview.CaptureOptions{
		FirstReadBytes: defaults.TranscriptInitialReadBytes, SliceReadBytes: defaults.TranscriptContinuationReadBytes,
		SourceCapBytes: defaults.KickstartWebPreviewSourceCapBytes, TurnCap: defaults.KickstartWebPreviewTurnCap,
	})
	<-r.worker
	if err != nil {
		r.fail(p, err)
		return
	}
	r.mu.Lock()
	if r.captures[p.conn] != p {
		r.mu.Unlock()
		capture.Close()
		return
	}
	p.capture = capture
	r.mu.Unlock()
	p.segmenter, err = sourcepreview.NewSegmenter(p.revision, sourcepreview.SegmenterOptions{BudgetBytes: schema.TranscriptSegmentBudgetBytes, FrameCapBytes: defaults.KickstartWebPreviewFrameCapBytes, SerializedCapBytes: defaults.KickstartWebPreviewSerializedCapBytes})
	if err != nil {
		r.fail(p, err)
		return
	}
	p.page = page
	p.segmenter.Push(page)
	header, err := transcript.SessionToDetailValidated(&page.Session)
	if err != nil {
		r.fail(p, err)
		return
	}
	opened := schema.LocalKickstartPreviewEvent{Kind: schema.LocalKickstartPreviewOpened, SessionID: p.sessionID, RequestID: p.requestID, CaptureID: p.id, Header: header, Directory: p.directory(false), Continuation: schema.LocalKickstartPreviewContinuationMore}
	if err := r.send(p, opened); err != nil {
		r.sendFailure(p, err)
		return
	}
	r.advance(p)
}

func (r *previewRegistry) advance(p *webPreview) {
	for !p.segmenter.Pending() && p.page.Continuation == sourcepreview.ContinuationMore {
		select {
		case r.worker <- struct{}{}:
		case <-p.ctx.Done():
			return
		}
		page, err := p.capture.Next(p.ctx)
		<-r.worker
		if err != nil {
			r.fail(p, err)
			return
		}
		p.page = page
		p.segmenter.Push(page)
	}
	final := p.page.Continuation != sourcepreview.ContinuationMore
	segment, ok, err := p.segmenter.Next(final)
	if errors.Is(err, sourcepreview.ErrSerializedBudget) {
		final = true
		p.page.Continuation = sourcepreview.ContinuationCut
		p.page.CutReason = sourcepreview.CutSerializedBudget
		p.page.Notice = p.segmenter.SerializedCutNotice()
	} else if err != nil {
		r.fail(p, err)
		return
	}
	final = final && !p.segmenter.Pending()
	header, err := transcript.SessionToDetailValidated(&p.page.Session)
	if err != nil {
		r.fail(p, err)
		return
	}
	event := schema.LocalKickstartPreviewEvent{Kind: schema.LocalKickstartPreviewSegmentEvent, SessionID: p.sessionID, CaptureID: p.id, Header: header, Directory: p.directory(final), Continuation: schema.LocalKickstartPreviewContinuationMore}
	if ok {
		event.Segment = &schema.LocalKickstartPreviewSegment{TranscriptSegment: segment.Segment, NativeMetadata: segment.NativeMetadata, RetainedUnknown: segment.RetainedUnknown}
	}
	if final {
		event.Continuation = schema.LocalKickstartPreviewContinuationEnd
		if p.page.Continuation == sourcepreview.ContinuationCut {
			event.Continuation = schema.LocalKickstartPreviewContinuationCut
			var mapped bool
			event.CutReason, mapped = previewCutReason(p.page.CutReason)
			if !mapped {
				r.fail(p, errPreviewReasonUnmapped)
				return
			}
		}
		if p.page.Notice != "" {
			event.Lines = []schema.LocalKickstartLine{{Kind: schema.LocalKickstartLineText, Text: p.page.Notice}}
		}
	}
	r.mu.Lock()
	if r.captures[p.conn] != p {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	if err := r.send(p, event); err != nil {
		r.sendFailure(p, err)
		return
	}
}

func (r *previewRegistry) sendFailure(p *webPreview, err error) {
	if errors.Is(err, errPreviewFrameTooLarge) {
		r.fail(p, err)
		return
	}
	r.removeIf(p, schema.LocalKickstartPreviewSlowClient, true)
}

func (p *webPreview) directory(complete bool) *schema.LocalKickstartPreviewDirectory {
	refs := p.segmenter.Refs()
	if refs == nil {
		refs = []schema.TranscriptSegmentRef{}
	}
	conversation := p.segmenter.LoadedConversationTurns()
	d := &schema.LocalKickstartPreviewDirectory{SessionID: schema.SessionID(p.page.Session.ID), Revision: p.revision, Complete: complete, LoadedSegments: len(refs), LoadedTurns: p.segmenter.LoadedTurns(), LoadedConversationTurns: &conversation, Segments: refs}
	if complete {
		d.TotalSegments, d.TotalTurns, d.TotalConversationTurns = &d.LoadedSegments, &d.LoadedTurns, &conversation
	}
	return d
}

func (r *previewRegistry) send(p *webPreview, event schema.LocalKickstartPreviewEvent) error {
	p.outMu.Lock()
	defer p.outMu.Unlock()
	r.mu.Lock()
	if r.captures[p.conn] != p || p.ctx.Err() != nil {
		r.mu.Unlock()
		return context.Canceled
	}
	if event.Kind != schema.LocalKickstartPreviewOpened {
		event.Seq = p.seq + 1
		p.seq = event.Seq
		if !event.Directory.Complete {
			p.busy = false
		}
	}
	r.mu.Unlock()
	err := p.conn.SendWait(p.ctx, ServerMessage{Type: schema.MsgKickstartPreview, Topic: TopicKickstart, Data: event})
	if err != nil && event.Kind == schema.LocalKickstartPreviewSegmentEvent {
		r.mu.Lock()
		p.seq = event.Seq - 1
		r.mu.Unlock()
	}
	if err == nil {
		r.mu.Lock()
		p.opened = true
		if event.Directory.Complete && r.captures[p.conn] == p {
			delete(r.captures, p.conn)
			p.cancel()
			go p.capture.Close()
		} else if event.Kind == schema.LocalKickstartPreviewSegmentEvent && r.captures[p.conn] == p && !p.busy && p.seq == event.Seq {
			p.last = r.sessions.env.now()
			p.stopIdle = r.sessions.env.afterFunc(defaults.KickstartWebPreviewIdle, func() {
				r.mu.Lock()
				defer r.mu.Unlock()
				if r.captures[p.conn] == p && !p.busy && p.seq == event.Seq {
					r.removeLocked(p, schema.LocalKickstartPreviewIdle, true)
				}
			})
		}
		r.mu.Unlock()
	}
	return err
}

func (r *previewRegistry) fail(p *webPreview, err error) {
	reason, code, message := schema.LocalKickstartPreviewUnreadable, schema.LocalKickstartErrorPreviewUnreadable, "the preview could not be read; check that the transcript is readable and try again"
	if sourceReason, ok := sourcepreview.ReasonOf(err); ok {
		var mapped bool
		reason, mapped = previewCloseReason(sourceReason)
		if !mapped {
			err = errPreviewReasonUnmapped
		}
	}
	switch {
	case errors.Is(err, errPreviewReasonUnmapped):
		reason, code, message = schema.LocalKickstartPreviewUnavailable, schema.LocalKickstartErrorFailed, "the preview ended because of an internal error; reopen it or report this defect"
	case errors.Is(err, errPreviewFrameTooLarge), errors.Is(err, sourcepreview.ErrFrameExceedsCap):
		reason, code, message = schema.LocalKickstartPreviewUnavailable, schema.LocalKickstartErrorFailed, "the preview cannot be displayed because a frame exceeds the safe size; reopen it or report this defect"
	case errors.Is(err, sourcepreview.ErrUnavailable):
		reason, code, message = schema.LocalKickstartPreviewUnavailable, "", discovery.PreviewUnavailableNote
	case errors.Is(err, sourcepreview.ErrNoTurns):
		reason, code, message = schema.LocalKickstartPreviewNoTurns, "", discovery.PreviewNoTurnsNote
	case errors.Is(err, sourcepreview.ErrSourceChanged):
		reason, code, message = schema.LocalKickstartPreviewSourceChanged, schema.LocalKickstartErrorPreviewSourceChanged, sourcepreview.ErrSourceChanged.Error()
	case errors.Is(err, sourcepreview.ErrLateResult):
		reason, code, message = schema.LocalKickstartPreviewLateResult, schema.LocalKickstartErrorPreviewLateResult, sourcepreview.ErrLateResult.Error()
	case errors.Is(err, sourcepreview.ErrClosed):
		reason, code, message = schema.LocalKickstartPreviewUnavailable, schema.LocalKickstartErrorFailed, "the preview closed unexpectedly; reopen it"
	case errors.Is(err, sourcepreview.ErrBusy):
		reason, code, message = schema.LocalKickstartPreviewOutOfOrder, schema.LocalKickstartErrorPreviewOutOfOrder, "the preview is still reading; open it again"
	}
	r.mu.Lock()
	if r.captures[p.conn] != p {
		r.mu.Unlock()
		return
	}
	delete(r.captures, p.conn)
	if p.stopIdle != nil {
		p.stopIdle()
	}
	p.cancel()
	if p.capture != nil {
		go p.capture.Close()
	}
	r.mu.Unlock()
	p.outMu.Lock()
	defer p.outMu.Unlock()
	r.mu.Lock()
	event := schema.LocalKickstartPreviewEvent{Kind: schema.LocalKickstartPreviewClosed, SessionID: p.sessionID, Reason: reason, Code: code, Message: message}
	if p.opened {
		event.CaptureID, event.Seq = p.id, p.seq+1
	} else {
		event.RequestID = p.requestID
	}
	r.mu.Unlock()
	_ = p.conn.SendWait(context.Background(), ServerMessage{Type: schema.MsgKickstartPreview, Topic: TopicKickstart, Data: event})
}

func (r *previewRegistry) refuse(ctx context.Context, conn *Conn, req schema.LocalKickstartPreviewRequest, reason schema.LocalKickstartPreviewCloseReason, code schema.LocalKickstartErrorCode, message string) {
	event := schema.LocalKickstartPreviewEvent{Kind: schema.LocalKickstartPreviewClosed, SessionID: req.SessionID, RequestID: req.RequestID, Reason: reason, Code: code, Message: message}
	_ = conn.SendWait(ctx, ServerMessage{Type: schema.MsgKickstartPreview, Topic: TopicKickstart, Data: event})
}

func (r *previewRegistry) remove(conn *Conn, reason schema.LocalKickstartPreviewCloseReason, notify bool) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.captures[conn]; p != nil {
		return r.removeLocked(p, reason, notify)
	}
	return nil
}

func (r *previewRegistry) removeIf(p *webPreview, reason schema.LocalKickstartPreviewCloseReason, notify bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.captures[p.conn] == p {
		r.removeLocked(p, reason, notify)
	}
}

func (r *previewRegistry) removeLocked(p *webPreview, reason schema.LocalKickstartPreviewCloseReason, notify bool) <-chan struct{} {
	delete(r.captures, p.conn)
	if p.stopIdle != nil {
		p.stopIdle()
	}
	p.cancel()
	if p.capture != nil {
		go p.capture.Close()
	}
	if notify {
		done := make(chan struct{})
		go func() {
			defer close(done)
			p.outMu.Lock()
			defer p.outMu.Unlock()
			r.mu.Lock()
			event := schema.LocalKickstartPreviewEvent{Kind: schema.LocalKickstartPreviewClosed, SessionID: p.sessionID, Reason: reason}
			if p.opened {
				event.CaptureID, event.Seq = p.id, p.seq+1
			} else {
				event.RequestID = p.requestID
			}
			r.mu.Unlock()
			if reason == schema.LocalKickstartPreviewOutOfOrder {
				event.Code = schema.LocalKickstartErrorPreviewOutOfOrder
				event.Message = "the preview request was out of order; open the preview again"
			}
			_ = p.conn.SendWait(context.Background(), ServerMessage{Type: schema.MsgKickstartPreview, Topic: TopicKickstart, Data: event})
		}()
		return done
	}
	return nil
}

func (r *previewRegistry) closeSession(id string, reason schema.LocalKickstartPreviewCloseReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.captures {
		if p.sessionID == id {
			r.removeLocked(p, reason, true)
		}
	}
}

func (r *previewRegistry) closeAll(reason schema.LocalKickstartPreviewCloseReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.captures {
		r.removeLocked(p, reason, true)
	}
}
