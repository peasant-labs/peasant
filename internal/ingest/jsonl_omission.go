package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// omittedRecordSentinelKey is the single key of the one-line stand-in the
// source filter writes in place of a record it omitted. The stand-in keeps the
// omission at the exact physical line the omitted record held, so every
// harness indexer can place its placeholder entry at the right position
// without a per-harness side channel. The key is Peasant's own: no harness
// writes it, and the shared record reader intercepts the line, so no harness
// parser ever sees it.
const omittedRecordSentinelKey = "peasantOmittedRecord"

// maxOmittedRecordSentinelBytes bounds the stand-in line. The object holds a
// reason, three numbers and at most one tool call id, so it is well under
// this; the bound exists so an ordinary record is rejected on its length
// alone, without a second scan of its bytes.
const maxOmittedRecordSentinelBytes = 8 << 10

type omittedRecordSentinel struct {
	Reason     OmittedRecordReason `json:"reason"`
	Line       int                 `json:"line"`
	Bytes      int64               `json:"bytes"`
	LimitBytes int64               `json:"limitBytes"`
	ToolCallID *string             `json:"toolCallId,omitempty"`
	// EntryID and ParentEntryID keep the omitted record's place in an entry
	// tree, for the harness that projects one. The filter reads them from the
	// omitted record's opening bytes and the stand-in carries them, because
	// the harness source reads the FILTERED artifact and the omitted record's
	// own bytes are gone by then.
	EntryID       *string `json:"entryId,omitempty"`
	ParentEntryID *string `json:"parentEntryId,omitempty"`
}

// encodeOmittedRecordSentinel renders the stand-in line, without its newline.
func encodeOmittedRecordSentinel(at OmittedRecordAt) ([]byte, error) {
	if _, err := ParseOmittedRecordReason(string(at.Record.Reason)); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(map[string]omittedRecordSentinel{
		omittedRecordSentinelKey: {
			Reason:        at.Record.Reason,
			Line:          at.Record.Line,
			Bytes:         at.Record.Bytes,
			LimitBytes:    at.Record.LimitBytes,
			ToolCallID:    at.ToolCallID,
			EntryID:       at.EntryID,
			ParentEntryID: at.ParentEntryID,
		},
	})
	if err != nil {
		return nil, fmt.Errorf(
			"ingest: could not render the stand-in line for the %s record omitted at line %d, in encodeOmittedRecordSentinel (internal/ingest/jsonl_omission.go) while filtering a transcript before redaction; without the stand-in the stored session would lose the position of the missing record and no placeholder entry could be shown to a reader; rerun the harvest and report this if it repeats: %w",
			at.Record.Reason, at.Record.Line, err,
		)
	}
	return encoded, nil
}

// parseOmittedRecordSentinel reports whether raw is a stand-in line and, if it
// is, the omission it carries. A line that is not a stand-in is left to the
// harness parser untouched.
func parseOmittedRecordSentinel(raw []byte) (OmittedRecordAt, bool) {
	// A stand-in is one small object this code writes, so anything longer
	// cannot be one. Checking the length first keeps an ordinary large record
	// from being scanned again for the key.
	if len(raw) > maxOmittedRecordSentinelBytes {
		return OmittedRecordAt{}, false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !bytes.Contains(trimmed, []byte(`"`+omittedRecordSentinelKey+`"`)) {
		return OmittedRecordAt{}, false
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return OmittedRecordAt{}, false
	}
	body, present := envelope[omittedRecordSentinelKey]
	if !present || len(envelope) != 1 {
		return OmittedRecordAt{}, false
	}
	var sentinel omittedRecordSentinel
	if err := json.Unmarshal(body, &sentinel); err != nil {
		return OmittedRecordAt{}, false
	}
	record, err := NewOmittedRecord(sentinel.Reason, sentinel.Line, sentinel.Bytes, sentinel.LimitBytes)
	if err != nil {
		return OmittedRecordAt{}, false
	}
	return OmittedRecordAt{
		Record:        record,
		Line:          sentinel.Line,
		ToolCallID:    sentinel.ToolCallID,
		EntryID:       sentinel.EntryID,
		ParentEntryID: sentinel.ParentEntryID,
	}, true
}

// omissionPlaceholderEntry builds the entry that stands in the indexed
// transcript for a source record Peasant omitted. It occupies the omitted
// record's position so a reader sees, at the right point in the conversation,
// that one tool output is missing and why. It carries no bytes of the omitted
// record: the machine-readable reason rides in Extra and the human-readable
// note in ContentPreview, both existing SessionEntry fields, so the placeholder
// reaches every transcript UI over the wire that already exists.
func omissionPlaceholderEntry(
	sessionID SessionID,
	harness Harness,
	entryIndex int,
	at OmittedRecordAt,
) (schema.SessionEntry, error) {
	extra, err := at.Record.Extra()
	if err != nil {
		return schema.SessionEntry{}, err
	}
	note := omissionPlaceholderNote(at.Record)
	rawByteLength := int(at.Record.Bytes)
	if int64(rawByteLength) != at.Record.Bytes {
		// A size that does not fit the wire's int field would be reported
		// wrong; report no length rather than a false one. The exact size
		// stays in the typed record in Extra.
		rawByteLength = 0
	}
	entry := schema.SessionEntry{
		SessionID:      schema.SessionID(sessionID),
		EntryIndex:     entryIndex,
		Harness:        schema.Harness(harness),
		EntryType:      schema.EntryTypeToolResult,
		Role:           schema.RoleTool,
		Depth:          0,
		ContentPreview: &note,
		IsError:        false,
		Extra:          &extra,
		ToolCallID:     at.ToolCallID,
	}
	if rawByteLength > 0 {
		entry.RawByteLength = &rawByteLength
	}
	return entry, nil
}

// omissionPlaceholderNote is the reader-facing sentence the placeholder shows
// where the omitted record used to be. It stays well under the wire's 500
// character content-preview bound.
func omissionPlaceholderNote(record OmittedRecord) string {
	return fmt.Sprintf(
		"tool output omitted: %s record at line %d is over the %s limit; the rest of the session was kept",
		humanByteSize(record.Bytes), record.Line, humanByteSize(record.LimitBytes),
	)
}

// humanByteSize renders a byte count the way the diagnostics and the reader
// note both state it, so the two never disagree.
func humanByteSize(size int64) string {
	const unit = 1024
	switch {
	case size >= unit*unit*unit:
		return fmt.Sprintf("%.1f GiB", float64(size)/float64(unit*unit*unit))
	case size >= unit*unit:
		return fmt.Sprintf("%.0f MiB", float64(size)/float64(unit*unit))
	case size >= unit:
		return fmt.Sprintf("%.0f KiB", float64(size)/float64(unit))
	default:
		return fmt.Sprintf("%d bytes", size)
	}
}

// appendOmissionPlaceholders adds one placeholder entry for every omission the
// shared record reader has passed since the last call, in source order. An
// indexer calls it at the top of its record loop and once after the loop, so a
// placeholder lands at exactly the position the omitted record held rather
// than at the end of the transcript.
func appendOmissionPlaceholders(
	entries []schema.SessionEntry,
	scanner *jsonlRecordScanner,
	sessionID SessionID,
	harness Harness,
	entryIndex *int,
) ([]schema.SessionEntry, error) {
	for _, at := range scanner.TakeOmissions() {
		entry, err := omissionPlaceholderEntry(sessionID, harness, *entryIndex, at)
		if err != nil {
			return entries, err
		}
		entries = append(entries, entry)
		*entryIndex++
	}
	return entries, nil
}

// uncertifiableOversizedRecord is the refusal a strict index returns when it
// met an over-limit record itself. The tolerant index keeps going and shows a
// placeholder; a certified index cannot, because the record was left out.
func uncertifiableOversizedRecord(skipped []OversizedRecord, limit int) error {
	if len(skipped) == 0 {
		return nil
	}
	first := skipped[0]
	return fmt.Errorf(
		"a %d-byte record at line %d is over this build's %d-byte per-record limit and was left out, so this index covers %d record(s) fewer than the transcript; the transcript was indexed directly instead of through the ingest filter that writes an omission record for such a record, so nothing here certifies what is missing; rerun `peasant harvest index --force` for this session so ingest records the omission and stores the session as a partial capture, or shrink the oversized record in the source",
		first.Size, first.Line, limit, len(skipped),
	)
}

// outputRecordsItsOmissions reports whether an indexed result accounts for the
// records ingest left out, by carrying at least one omission placeholder entry.
//
// It is the discriminator between the two very different things the
// source_records_omitted code covers today. An oversized JSONL record is
// omitted with a placeholder standing in its position, so the stored entries
// still describe the whole session and the capture may be certified as full
// text and published. An OpenCode part this build cannot render, and an orphan
// graph part, raise the SAME code with NO placeholder: content is simply
// missing, and such a capture must stay the bounded preview it is.
//
// Reading the answer out of the entries themselves, rather than out of a
// parallel flag, is what keeps the two from drifting: a capture is certified
// full exactly when the omission is visible in what was stored.
func outputRecordsItsOmissions(output indexformat.Result) bool {
	v1, ok := output.(indexformat.V1)
	if !ok {
		return false
	}
	for _, entry := range v1.Entries {
		if _, omitted := OmittedRecordOf(entry); omitted {
			return true
		}
	}
	return false
}

// OmittedRecordOf reports the omission a stored entry stands for, when that
// entry is an omission placeholder.
//
// It is the read-side boundary of the placeholder: a reader asks the entry what
// it is instead of guessing from its shape, so the projection that shows a
// reader the note and the writer that stored it agree on one definition.
func OmittedRecordOf(entry schema.SessionEntry) (OmittedRecord, bool) {
	if entry.Extra == nil {
		return OmittedRecord{}, false
	}
	record, err := ParseOmittedRecord(*entry.Extra)
	if err != nil {
		return OmittedRecord{}, false
	}
	return record, true
}
