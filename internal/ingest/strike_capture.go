package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// captureDecodedStrike validates and assembles each physical record together.
// Decoded payloads live for one record; only the existing correlation maps and
// assembled entries survive it. No partial assembly escapes a failed capture.
func (idx *StrikeIndexer) captureDecodedStrike(ctx context.Context, s DiscoveredSession, data []byte) (TranscriptCaptureResult, error) {
	a := &strikeAssembly{
		sessionID:      s.SessionID,
		fullContent:    true,
		parents:        make(map[string]*strikeParentState),
		turnParents:    make(map[string]string),
		ordinalParents: make(map[int]string),
		calls:          make(map[string]*strikeCallState),
		processCalls:   make(map[string]string),
	}
	v := strikeCaptureValidation{
		calls:     make(map[string]bool),
		processes: make(map[string]string),
		pending:   make(map[string]bool),
	}
	var traversal unknownJSONLTraversal
	var ignored []IgnoredSourceRecord
	var failure error
	sawOmission := false
	forEachStrikeRecord(data, func(line int, physical []byte) {
		if failure != nil {
			return
		}
		fail := func(err error) { failure = &captureCorruptionError{err: captureFailure(s, line, err)} }
		if err := ctx.Err(); err != nil {
			failure = &captureCorruptionError{err: err}
			return
		}
		raw := bytes.TrimSpace(physical)
		if len(raw) == 0 {
			return
		}
		if !json.Valid(raw) {
			fail(malformedSourceRecord{})
			return
		}
		if at, yes := parseOmittedRecordSentinel(raw); yes {
			sawOmission = true
			if strikeRecordTooLarge(physical, idx.maxRecordBytes) {
				return
			}
			entry, err := omissionPlaceholderEntry(s.SessionID, HarnessStrike, a.nextIndex, at)
			if err != nil {
				failure = err
				return
			}
			a.entries = append(a.entries, entry)
			a.nextIndex++
			return
		}
		tooLarge := strikeRecordTooLarge(physical, idx.maxRecordBytes)
		position := &traversal
		if tooLarge {
			// Validation still visits skipped records, but their content blocks
			// never participated in the assembly's public traversal coordinates.
			position = nil
		}
		filtered, unknown, whole, err := prepareUnknownJSONL(HarnessStrike, physical, line, position)
		if err != nil {
			fail(err)
			return
		}
		var env strikeEnvelope
		var event strikeEventData
		if !whole {
			if strikeRecordTooLarge(bytes.TrimSpace(filtered), 0) {
				fail(fmt.Errorf("record exceeds Strike format limit"))
				return
			}
			if err := json.Unmarshal(filtered, &env); err != nil {
				fail(err)
				return
			}
			if !isKnownStrikeEvent(env.Type) {
				fail(fmt.Errorf("event lacks its type"))
				return
			}
			if len(env.Data) == 0 || bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
				fail(fmt.Errorf("event requires data object"))
				return
			}
			event, err = decodeStrikeEventData(env.Data)
			if err != nil {
				fail(err)
				return
			}
			record, err := v.observe(env, event)
			if err != nil {
				fail(err)
				return
			}
			if record != nil {
				ignored = append(ignored, *record)
			}
		}
		// Tolerant assembly historically skips physical records over its
		// configured limit, even when strict validation accepts their payload.
		if tooLarge {
			return
		}
		for _, record := range unknown {
			entry, err := RetainedUnknownEntry(s.SessionID, a.nextIndex, record)
			if err != nil {
				failure = err
				return
			}
			a.entries = append(a.entries, entry)
			a.nextIndex++
		}
		if !whole {
			a.applyDecodedEvent(env, event, len(filtered))
		}
	})
	if failure != nil {
		return TranscriptCaptureResult{}, failure
	}
	if s.ContentOmitted && !sawOmission {
		return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("the stored transcript omits source records without positional evidence; restore the source before certifying capture"))
	}
	if len(v.pending) != 0 {
		return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("tool output has no terminal event; restore complete tool trace"))
	}
	unknown, err := retainedUnknownEntries(a.entries)
	return TranscriptCaptureResult{Entries: a.entries, IgnoredRecords: ignored, RetainedUnknown: unknown}, err
}
