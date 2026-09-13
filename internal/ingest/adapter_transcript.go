package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// TranscriptMetadataExtractor extracts the adapter-owned fields represented in
// transcript bytes, preserving the original metadata's native-only context.
// Implementations refuse input that cannot support their current behavior.
type TranscriptMetadataExtractor interface {
	ExtractMetadataFromTranscript(context.Context, []byte, *UnifiedMetadata) (*UnifiedMetadata, error)
}

// InsufficientRetainedInputError means adapter refresh requires native input.
// It does not prevent a supported indexer from reading the last-good artifact.
type InsufficientRetainedInputError struct {
	SessionID SessionID
	Harness   Harness
	Reason    string
}

func (e *InsufficientRetainedInputError) Error() string {
	return fmt.Sprintf("extract metadata from retained %s session %s during adapter refresh: %s; previous artifacts and adapter provenance were preserved; restore the native source and retry harvest", e.Harness, e.SessionID, e.Reason)
}

var _ TranscriptMetadataExtractor = (*ClaudeAdapter)(nil)
var _ TranscriptMetadataExtractor = (*CodexAdapter)(nil)
var _ TranscriptMetadataExtractor = (*CursorAdapter)(nil)
var _ TranscriptMetadataExtractor = (*PiAdapter)(nil)

func (a *PiAdapter) ExtractMetadataFromTranscript(ctx context.Context, data []byte, original *UnifiedMetadata) (*UnifiedMetadata, error) {
	metadata, err := metadataForTranscriptExtraction(ctx, HarnessPi, data, original)
	if err != nil {
		return nil, err
	}
	doc, err := parsePiDocument(ctx, data)
	if err != nil {
		return nil, err
	}
	if doc.header.ID != original.SessionID.String() {
		return nil, &InsufficientRetainedInputError{SessionID: original.SessionID, Harness: HarnessPi, Reason: "retained header identifies another session"}
	}
	parsed, err := piTranscriptMetadata(doc, DiscoveredSession{SessionID: original.SessionID, CreatedAt: time.UnixMilli(original.Timestamp.Start), ModTime: time.UnixMilli(original.Timestamp.End)})
	if err != nil {
		return nil, err
	}
	metadata.Model, metadata.Version = parsed.Model, parsed.Version
	metadata.Timestamp.Start, metadata.Timestamp.End = parsed.Timestamp.Start, parsed.Timestamp.End
	parsed.Stats.SubagentCount = metadata.Stats.SubagentCount
	metadata.Stats = parsed.Stats
	metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, parsed.Diagnostics.Warnings...)
	return metadata, nil
}

func metadataForTranscriptExtraction(ctx context.Context, harness Harness, data []byte, original *UnifiedMetadata) (*UnifiedMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if original == nil {
		return nil, &InsufficientRetainedInputError{Harness: harness, Reason: "original identity and context metadata are missing"}
	}
	insufficient := func(reason string) error {
		return &InsufficientRetainedInputError{SessionID: original.SessionID, Harness: harness, Reason: reason}
	}
	if original.ModelHarness != harness || original.Source.Format != SourceFormatJSONL || metadataNeedsNativeRefresh(original.SchemaVersion) || original.SchemaVersion > CurrentSchemaVersion {
		return nil, insufficient("the retained harness, format or metadata schema does not support JSONL metadata extraction")
	}
	if _, err := NewHostSlug(string(original.HostSlug)); err != nil {
		return nil, insufficient("the original managed project locator is missing or invalid")
	}
	if err := validateRetainedJSONL(ctx, data); err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return nil, insufficient(err.Error())
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		return nil, insufficient("original metadata could not be copied: " + err.Error())
	}
	var metadata UnifiedMetadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return nil, insufficient("original metadata could not be decoded: " + err.Error())
	}
	return &metadata, nil
}

// validateRetainedJSONL checks complete object records before metadata recovery
// or the adapters' historically tolerant metadata extraction scans.
func validateRetainedJSONL(ctx context.Context, data []byte) error {
	scanner := newJSONLRecordScanner(data, defaults.MaxJSONLRecordBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) != 0 && (!json.Valid(raw) || raw[0] != '{') {
			return fmt.Errorf("a retained JSONL record is malformed or is not an object")
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("retained JSONL could not be completely scanned: %w", err)
	}
	return ctx.Err()
}

func (a *ClaudeAdapter) ExtractMetadataFromTranscript(ctx context.Context, data []byte, original *UnifiedMetadata) (*UnifiedMetadata, error) {
	metadata, err := metadataForTranscriptExtraction(ctx, HarnessClaudeCode, data, original)
	if err != nil {
		return nil, err
	}
	parsed := NewUnifiedMetadata()
	parsed.SessionID = original.SessionID
	parsed.ParentUUID = original.ParentUUID
	first, parseErr := parseClaudeTranscriptMetadata(data, &parsed)
	if parseErr != nil {
		return nil, parseErr
	}
	if first == nil || first.Type == "" {
		return nil, &InsufficientRetainedInputError{SessionID: original.SessionID, Harness: HarnessClaudeCode, Reason: "retained input has no identifiable Claude record"}
	}
	if err := transcriptMetadataParsingError(HarnessClaudeCode, &parsed); err != nil {
		return nil, err
	}
	metadata.Model, metadata.Version = parsed.Model, parsed.Version
	metadata.Timestamp.Start, metadata.Timestamp.End = parsed.Timestamp.Start, parsed.Timestamp.End
	metadata.Stats.DurationMs = parsed.Stats.DurationMs
	metadata.Stats.TurnCount, metadata.Stats.ToolCallCount = parsed.Stats.TurnCount, parsed.Stats.ToolCallCount
	metadata.Stats.TokensIn, metadata.Stats.TokensOut = parsed.Stats.TokensIn, parsed.Stats.TokensOut
	metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, parsed.Diagnostics.Warnings...)
	return metadata, nil
}

func (a *CodexAdapter) ExtractMetadataFromTranscript(ctx context.Context, data []byte, original *UnifiedMetadata) (*UnifiedMetadata, error) {
	metadata, err := metadataForTranscriptExtraction(ctx, HarnessCodex, data, original)
	if err != nil {
		return nil, err
	}
	parsed := NewUnifiedMetadata()
	parsed.SessionID = original.SessionID
	sessionMeta, err := parseCodexTranscriptMetadata(ctx, data, &parsed)
	if err != nil {
		return nil, err
	}
	if sessionMeta == nil {
		return nil, &InsufficientRetainedInputError{SessionID: original.SessionID, Harness: HarnessCodex, Reason: "retained input has no usable session_meta record"}
	}
	if err := transcriptMetadataParsingError(HarnessCodex, &parsed); err != nil {
		return nil, err
	}
	metadata.Model, metadata.Version = parsed.Model, parsed.Version
	metadata.Timestamp.Start, metadata.Timestamp.End = parsed.Timestamp.Start, parsed.Timestamp.End
	metadata.Stats.DurationMs = parsed.Stats.DurationMs
	metadata.Stats.TurnCount, metadata.Stats.ToolCallCount = parsed.Stats.TurnCount, parsed.Stats.ToolCallCount
	metadata.Stats.TokensIn, metadata.Stats.TokensOut = parsed.Stats.TokensIn, parsed.Stats.TokensOut
	metadata.Stats.CachedReadTokens, metadata.Stats.ThoughtTokens = parsed.Stats.CachedReadTokens, parsed.Stats.ThoughtTokens
	metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, parsed.Diagnostics.Warnings...)
	return metadata, nil
}

func (a *CursorAdapter) ExtractMetadataFromTranscript(ctx context.Context, data []byte, original *UnifiedMetadata) (*UnifiedMetadata, error) {
	metadata, err := metadataForTranscriptExtraction(ctx, HarnessCursor, data, original)
	if err != nil {
		return nil, err
	}
	parsed := NewUnifiedMetadata()
	parsed.SessionID = original.SessionID
	start, end := parseCursorTranscriptMetadata(data, &parsed)
	if err := transcriptMetadataParsingError(HarnessCursor, &parsed); err != nil {
		return nil, err
	}
	if start == 0 {
		start = original.Timestamp.Start
	}
	if end == 0 {
		end = original.Timestamp.End
	}
	if end < start {
		end = start
	}
	metadata.Model = parsed.Model
	metadata.Timestamp.Start, metadata.Timestamp.End = start, end
	metadata.Stats.DurationMs = end - start
	metadata.Stats.TurnCount, metadata.Stats.ToolCallCount = parsed.Stats.TurnCount, parsed.Stats.ToolCallCount
	metadata.Stats.TokensIn, metadata.Stats.TokensOut = parsed.Stats.TokensIn, parsed.Stats.TokensOut
	metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, parsed.Diagnostics.Warnings...)
	return metadata, nil
}

func transcriptMetadataParsingError(harness Harness, parsed *UnifiedMetadata) error {
	for _, warning := range parsed.Diagnostics.Warnings {
		if warning.ErrorType == "parse_error" || warning.ErrorType == "read_error" {
			return &InsufficientRetainedInputError{SessionID: parsed.SessionID, Harness: harness, Reason: warning.Message}
		}
	}
	return nil
}
