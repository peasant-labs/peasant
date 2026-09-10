package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// OversizedRecordDiagnosticType is the diagnostic a source record's omission
// is reported under. It is the same for every harness.
const OversizedRecordDiagnosticType = string(OmittedRecordTooLarge)

// filterOversizedJSONLRecords is the one pre-redaction filter every JSONL
// harness runs. A record within maxRecordBytes passes through byte for byte.
// A record over it is replaced, in place, by a compact stand-in line that
// records the omission, and is reported as a warning. Nothing else changes,
// so:
//
//   - an over-cap record never reaches the redaction engine, which would
//     refuse the whole file for it, and never reaches a harness parser;
//   - the records after it are kept, and every later record keeps its
//     physical line, so a diagnostic and a placeholder entry both point at
//     the position the omitted record held;
//   - the session still ingests and is stored partial. It is never failed and
//     the omission is never silent.
//
// When no record is over the limit the original slice is returned unchanged,
// so an ordinary transcript is not copied and its fingerprint does not move.
func filterOversizedJSONLRecords(
	ctx context.Context,
	data []byte,
	sourcePath string,
	maxRecordBytes int,
) ([]byte, []DiagnosticEntry, error) {
	scanner := newJSONLRecordScanner(data, maxRecordBytes)
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}

	var (
		filtered      bytes.Buffer
		diagnostics   []DiagnosticEntry
		omitted       bool
		lastWasRecord bool
	)
	filtered.Grow(len(data))

	writeStandIn := func(at OmittedRecordAt) error {
		sentinel, err := encodeOmittedRecordSentinel(at)
		if err != nil {
			return err
		}
		filtered.Write(sentinel)
		filtered.WriteByte('\n')
		diagnostics = append(diagnostics, oversizedRecordDiagnostic(sourcePath, at.Record))
		omitted = true
		lastWasRecord = false
		return nil
	}

	// writeOmissions puts a stand-in at the position of each record the reader
	// left out since the last call. The reader reports both a record this pass
	// skipped for its size and a stand-in the input already carried, so
	// filtering an artifact that already records an omission keeps that
	// omission rather than losing it.
	writeOmissions := func() error {
		for _, omission := range scanner.TakeOmissions() {
			if err := writeStandIn(omission); err != nil {
				return err
			}
		}
		return nil
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if err := writeOmissions(); err != nil {
			return nil, nil, err
		}
		filtered.Write(scanner.Bytes())
		filtered.WriteByte('\n')
		lastWasRecord = true
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	if err := writeOmissions(); err != nil {
		return nil, nil, err
	}
	if !omitted {
		return data, nil, nil
	}
	out := filtered.Bytes()
	// Keep the source's own ending. A transcript still being written has no
	// final newline, and adding one would make an unfinished last record look
	// finished to every later reader.
	if lastWasRecord && len(data) > 0 && data[len(data)-1] != '\n' {
		out = out[:len(out)-1]
	}
	return out, diagnostics, nil
}

// oversizedRecordDiagnostic states what was omitted, why, where, what it means
// for the stored session, and what makes it come back.
func oversizedRecordDiagnostic(sourcePath string, record OmittedRecord) DiagnosticEntry {
	return DiagnosticEntry{
		ErrorType: OversizedRecordDiagnosticType,
		Location:  fmt.Sprintf("%s line %d", sourcePath, record.Line),
		Message: fmt.Sprintf(
			"omitted one JSONL record of %d bytes (%s) at line %d because it is over this build's %d-byte (%s) per-record limit; the record was left out before redaction, every other record in the session was kept and indexed, and a placeholder entry marks the position it held",
			record.Bytes, humanByteSize(record.Bytes), record.Line,
			record.LimitBytes, humanByteSize(record.LimitBytes),
		),
		Remediation: fmt.Sprintf(
			"No action is needed to keep the session: it is stored as a partial capture and stays readable and searchable without that one record. The record returns on the next harvest of this session if it becomes smaller than %s in the source, or if a build with a larger per-record limit indexes it. Publishing refuses a partial capture, so shrink or re-record the oversized tool output before sharing this session.",
			humanByteSize(record.LimitBytes),
		),
	}
}

// toolCallIDFromRecordPrefix reads a correlation id out of the opening bytes
// of a record that was omitted, so the placeholder entry can pair with the
// tool call it answers. Only the id is taken; no other byte of the omitted
// record is kept or stored. A prefix that does not clearly carry one yields
// no id rather than a guess.
func toolCallIDFromRecordPrefix(prefix []byte) *string {
	if len(prefix) == 0 {
		return nil
	}
	for _, key := range []string{`"tool_use_id"`, `"toolCallId"`, `"toolUseId"`, `"call_id"`, `"callId"`} {
		at := bytes.Index(prefix, []byte(key))
		if at < 0 {
			continue
		}
		rest := prefix[at+len(key):]
		colon := bytes.IndexByte(rest, ':')
		if colon < 0 {
			continue
		}
		rest = bytes.TrimLeft(rest[colon+1:], " \t")
		if len(rest) == 0 || rest[0] != '"' {
			continue
		}
		end := bytes.IndexByte(rest[1:], '"')
		if end <= 0 {
			continue
		}
		var value string
		if err := json.Unmarshal(rest[:end+2], &value); err != nil || value == "" {
			continue
		}
		return &value
	}
	return nil
}

// productionJSONLRecordLimit is the limit a caller uses when it has no
// injected one. Tests pass their own small limit through the parameter, so no
// global is ever mutated and parallel tests cannot interfere.
func productionJSONLRecordLimit(injected int) int {
	if injected > 0 {
		return injected
	}
	return defaults.MaxJSONLRecordBytes
}

// jsonlRecordTooLarge reports whether a record is over the per-record limit
// this build reads whole. A record exactly at the limit is within it: the
// shared record reader needs no headroom beyond the record itself, unlike the
// bufio.Scanner this replaced.
func jsonlRecordTooLarge(record []byte, maxRecordBytes int) bool {
	return len(record) > productionJSONLRecordLimit(maxRecordBytes)
}

// strikeRecordTooLarge applies the shared per-record limit at the Strike
// call sites that check a record they already hold.
func strikeRecordTooLarge(record []byte) bool {
	return jsonlRecordTooLarge(record, 0)
}
