package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// CodexIndexer parses OpenAI Codex CLI JSONL rollouts into SessionEntry rows.
//
// Each rollout line carries {timestamp, type, payload}. The indexer emits one
// SessionEntry per response_item it recognises (messages, function/tool calls,
// reasoning); session_meta, turn_context, and event_msg lines are
// handled by CodexAdapter.ExtractMetadata and skipped here.
//
// Unlike Claude (where one assistant message embeds an array of content
// blocks that decompose into depth=1 child entries), Codex response_items are
// already atomic — one event = one logical action. We therefore emit depth=0
// entries directly; there is no depth=1 decomposition pass.
type CodexIndexer struct {
	fs             FileSystem
	fullContent    bool
	historyCapture bool
	// provenanceCapture wires the native provenance candidate path. It stays
	// disabled until the maintenance activation composes it with a real
	// generation identity; while disabled every retained V1 flow is untouched.
	provenanceCapture CodexProvenanceIndexerConfig
	// pointerStore is the injected native current-rollout pointer store for
	// the capture path. Nil means the capture derives the native state
	// database from the session's sessions tree.
	pointerStore CodexNativePointerStore
	// maxRecordBytes is the per-record read limit. Zero means the
	// production limit; a test injects a small one so it can prove the
	// over-limit path without building a record of production size.
	maxRecordBytes int
}

// CodexIndexerOption configures a CodexIndexer.
type CodexIndexerOption func(*CodexIndexer)

// WithCodexFullContent disables ContentPreview truncation. Tool input and output
// remain complete independently of preview mode.
func WithCodexFullContent(enabled bool) CodexIndexerOption {
	return func(idx *CodexIndexer) { idx.fullContent = enabled }
}

// WithCodexHistoryCapture enables the read-only current-history capture
// path for IndexTranscriptResult: authority selection, bounded capture and
// fingerprint recheck before entry parsing. It stays disabled by default so
// existing file indexing keeps its exact retained behavior until the native
// repair path enables it explicitly. Enabling it also refuses incomplete
// captures (missing reference proof, unsupported mode, unstable source) so
// the last good entries are retained instead of replaced.
func WithCodexHistoryCapture(enabled bool) CodexIndexerOption {
	return func(idx *CodexIndexer) { idx.historyCapture = enabled }
}

// WithCodexCaptureNativePointerStore injects the native current-rollout pointer
// store used by the capture path. It has no effect unless the history capture
// is enabled. A production caller leaves it unset so the capture derives the
// native state database from the session's sessions tree.
func WithCodexCaptureNativePointerStore(store CodexNativePointerStore) CodexIndexerOption {
	return func(idx *CodexIndexer) { idx.pointerStore = store }
}

// WithCodexProvenanceCapture enables the native provenance candidate path with
// its injected prior-state, generation-identity and reference-allocator
// dependencies. A caller enables it only when it can assign a real generation
// identity and persist the validated V2 candidate; while disabled every
// retained V1 flow is untouched.
func WithCodexProvenanceCapture(config CodexProvenanceIndexerConfig) CodexIndexerOption {
	return func(idx *CodexIndexer) { idx.provenanceCapture = config }
}

// WithCodexMaxRecordBytes sets the per-record read limit. Zero keeps the
// production limit defaults.MaxJSONLRecordBytes. Passing the limit here
// keeps it out of any global, so tests that inject a small one stay safe
// to run in parallel.
func WithCodexMaxRecordBytes(limit int) CodexIndexerOption {
	return func(idx *CodexIndexer) { idx.maxRecordBytes = limit }
}

var _ TranscriptIndexer = (*CodexIndexer)(nil)
var _ VersionedTranscriptIndexer = (*CodexIndexer)(nil)

// IndexTranscriptResult verifies completion before authorizing persistent replacement.
func (idx *CodexIndexer) IndexTranscriptResult(ctx context.Context, session DiscoveredSession) (indexformat.Result, error) {
	// The native provenance candidate exit owns a safe refusal boundary of its
	// own: it never goes through the path-bearing V1 completion wrapper, so a
	// capture, classifier, projection, or dependency failure cannot surface the
	// private source locator. It stays disabled until the maintenance activation
	// composes it with a real generation identity; while disabled the retained
	// entry parsing below keeps its exact behavior.
	if idx.provenanceCapture.Enabled {
		return idx.indexCodexCandidateResult(ctx, session)
	}
	completion := &indexCompletion{ctx: ctx, session: session}
	if err := ctx.Err(); err != nil {
		return nil, completion.failure(err)
	}
	data, identified, err := idx.readCurrentSource(ctx, session)
	if err != nil {
		return nil, completion.failure(err)
	}
	return idx.IndexTranscriptBytesResult(ctx, identified, data)
}

// indexCodexCandidateResult is the registered Codex candidate exit. On success
// it returns the validated V2 generation the maintenance activation consumes.
// On refusal it returns a fixed completion diagnostic that names the validated
// harness and session identity plus the fixed operation, reason, effect, and
// recovery categories; the native source locator and the underlying failure are
// never carried, even when a capture or dependency error is path-bearing.
func (idx *CodexIndexer) indexCodexCandidateResult(ctx context.Context, session DiscoveredSession) (indexformat.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, codexCandidateCompletionFailure(session, err)
	}
	candidate, err := idx.IndexCodexCandidateV2(ctx, session)
	if err != nil {
		return nil, codexCandidateCompletionFailure(session, err)
	}
	return candidate, nil
}

// codexCandidateCompletionRefusal is the safe completion diagnostic for the
// registered Codex candidate exit. It carries a validated harness and session
// identity beside the fixed operation, reason, effect, and recovery categories
// and never contains the native source locator, a raw validator value, or a
// wrapped dependency, capture, classifier, or filesystem error.
type codexCandidateCompletionRefusal struct {
	Operation string
	Harness   string
	SessionID string
	Reason    string
	Effect    string
	Recovery  string
}

func (e *codexCandidateCompletionRefusal) Error() string {
	return fmt.Sprintf("ingest.%s: harness %s session %s could not emit a native Codex candidate because %s; %s; %s", e.Operation, e.Harness, e.SessionID, e.Reason, e.Effect, e.Recovery)
}

// codexCandidateCompletionFailure translates a provenance candidate failure
// into the safe completion diagnostic. A typed candidate refusal keeps its
// fixed categories; every other failure receives the fixed capture category.
// The underlying failure is classified here and then dropped, so nothing it
// carries can reach the diagnostic.
func codexCandidateCompletionFailure(session DiscoveredSession, failure error) error {
	refusal := &codexCandidateCompletionRefusal{
		Operation: "CodexIndexer.IndexTranscriptResult",
		Harness:   string(HarnessCodex),
		SessionID: validatedCompletionSessionReference(session),
		Reason:    "the native Codex source could not be read as a complete candidate",
		Effect:    "no candidate was emitted and the retained generation and producer stamps are unchanged",
		Recovery:  "restore the native Codex source and retry harvest",
	}
	var candidateRefusal *codexCandidateRefusal
	if errors.As(failure, &candidateRefusal) {
		if candidateRefusal.Operation != "" {
			refusal.Operation = candidateRefusal.Operation
		}
		if candidateRefusal.Reason != "" {
			refusal.Reason = candidateRefusal.Reason
		}
		if candidateRefusal.Effect != "" {
			refusal.Effect = candidateRefusal.Effect
		}
		if candidateRefusal.Recovery != "" {
			refusal.Recovery = candidateRefusal.Recovery
		}
	}
	return refusal
}

// validatedCompletionSessionReference returns the session identity only when it
// is a canonical identifier. An absent or malformed identity reports itself as
// unidentified rather than echoing the raw value, which may be a private path.
func validatedCompletionSessionReference(session DiscoveredSession) string {
	identifier, err := NewSessionID(session.SessionID.String())
	if err != nil {
		return "unidentified"
	}
	return string(identifier)
}

// readCurrentSource returns the transcript bytes for the completion-gated
// path. Without the history capture it reads the file exactly as the
// retained file indexer always has: one read, no authority selection, no
// refusal. With the capture it verifies authority, bounds and stability
// first, then re-points the session at the stable native identity and
// authoritative current source the capture selected, so entry parsing
// consumes the same verified incarnation instead of the discovered path.
func (idx *CodexIndexer) readCurrentSource(ctx context.Context, session DiscoveredSession) ([]byte, DiscoveredSession, error) {
	if !idx.historyCapture {
		data, err := idx.fs.ReadFile(session.SourcePath.String())
		if err != nil {
			return nil, session, err
		}
		return data, session, nil
	}
	return idx.captureCurrentSource(ctx, session)
}

// captureCurrentSource reads the authoritative current Codex source through the
// bounded capture, authority and fingerprint-recheck path, and returns the
// verified decoded prefix together with the discovered session re-identified by
// the stable native id and current pointer. A capture without a stable id keeps
// the discovered identity.
func (idx *CodexIndexer) captureCurrentSource(ctx context.Context, session DiscoveredSession) ([]byte, DiscoveredSession, error) {
	var options []CodexFileSourceOption
	if idx.pointerStore != nil {
		options = append(options, WithCodexNativePointerStore(idx.pointerStore))
	}
	history, err := CaptureCodexHistoryWithRetry(ctx, NewCodexFileSource(idx.fs, options...), session, nil)
	if err != nil {
		return nil, session, err
	}
	if history.Completeness != indexformat.GenerationCompletenessComplete {
		return nil, session, &CodexIncompleteCaptureError{
			StableThreadID: history.StableThreadID,
			Completeness:   history.Completeness,
			Diagnostics:    history.Diagnostics,
		}
	}
	identified := session
	if stableID, idErr := NewSessionID(history.StableThreadID); idErr == nil && history.StableThreadID != "" {
		identified.SessionID = stableID
	}
	if history.Pointer != "" {
		identified.SourcePath = ResolvedPath(history.Pointer)
	}
	return history.RawBytes, identified, nil
}

// IndexTranscriptBytesResult consumes precisely the supplied transcript snapshot.
func (idx *CodexIndexer) IndexTranscriptBytesResult(ctx context.Context, session DiscoveredSession, data []byte) (indexformat.Result, error) {
	// The native provenance candidate cannot be built from the supplied bytes
	// alone: it needs the captured native node graph, ordered segments, proven
	// correlations and the verified source authority. The candidate path
	// therefore re-runs the bounded read-only capture over the same session
	// instead of treating one file read as a complete native graph.
	if idx.provenanceCapture.Enabled {
		return idx.IndexTranscriptResult(ctx, session)
	}
	completion := &indexCompletion{ctx: ctx, session: session}
	entries, err := idx.parseRolloutWithCompletion(session.SessionID, data, completion)
	return completion.result(entries, err)
}

// SourceKind reports that Codex's entries come from a single rollout JSONL file; every entry is in its bytes.
func (idx *CodexIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }

// NewCodexIndexer constructs a CodexIndexer with an injected FileSystem.
func NewCodexIndexer(fs FileSystem, opts ...CodexIndexerOption) *CodexIndexer {
	idx := &CodexIndexer{fs: fs}
	for _, opt := range opts {
		opt(idx)
	}
	return idx
}

// IndexTranscript reads a Codex rollout from disk and returns SessionEntry
// rows. Malformed lines are skipped (not fatal); empty rollouts return nil.
func (idx *CodexIndexer) IndexTranscript(_ context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	data, err := idx.fs.ReadFile(string(session.SourcePath))
	if err != nil {
		return nil, fmt.Errorf("codex indexer: read %s: %w", session.SourcePath, err)
	}
	return idx.parseRollout(session.SessionID, data)
}

// IndexTranscriptBytes parses a Codex rollout from in-memory bytes. Used by
// the parallel ingest path to avoid re-reading the file from disk after the
// EXTRACT+WRITE stage.
func (idx *CodexIndexer) IndexTranscriptBytes(_ context.Context, session DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	return idx.parseRollout(session.SessionID, data)
}

// parseRollout is the shared kernel for both Index methods. It scans the JSONL,
// dispatches each envelope on its `type` (and the nested `payload.type` for
// response_item / event_msg), and produces SessionEntry rows.
func (idx *CodexIndexer) parseRollout(sessionID SessionID, data []byte) ([]schema.SessionEntry, error) {
	return idx.parseRolloutWithCompletion(sessionID, data, nil)
}

func (idx *CodexIndexer) parseRolloutWithCompletion(sessionID SessionID, data []byte, completion *indexCompletion) ([]schema.SessionEntry, error) {
	scanner := newJSONLRecordScanner(data, productionJSONLRecordLimit(idx.maxRecordBytes))

	var entries []schema.SessionEntry
	entryIndex := 0
	var traversalPosition int64
	previousLine := 0

	for scanner.Scan() {
		var placeholderErr error
		entries, placeholderErr = appendOmissionPlaceholders(entries, scanner, sessionID, HarnessCodex, &entryIndex)
		if placeholderErr != nil {
			return entries, placeholderErr
		}
		if completion != nil {
			completion.line = scanner.Line()
		}
		raw := scanner.Bytes()
		trimmed := bytes.TrimSpace(raw)
		traversalPosition += int64(max(0, scanner.Line()-previousLine-1))
		previousLine = scanner.Line()
		if len(trimmed) == 0 {
			traversalPosition++
			continue
		}
		position := UnknownSourcePosition{Line: scanner.Line(), Public: codexPublicPosition(sessionID.String(), scanner.Line(), traversalPosition)}
		traversalPosition += int64(len(codexTraversalPointers(trimmed)))
		prepared, unknown, prepareErr := prepareCodexRecord(trimmed, position, false)
		if prepareErr != nil {
			if completion == nil && !json.Valid(trimmed) {
				continue
			}
			return nil, prepareErr
		}
		if prepared == nil {
			for _, evidence := range unknown {
				entry, err := RetainedUnknownEntry(sessionID, entryIndex, evidence)
				if err != nil {
					return nil, err
				}
				entries = append(entries, entry)
				entryIndex++
			}
			continue
		}
		trimmed = prepared

		var env codexRolloutLine
		if completion != nil {
			if err := completion.record(trimmed); err != nil {
				return nil, err
			}
		}
		if err := json.Unmarshal(trimmed, &env); err != nil {
			if completion != nil {
				return nil, err
			}
			continue // malformed line — skip silently
		}
		if completion != nil {
			if env.Type == "" {
				return nil, fmt.Errorf("rollout record lacks its type")
			}
			if err := requireJSONObject(env.Payload); err != nil {
				return nil, fmt.Errorf("rollout payload: %w", err)
			}
			switch env.Type {
			case codexTypeSessionMeta, codexTypeTurnContext, codexTypeEventMsg:
				completion.recognized++
			}
		}

		// Only response_item lines become indexed entries. session_meta /
		// turn_context drive ExtractMetadata; event_msg.{token_count,
		// task_*} are session-level signals or UI mirrors of response_item
		// content and would double-count if indexed.
		if env.Type != codexTypeResponse {
			continue
		}
		if completion != nil {
			var header struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(env.Payload, &header); err != nil {
				return nil, err
			}
			if header.Type == "" {
				return nil, fmt.Errorf("response item lacks its type")
			}
			switch header.Type {
			case codexResponseMessage, codexResponseReasoning, codexResponseFunctionCall, codexResponseCustomCall, codexResponseFunctionOut, codexResponseCustomCallOut:
			default:
				// Future variants are opaque: fields resembling a known variant
				// need not have that variant's shape. They cannot alone prove completion.
				continue
			}
		}

		var payload codexResponseItemPayload
		decodeErr := json.Unmarshal(env.Payload, &payload)
		if completion != nil {
			if decodeErr != nil {
				return nil, decodeErr
			}
			if payload.Type == "" {
				return nil, fmt.Errorf("response item lacks its type")
			}
		}
		entry, ok := codexResponseItemEntry(sessionID, entryIndex, env, len(trimmed), idx.fullContent, payload, decodeErr)
		if !ok {
			continue
		}
		if err := AttachRetainedUnknown(&entry, unknown); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		if completion != nil {
			completion.recognized++
		}
		entryIndex++
	}

	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("codex indexer: scanner error for %s: %w", sessionID, err)
	}
	if completion != nil && scanner.SawOversized() {
		return entries, uncertifiableOversizedRecord(scanner.Oversized(), scanner.limit)
	}
	entries, err := appendOmissionPlaceholders(entries, scanner, sessionID, HarnessCodex, &entryIndex)
	if err != nil {
		return entries, err
	}
	if completion != nil {
		completion.line = scanner.Line()
	}
	return entries, nil
}

// codexResponseItemPayload is the union of fields the indexer reads from any
// response_item payload. Discriminated by Type; unrelated fields stay zero.
type codexResponseItemPayload struct {
	Type    string                `json:"type"`
	Role    string                `json:"role,omitempty"`
	Content []codexMessageContent `json:"content,omitempty"` // message variant
	Summary []codexMessageContent `json:"summary,omitempty"`
	Name    string                `json:"name,omitempty"`    // function_call / custom_tool_call
	CallID  string                `json:"call_id,omitempty"` // function_call(_output) / custom_tool_call(_output)
	// Arguments is a JSON string for function_call (e.g., `{"cmd":"pwd"}`),
	// or a raw string for custom_tool_call's `input` field. We read both as
	// json.RawMessage so we can preserve the original form for ToolInput.
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"` // function_call_output / custom_tool_call_output
}

// codexMessageContent is one element of response_item.message.content[]. Codex
// uses {"type":"input_text","text":...} for user/developer messages and
// {"type":"output_text","text":...} for assistant messages.
type codexMessageContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// codexResponseItemEntry converts one decoded response_item envelope into a
// SessionEntry. Returns (zero, false) for response_item variants the indexer
// does not surface (e.g. unknown future payload types).
func codexResponseItemEntry(sessionID SessionID, index int, env codexRolloutLine, rawLen int, fullContent bool, p codexResponseItemPayload, decodeErr error) (schema.SessionEntry, bool) {
	if decodeErr != nil {
		return schema.SessionEntry{}, false
	}

	entry := schema.SessionEntry{
		SessionID:     sessionID,
		EntryIndex:    index,
		Harness:       HarnessCodex,
		Depth:         0,
		RawByteLength: &rawLen,
	}

	if ms := parseTimestampMillis(env.Timestamp); ms > 0 {
		entry.TimestampMs = &ms
	}

	switch p.Type {
	case codexResponseMessage:
		entry.EntryType = EntryTypeText
		switch CodexRole(p.Role) {
		case codexRoleAssistant:
			entry.Role = RoleAssistant
		case codexRoleUser:
			entry.Role = RoleUser
		case codexRoleDeveloper:
			// Developer-role messages are Codex's system-injected prompts
			// (permissions instructions, AGENTS.md context, etc.). Match
			// Claude's treatment of system-injected user content.
			entry.Role = RoleSystem
			entry.EntryType = EntryTypeSystem
		default:
			entry.Role = RoleSystem
			entry.EntryType = EntryTypeSystem
		}
		if text := joinCodexContentText(p.Content); text != "" {
			preview := text
			if !fullContent {
				preview = truncateString(preview, defaults.ContentPreviewLimit)
			}
			entry.ContentPreview = &preview
		}

	case codexResponseReasoning:
		// Reasoning content is encrypted/opaque; record that thinking
		// happened without storing the body, mirroring how Claude indexes
		// `thinking` blocks when content is hidden.
		entry.EntryType = EntryTypeThinking
		entry.Role = RoleAssistant
		entry.HasThinking = true
		text := joinCodexContentText(p.Summary)
		if body := joinCodexContentText(p.Content); body != "" {
			if text != "" {
				text += "\n"
			}
			text += body
		}
		if text != "" {
			if !fullContent {
				text = truncateString(text, defaults.ContentPreviewLimit)
			}
			entry.ContentPreview = &text
		}

	case codexResponseFunctionCall, codexResponseCustomCall:
		entry.EntryType = EntryTypeToolUse
		entry.Role = RoleAssistant
		entry.HasToolUse = true
		if p.Name != "" {
			name := p.Name
			entry.ToolNamesCSV = &name
			if kind := classifyToolKind(p.Name); kind != "" {
				entry.ToolKind = &kind
			}
		}
		if p.CallID != "" {
			callID := p.CallID
			entry.ToolCallID = &callID
		}
		// function_call carries arguments as a JSON string; custom_tool_call
		// carries `input` as a raw string. Either way, preserve the source
		// bytes verbatim as ToolInput.
		switch {
		case len(p.Arguments) > 0:
			entry.ToolInput = codexRawJSONToString(p.Arguments)
		case len(p.Input) > 0:
			entry.ToolInput = codexRawJSONToString(p.Input)
		}

	case codexResponseFunctionOut, codexResponseCustomCallOut:
		entry.EntryType = EntryTypeToolResult
		entry.Role = RoleTool
		if p.CallID != "" {
			callID := p.CallID
			entry.ToolCallID = &callID
		}
		if len(p.Output) > 0 {
			out := codexRawJSONToString(p.Output)
			if out != nil {
				// Derive the compatibility preview without shortening the
				// semantic tool output consumed by metrics and annotations.
				preview := *out
				if !fullContent {
					preview = truncateString(preview, defaults.ContentPreviewLimit)
				}
				entry.ToolOutput = out
				entry.ContentPreview = &preview
			}
		}

	default:
		// Unknown response_item type (likely a Codex CLI version drift).
		// Skip rather than emit a half-classified row.
		return schema.SessionEntry{}, false
	}

	return entry, true
}

// joinCodexContentText flattens response_item.message.content[] text blocks
// into a single string. Codex uses input_text for user/developer messages and
// output_text for assistant messages; we treat both as plain text.
func joinCodexContentText(blocks []codexMessageContent) string {
	if len(blocks) == 0 {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// codexRawJSONToString unwraps a json.RawMessage to a string. If the raw bytes
// are a JSON string literal, the unquoted string is returned. Otherwise the
// raw bytes are returned as-is (so structured tool outputs survive). Returns
// nil for empty/null input so callers can distinguish "absent" from "empty".
func codexRawJSONToString(raw json.RawMessage) *string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return &s
	}
	out := string(raw)
	return &out
}
