// Package sourcepreview reads a discovered session in place, from the
// transcript its harness wrote, for a raw local preview.
//
// It is the one reader behind every surface that previews a session Peasant has
// not imported. A [Capture] holds what one preview has loaded so far and hands
// out turns that never change once handed out; a [Segmenter] turns those turns
// into contract transcript segments for a surface that pages by segment.
//
// The package is neutral: it imports neither the terminal UI nor net/http, so
// the web server and the terminal share it. It writes nothing to disk or to the
// database, and it keeps nothing after a capture closes.
//
// Every bound is a per-capture option. The package states no surface's policy:
// the web caller passes its caps, the terminal passes its own.
package sourcepreview

import "fmt"

// CaptureOptions bound one capture. A zero cap means unbounded.
type CaptureOptions struct {
	// FirstReadBytes is the first source read. Both surfaces use
	// defaults.TranscriptInitialReadBytes, so first paint never waits on a
	// large read.
	FirstReadBytes int64
	// SliceReadBytes is one later source read. Both surfaces use
	// defaults.TranscriptContinuationReadBytes.
	SliceReadBytes int64
	// SourceCapBytes is the cumulative source bytes this capture may load.
	// Zero is unbounded.
	SourceCapBytes int64
	// TurnCap is the most turns this capture may emit. Zero is unbounded.
	TurnCap int
}

// Validate refuses options a capture cannot be proven bounded under.
func (o CaptureOptions) Validate() error {
	const where = "sourcepreview.CaptureOptions.Validate, before any source is opened"
	switch {
	case o.FirstReadBytes <= 0:
		return fmt.Errorf("capture options refused at %s: the first read is %d bytes, so the first read could not be proven bounded; nothing was read; pass defaults.TranscriptInitialReadBytes", where, o.FirstReadBytes)
	case o.SliceReadBytes <= 0:
		return fmt.Errorf("capture options refused at %s: one later read is %d bytes, so a continuation could not be proven bounded; nothing was read; pass defaults.TranscriptContinuationReadBytes", where, o.SliceReadBytes)
	case o.SourceCapBytes < 0:
		return fmt.Errorf("capture options refused at %s: the source cap is %d bytes; a cap is zero (unbounded) or positive; nothing was read; pass 0 or a positive cap", where, o.SourceCapBytes)
	case o.TurnCap < 0:
		return fmt.Errorf("capture options refused at %s: the turn cap is %d; a cap is zero (unbounded) or positive; nothing was read; pass 0 or a positive cap", where, o.TurnCap)
	}
	return nil
}

// SegmenterOptions bound the segments one capture sends. A zero cap means
// unbounded.
type SegmenterOptions struct {
	// BudgetBytes is the segment cut budget. A surface that pages by contract
	// segment passes schema.TranscriptSegmentBudgetBytes.
	BudgetBytes int
	// FrameCapBytes is the largest encoded segment the caller can send. The
	// served-text bound of an oversized turn is tightened to fit it, and a
	// segment that still does not fit is refused before it is handed out.
	// Zero is unbounded.
	FrameCapBytes int
	// SerializedCapBytes is the most encoded segment bytes this capture may
	// hand out in total. Zero is unbounded.
	SerializedCapBytes int64
}

// Validate refuses options a segmenter cannot be proven bounded under.
func (o SegmenterOptions) Validate() error {
	const where = "sourcepreview.SegmenterOptions.Validate, before any segment is cut"
	switch {
	case o.BudgetBytes <= 0:
		return fmt.Errorf("segmenter options refused at %s: the segment budget is %d bytes, so no segment could be cut; pass schema.TranscriptSegmentBudgetBytes", where, o.BudgetBytes)
	case o.FrameCapBytes < 0:
		return fmt.Errorf("segmenter options refused at %s: the frame cap is %d bytes; a cap is zero (unbounded) or positive; pass 0 or a positive cap", where, o.FrameCapBytes)
	case o.FrameCapBytes > 0 && o.FrameCapBytes < minimumFrameCapBytes:
		return fmt.Errorf("segmenter options refused at %s: the frame cap is %d bytes, below the %d bytes one bounded turn needs; pass a larger cap", where, o.FrameCapBytes, minimumFrameCapBytes)
	case o.SerializedCapBytes < 0:
		return fmt.Errorf("segmenter options refused at %s: the serialized cap is %d bytes; a cap is zero (unbounded) or positive; pass 0 or a positive cap", where, o.SerializedCapBytes)
	}
	return nil
}

// minimumFrameCapBytes leaves room for the frame envelope after the served-text
// bound is tightened to the frame.
const minimumFrameCapBytes = 64 << 10
