package ingest

import (
	"bytes"
	"fmt"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// filterStrikeOversizedRecords removes complete Strike JSONL records that are
// too large for the existing parsers before any redaction, artifact write, or
// indexing step can observe them. Newline boundaries are found directly, so an
// oversized record cannot make scanning stop before later valid records.
func filterStrikeOversizedRecords(data []byte, sourcePath string) ([]byte, []DiagnosticEntry) {
	var filtered bytes.Buffer
	filtered.Grow(len(data))
	var diagnostics []DiagnosticEntry

	line := 1
	for start := 0; start < len(data); line++ {
		relEnd := bytes.IndexByte(data[start:], '\n')
		end := len(data)
		next := len(data)
		if relEnd >= 0 {
			end = start + relEnd
			next = end + 1
		}

		record := data[start:end]
		if strikeRecordTooLarge(record) {
			diagnostics = append(diagnostics, DiagnosticEntry{
				ErrorType: "record_too_large",
				Location:  fmt.Sprintf("%s line %d", sourcePath, line),
				Message: fmt.Sprintf(
					"omitted a complete Strike JSONL record of %d bytes because it exceeds the %d-byte safe processing limit; the record was removed before redaction and later records were retained",
					len(record), defaults.ScannerMaxLine,
				),
				Remediation: "Remove or reduce the image-bearing event in the source transcript, then rerun peasant ingest; full large-record support is not available yet.",
			})
		} else {
			filtered.Write(record)
			if relEnd >= 0 {
				filtered.WriteByte('\n')
			}
		}

		if relEnd < 0 {
			break
		}
		start = next
	}

	if len(diagnostics) == 0 {
		return data, nil
	}
	return filtered.Bytes(), diagnostics
}

// bufio.Scanner needs room for the split delimiter or EOF probe in addition to
// the token itself, so a record at the configured maximum is not safe to retain.
func strikeRecordTooLarge(record []byte) bool {
	return len(record) >= defaults.ScannerMaxLine
}
