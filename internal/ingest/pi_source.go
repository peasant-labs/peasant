package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/peasant-labs/schema"
)

const (
	piMaxFile  = 64 << 20
	piMaxLine  = 8 << 20
	piMaxLines = 200000
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
	header   piEntry
	active   []piEntry
	title    string
	hasTitle bool
	warnings []DiagnosticEntry
}

func piSourceError(step string, line int, cause error) error {
	return fmt.Errorf("Pi %s failed in ingest at physical line %d: %w; this session was not accepted and other sessions may continue; restore a valid Pi v3 recording and retry harvest", step, line, cause)
}

func readPiSource(ctx context.Context, fs FileSystem, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := fs.Stat(path)
	if err != nil {
		return nil, piSourceError("source read", 0, err)
	}
	if info.Size() > piMaxFile {
		return nil, piSourceError("source read", 0, fmt.Errorf("file exceeds 64 MiB"))
	}
	var data []byte
	if streaming, ok := fs.(interface {
		Open(string) (io.ReadCloser, error)
	}); ok {
		reader, openErr := streaming.Open(path)
		if openErr != nil {
			return nil, piSourceError("source read", 0, openErr)
		}
		defer reader.Close()
		data, err = io.ReadAll(io.LimitReader(reader, piMaxFile+1))
	} else {
		data, err = fs.ReadFile(path)
	}
	if err != nil {
		return nil, piSourceError("source read", 0, err)
	}
	if len(data) > piMaxFile {
		return nil, piSourceError("source read", 0, fmt.Errorf("file exceeds 64 MiB"))
	}
	return data, nil
}

func parsePiDocument(ctx context.Context, data []byte) (piDocument, error) {
	var doc piDocument
	if len(data) > piMaxFile {
		return doc, piSourceError("parse", 0, fmt.Errorf("file exceeds 64 MiB"))
	}
	lines := bytes.Split(data, []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > piMaxLines {
		return doc, piSourceError("parse", 0, fmt.Errorf("file exceeds 200000 physical lines"))
	}
	entries := make(map[string]piEntry)
	var order []string
	for line, raw := range lines {
		if err := ctx.Err(); err != nil {
			return doc, err
		}
		if len(raw) > piMaxLine {
			return doc, piSourceError("parse", line+1, fmt.Errorf("line exceeds 8 MiB"))
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		if err := schema.ScanRawJSONDocument(raw, schema.RawJSONPathPolicy{MaxDocumentBytes: piMaxLine, MaxDocumentDepth: 128}); err != nil {
			var probe json.RawMessage
			syntax := json.Unmarshal(raw, &probe)
			if line == len(lines)-1 && syntax != nil && syntax.Error() == "unexpected end of JSON input" && !strings.Contains(err.Error(), "duplicate") {
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

func piWarning(code string, line int, message string) DiagnosticEntry {
	return DiagnosticEntry{ErrorType: code, Location: fmt.Sprintf("Pi parser line %d", line), Message: message, Remediation: "Let Pi finish writing or restore the referenced source entries, then retry harvest."}
}
