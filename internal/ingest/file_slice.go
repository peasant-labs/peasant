package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// RangeReadableFileSystem is the OPTIONAL capability of a [FileSystem] that can
// read a byte RANGE of a file without loading the whole file into memory.
//
// It exists for the preview of a very long line-oriented transcript. A recorded
// Claude session reaches hundreds of megabytes on a real machine, and the
// preview used to read every byte of it, index every entry, and fold every turn
// before anything appeared in the pane. A range read lets the pane paint the
// first screenful at once and take the rest as the reader scrolls.
//
// A FileSystem without it keeps the whole-file read, so no caller has to change.
type RangeReadableFileSystem interface {
	// ReadFileRange returns up to length bytes of the file at path, starting at
	// offset. A short result means the file ended; an offset at or past the end
	// returns no bytes and no error.
	ReadFileRange(path string, offset, length int64) ([]byte, error)
}

// ReadFileRange implements [RangeReadableFileSystem] by seeking, so the cost is
// the range read rather than the file.
func (f *OSFileSystem) ReadFileRange(path string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("read a byte range of %q failed before opening it: offset %d and length %d must be non-negative and positive; no file was opened; pass a valid range", path, offset, length)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("read a byte range of %q failed while seeking to offset %d: %w; nothing was read and the file is unchanged; verify the transcript is a regular seekable file", path, offset, err)
	}
	data := make([]byte, length)
	read, err := io.ReadFull(file, data)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("read a byte range of %q failed after %d of %d bytes from offset %d: %w; the partial read was discarded and the file is unchanged; retry once the transcript is readable", path, read, length, offset, err)
	}
	return data[:read], nil
}

var _ RangeReadableFileSystem = (*OSFileSystem)(nil)

// FileTranscriptSlicer reads one budget-sized slice of a LINE-ORIENTED
// transcript file, so a preview can paint a leading slice at once and extend it
// as the reader scrolls.
//
// Every slice holds only COMPLETE lines: it ends at the last newline inside its
// budget, and the continuation resumes at the byte after it. A single line
// longer than the budget is delivered WHOLE rather than split, the same "budget
// plus one oversized row" bound the SQLite slice read accepts.
//
// Be clear about what that buys, because it is easy to overstate. It is NOT
// what makes the read correct: the caller re-parses every byte it has
// accumulated, so a slice that ended mid-line would be completed by the next
// one and the final result would be the same either way. What it buys is that
// every INTERMEDIATE state is clean - the body on screen after each slice holds
// no trailing half-record that briefly parses as nothing and then appears - and
// that an oversized line is read deliberately rather than in as many pieces as
// the budget happens to cut it into.
//
// What it deliberately does NOT do is decide where a TURN begins. A turn folds
// from entries that span several lines - a tool call and the result that
// answers it are two lines - and every file indexer carries state across
// records as it parses. Neither is a property of line boundaries, so neither is
// this reader's to solve; the caller re-parses the whole accumulated prefix.
type FileTranscriptSlicer struct {
	fs FileSystem
}

// NewFileTranscriptSlicer builds a slicer over fs. Use [FileTranscriptSlicer.Supported]
// to tell whether fs can actually serve a range read.
func NewFileTranscriptSlicer(fs FileSystem) FileTranscriptSlicer {
	return FileTranscriptSlicer{fs: fs}
}

// Supported reports whether the filesystem can read a byte range. A filesystem
// that cannot leaves the caller on its existing whole-file read.
func (s FileTranscriptSlicer) Supported() bool {
	_, ok := s.fs.(RangeReadableFileSystem)
	return ok
}

// MaterializeTranscriptSlice reads the next budgetBytes-sized slice of complete
// lines from the transcript file, continuing after the given cursor. Pass the
// zero cursor for the first slice.
func (s FileTranscriptSlicer) MaterializeTranscriptSlice(ctx context.Context, session DiscoveredSession, budgetBytes int64, after TranscriptSliceCursor) (TranscriptSlice, error) {
	return s.MaterializeTranscriptSliceWithin(ctx, session, budgetBytes, after, TranscriptSliceLimits{})
}

// MaterializeTranscriptSliceWithin is MaterializeTranscriptSlice with an
// allocation ceiling for one record. A line that is still longer than
// limits.RecordCeilingBytes when the slicer chases its end is refused with a
// [*RecordExceedsBudgetError] naming the line and the size read so far, before
// any byte past the ceiling (plus one chase chunk) is read. A zero ceiling keeps
// the unbounded chase of MaterializeTranscriptSlice.
//
// The read never passes the extent the first slice measured: a file that grew
// after the first slice is read only up to the size it had then, so the bytes a
// preview shows always belong to one fixed extent.
func (s FileTranscriptSlicer) MaterializeTranscriptSliceWithin(_ context.Context, session DiscoveredSession, budgetBytes int64, after TranscriptSliceCursor, limits TranscriptSliceLimits) (TranscriptSlice, error) {
	path := string(session.SourcePath)
	if budgetBytes <= 0 {
		return TranscriptSlice{}, fmt.Errorf("materialize a slice of transcript %q failed before opening it: the byte budget %d is not positive, so the read could not be proven bounded; nothing was read; pass the preview slice budget", path, budgetBytes)
	}
	ranged, ok := s.fs.(RangeReadableFileSystem)
	if !ok {
		return TranscriptSlice{}, fmt.Errorf("materialize a slice of transcript %q failed before opening it: this filesystem cannot read a byte range, so a bounded slice cannot be proven bounded; nothing was read; preview this transcript through the whole-file read instead", path)
	}
	if !after.IsZero() && after.origin != session.TranscriptOrigin {
		return TranscriptSlice{}, fmt.Errorf("materialize a slice of transcript %q failed before opening it: the continuation cursor was produced from transcript origin %d but the session is origin %d; nothing was read; restart the preview of this session from its first slice", path, after.origin, session.TranscriptOrigin)
	}

	total := after.totalBytes
	if total == 0 {
		info, err := s.fs.Stat(path)
		if err != nil {
			return TranscriptSlice{}, fmt.Errorf("materialize a slice of transcript %q failed while measuring it: %w; nothing was read; verify the transcript still exists and is readable", path, err)
		}
		total = info.Size()
	}

	offset := after.fileOffset
	if offset >= total {
		next := after
		next.started = true
		next.origin = session.TranscriptOrigin
		next.totalBytes = total
		return TranscriptSlice{Next: next, More: false}, nil
	}

	length := budgetBytes
	if remaining := total - offset; length > remaining {
		// Never read past the extent the first slice measured, even when the
		// file has grown since: appended bytes belong to a later preview.
		length = remaining
	}
	data, err := ranged.ReadFileRange(path, offset, length)
	if err != nil {
		return TranscriptSlice{}, err
	}
	if int64(len(data)) < length {
		// The range lies inside the extent the first slice measured, so a
		// short read means the file is now shorter than it was then.
		return TranscriptSlice{}, shrankError(offset+int64(len(data)), total)
	}
	complete, err := s.completeLines(ranged, path, offset, data, total, limits.RecordCeilingBytes)
	if err != nil {
		var oversized *RecordExceedsBudgetError
		if errors.As(err, &oversized) {
			oversized.Line = after.fileLines + 1
		}
		return TranscriptSlice{}, err
	}

	next := after
	next.started = true
	next.origin = session.TranscriptOrigin
	next.fileOffset = offset + int64(len(complete))
	next.consumedBytes = next.fileOffset
	next.fileLines += int64(bytes.Count(complete, []byte{'\n'}))
	next.totalBytes = total
	next.unit = MaterializeUnitLines
	return TranscriptSlice{Data: complete, Next: next, More: next.fileOffset < total}, nil
}

// completeLines trims a raw range read back to its last complete line, and
// extends it when the budget did not reach one at all.
//
// Only the EXTENSION carries the meaning. It is what delivers a line longer
// than the whole budget in one piece instead of in as many pieces as the budget
// happens to cut it into.
//
// The two returns before it are FAST PATHS, and deliberately so: the extension
// alone would produce the same bytes for every input, by reading forward to the
// next newline. Recognising that the read already reached the end of the file,
// and that it already contains a newline, is what keeps the common slice from
// paying an extra forward read it does not need. Neither is load-bearing for
// correctness, which is why removing either one changes no result - only the
// number of reads.
func (s FileTranscriptSlicer) completeLines(ranged RangeReadableFileSystem, path string, offset int64, data []byte, total, ceiling int64) ([]byte, error) {
	if int64(len(data)) == 0 {
		return nil, nil
	}
	if offset+int64(len(data)) >= total {
		// The read reached the end of the file, so the last line is complete
		// whether or not the file ends with a newline.
		return data, nil
	}
	if cut := bytes.LastIndexByte(data, '\n'); cut >= 0 {
		return data[:cut+1], nil
	}
	// Not one newline in the whole budget: this line is longer than a slice.
	// Keep reading until it ends, so the slice carries it whole.
	extended := data
	readOffset := offset + int64(len(data))
	for readOffset < total {
		if ceiling > 0 && int64(len(extended)) > ceiling {
			// The line is already longer than the caller can hold. Stop before
			// reading any more of it; the caller names the line and the size.
			return nil, &RecordExceedsBudgetError{AtLeastBytes: int64(len(extended)), CeilingBytes: ceiling}
		}
		chunk := int64(fileSliceOversizedLineChunk)
		if remaining := total - readOffset; chunk > remaining {
			chunk = remaining
		}
		if ceiling > 0 {
			// Read at most one byte past the ceiling, so a line over it is
			// known to be over it without allocating the rest.
			chunk = min(chunk, ceiling-int64(len(extended))+1)
		}
		more, err := ranged.ReadFileRange(path, readOffset, chunk)
		if err != nil {
			return nil, err
		}
		if int64(len(more)) < chunk {
			return nil, shrankError(readOffset+int64(len(more)), total)
		}
		readOffset += int64(len(more))
		if cut := bytes.IndexByte(more, '\n'); cut >= 0 {
			line := append(extended, more[:cut+1]...)
			if ceiling > 0 && int64(len(line)) > ceiling {
				return nil, &RecordExceedsBudgetError{AtLeastBytes: int64(len(line)), CeilingBytes: ceiling}
			}
			return line, nil
		}
		extended = append(extended, more...)
	}
	if ceiling > 0 && int64(len(extended)) > ceiling {
		return nil, &RecordExceedsBudgetError{AtLeastBytes: int64(len(extended)), CeilingBytes: ceiling}
	}
	return extended, nil
}

// TranscriptSliceLimits bounds what one slice read may allocate beyond its byte
// budget. The zero value adds no bound.
type TranscriptSliceLimits struct {
	// RecordCeilingBytes is the largest single record (a line, or a SQLite row)
	// the caller can hold. A record over it is refused before it is read whole.
	// Zero means no ceiling.
	RecordCeilingBytes int64
}

// LimitedResumableTranscriptMaterializer is the OPTIONAL capability of a slice
// reader that can refuse a record over a ceiling before allocating it.
type LimitedResumableTranscriptMaterializer interface {
	MaterializeTranscriptSliceWithin(ctx context.Context, session DiscoveredSession, budgetBytes int64, after TranscriptSliceCursor, limits TranscriptSliceLimits) (TranscriptSlice, error)
}

var (
	_ LimitedResumableTranscriptMaterializer = FileTranscriptSlicer{}
	_ LimitedResumableTranscriptMaterializer = (*OpenCodeAdapter)(nil)
)

// ErrTranscriptShrank reports a slice read that found the file shorter than the
// extent the first slice measured: the file was truncated or rewritten after a
// preview began reading it. Continuing would read bytes that never belonged to
// that extent, so the caller ends the read.
var ErrTranscriptShrank = errors.New("the transcript is shorter than when its first slice was read")

func shrankError(at, total int64) error {
	return fmt.Errorf("%w: the file ended at byte %d of the %d bytes it held when the first slice was read; nothing past it was read; open the transcript again from its start", ErrTranscriptShrank, at, total)
}

// ErrRecordExceedsBudget matches every [*RecordExceedsBudgetError].
var ErrRecordExceedsBudget = errors.New("a transcript record is larger than the read may hold")

// RecordExceedsBudgetError reports one record a slice read refused before it
// read it whole. It names the record by its 1-based line (file origin) or its
// 1-based row ordinal within the session (SQLite origin) and never by content
// or path, so it is safe to show and to log.
type RecordExceedsBudgetError struct {
	// Line is the 1-based line of the record in a line-oriented file, or 0.
	Line int64
	// Row is the 1-based ordinal of the row within the session, or 0.
	Row int64
	// AtLeastBytes is how large the record is known to be: the exact size for
	// a SQLite row, a lower bound for a line the slicer stopped chasing.
	AtLeastBytes int64
	// CeilingBytes is the ceiling the record exceeded.
	CeilingBytes int64
}

func (e *RecordExceedsBudgetError) Error() string {
	where := "a record"
	switch {
	case e.Line > 0:
		where = fmt.Sprintf("line %d", e.Line)
	case e.Row > 0:
		where = fmt.Sprintf("row %d", e.Row)
	}
	return fmt.Sprintf("read a transcript slice stopped before %s: the record is at least %d bytes, over the %d-byte ceiling this read may hold; nothing past the ceiling was read and the source is unchanged; the full session still ingests normally, and a preview with a larger budget can read it", where, e.AtLeastBytes, e.CeilingBytes)
}

// Is makes errors.Is(err, ErrRecordExceedsBudget) match.
func (e *RecordExceedsBudgetError) Is(target error) bool { return target == ErrRecordExceedsBudget }

// fileSliceOversizedLineChunk is how much more is read at a time while chasing
// the end of a line that is longer than a whole slice budget. It is small
// because the case is rare and the read is only ever finishing one line.
const fileSliceOversizedLineChunk = 1 << 20 // 1 MiB

// LineOrientedTranscriptIndexer is the marker a file indexer implements to
// declare that its transcript holds one record per LINE, so a prefix of
// complete lines is itself a readable transcript.
//
// Slicing a file by lines is only safe for such a format. A transcript held as
// one JSON document would parse a prefix as nothing at all, and every indexer
// here skips content it cannot read rather than failing, so the preview would
// render empty instead of reporting a problem. The marker makes the property
// DECLARED: a future file format that is not line-oriented simply does not
// implement it and keeps the whole-file read.
type LineOrientedTranscriptIndexer interface {
	TranscriptIndexer
	// RecordsAreLines reports that one record occupies one line.
	RecordsAreLines() bool
}

// RecordsAreLines declares the Claude JSONL transcript line-oriented.
func (idx *ClaudeIndexer) RecordsAreLines() bool { return true }

// RecordsAreLines declares the Cursor JSONL transcript line-oriented.
func (idx *CursorIndexer) RecordsAreLines() bool { return true }

// RecordsAreLines declares the Codex rollout JSONL line-oriented.
func (idx *CodexIndexer) RecordsAreLines() bool { return true }

// RecordsAreLines declares the Strike record stream line-oriented.
func (i *StrikeIndexer) RecordsAreLines() bool { return true }

var (
	_ LineOrientedTranscriptIndexer = (*ClaudeIndexer)(nil)
	_ LineOrientedTranscriptIndexer = (*CursorIndexer)(nil)
	_ LineOrientedTranscriptIndexer = (*CodexIndexer)(nil)
	_ LineOrientedTranscriptIndexer = (*StrikeIndexer)(nil)
)
