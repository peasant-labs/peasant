package sourcepreview

import (
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// Continuation says whether a capture can load more. Closed set.
type Continuation uint8

const (
	// ContinuationMore: the capture can load more of the session.
	ContinuationMore Continuation = iota + 1
	// ContinuationEnd: the capture read the session to the extent fixed at open.
	ContinuationEnd
	// ContinuationCut: the capture stopped before the end; CutReason says why.
	ContinuationCut
)

// String returns the contract spelling (more | end | cut).
func (c Continuation) String() string {
	switch c {
	case ContinuationMore:
		return "more"
	case ContinuationEnd:
		return "end"
	case ContinuationCut:
		return "cut"
	}
	return fmt.Sprintf("continuation(%d)", uint8(c))
}

// CutReason says why a capture stopped before the end. Closed set.
type CutReason uint8

const (
	// CutNone: the capture was not cut.
	CutNone CutReason = iota
	// CutSourceBudget: the cumulative source cap was reached.
	CutSourceBudget
	// CutSerializedBudget: the segmenter's serialized cap was reached.
	CutSerializedBudget
	// CutTurnBudget: the turn cap was reached.
	CutTurnBudget
	// CutOversizedRecord: the next record is larger than the capture may hold.
	CutOversizedRecord
	// CutOriginNotResumable: the source cannot be continued safely.
	CutOriginNotResumable
)

// AllCutReasons lists every cut reason, in declaration order.
func AllCutReasons() []CutReason {
	return []CutReason{CutSourceBudget, CutSerializedBudget, CutTurnBudget, CutOversizedRecord, CutOriginNotResumable}
}

// String returns the contract spelling of the reason.
func (r CutReason) String() string {
	switch r {
	case CutNone:
		return ""
	case CutSourceBudget:
		return "source_budget"
	case CutSerializedBudget:
		return "serialized_budget"
	case CutTurnBudget:
		return "turn_budget"
	case CutOversizedRecord:
		return "oversized_record"
	case CutOriginNotResumable:
		return "origin_not_resumable"
	}
	return fmt.Sprintf("cut_reason(%d)", uint8(r))
}

// Page is what one Open or Next adds to a capture: the session header as far
// as loaded, and the sealed turns not handed out before, in order.
type Page struct {
	// Session is the header as far as loaded. Turns is always nil. Its
	// NativeMetadata holds only session-level records (no turn attachment),
	// and its Diagnostics are cumulative: once partial, always partial.
	Session ingest.Session
	// Turns are sealed turns, strictly after every turn of earlier pages.
	Turns []ingest.Turn
	// NativeMetadata holds the records attached to these turns.
	NativeMetadata []schema.NativeMetadataRecord
	// RetainedUnknown holds the retained records positioned since the last
	// page, each with the entry index of the record that carried it.
	RetainedUnknown []RetainedRecord
	// Continuation says whether more can load.
	Continuation Continuation
	// CutReason is set exactly when Continuation is ContinuationCut.
	CutReason CutReason
	// Notice is the sentence a surface shows for a cut or a grown source, or
	// the empty string.
	Notice string
}

// RetainedRecord is one retained unknown record and the entry index of the
// source record that carried it, so a segmenter can place it beside the turns
// it sits between.
type RetainedRecord struct {
	Record     schema.RetainedUnknownRecord
	EntryIndex int
}

// CloseReason names why a capture ended without a page. Closed set.
type CloseReason uint8

const (
	// CloseSourceChanged: the source changed under what was already shown.
	CloseSourceChanged CloseReason = iota + 1
	// CloseLateResult: a later record answered a tool call of a turn already
	// shown, so the turn as shown is no longer how the session reads.
	CloseLateResult
	// CloseUnavailable: discovery recorded no transcript location.
	CloseUnavailable
	// CloseNoTurns: the transcript holds no readable turns.
	CloseNoTurns
	// CloseUnreadable: the transcript could not be read or parsed.
	CloseUnreadable
	// CloseClosed: the capture was closed or cancelled.
	CloseClosed
	// CloseBusy: another operation on the capture is still running.
	CloseBusy
)

// AllCloseReasons lists the capture close reasons in declaration order.
func AllCloseReasons() []CloseReason {
	return []CloseReason{CloseSourceChanged, CloseLateResult, CloseUnavailable, CloseNoTurns, CloseUnreadable, CloseClosed, CloseBusy}
}

// String returns the reason's stable spelling.
func (r CloseReason) String() string {
	switch r {
	case CloseSourceChanged:
		return "source_changed"
	case CloseLateResult:
		return "late_result"
	case CloseUnavailable:
		return "unavailable"
	case CloseNoTurns:
		return "no_turns"
	case CloseUnreadable:
		return "unreadable"
	case CloseClosed:
		return "closed"
	case CloseBusy:
		return "busy"
	}
	return fmt.Sprintf("close_reason(%d)", uint8(r))
}

// The sentinels a capture error matches with errors.Is. Each one's text is a
// fixed sentence: it names no path, no identifier and no source text, so a
// surface may show it and a log may record it.
var (
	ErrSourceChanged = errors.New("this session's transcript changed after part of it was shown. read it again from the start.")
	ErrLateResult    = errors.New("a later part of this session answers a tool call in a turn already shown. read it again from the start to see the answer in place.")
	ErrUnavailable   = errors.New("no transcript preview is available for this session; it still ingests normally.")
	ErrNoTurns       = errors.New("no readable turns are in this session's transcript yet.")
	ErrUnreadable    = errors.New("this session's transcript could not be read for a preview. the full session still ingests normally; check that the file or database is readable and read it again.")
	ErrClosed        = errors.New("this preview was closed. open the preview again to read the session.")
	ErrBusy          = errors.New("this preview is still reading. wait for it to finish before asking for more.")
)

func (r CloseReason) sentinel() error {
	switch r {
	case CloseSourceChanged:
		return ErrSourceChanged
	case CloseLateResult:
		return ErrLateResult
	case CloseUnavailable:
		return ErrUnavailable
	case CloseNoTurns:
		return ErrNoTurns
	case CloseUnreadable:
		return ErrUnreadable
	case CloseClosed:
		return ErrClosed
	case CloseBusy:
		return ErrBusy
	}
	return ErrUnreadable
}

// CaptureError ends a capture. Error() is the reason's fixed sentence plus a
// non-identifying "where and when" (an operation and byte or turn counts); the
// underlying cause, which may name a path, is reachable only through
// errors.As / errors.Unwrap and is never part of the text.
type CaptureError struct {
	Reason CloseReason
	// Detail says where and when, in counts only.
	Detail string
	cause  error
}

func (e *CaptureError) Error() string {
	if e.Detail == "" {
		return e.Reason.sentinel().Error()
	}
	return e.Reason.sentinel().Error() + " (" + e.Detail + ")"
}

// Unwrap exposes the reason's sentinel and the underlying cause.
func (e *CaptureError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.Reason.sentinel()}
	}
	return []error{e.Reason.sentinel(), e.cause}
}

func captureError(reason CloseReason, cause error, format string, args ...any) *CaptureError {
	return &CaptureError{Reason: reason, Detail: fmt.Sprintf(format, args...), cause: cause}
}

// ReasonOf returns the close reason of an error a capture returned, and false
// for any other error.
func ReasonOf(err error) (CloseReason, bool) {
	var captureErr *CaptureError
	if errors.As(err, &captureErr) {
		return captureErr.Reason, true
	}
	return 0, false
}
