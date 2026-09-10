package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

type piEntryType string

const (
	piSession        piEntryType = "session"
	piMessage        piEntryType = "message"
	piThinkingChange piEntryType = "thinking_level_change"
	piModelChange    piEntryType = "model_change"
	piCompaction     piEntryType = "compaction"
	piBranchSummary  piEntryType = "branch_summary"
	piCustom         piEntryType = "custom"
	piCustomMessage  piEntryType = "custom_message"
	piLabel          piEntryType = "label"
	piSessionInfo    piEntryType = "session_info"
)

func (t *piEntryType) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	switch value {
	case string(piSession):
		*t = piSession
	case string(piMessage):
		*t = piMessage
	case string(piThinkingChange):
		*t = piThinkingChange
	case string(piModelChange):
		*t = piModelChange
	case string(piCompaction):
		*t = piCompaction
	case string(piBranchSummary):
		*t = piBranchSummary
	case string(piCustom):
		*t = piCustom
	case string(piCustomMessage):
		*t = piCustomMessage
	case string(piLabel):
		*t = piLabel
	case string(piSessionInfo):
		*t = piSessionInfo
	default:
		return fmt.Errorf("unknown Pi entry type")
	}
	return nil
}

type piEntry struct {
	Type          piEntryType     `json:"type"`
	ID            string          `json:"id"`
	ParentID      *string         `json:"parentId"`
	Timestamp     json.RawMessage `json:"timestamp"`
	Version       int             `json:"version"`
	CWD           string          `json:"cwd"`
	ParentSession string          `json:"parentSession"`
	Name          string          `json:"name"`
	Message       json.RawMessage `json:"message"`
	Content       json.RawMessage `json:"content"`
	Summary       string          `json:"summary"`
	CustomType    string          `json:"customType"`
	Data          json.RawMessage `json:"data"`
	Details       json.RawMessage `json:"details"`
	Usage         json.RawMessage `json:"usage"`
}

type piDocument struct {
	// consumedBytes excludes only a recoverable incomplete physical final line.
	consumedBytes int
	header        piEntry
	active        []piEntry
	title         string
	hasTitle      bool
	warnings      []DiagnosticEntry
	// omissions lists the records ingest left out of this document, each with
	// the number of accepted entries that preceded it, so the projection can
	// put a placeholder entry where the omitted record stood.
	omissions []piOmission
}

// piOmission is one omitted source record and its position in the document.
type piOmission struct {
	At OmittedRecordAt
	// AfterEntries is how many entries the document had accepted before the
	// omitted record. A Pi recording is an append-only log, so for an
	// unbranched session this is the omitted record's own position among the
	// entries; for a branched one it is the nearest position on the active
	// path. The record's true physical line stays in the typed omission
	// record and in the reader-facing note either way.
	AfterEntries int
}

func piSourceError(step string, line int, cause error) error {
	return fmt.Errorf("Pi %s failed in ingest at physical line %d: %w; this session was not accepted and other sessions may continue; restore a valid Pi v3 recording and retry harvest", step, line, cause)
}

func readPiSource(ctx context.Context, fs FileSystem, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := fs.Stat(path); err != nil {
		return nil, piSourceError("source read", 0, err)
	}
	// No size refuses a Pi recording. The former 64 MiB file bound failed the
	// whole session, which lost every record in it; a record that is too large
	// to process is left out one record at a time by parsePiDocument instead.
	var (
		data []byte
		err  error
	)
	if streaming, ok := fs.(interface {
		Open(string) (io.ReadCloser, error)
	}); ok {
		reader, openErr := streaming.Open(path)
		if openErr != nil {
			return nil, piSourceError("source read", 0, openErr)
		}
		defer reader.Close()
		data, err = io.ReadAll(reader)
	} else {
		data, err = fs.ReadFile(path)
	}
	if err != nil {
		return nil, piSourceError("source read", 0, err)
	}
	return data, nil
}

func parsePiDocument(ctx context.Context, data []byte) (piDocument, error) {
	return parsePiDocumentWithLimit(ctx, data, defaults.MaxJSONLRecordBytes)
}

// parsePiDocumentWithLimit reads a Pi recording with the shared JSONL record
// reader and the shared per-record limit.
//
// The three bounds this replaced each failed the WHOLE session and lost every
// record in it: a file over 64 MiB, a line over 8 MiB and a recording of more
// than 200000 physical lines. A Pi recording is now read like every other
// JSONL harness: a record up to the limit is read and indexed whole, and a
// record over it is left out one record at a time, reported as a warning, and
// marked by a placeholder entry, while the session still imports and is stored
// partial. The incomplete-final-line warning is unchanged.
func parsePiDocumentWithLimit(ctx context.Context, data []byte, maxRecordBytes int) (piDocument, error) {
	limit := productionJSONLRecordLimit(maxRecordBytes)
	doc := piDocument{consumedBytes: len(data)}
	scanner := newJSONLRecordScanner(data, limit)
	if err := scanner.Err(); err != nil {
		return doc, piSourceError("parse", 0, err)
	}
	entries := make(map[string]piEntry)
	var order []string
	offset := 0

	// takeOmissions records the records this read left out, at the position
	// they held, before the next accepted record is processed.
	takeOmissions := func() error {
		for _, skipped := range scanner.TakeOversized() {
			offset += skipped.Size + 1
			record, err := NewOmittedRecord(OmittedRecordTooLarge, skipped.Line, int64(skipped.Size), int64(limit))
			if err != nil {
				return piSourceError("parse", skipped.Line, err)
			}
			at := OmittedRecordAt{Record: record, Line: skipped.Line, ToolCallID: toolCallIDFromRecordPrefix(skipped.Prefix)}
			doc.omissions = append(doc.omissions, piOmission{At: at, AfterEntries: len(order)})
			doc.warnings = append(doc.warnings, oversizedRecordDiagnostic("Pi recording", record))
		}
		for _, at := range scanner.TakeOmissions() {
			doc.omissions = append(doc.omissions, piOmission{At: at, AfterEntries: len(order)})
			doc.warnings = append(doc.warnings, oversizedRecordDiagnostic("Pi recording", at.Record))
		}
		return nil
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return doc, err
		}
		if err := takeOmissions(); err != nil {
			return doc, err
		}
		physical := scanner.Bytes()
		line := scanner.Line() - 1
		lineStart := offset
		offset += len(physical) + 1
		// The record is the document's last when nothing follows it, whether
		// or not the source ended with a newline.
		isLast := lineStart+len(physical)+1 >= len(data)
		raw := bytes.TrimSpace(physical)
		if len(raw) == 0 {
			continue
		}
		if err := schema.ScanRawJSONDocument(raw, schema.RawJSONPathPolicy{MaxDocumentBytes: limit, MaxDocumentDepth: 128}); err != nil {
			if isLast && piIncompleteTail(raw, err) {
				doc.consumedBytes = lineStart
				doc.warnings = append(doc.warnings, piWarning("incomplete_tail", line+1, "Incomplete final JSONL line was ignored; the complete prefix was imported."))
				break
			}
			return doc, piSourceError("raw validation", line+1, err)
		}
		var entry piEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return doc, piSourceError("decode", line+1, err)
		}
		if entry.Type == "" || strings.TrimSpace(entry.ID) == "" {
			return doc, piSourceError("decode", line+1, fmt.Errorf("entry type and id are required"))
		}
		if doc.header.ID == "" {
			if entry.Type != piSession || entry.Version != 3 || !strings.HasPrefix(entry.CWD, "/") {
				return doc, piSourceError("header", line+1, fmt.Errorf("first nonempty record must be a v3 session header with absolute cwd"))
			}
			if _, err := NewSessionID(entry.ID); err != nil {
				return doc, piSourceError("header", line+1, err)
			}
			doc.header = entry
			continue
		}
		if entry.Type == piSession || entry.ID == doc.header.ID {
			return doc, piSourceError("tree", line+1, fmt.Errorf("duplicate header or id"))
		}
		if _, exists := entries[entry.ID]; exists {
			return doc, piSourceError("tree", line+1, fmt.Errorf("duplicate entry id"))
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		if _, exists := fields["parentId"]; !exists {
			return doc, piSourceError("tree", line+1, fmt.Errorf("nonheader entry requires parentId (null for root)"))
		}
		entries[entry.ID] = entry
		order = append(order, entry.ID)
		if entry.Type == piSessionInfo {
			doc.title = strings.TrimSpace(entry.Name)
			doc.hasTitle = true
		}
	}
	if err := scanner.Err(); err != nil {
		return doc, piSourceError("parse", scanner.Line(), err)
	}
	if err := takeOmissions(); err != nil {
		return doc, err
	}
	if doc.header.ID == "" {
		return doc, piSourceError("header", 0, fmt.Errorf("session header is missing"))
	}
	if len(order) == 0 {
		return doc, nil
	}
	seen := make(map[string]bool)
	for id := order[len(order)-1]; id != ""; {
		if seen[id] {
			return doc, piSourceError("active tree", 0, fmt.Errorf("cycle in active path"))
		}
		entry, ok := entries[id]
		if !ok {
			if len(doc.omissions) > 0 {
				// The missing parent is a record this read left out for its
				// size. Ending the walk here keeps the rest of the session,
				// which is the point of omitting one record instead of
				// failing the session; the placeholder entry states what is
				// missing and where.
				break
			}
			return doc, piSourceError("active tree", 0, fmt.Errorf("dangling active parent"))
		}
		seen[id] = true
		doc.active = append(doc.active, entry)
		id = ""
		if entry.ParentID != nil {
			id = *entry.ParentID
			if id == "" {
				return doc, piSourceError("active tree", 0, fmt.Errorf("empty parent is not a root; use null"))
			}
		}
	}
	for left, right := 0, len(doc.active)-1; left < right; left, right = left+1, right-1 {
		doc.active[left], doc.active[right] = doc.active[right], doc.active[left]
	}
	// Validate cycles globally without making an inactive missing parent fatal.
	done := make(map[string]bool)
	for _, start := range order {
		path := make(map[string]bool)
		for id := start; id != "" && !done[id]; {
			if path[id] {
				return doc, piSourceError("tree", 0, fmt.Errorf("cycle in inactive path"))
			}
			entry, ok := entries[id]
			if !ok {
				doc.warnings = append(doc.warnings, piWarning("inactive_dangling_parent", 0, "An inactive branch has a missing parent; only the complete active path was imported."))
				break
			}
			path[id] = true
			id = ""
			if entry.ParentID != nil {
				id = *entry.ParentID
			}
		}
		for id := range path {
			done[id] = true
		}
	}
	return doc, nil
}

// A syntactic EOF cannot override an earlier raw-scanner safety failure (such
// as duplicate keys, invalid Unicode, or excessive depth).
func piIncompleteTail(raw []byte, scanErr error) bool {
	if !errors.Is(scanErr, io.EOF) && !errors.Is(scanErr, io.ErrUnexpectedEOF) {
		return false
	}
	var probe json.RawMessage
	var syntax *json.SyntaxError
	err := json.Unmarshal(raw, &probe)
	return errors.As(err, &syntax) && syntax.Error() == "unexpected end of JSON input"
}

func piWarning(code string, line int, message string) DiagnosticEntry {
	return DiagnosticEntry{ErrorType: code, Location: fmt.Sprintf("Pi parser line %d", line), Message: message, Remediation: "Let Pi finish writing or restore the referenced source entries, then retry harvest."}
}
