package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
)

var _ AuthoritativeTranscriptIndexer = (*OpenCodeIndexer)(nil)
var _ RetainedContentCapturer = (*OpenCodeIndexer)(nil)

// CaptureRetainedContent reports the retained OpenCode input's own
// completeness. A legacy directory-origin session is parsed from the exact
// native message/part tree captured for it, the same tree an ordinary index
// run hashes; a message whose part directory is absent means rows the adapter
// could not read, so that capture is incomplete, never certified. A managed
// projection is captured through the strict parser, which refuses anything
// it cannot certify.
func (idx *OpenCodeIndexer) CaptureRetainedContent(ctx context.Context, s DiscoveredSession) (ContentCaptureResult, error) {
	if err := ctx.Err(); err != nil {
		return ContentCaptureResult{}, err
	}
	if s.ContentOmitted {
		return ContentCaptureResult{}, captureFailure(s, 0, fmt.Errorf("upstream extraction omitted source records; regenerate the complete source with a supported Peasant version before retrying"))
	}
	if s.TranscriptOrigin != TranscriptOriginFile {
		data, err := idx.fs.ReadFile(s.SourcePath.String())
		if err != nil {
			return ContentCaptureResult{}, captureFailure(s, 0, err)
		}
		capture, err := idx.IndexTranscriptBytesForCapture(ctx, s, data)
		if err != nil {
			return ContentCaptureResult{}, err
		}
		return ContentCaptureResult{Entries: capture.Entries, Complete: true, InputHash: indexInputDigest(s, data, nil), InputBytes: int64(len(data))}, nil
	}
	tree, err := idx.captureJSONInput(ctx, s)
	if err != nil {
		return ContentCaptureResult{}, captureFailure(s, 0, err)
	}
	inputHash := indexInputDigest(s, nil, tree)
	for _, message := range tree.Messages {
		if message.PartsMissing {
			return ContentCaptureResult{Complete: false, InputHash: inputHash, InputBytes: openCodeTreeBytes(tree)}, nil
		}
	}
	messages, err := parseOpenCodeJSONInput(tree, &indexCompletion{ctx: ctx, session: s})
	treeBytes := openCodeTreeBytes(tree)
	if err != nil {
		return ContentCaptureResult{}, captureFailure(s, 0, err)
	}
	capture, err := idx.captureSemanticMessages(ctx, s, messages)
	if err != nil {
		return ContentCaptureResult{}, err
	}
	return ContentCaptureResult{Entries: capture.Entries, Complete: true, InputHash: inputHash, InputBytes: treeBytes}, nil
}

func (idx *OpenCodeIndexer) IndexTranscriptForCapture(ctx context.Context, s DiscoveredSession) (TranscriptCaptureResult, error) {
	if s.ContentOmitted {
		return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("upstream extraction omitted source records; regenerate the complete source with a supported Peasant version before retrying"))
	}
	if s.TranscriptOrigin != TranscriptOriginFile {
		if err := refuseOpenCodeProviderDatabase(idx.fs, s, s.SourcePath.String()); err != nil {
			return TranscriptCaptureResult{}, captureFailure(s, 0, err)
		}
		return captureTranscriptFile(ctx, idx.fs, idx, s)
	}
	root := resolveStorageRoot(s)
	dir := filepath.Join(root, defaults.OpenCodeDirMessage.String(), s.SessionID.String())
	files, err := idx.fs.ReadDir(dir)
	if err != nil {
		return TranscriptCaptureResult{}, captureFailure(s, 0, err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	var messages []openCodeSemanticMessage
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return TranscriptCaptureResult{}, err
		}
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		raw, err := idx.fs.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			return TranscriptCaptureResult{}, captureFailure(s, 0, err)
		}
		id := strings.TrimSuffix(file.Name(), ".json")
		message, err := parseOpenCodeSemanticMessage(id, 0, raw)
		if err != nil {
			return TranscriptCaptureResult{}, captureFailure(s, 0, err)
		}
		partDir := filepath.Join(root, defaults.OpenCodeDirPart.String(), id)
		parts, err := idx.fs.ReadDir(partDir)
		if err != nil && !os.IsNotExist(err) {
			return TranscriptCaptureResult{}, captureFailure(s, 0, err)
		}
		sort.Slice(parts, func(i, j int) bool { return parts[i].Name() < parts[j].Name() })
		for _, file := range parts {
			if err := ctx.Err(); err != nil {
				return TranscriptCaptureResult{}, err
			}
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}
			raw, err := idx.fs.ReadFile(filepath.Join(partDir, file.Name()))
			if err != nil {
				return TranscriptCaptureResult{}, captureFailure(s, 0, err)
			}
			part, err := parseOpenCodeSemanticPart(strings.TrimSuffix(file.Name(), ".json"), 0, raw)
			if err != nil {
				return TranscriptCaptureResult{}, captureFailure(s, 0, err)
			}
			message.Parts = append(message.Parts, part)
		}
		messages = append(messages, message)
	}
	return idx.captureSemanticMessages(ctx, s, messages)
}

func (idx *OpenCodeIndexer) IndexTranscriptBytesForCapture(ctx context.Context, s DiscoveredSession, data []byte) (TranscriptCaptureResult, error) {
	if s.ContentOmitted {
		return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("upstream extraction omitted source records; regenerate the complete source with a supported Peasant version before retrying"))
	}
	if s.TranscriptOrigin == TranscriptOriginFile {
		return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("legacy directory source cannot be certified from single-file bytes"))
	}
	format, version, err := managedOpenCodeProjectionFormat(s.TranscriptOrigin)
	if err != nil {
		return TranscriptCaptureResult{}, err
	}
	projection, err := decodeManagedOpenCodeProjection(data, format, version, s.SessionID)
	if err != nil {
		return TranscriptCaptureResult{}, captureFailure(s, 0, err)
	}
	if projection.ContentOmitted {
		return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("managed projection omitted unsupported native records; upgrade Peasant and regenerate harvest before retrying full capture"))
	}
	for _, message := range projection.Messages {
		if err := ctx.Err(); err != nil {
			return TranscriptCaptureResult{}, err
		}
		for _, row := range message.Parts {
			part, err := parseOpenCodeSemanticPart(row.ID, row.TimeCreated, row.Data)
			if err != nil {
				return TranscriptCaptureResult{}, captureFailure(s, 0, err)
			}
			if isOpenCodeCaptureControl(part.Data.Type) && (part.Data.Text != "" || len(part.Data.Input) > 0 || len(part.Data.Content) > 0 || len(part.Data.Output) > 0 || part.Data.State != nil) {
				return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("control part unexpectedly carries conversation/tool content"))
			}
		}
	}
	messages, unknown, err := parseManagedOpenCodeSemanticMessages(projection, managedOpenCodeProjectionKind(s.TranscriptOrigin))
	if err != nil {
		return TranscriptCaptureResult{}, captureFailure(s, 0, err)
	}
	var ignored []IgnoredSourceRecord
	for kind, count := range unknown {
		if !isOpenCodeCaptureControl(kind) {
			return TranscriptCaptureResult{}, captureFailure(s, 0, &UnrepresentedRecordError{Harness: HarnessOpenCode, Kind: kind})
		}
		for range count {
			ignored = append(ignored, IgnoredSourceRecord{Kind: kind, Reason: IgnoredRecordControl})
		}
	}
	result, err := idx.captureSemanticMessages(ctx, s, messages)
	if err != nil {
		return TranscriptCaptureResult{}, err
	}
	result.IgnoredRecords = append(result.IgnoredRecords, ignored...)
	return result, nil
}

func isOpenCodeCaptureControl(kind string) bool {
	switch kind {
	case "step-start", "step-finish", "snapshot", "patch":
		return true
	}
	return false
}

func (idx *OpenCodeIndexer) captureSemanticMessages(ctx context.Context, s DiscoveredSession, messages []openCodeSemanticMessage) (TranscriptCaptureResult, error) {
	var ignored []IgnoredSourceRecord
	for m, message := range messages {
		if err := ctx.Err(); err != nil {
			return TranscriptCaptureResult{}, err
		}
		if err := validateCaptureRole(message.Data.Role); err != nil {
			return TranscriptCaptureResult{}, captureFailure(s, 0, err)
		}
		if raw := message.Data.Content; len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			var text string
			if json.Unmarshal(raw, &text) != nil {
				var blocks []struct {
					Type string  `json:"type"`
					Text *string `json:"text"`
				}
				if err := json.Unmarshal(raw, &blocks); err != nil {
					return TranscriptCaptureResult{}, captureFailure(s, 0, err)
				}
				for _, block := range blocks {
					if block.Type != "text" || block.Text == nil {
						return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("unrepresented inline OpenCode message block"))
					}
				}
			}
		}
		parts := make([]openCodeSemanticPart, 0, len(message.Parts))
		for _, part := range message.Parts {
			if isOpenCodeCaptureControl(part.Data.Type) {
				if part.Data.Text != "" || len(part.Data.Input) > 0 || len(part.Data.Content) > 0 || len(part.Data.Output) > 0 || part.Data.State != nil {
					return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("control part unexpectedly carries conversation/tool content"))
				}
				ignored = append(ignored, IgnoredSourceRecord{Kind: part.Data.Type, Reason: IgnoredRecordControl})
				continue
			}
			if !isKnownOpenCodeSemanticPartType(part.Data.Type) {
				return TranscriptCaptureResult{}, captureFailure(s, 0, &UnrepresentedRecordError{Harness: HarnessOpenCode, Kind: part.Data.Type})
			}
			if part.Data.Type == "tool" || part.Data.Type == "tool_use" {
				if openCodeSemanticToolName(part.Data) == "" {
					return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("tool part requires name"))
				}
				if part.Data.Type == "tool" && part.Data.State == nil {
					return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("tool part requires state"))
				}
				if part.Data.Type == "tool_use" && len(part.Data.Input) == 0 {
					return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("tool_use part requires input"))
				}
			}
			if part.Data.Type == "text" || part.Data.Type == "reasoning" {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(part.Raw, &fields); err != nil || fields["text"] == nil {
					return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("text/reasoning part requires text"))
				}
			}
			parts = append(parts, part)
		}
		messages[m].Parts = parts
	}
	copy := *idx
	copy.fullContent = true
	copy.fullDepth = true
	entries := copy.indexSemanticMessages(s.SessionID, messages)
	// Expand inline arrays only after the historical shape/dedup decision. A
	// longer parent must not remove or renumber an existing child anchor.
	inline := make(map[string]string)
	for _, message := range messages {
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(message.Data.Content, &blocks) == nil && len(blocks) > 1 {
			var text []string
			for _, block := range blocks {
				text = append(text, unwrapOpenCodeDoubleEncodedText(block.Text))
			}
			inline[message.EntryID] = strings.Join(text, "\n")
		}
	}
	for i := range entries {
		if entries[i].Depth == 0 && entries[i].EntryID != nil {
			if text, ok := inline[*entries[i].EntryID]; ok {
				entries[i].ContentPreview = &text
			}
		}
	}
	return TranscriptCaptureResult{Entries: entries, IgnoredRecords: ignored}, nil
}

// refuseOpenCodeProviderDatabase keeps the managed-projection reader off the
// provider database. An OpenCode SQLite session's DISCOVERED source path is
// that database, and only the post-harvest managed projection ever belongs at
// the path this reader opens, so a wiring mistake would otherwise load a
// multi-gigabyte database into memory and end the process.
//
// It answers that with a content test, not a size one. A projection is
// identified by what it IS: a database announces itself in its first bytes,
// and the Peasant-owned envelope is verified in full when the projection is
// decoded. A long OpenCode session legitimately produces a large projection,
// and refusing it for its size alone would fail the whole session and lose it,
// which no size may do.
//
// The header is read on its own where the filesystem can do that, so the
// database is never loaded even to identify it.
func refuseOpenCodeProviderDatabase(filesystem FileSystem, session DiscoveredSession, path string) error {
	limit := len(openCodeSQLiteHeader)
	var header []byte
	if reader, ok := filesystem.(fileHeaderReader); ok {
		read, err := reader.ReadFileHeader(path, limit)
		if err != nil {
			return fmt.Errorf("identify the managed OpenCode projection at %q for session %q: %w; no entry rows were stored; restore read access to the managed artifact and rerun harvest", path, session.SessionID, err)
		}
		header = read
	} else {
		// A filesystem with no header capability is an in-memory one holding
		// only what a caller put there, so reading it whole costs nothing real.
		data, err := filesystem.ReadFile(path)
		if err != nil {
			return fmt.Errorf("identify the managed OpenCode projection at %q for session %q: %w; no entry rows were stored; restore read access to the managed artifact and rerun harvest", path, session.SessionID, err)
		}
		header = data[:min(limit, len(data))]
	}
	if string(header) == openCodeSQLiteHeader {
		return fmt.Errorf("read the managed OpenCode projection for session %q: %q is an OpenCode SQLite database, not the Peasant-owned projection this path must hold; the file was not read into memory and no entry rows were stored; rerun harvest to regenerate the managed projection, and never point the reader at the provider database", session.SessionID, path)
	}
	return nil
}

// openCodeTreeBytes sums the message and part bytes of a captured native tree,
// so the one-time content pass can charge what it read against its budget.
func openCodeTreeBytes(tree *openCodeJSONInput) int64 {
	if tree == nil {
		return 0
	}
	var total int64
	for _, message := range tree.Messages {
		total += int64(len(message.File.Data))
		for _, part := range message.Parts {
			total += int64(len(part.Data))
		}
	}
	return total
}
