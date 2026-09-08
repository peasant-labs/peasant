package ingest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// RetainedInputReplayer rebuilds the adapter-owned fields represented in retained
// transcript bytes, preserving the original metadata's native-only context.
// Implementations refuse input that cannot support their current behavior.
type RetainedInputReplayer interface {
	ReplayRetained(context.Context, []byte, *UnifiedMetadata) (*UnifiedMetadata, []byte, error)
}

// InsufficientRetainedInputError means adapter refresh requires native input.
// It does not prevent a supported indexer from reading the last-good artifact.
type InsufficientRetainedInputError struct {
	SessionID SessionID
	Harness   Harness
	Reason    string
}

func (e *InsufficientRetainedInputError) Error() string {
	return fmt.Sprintf("replay retained %s session %s during adapter refresh: %s; previous artifacts and adapter provenance were preserved; restore the native source and retry harvest", e.Harness, e.SessionID, e.Reason)
}

var _ RetainedInputReplayer = (*ClaudeAdapter)(nil)
var _ RetainedInputReplayer = (*CodexAdapter)(nil)
var _ RetainedInputReplayer = (*CursorAdapter)(nil)

func retainedReplayMetadata(ctx context.Context, harness Harness, data []byte, original *UnifiedMetadata) (*UnifiedMetadata, error) {
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
		return nil, insufficient("the retained harness, format or metadata schema does not support JSONL replay")
	}
	if _, err := NewHostSlug(string(original.HostSlug)); err != nil {
		return nil, insufficient("the original managed project locator is missing or invalid")
	}
	// Validate the captured records before accepting the adapters' historically
	// tolerant metadata scans as a successful refresh.
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, defaults.ScannerInitBuf), defaults.ScannerMaxLine)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) != 0 && (!json.Valid(raw) || raw[0] != '{') {
			return nil, insufficient("a retained JSONL record is malformed or is not an object")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, insufficient("retained JSONL could not be completely scanned: " + err.Error())
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

func (a *ClaudeAdapter) ReplayRetained(ctx context.Context, data []byte, original *UnifiedMetadata) (*UnifiedMetadata, []byte, error) {
	metadata, err := retainedReplayMetadata(ctx, HarnessClaudeCode, data, original)
	if err != nil {
		return nil, nil, err
	}
	parsed := NewUnifiedMetadata()
	parsed.SessionID = original.SessionID
	if first := parseClaudeTranscriptMetadata(data, &parsed); first == nil || first.Type == "" {
		return nil, nil, &InsufficientRetainedInputError{SessionID: original.SessionID, Harness: HarnessClaudeCode, Reason: "retained input has no identifiable Claude record"}
	}
	if err := retainedReplayParsingError(HarnessClaudeCode, &parsed); err != nil {
		return nil, nil, err
	}
	metadata.Model, metadata.Version = parsed.Model, parsed.Version
	metadata.Timestamp.Start, metadata.Timestamp.End = parsed.Timestamp.Start, parsed.Timestamp.End
	metadata.Stats.DurationMs = parsed.Stats.DurationMs
	metadata.Stats.TurnCount, metadata.Stats.ToolCallCount = parsed.Stats.TurnCount, parsed.Stats.ToolCallCount
	metadata.Stats.TokensIn, metadata.Stats.TokensOut = parsed.Stats.TokensIn, parsed.Stats.TokensOut
	metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, parsed.Diagnostics.Warnings...)
	return metadata, bytes.Clone(data), nil
}

func (a *CodexAdapter) ReplayRetained(ctx context.Context, data []byte, original *UnifiedMetadata) (*UnifiedMetadata, []byte, error) {
	metadata, err := retainedReplayMetadata(ctx, HarnessCodex, data, original)
	if err != nil {
		return nil, nil, err
	}
	parsed := NewUnifiedMetadata()
	parsed.SessionID = original.SessionID
	sessionMeta, err := parseCodexTranscriptMetadata(ctx, data, &parsed)
	if err != nil {
		return nil, nil, err
	}
	if sessionMeta == nil {
		return nil, nil, &InsufficientRetainedInputError{SessionID: original.SessionID, Harness: HarnessCodex, Reason: "retained input has no usable session_meta record"}
	}
	if err := retainedReplayParsingError(HarnessCodex, &parsed); err != nil {
		return nil, nil, err
	}
	metadata.Model, metadata.Version = parsed.Model, parsed.Version
	metadata.Timestamp.Start, metadata.Timestamp.End = parsed.Timestamp.Start, parsed.Timestamp.End
	metadata.Stats.DurationMs = parsed.Stats.DurationMs
	metadata.Stats.TurnCount, metadata.Stats.ToolCallCount = parsed.Stats.TurnCount, parsed.Stats.ToolCallCount
	metadata.Stats.TokensIn, metadata.Stats.TokensOut = parsed.Stats.TokensIn, parsed.Stats.TokensOut
	metadata.Stats.CachedReadTokens, metadata.Stats.ThoughtTokens = parsed.Stats.CachedReadTokens, parsed.Stats.ThoughtTokens
	metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, parsed.Diagnostics.Warnings...)
	return metadata, bytes.Clone(data), nil
}

func (a *CursorAdapter) ReplayRetained(ctx context.Context, data []byte, original *UnifiedMetadata) (*UnifiedMetadata, []byte, error) {
	metadata, err := retainedReplayMetadata(ctx, HarnessCursor, data, original)
	if err != nil {
		return nil, nil, err
	}
	parsed := NewUnifiedMetadata()
	parsed.SessionID = original.SessionID
	start, end := parseCursorTranscriptMetadata(data, &parsed)
	if err := retainedReplayParsingError(HarnessCursor, &parsed); err != nil {
		return nil, nil, err
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
	return metadata, bytes.Clone(data), nil
}

func retainedReplayParsingError(harness Harness, parsed *UnifiedMetadata) error {
	for _, warning := range parsed.Diagnostics.Warnings {
		if warning.ErrorType == "parse_error" || warning.ErrorType == "read_error" {
			return &InsufficientRetainedInputError{SessionID: parsed.SessionID, Harness: harness, Reason: warning.Message}
		}
	}
	return nil
}
