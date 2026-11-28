package ingest

import (
	"context"
	"errors"
	"fmt"
)

// TranscriptSliceCursor marks where one bounded read of a session stopped, so a
// later read continues from that point instead of re-reading what came before.
//
// It is a VALUE, not a handle: it carries the source's own keyset positions and
// the running totals a note quotes, and nothing that holds a database open. The
// preview pane may sit on one for as long as the reader looks at the pane; the
// store is opened read-only for the length of one slice and closed again.
//
// The zero value means "from the beginning of the session", which is what makes
// a first slice and a continuation the same call.
type TranscriptSliceCursor struct {
	// started separates a real continuation from the zero value. The file
	// origin is the zero TranscriptOrigin, so the origin field alone cannot say
	// whether a slice has run.
	started bool
	// origin pins the cursor to the representation it was produced from, so a
	// cursor can never be replayed against the other one.
	origin TranscriptOrigin

	legacyMessage *OpenCodeLegacyMessageCursor
	legacyPart    *OpenCodeLegacyPartCursor
	// legacyBlockedPart names the part the PREVIOUS slice stopped at because its
	// message lay outside that slice's message window. It is the termination
	// guard: a part that blocks two slices in a row is not merely ahead of the
	// window, so the second slice steps over it rather than stalling forever.
	legacyBlockedPart *OpenCodeLegacyPartID

	current *OpenCodeCurrentCursor

	// fileOffset is where a FILE-origin slice read stopped: the byte after the
	// last complete line it took. A line-oriented transcript needs nothing else
	// to be resumed, so the other fields stay zero for it.
	fileOffset int64
	// fileLines is how many complete lines the file-origin slices so far took,
	// so a refused record can be named by its line.
	fileLines int64

	// consumedBytes and consumedRows accumulate across slices: what every slice
	// so far put into the preview. total* are the whole-session figures, read
	// once by the first bounded read and carried, so a continuation never pays
	// the payload-size probe again.
	consumedBytes int64
	consumedRows  int64
	totalBytes    int64
	totalRows     int64
	unit          MaterializeTruncationUnit
}

// IsZero reports whether the cursor is the start-of-session value.
func (c TranscriptSliceCursor) IsZero() bool { return !c.started }

// ConsumedBytes reports how many payload bytes every slice through this cursor
// put into the preview.
func (c TranscriptSliceCursor) ConsumedBytes() int64 { return c.consumedBytes }

// ConsumedRows reports how many source rows every slice through this cursor put
// into the preview.
func (c TranscriptSliceCursor) ConsumedRows() int64 { return c.consumedRows }

// TotalBytes reports the whole session's payload byte count, or zero when no
// read has measured it yet.
func (c TranscriptSliceCursor) TotalBytes() int64 { return c.totalBytes }

// TotalRows reports the whole session's payload row count, or zero when no read
// has measured it yet.
func (c TranscriptSliceCursor) TotalRows() int64 { return c.totalRows }

// Unit names what the consumed and total counts count.
func (c TranscriptSliceCursor) Unit() MaterializeTruncationUnit { return c.unit }

// TranscriptSlice is one budget-sized piece of a session plus the cursor that
// continues it.
//
// Data holds ONLY the rows this slice read, so the caller folds it and APPENDS
// the result. A message belongs to exactly one slice, so appending can never
// duplicate a turn.
type TranscriptSlice struct {
	Metadata *UnifiedMetadata
	Data     []byte
	// Next continues the session after this slice. It is meaningful only while
	// More is true.
	Next TranscriptSliceCursor
	// More reports that the session continues past this slice.
	More bool
}

// FileOffset reports where a file-origin slice read stopped, as a byte offset
// into the transcript.
func (c TranscriptSliceCursor) FileOffset() int64 { return c.fileOffset }

// FileLines reports how many complete lines the file-origin slices through this
// cursor took.
func (c TranscriptSliceCursor) FileLines() int64 { return c.fileLines }

// CurrentSeq reports the native sequence of the last current OpenCode row the
// slices through this cursor took, and false when none was taken.
func (c TranscriptSliceCursor) CurrentSeq() (int64, bool) {
	if c.current == nil {
		return 0, false
	}
	return c.current.Seq().Value(), true
}

// ResumableTranscriptMaterializer is the OPTIONAL capability of a source
// adapter that can read ONE budget-sized slice of a session starting where a
// previous slice stopped.
//
// It exists for the preview alone. A very long session cannot be read whole
// into a preview pane, and a single bound turns the pane into a dead end: the
// reader reaches the bottom and the session simply stops. This seam lets the
// pane extend the preview as the reader scrolls, one bounded read at a time,
// without ever holding a source handle open between reads.
//
// Ingest and harvest still materialize the whole session and never call this.
type ResumableTranscriptMaterializer interface {
	// MaterializeTranscriptSlice reads the next budgetBytes-sized slice of
	// session after the given cursor. Pass the zero cursor for the first slice.
	MaterializeTranscriptSlice(ctx context.Context, session DiscoveredSession, budgetBytes int64, after TranscriptSliceCursor) (TranscriptSlice, error)
}

var _ ResumableTranscriptMaterializer = (*OpenCodeAdapter)(nil)

// MaterializeTranscriptSlice reads one budget-sized slice of an OpenCode SQLite
// session, continuing after the given cursor.
//
// The read re-opens the provider database read-only, seeks by the cursor's
// keyset positions, reads one slice, and closes. Nothing stays open between
// slices, so a reader who leaves the pane open holds no source handle.
func (a *OpenCodeAdapter) MaterializeTranscriptSlice(ctx context.Context, session DiscoveredSession, budgetBytes int64, after TranscriptSliceCursor) (TranscriptSlice, error) {
	return a.MaterializeTranscriptSliceWithin(ctx, session, budgetBytes, after, TranscriptSliceLimits{})
}

// MaterializeTranscriptSliceWithin is MaterializeTranscriptSlice with a ceiling
// on one row. A current row whose payload is longer than the ceiling is refused
// with a [*RecordExceedsBudgetError] before its payload is read, when the source
// can report row lengths; the slice stops just before such a row when it has
// already taken others. The legacy origin reads documents in pages and is not
// bounded per row; its limits are ignored.
func (a *OpenCodeAdapter) MaterializeTranscriptSliceWithin(ctx context.Context, session DiscoveredSession, budgetBytes int64, after TranscriptSliceCursor, limits TranscriptSliceLimits) (TranscriptSlice, error) {
	if budgetBytes <= 0 {
		return TranscriptSlice{}, fmt.Errorf("materialize a slice of OpenCode SQLite session %q failed before source access: the byte budget %d is not positive, so the read could not be proven bounded; no managed state was written; pass the preview slice budget", session.SessionID, budgetBytes)
	}
	if !after.IsZero() && after.origin != session.TranscriptOrigin {
		return TranscriptSlice{}, fmt.Errorf("materialize a slice of OpenCode SQLite session %q failed before source access: the continuation cursor was produced from transcript origin %d but the session is origin %d, so its keyset positions name rows of a different representation; no managed state was written; restart the preview of this session from its first slice", session.SessionID, after.origin, session.TranscriptOrigin)
	}
	switch session.TranscriptOrigin {
	case TranscriptOriginOpenCodeCurrentSQLite:
		return a.materializeCurrentTranscriptSlice(ctx, session, budgetBytes, after, openCodeRowCeiling{bytes: limits.RecordCeilingBytes, rowsBefore: after.consumedRows})
	case TranscriptOriginOpenCodeLegacySQLite:
		return a.materializeLegacyTranscriptSlice(ctx, session, budgetBytes, after)
	default:
		return TranscriptSlice{}, fmt.Errorf("materialize a slice of OpenCode session %q failed before source access: transcript origin %d is not a supported managed OpenCode SQLite origin; no managed state was written; use the file origin for JSON sessions or return a supported typed SQLite origin from discovery", session.SessionID, session.TranscriptOrigin)
	}
}

// materializeCurrentTranscriptSlice reads one slice of a current-schema
// session. A current row is a whole message, so every slice boundary is a
// message boundary and the seam needs no reconciliation.
func (a *OpenCodeAdapter) materializeCurrentTranscriptSlice(ctx context.Context, session DiscoveredSession, budgetBytes int64, after TranscriptSliceCursor, ceiling openCodeRowCeiling) (TranscriptSlice, error) {
	currentID, err := NewOpenCodeCurrentSessionID(string(session.SessionID))
	if err != nil {
		return TranscriptSlice{}, err
	}
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return TranscriptSlice{}, err
	}
	var projection openCodeCurrentProjection
	var unknownControlTypes map[string]int
	var partOmissions []openCodeCurrentPartOmission
	var stop openCodeCurrentSliceStop
	size := OpenCodePayloadSize{Rows: after.totalRows, Bytes: after.totalBytes}
	if err := a.withOpenCodeSQLiteSource(ctx, session.SourcePath.String(), func(source OpenCodeSQLiteSource) error {
		if size.Bytes == 0 {
			// The whole-session totals are read ONCE, on the first slice, and
			// carried by the cursor from there. They are what the pane's note
			// quotes, and re-summing them on every continuation would put a
			// full-table scan between the reader and every scroll.
			measured, sizeErr := source.CurrentSessionPayloadSize(ctx, currentID)
			if sizeErr != nil {
				return sizeErr
			}
			size = measured
		}
		var readErr error
		projection, unknownControlTypes, partOmissions, _, stop, readErr = readOpenCodeCurrentProjectionSliceWithOmissions(ctx, source, currentID, pageSize, budgetBytes, size, after.current, ceiling)
		return readErr
	}); err != nil {
		var oversized *RecordExceedsBudgetError
		if errors.As(err, &oversized) {
			// A refused row is the caller's budget decision, not a malformed
			// source; return it unwrapped so the caller can name the row.
			return TranscriptSlice{}, oversized
		}
		return TranscriptSlice{}, fmt.Errorf("materialize a slice of current OpenCode SQLite session %q failed while reading selected session_message rows and closing the bounded source: %w; no partial managed artifact or store row was written; fix malformed current rows in OpenCode and retry", session.SessionID, err)
	}
	metadata, data, err := a.finishCurrentManagedProjection(ctx, session, projection, unknownControlTypes, partOmissions)
	if err != nil {
		return TranscriptSlice{}, err
	}
	next := after
	next.started = true
	next.origin = session.TranscriptOrigin
	next.current = stop.cursor
	next.consumedBytes += stop.includedBytes
	next.consumedRows += stop.includedRows
	next.totalBytes = size.Bytes
	next.totalRows = size.Rows
	next.unit = MaterializeUnitMessages
	return TranscriptSlice{Metadata: metadata, Data: data, Next: next, More: !stop.exhausted}, nil
}

// materializeLegacyTranscriptSlice reads one slice of a legacy-schema session.
// See readOpenCodeLegacyProjectionSlice for the rule that keeps the seam
// between two slices free of duplicated or halved turns.
func (a *OpenCodeAdapter) materializeLegacyTranscriptSlice(ctx context.Context, session DiscoveredSession, budgetBytes int64, after TranscriptSliceCursor) (TranscriptSlice, error) {
	legacyID, err := NewOpenCodeLegacySessionID(string(session.SessionID))
	if err != nil {
		return TranscriptSlice{}, err
	}
	pageSize, err := NewOpenCodeLegacyPageSize(openCodeLegacyMaterializePage)
	if err != nil {
		return TranscriptSlice{}, err
	}
	var projection openCodeLegacyProjection
	var dropped []openCodeDroppedOrphanPart
	var stop openCodeLegacySliceStop
	size := OpenCodePayloadSize{Rows: after.totalRows, Bytes: after.totalBytes}
	if err := a.withOpenCodeSQLiteSource(ctx, session.SourcePath.String(), func(source OpenCodeSQLiteSource) error {
		if size.Bytes == 0 {
			measured, sizeErr := source.LegacySessionPayloadSize(ctx, legacyID)
			if sizeErr != nil {
				return sizeErr
			}
			size = measured
		}
		from := openCodeLegacySliceStart{message: after.legacyMessage, part: after.legacyPart, blockedPart: after.legacyBlockedPart, resumable: true}
		var readErr error
		projection, dropped, _, stop, readErr = readOpenCodeLegacyProjectionSlice(ctx, source, legacyID, pageSize, budgetBytes, size, from)
		return readErr
	}); err != nil {
		return TranscriptSlice{}, fmt.Errorf("materialize a slice of legacy OpenCode SQLite session %q failed while reading selected message/part rows and closing the bounded source: %w; no partial managed artifact or store row was written; fix malformed required row JSON or retry after source locks clear", session.SessionID, err)
	}
	if len(projection.Messages) == 0 && !after.IsZero() {
		// A continuation that finds no message has walked off the end of the
		// session. That is the ordinary way a scrolled preview finishes, not a
		// failure, so it reports an empty final slice rather than the
		// no-messages error a first read raises for a genuinely empty session.
		next := after
		next.started = true
		next.origin = session.TranscriptOrigin
		return TranscriptSlice{Next: next, More: false}, nil
	}
	metadata, data, err := a.finishLegacyManagedProjection(ctx, session, projection, dropped)
	if err != nil {
		return TranscriptSlice{}, err
	}
	next := after
	next.started = true
	next.origin = session.TranscriptOrigin
	next.legacyMessage = stop.message
	next.legacyPart = stop.part
	next.legacyBlockedPart = stop.blockedPart
	next.consumedBytes += stop.includedBytes
	next.consumedRows += stop.includedRows
	next.totalBytes = size.Bytes
	next.totalRows = size.Rows
	next.unit = MaterializeUnitRows
	return TranscriptSlice{Metadata: metadata, Data: data, Next: next, More: !stop.exhausted}, nil
}

// TranscriptChangeMarker summarizes the part of a session a resumable read has
// already taken, so a later read can tell that part changed underneath it.
// Equal markers mean nothing already read was added, removed, edited or
// resized. It carries no row content.
type TranscriptChangeMarker struct {
	Rows       int64
	MaxUpdated int64
	Bytes      int64
}

// ChangeMarkedTranscriptMaterializer is the OPTIONAL capability of a resumable
// source that can compute a [TranscriptChangeMarker] for the rows a cursor
// already covers. supported is false when the session's origin exposes no
// update marker; a caller must then not continue a read it cannot check.
type ChangeMarkedTranscriptMaterializer interface {
	TranscriptChangeMarker(ctx context.Context, session DiscoveredSession, through TranscriptSliceCursor) (marker TranscriptChangeMarker, supported bool, err error)
}

var _ ChangeMarkedTranscriptMaterializer = (*OpenCodeAdapter)(nil)

// TranscriptChangeMarker computes the change marker of the current-origin rows
// at or before the cursor's last sequence. The legacy origin exposes no single
// update marker over its message and part documents, so it reports
// unsupported rather than guessing.
func (a *OpenCodeAdapter) TranscriptChangeMarker(ctx context.Context, session DiscoveredSession, through TranscriptSliceCursor) (TranscriptChangeMarker, bool, error) {
	if session.TranscriptOrigin != TranscriptOriginOpenCodeCurrentSQLite {
		return TranscriptChangeMarker{}, false, nil
	}
	currentID, err := NewOpenCodeCurrentSessionID(string(session.SessionID))
	if err != nil {
		return TranscriptChangeMarker{}, false, err
	}
	if through.current == nil {
		// Nothing read yet: the empty prefix has the zero marker.
		return TranscriptChangeMarker{}, true, nil
	}
	var marker OpenCodeCurrentChangeMarker
	supported := true
	if err := a.withOpenCodeSQLiteSource(ctx, session.SourcePath.String(), func(source OpenCodeSQLiteSource) error {
		marked, ok := source.(OpenCodeCurrentChangeMarkerSource)
		if !ok {
			supported = false
			return nil
		}
		var readErr error
		marker, readErr = marked.CurrentMessagesMarker(ctx, currentID, through.current.Seq())
		return readErr
	}); err != nil {
		return TranscriptChangeMarker{}, false, fmt.Errorf("read the change marker of current OpenCode SQLite session %q failed while summarizing the rows already read: %w; nothing was written; retry once the OpenCode database is readable", session.SessionID, err)
	}
	return TranscriptChangeMarker{Rows: marker.Rows, MaxUpdated: marker.MaxUpdated, Bytes: marker.Bytes}, supported, nil
}
