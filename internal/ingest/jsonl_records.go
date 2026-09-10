package ingest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// OmittedRecordReason is the typed reason a source record was left out of the
// stored session. It is a closed set: a value outside it never reaches a
// stored entry or the wire.
type OmittedRecordReason string

const (
	// OmittedRecordTooLarge marks a record that is larger than the per-record
	// limit this build reads whole. It is the only reason defined today.
	OmittedRecordTooLarge OmittedRecordReason = "record_too_large"
)

func (r OmittedRecordReason) String() string { return string(r) }

// ParseOmittedRecordReason accepts only a reason this build understands.
func ParseOmittedRecordReason(s string) (OmittedRecordReason, error) {
	switch OmittedRecordReason(s) {
	case OmittedRecordTooLarge:
		return OmittedRecordTooLarge, nil
	}
	return "", fmt.Errorf(
		"ingest: unknown omitted-record reason %q read while decoding a stored session entry's omission record in internal/ingest/jsonl_records.go; this build only records %q, so the value comes from a newer build or from edited data and the caller cannot tell why the record is missing; reindex the session with the build that wrote it, or upgrade this build to one that defines the reason",
		s, OmittedRecordTooLarge,
	)
}

// OmittedRecordExtraKey is the fixed key the typed omission record is
// marshalled under inside a placeholder entry's Extra JSON object. It travels
// on the existing SessionEntry.extra wire field, so no schema change is
// needed to carry an omission to a transcript UI.
const OmittedRecordExtraKey = "omittedRecord"

// OmittedRecord is Peasant's own record that one source record was omitted
// from a stored session. It rides in the placeholder entry's Extra field so
// every reader of the session, not only the ingest log, can tell that the
// session is a partial capture and exactly what is missing.
type OmittedRecord struct {
	Reason     OmittedRecordReason `json:"reason"`
	Line       int                 `json:"line"`
	Bytes      int64               `json:"bytes"`
	LimitBytes int64               `json:"limitBytes"`
}

// NewOmittedRecord validates the omission at the boundary where it is made.
func NewOmittedRecord(reason OmittedRecordReason, line int, size int64, limit int64) (OmittedRecord, error) {
	parsed, err := ParseOmittedRecordReason(string(reason))
	if err != nil {
		return OmittedRecord{}, err
	}
	if line < 1 {
		return OmittedRecord{}, fmt.Errorf(
			"ingest: omitted-record line %d is not a line number while building the %s omission record in internal/ingest/jsonl_records.go during source filtering; a placeholder entry with no real position would point a reader at the wrong turn; pass the 1-based physical line of the omitted record",
			line, parsed,
		)
	}
	if size <= limit {
		return OmittedRecord{}, fmt.Errorf(
			"ingest: omitted-record size %d bytes is not over the %d-byte limit while building the %s omission record in internal/ingest/jsonl_records.go during source filtering; recording an omission for a record that fits would mark a complete session partial; omit a record only when its size is over the limit",
			size, limit, parsed,
		)
	}
	return OmittedRecord{Reason: parsed, Line: line, Bytes: size, LimitBytes: limit}, nil
}

// ParseOmittedRecord decodes a stored entry's Extra JSON back into the typed
// record, rejecting anything this build cannot represent.
func ParseOmittedRecord(extra string) (OmittedRecord, error) {
	var envelope struct {
		Record *struct {
			Reason     string `json:"reason"`
			Line       int    `json:"line"`
			Bytes      int64  `json:"bytes"`
			LimitBytes int64  `json:"limitBytes"`
		} `json:"omittedRecord"`
	}
	if err := json.Unmarshal([]byte(extra), &envelope); err != nil {
		return OmittedRecord{}, fmt.Errorf(
			"ingest: entry extra field is not a JSON object while reading an omission record in internal/ingest/jsonl_records.go after loading a stored session entry; a reader cannot tell whether this entry stands for an omitted source record; reindex the session so the entry is rewritten by this build: %w",
			err,
		)
	}
	if envelope.Record == nil {
		return OmittedRecord{}, fmt.Errorf(
			"ingest: entry extra field has no %q key while reading an omission record in internal/ingest/jsonl_records.go after loading a stored session entry; this entry is an ordinary entry, not an omission placeholder; call ParseOmittedRecord only for an entry the caller already believes is a placeholder",
			OmittedRecordExtraKey,
		)
	}
	return NewOmittedRecord(
		OmittedRecordReason(envelope.Record.Reason),
		envelope.Record.Line,
		envelope.Record.Bytes,
		envelope.Record.LimitBytes,
	)
}

// Extra marshals the record into the Extra JSON object a placeholder entry
// carries.
func (o OmittedRecord) Extra() (string, error) {
	if _, err := ParseOmittedRecordReason(string(o.Reason)); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(map[string]OmittedRecord{OmittedRecordExtraKey: o})
	if err != nil {
		return "", fmt.Errorf(
			"ingest: could not marshal the %s omission record for line %d in internal/ingest/jsonl_records.go while building a placeholder entry; the stored session would lose the machine-readable reason a record is missing; report this with the session id, the omission is otherwise still described in the entry's content preview: %w",
			o.Reason, o.Line, err,
		)
	}
	return string(encoded), nil
}

// OversizedRecord names one source record that a reader skipped because it is
// larger than the limit the reader was given. Size is the record's true byte
// length; the bytes themselves are never held.
type OversizedRecord struct {
	Line int
	Size int
	// Prefix is a copy of the record's leading bytes, at most
	// omittedRecordPrefixBytes of them. It exists only so a caller can read a
	// correlation id such as a tool call id out of the record's opening
	// object. It is never stored as content.
	Prefix []byte
}

// omittedRecordPrefixBytes bounds the leading bytes kept from a record that is
// skipped for being too large. It is large enough to hold the opening keys of
// a harness record and small enough that keeping it costs nothing.
const omittedRecordPrefixBytes = 4 << 10

// jsonlRecordScanner reads newline-separated records with the same pull API as
// bufio.Scanner, and with one behavior difference that is the whole point of
// it: a record larger than the limit never produces an error. Such a record is
// discarded to the next newline without being buffered, is listed by
// Oversized(), and scanning continues with the record after it. Err() is
// therefore never a size error, so no transcript can fail to ingest, and no
// record after a large one is lost.
//
// Bytes() is valid only until the next call to Scan(), exactly as with
// bufio.Scanner.
type jsonlRecordScanner struct {
	reader    *bufio.Reader
	limit     int
	record    []byte
	line      int
	oversized []OversizedRecord
	omissions []OmittedRecordAt
	err       error
	done      bool
}

// OmittedRecordAt is an omission the reader found already recorded in the
// input, at the physical line where the omitted record used to be.
type OmittedRecordAt struct {
	Record     OmittedRecord
	Line       int
	ToolCallID *string
}

// newJSONLRecordScanner reads data with the given per-record limit. Production
// callers pass defaults.MaxJSONLRecordBytes; tests may inject a small limit
// through the parameter, so no global is ever mutated.
func newJSONLRecordScanner(data []byte, maxRecordBytes int) *jsonlRecordScanner {
	return newJSONLRecordStreamScanner(bytes.NewReader(data), maxRecordBytes)
}

func newJSONLRecordStreamScanner(r io.Reader, maxRecordBytes int) *jsonlRecordScanner {
	s := &jsonlRecordScanner{limit: maxRecordBytes}
	if maxRecordBytes < 1 {
		s.err = fmt.Errorf(
			"ingest: JSONL record limit %d is not a usable size, passed to newJSONLRecordScanner in internal/ingest/jsonl_records.go before reading a transcript; with no room for a record every record would be reported omitted and the session would store empty; pass defaults.MaxJSONLRecordBytes in production, or a positive test limit",
			maxRecordBytes,
		)
		s.done = true
		return s
	}
	initial := defaults.ScannerInitBuf
	if maxRecordBytes < initial {
		initial = maxRecordBytes
	}
	s.reader = bufio.NewReaderSize(r, initial)
	return s
}

// Scan advances to the next record within the limit. It returns false at the
// end of the input or after a read error.
func (s *jsonlRecordScanner) Scan() bool {
	for !s.done {
		record, prefix, size, oversized, more, err := readOneJSONLRecord(s.reader, s.limit)
		if err != nil {
			s.err = fmt.Errorf(
				"ingest: could not read physical line %d of a JSONL transcript in jsonlRecordScanner.Scan (internal/ingest/jsonl_records.go) during source reading; the records after it were not seen, so this input cannot be read to the end; check that the source file is readable and is not being replaced while harvest runs, then rerun the harvest: %w",
				s.line+1, err,
			)
			s.done = true
			return false
		}
		if !more && size == 0 && !oversized {
			s.done = true
			return false
		}
		s.line++
		if !more {
			s.done = true
		}
		if oversized {
			s.oversized = append(s.oversized, OversizedRecord{Line: s.line, Size: size, Prefix: prefix})
			// An over-limit record is an omission wherever it is met, not
			// only where the source filter met it first. Recording it here
			// too means a reader that indexes a transcript directly still
			// gets a placeholder at the record's position instead of a
			// silent gap.
			record, recordErr := NewOmittedRecord(OmittedRecordTooLarge, s.line, int64(size), int64(s.limit))
			if recordErr == nil {
				s.omissions = append(s.omissions, OmittedRecordAt{
					Record:     record,
					Line:       s.line,
					ToolCallID: toolCallIDFromRecordPrefix(prefix),
				})
			}
			continue
		}
		if omission, ok := parseOmittedRecordSentinel(record); ok {
			// A stand-in the source filter wrote for a record it omitted. The
			// harness parsers never see it; the indexer reads it from
			// TakeOmissions and emits its placeholder entry at this position.
			omission.Line = s.line
			s.omissions = append(s.omissions, omission)
			continue
		}
		s.record = record
		return true
	}
	return false
}

// Bytes returns the current record without its newline.
func (s *jsonlRecordScanner) Bytes() []byte { return s.record }

// Line is the 1-based physical line of the current record in the source,
// counting the records that were skipped for being too large.
func (s *jsonlRecordScanner) Line() int { return s.line }

// Oversized lists every record skipped so far, in source order.
func (s *jsonlRecordScanner) Oversized() []OversizedRecord { return s.oversized }

// TakeOmissions returns the omissions the reader has passed since the last
// call and clears them. An indexer calls it at the top of its record loop and
// once after the loop, so every omission becomes a placeholder entry at the
// position the omitted record held.
func (s *jsonlRecordScanner) TakeOmissions() []OmittedRecordAt {
	taken := s.omissions
	s.omissions = nil
	return taken
}

// SawOversized reports whether any record was over the limit. A caller that
// must certify a complete index uses it: an index that silently leaves a
// record out cannot be certified, even though the read itself succeeded.
func (s *jsonlRecordScanner) SawOversized() bool { return len(s.oversized) > 0 }

// Err reports a read failure. A record's size is never one.
func (s *jsonlRecordScanner) Err() error { return s.err }

// forEachJSONLRecord reads r as newline-separated records and hands each one
// to fn whole, including a final record with no trailing newline.
//
// A record up to maxRecordBytes is yielded complete. A record over
// maxRecordBytes is never held in memory: the reader discards it to the next
// newline, reports its line and its true size through onOversized, and goes
// on to the next record. The reader never truncates a record and never
// returns an error because of a record's size, so no transcript can fail
// ingestion for being large. The slice handed to fn is only valid until fn
// returns; a caller that keeps it must copy it.
//
// The line number is the 1-based physical line of the source, counting
// omitted records, so a diagnostic points at the record in the original file.
func forEachJSONLRecord(
	ctx context.Context,
	r io.Reader,
	maxRecordBytes int,
	fn func(line int, raw []byte) error,
	onOversized func(line int, size int),
) error {
	scanner := newJSONLRecordStreamScanner(r, maxRecordBytes)
	if err := scanner.Err(); err != nil {
		return err
	}
	reported := 0
	drainOversized := func() {
		for ; reported < len(scanner.oversized); reported++ {
			if onOversized != nil {
				skipped := scanner.oversized[reported]
				onOversized(skipped.Line, skipped.Size)
			}
		}
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		drainOversized()
		if err := fn(scanner.Line(), scanner.Bytes()); err != nil {
			return err
		}
	}
	drainOversized()
	if err := scanner.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

// readOneJSONLRecord returns the next record without its newline. oversized
// reports that the record was over the limit and was discarded rather than
// returned; size is the record's true byte length either way. more reports
// that the input continues past this record.
func readOneJSONLRecord(reader *bufio.Reader, maxRecordBytes int) (record []byte, prefix []byte, size int, oversized bool, more bool, err error) {
	var accumulated []byte
	discarding := false
	for {
		slice, readErr := reader.ReadSlice('\n')
		if readErr != nil && !errors.Is(readErr, bufio.ErrBufferFull) && !errors.Is(readErr, io.EOF) {
			return nil, nil, 0, false, false, readErr
		}
		chunk := slice
		terminated := readErr == nil
		if terminated {
			chunk = bytes.TrimSuffix(chunk, []byte{'\n'})
		}
		size += len(chunk)

		if !discarding && size > maxRecordBytes {
			// Stop holding the record. Everything already read is released
			// and the rest is counted but discarded, so a record far over the
			// limit costs the read buffer, not its own length.
			discarding = true
			opening := accumulated
			if opening == nil {
				opening = chunk
			}
			if len(opening) > omittedRecordPrefixBytes {
				opening = opening[:omittedRecordPrefixBytes]
			}
			prefix = append([]byte(nil), opening...)
			accumulated = nil
		}
		if !discarding {
			if terminated && accumulated == nil {
				// Single-buffer record: hand out the buffer directly.
				record = chunk
			} else {
				accumulated = append(accumulated, chunk...)
				record = accumulated
			}
		}

		if terminated {
			return record, prefix, size, discarding, true, nil
		}
		if errors.Is(readErr, io.EOF) {
			return record, prefix, size, discarding, false, nil
		}
		// bufio.ErrBufferFull: the record continues past the buffer. The
		// bytes read so far are already in accumulated, so read on.
	}
}
