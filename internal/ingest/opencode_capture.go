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
		return ContentCaptureResult{Entries: capture.Entries, Complete: true, InputHash: indexInputDigest(s, data, nil)}, nil
	}
	tree, err := idx.captureJSONInput(ctx, s)
	if err != nil {
		return ContentCaptureResult{}, captureFailure(s, 0, err)
	}
	inputHash := indexInputDigest(s, nil, tree)
	for _, message := range tree.Messages {
		if message.PartsMissing {
			return ContentCaptureResult{Complete: false, InputHash: inputHash}, nil
		}
	}
	messages, err := parseOpenCodeJSONInput(tree, &indexCompletion{ctx: ctx, session: s})
	if err != nil {
		return ContentCaptureResult{}, captureFailure(s, 0, err)
	}
	capture, err := idx.captureSemanticMessages(ctx, s, messages)
	if err != nil {
		return ContentCaptureResult{}, err
	}
	return ContentCaptureResult{Entries: capture.Entries, Complete: true, InputHash: inputHash}, nil
}

func (idx *OpenCodeIndexer) IndexTranscriptForCapture(ctx context.Context, s DiscoveredSession) (TranscriptCaptureResult, error) {
	if s.ContentOmitted {
		return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("upstream extraction omitted source records; regenerate the complete source with a supported Peasant version before retrying"))
	}
	if s.TranscriptOrigin != TranscriptOriginFile {
		info, err := idx.fs.Stat(s.SourcePath.String())
		if err != nil {
			return TranscriptCaptureResult{}, captureFailure(s, 0, err)
		}
		if info.Size() > defaults.OpenCodeManagedProjectionMaxBytes {
			return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("managed projection exceeds supported file bound; regenerate harvest projection"))
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
			return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("unrepresented OpenCode part vocabulary %q", kind))
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
				return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("unrepresented OpenCode part %q", part.Data.Type))
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
