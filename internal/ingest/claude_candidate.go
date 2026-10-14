package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// ClaudeCode's managed-generation producer. It composes the retained identity
// kernel and the stored metadata extraction over the attempt's captured bytes
// and recorded metadata, classifies the captured records into the shared
// projection's blocks, and emits a genuine format-2 candidate. No provider
// source is reopened on the ordinary bytes-proven branch: every byte the
// classifier reads comes from the capture, and only the legacy missing-proof
// fallback consults structural sibling snapshots through the same accessor the
// stored adapter uses.

var _ GenerationBuilder = (*ClaudeIndexer)(nil)

// claudeCandidateRefusal is the fixed-shape, sanitized refusal for the Claude
// candidate boundary. It names the harness and session identity plus a fixed
// operation, reason, caller effect and recovery, and never carries a native
// locator, a raw kernel message, or a wrapped dependency error, so a refusal
// can be logged or surfaced without leaking private locations.
type claudeCandidateRefusal struct {
	Operation string
	Harness   string
	SessionID string
	Reason    string
	Effect    string
	Recovery  string
}

func (e *claudeCandidateRefusal) Error() string {
	return fmt.Sprintf("ingest.%s: harness %s session %s could not emit a managed Claude generation because %s; %s; %s", e.Operation, e.Harness, e.SessionID, e.Reason, e.Effect, e.Recovery)
}

// claudeCandidateCompletionFailure translates any candidate-build failure into
// the one fixed-shape Claude refusal. The underlying failure is not carried, so
// nothing it holds -- a recorded source locator, a raw kernel message, or a
// wrapped filesystem error -- can reach the diagnostic.
func claudeCandidateCompletionFailure(session DiscoveredSession) error {
	return &claudeCandidateRefusal{
		Operation: "ClaudeIndexer.BuildGeneration",
		Harness:   string(HarnessClaudeCode),
		SessionID: validatedCompletionSessionReference(session),
		Reason:    "the recorded Claude transcript could not be proven as a complete managed generation: its captured bytes, recorded identity, or record structure did not support one",
		Effect:    "no candidate was emitted and the retained generation and producer stamps are unchanged",
		Recovery:  "restore the recorded Claude transcript and rerun harvest",
	}
}

// BuildGeneration runs the production Claude candidate exit: retained identity
// proof over the captured bytes, stored metadata extraction, strict record
// validation, tolerant classification, and the shared validated generation
// projection. It returns the validated format-2 candidate together with the
// full captured bytes its content records address, so activation can stage a
// self-contained generation without reopening the mutable source. It enables
// no format target itself.
//
// Every failure -- an identity-kernel refusal, a malformed record, an absent
// capture, or a missing recorded agreement -- leaves through the one fixed
// sanitized refusal above, never a raw kernel value or the recorded locator.
func (idx *ClaudeIndexer) BuildGeneration(ctx context.Context, in GenerationBuildInput) (GenerationCandidate, error) {
	if err := ctx.Err(); err != nil {
		return GenerationCandidate{}, claudeCandidateCompletionFailure(in.Session)
	}
	candidate, err := idx.buildClaudeGeneration(ctx, in)
	if err != nil {
		return GenerationCandidate{}, claudeCandidateCompletionFailure(in.Session)
	}
	return candidate, nil
}

// buildClaudeGeneration composes the Claude candidate from the attempt's
// captured bytes and recorded metadata. It never consults the stored-capture
// authority: a Claude candidate is proven from the captured pair, and the prior
// only supplies the reusable alias state a refresh must retain.
func (idx *ClaudeIndexer) buildClaudeGeneration(ctx context.Context, in GenerationBuildInput) (GenerationCandidate, error) {
	if in.GenerationID == "" {
		return GenerationCandidate{}, fmt.Errorf("ingest.ClaudeIndexer.BuildGeneration: no generation identity was supplied for session %s; activation owns generation addressing and the projection invents none; wire the generation identity before requesting a candidate", in.Session.SessionID)
	}
	if in.CapturedTranscript == nil {
		return GenerationCandidate{}, fmt.Errorf("ingest.ClaudeIndexer.BuildGeneration: no captured transcript was supplied for session %s; a managed candidate indexes exactly the verified capture; capture the session bytes before requesting a candidate", in.Session.SessionID)
	}
	if in.RecordedMetadata == nil {
		return GenerationCandidate{}, fmt.Errorf("ingest.ClaudeIndexer.BuildGeneration: no recorded metadata agreement was supplied for session %s; the candidate cannot be indexed against an identity; provide the committed metadata", in.Session.SessionID)
	}
	data := in.CapturedTranscript

	metadata, err := metadataForTranscriptExtraction(ctx, HarnessClaudeCode, data, in.RecordedMetadata)
	if err != nil {
		return GenerationCandidate{}, err
	}
	allowed, relationships, err := verifyRetainedClaudeIdentityWith(data, in.RecordedMetadata, metadata.ContentHash, claudeIdentitySnapshotFrom(idx.fs))
	if err != nil {
		return GenerationCandidate{}, err
	}
	parsed := NewUnifiedMetadata()
	parsed.SessionID = in.RecordedMetadata.SessionID
	if _, err := parseClaudeTranscriptMetadataWithIdentity(data, &parsed, allowed); err != nil {
		return GenerationCandidate{}, err
	}
	if err := transcriptMetadataParsingError(HarnessClaudeCode, &parsed); err != nil {
		return GenerationCandidate{}, err
	}
	if _, err := validateClaudeProducerRecords(ctx, in.Session, data); err != nil {
		return GenerationCandidate{}, err
	}

	// The tolerant classification parse mirrors the retained harvest parse:
	// completion-less, full-depth, full-content, retaining unknown evidence, so
	// the accounted omissions and unrepresented kinds the strict validator
	// tolerated become placeholders and opaque carriers rather than gaps.
	parse := *idx
	parse.fullContent = true
	parse.fullDepth = true
	parse.retainUnknown = true
	entries, err := parse.parseJSONL(in.Session.SessionID, data)
	if err != nil {
		return GenerationCandidate{}, err
	}
	blocks, err := claudeClassifiedBlocks(in.Session.SessionID, entries)
	if err != nil {
		return GenerationCandidate{}, err
	}

	metadata.SessionID = in.Session.SessionID
	metadata.ModelHarness = HarnessClaudeCode
	metadata.Model = parsed.Model
	metadata.Version = parsed.Version
	metadata.Relationships = relationships
	// Relationships come from the proof, never a blind metadata copy; the root
	// stays unset because the captured artifact carries no native root evidence.
	metadata.RootSessionID = nil
	metadata.Timestamp.Start, metadata.Timestamp.End = parsed.Timestamp.Start, parsed.Timestamp.End
	metadata.Stats.DurationMs = parsed.Stats.DurationMs
	metadata.Stats.TurnCount, metadata.Stats.ToolCallCount = parsed.Stats.TurnCount, parsed.Stats.ToolCallCount
	metadata.Stats.TokensIn, metadata.Stats.TokensOut = parsed.Stats.TokensIn, parsed.Stats.TokensOut
	metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, parsed.Diagnostics.Warnings...)

	priorState := NewProjectionPriorState()
	if in.Prior != nil {
		prior, priorErr := in.Prior.ReadGenerationPrior(ctx)
		if priorErr != nil {
			return GenerationCandidate{}, priorErr
		}
		priorState = generationPriorState(prior)
	}
	allocator := in.Allocator
	if allocator == nil {
		allocator = RandomRefAllocator{}
	}
	capture := ClassifiedCapture{
		ID:                   in.GenerationID,
		SessionID:            in.Session.SessionID,
		Harness:              HarnessClaudeCode,
		Metadata:             *metadata,
		SourceEvidenceDigest: metadata.ContentHash,
		Completeness:         indexformat.GenerationCompletenessComplete,
		Blocks:               blocks,
		Prior:                priorState,
	}
	result, content, err := BuildV2WithContent(capture, allocator)
	if err != nil {
		return GenerationCandidate{}, err
	}
	return GenerationCandidate{Result: result, Blobs: content}, nil
}

// validateClaudeProducerRecords runs the shared strict Claude record validator
// over the captured bytes for the managed producer. It intercepts the
// positional omission stand-ins exactly as the stored capture exit does, but
// it never refuses a capture that carries the source-omitted flag without a
// stand-in: the flag is the disposition input the shared assessment reads, not
// a validation failure.
func validateClaudeProducerRecords(ctx context.Context, session DiscoveredSession, data []byte) ([]IgnoredSourceRecord, error) {
	relaxed := session
	relaxed.ContentOmitted = false
	return validateRetainingJSONL(ctx, relaxed, data, claudeCaptureRecordValidator)
}

// claudeNativeKeyPrefix is the fixed provider prefix of every Claude record and
// block native key.
const claudeNativeKeyPrefix = "claude:"

// claudeRecordKey is the stable record identity one classified Claude source
// record carries: the fixed prefix, then the session id, then the uuid or the
// stream-ordinal anchor. The pos anchor is the record's stream ordinal, which
// is stable under append; a uuid anchor is stable forever. The key is bounded
// like every persisted alias key.
func claudeRecordKey(sessionID SessionID, uuid string, streamOrdinal int) (string, error) {
	if uuid != "" {
		return claudeNativeKey(sessionID.String(), uuid)
	}
	return claudeNativeKey(sessionID.String(), "pos", strconv.Itoa(streamOrdinal))
}

// claudeDuplicateRecordKey derives the prefix-stable duplicate discriminator
// for a repeated uuid. Occurrence one is unsuffixed forever; later occurrences
// take :dup2, :dup3, ... by occurrence counter, so a later append never re-keys
// an earlier record.
func claudeDuplicateRecordKey(base string, occurrence int) (string, error) {
	return claudeBoundKey(base + ":dup" + strconv.Itoa(occurrence))
}

// claudeBlockKey names one content block of a record by its ordinal among the
// record's classified blocks.
func claudeBlockKey(recordKey string, blockOrdinal int) (string, error) {
	return claudeBoundKey(recordKey + ":" + strconv.Itoa(blockOrdinal))
}

// claudeAnswerKey names one AskUserQuestion answer submission inside its record.
func claudeAnswerKey(recordKey, toolCallKey string) (string, error) {
	return claudeBoundKey(recordKey + ":answer:" + toolCallKey)
}

// claudeNativeKey builds one bounded Claude native key from its identity parts
// under the fixed provider prefix, so no raw private identity can leak past the
// persisted alias bound.
func claudeNativeKey(parts ...string) (string, error) {
	return claudeBoundKey(claudeNativeKeyPrefix + strings.Join(parts, ":"))
}

// claudeBoundKey enforces the shared alias-key bound on one assembled key.
func claudeBoundKey(key string) (string, error) {
	if len(key) > indexformat.MaxNativeAliasKeyBytes {
		return "", fmt.Errorf("ingest.claudeBoundKey: Claude native key is %d bytes, over the %d-byte bound; a raw private identity may be leaking into persisted identity; encode a shorter opaque native key", len(key), indexformat.MaxNativeAliasKeyBytes)
	}
	return key, nil
}

// claudeClassifiedBlocks converts the retained classification parse's entries
// into the shared projection's blocks, assigning every record and block its
// stable native key. The conversion reuses the exact V1 entry projection, so
// every field the projection's block channels can express keeps V1 parity;
// fields the block channels do not carry (the raw provider entry ids, the
// tool-result error flag, the stop reason) are not expressible in a managed
// generation and are not reconstructed.
func claudeClassifiedBlocks(sessionID SessionID, entries []schema.SessionEntry) ([]ClassifiedBlock, error) {
	type claudeRecord struct {
		carrier  schema.SessionEntry
		children []schema.SessionEntry
	}
	records := make(map[int]*claudeRecord)
	order := make([]int, 0, len(entries))
	for i := range entries {
		entry := entries[i]
		if entry.Depth != 0 {
			continue
		}
		records[entry.EntryIndex] = &claudeRecord{carrier: entry}
		order = append(order, entry.EntryIndex)
	}
	for i := range entries {
		entry := entries[i]
		if entry.Depth == 0 {
			continue
		}
		if entry.ParentIndex == nil {
			return nil, fmt.Errorf("ingest.claudeClassifiedBlocks: depth-1 entry %d has no parent index; the record boundary cannot be resolved; rebuild the classification", entry.EntryIndex)
		}
		record, ok := records[*entry.ParentIndex]
		if !ok {
			return nil, fmt.Errorf("ingest.claudeClassifiedBlocks: depth-1 entry %d names parent %d, which is not a classified record; the block has no carrier; rebuild the classification", entry.EntryIndex, *entry.ParentIndex)
		}
		record.children = append(record.children, entry)
	}

	seenUUID := make(map[string]int, len(order))
	blocks := make([]ClassifiedBlock, 0, len(entries))
	seenCalls := make(map[string]bool)
	for ordinal, entryIndex := range order {
		record := records[entryIndex]
		carrier := record.carrier
		uuid := claudeRecordUUID(carrier)
		base, err := claudeRecordKey(sessionID, uuid, ordinal)
		if err != nil {
			return nil, err
		}
		recordKey := base
		if uuid != "" {
			seenUUID[uuid]++
			if seenUUID[uuid] > 1 {
				recordKey, err = claudeDuplicateRecordKey(base, seenUUID[uuid])
				if err != nil {
					return nil, err
				}
			}
		}
		hasToolResultContent := false
		for _, child := range record.children {
			if child.EntryType == EntryTypeToolResult {
				hasToolResultContent = true
				break
			}
		}
		carrierBlock, err := claudeCarrierBlock(recordKey, carrier, hasToolResultContent)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, carrierBlock)
		for blockOrdinal, child := range record.children {
			childKey, err := claudeBlockKey(recordKey, blockOrdinal)
			if err != nil {
				return nil, err
			}
			block, err := claudeChildBlock(childKey, recordKey, child, seenCalls)
			if err != nil {
				return nil, err
			}
			if block.Depth == 1 {
				seenCalls[derefString(child.ToolCallID)] = true
			}
			blocks = append(blocks, block)
		}
	}
	return blocks, nil
}

// claudeCarrierBlock builds one depth-0 block from a classified record. An
// accounted omission takes the omission channel instead, which owns the
// placeholder's whole shape. hasToolResultContent is the record's content
// shape: a record whose content holds a tool result is a wrapper, never a
// plain prompt.
func claudeCarrierBlock(recordKey string, entry schema.SessionEntry, hasToolResultContent bool) (ClassifiedBlock, error) {
	block := ClassifiedBlock{
		NativeKey:   recordKey,
		Role:        entry.Role,
		EntryType:   entry.EntryType,
		Depth:       0,
		TimestampMs: entry.TimestampMs,
		PartType:    entry.PartType,
		HasThinking: entry.HasThinking,
		ToolName:    derefString(entry.ToolNamesCSV),
		ToolKind:    derefToolKind(entry.ToolKind),
	}
	if entry.TokensIn != nil || entry.TokensOut != nil {
		block.Usage = &ClassifiedUsage{TokensIn: entry.TokensIn, TokensOut: entry.TokensOut}
	}
	if omitted, ok := OmittedRecordOf(entry); ok {
		// The omission channel owns the placeholder's whole shape: role tool,
		// entry type tool_result, the typed record under Extra, the reader note,
		// the source tool id and the raw size. The record's own role and entry
		// type are not consulted.
		block.Role = RoleTool
		block.EntryType = EntryTypeToolResult
		block.PartType = nil
		block.Omitted = &OmittedRecordAt{Record: omitted, ToolCallID: entry.ToolCallID}
		return block, nil
	}
	retained, observedModel, extra, err := claudeEntryExtra(entry)
	if err != nil {
		return ClassifiedBlock{}, err
	}
	block.RetainedUnknown = retained
	block.ObservedModel = observedModel
	block.Extra = extra
	claudeAssignContent(&block, entry)
	if err := claudeAssignSubmission(&block, entry, !hasToolResultContent); err != nil {
		return ClassifiedBlock{}, err
	}
	return block, nil
}

// claudeChildBlock builds one depth-1 block for a content block of a record.
// A tool result whose call was never observed earlier in the stream cannot be
// paired, so it stays depth 0 with its own key and carries its source tool id
// on the direct channel instead of a carrier and a call key.
func claudeChildBlock(childKey, recordKey string, entry schema.SessionEntry, seenCalls map[string]bool) (ClassifiedBlock, error) {
	block := ClassifiedBlock{
		NativeKey:        childKey,
		CarrierNativeKey: recordKey,
		Role:             entry.Role,
		EntryType:        entry.EntryType,
		Depth:            1,
		PartType:         entry.PartType,
		ToolName:         derefString(entry.ToolNamesCSV),
		ToolKind:         derefToolKind(entry.ToolKind),
	}
	retained, observedModel, extra, err := claudeEntryExtra(entry)
	if err != nil {
		return ClassifiedBlock{}, err
	}
	block.RetainedUnknown = retained
	block.ObservedModel = observedModel
	block.Extra = extra
	claudeAssignContent(&block, entry)

	callKey := derefString(entry.ToolCallID)
	switch entry.EntryType {
	case EntryTypeToolUse:
		if callKey == "" {
			return ClassifiedBlock{}, fmt.Errorf("ingest.claudeChildBlock: tool_use block %q names no tool identity; a later result could not be paired; restore the source tool id", childKey)
		}
		block.ToolCallKey = callKey
	case EntryTypeToolResult:
		if callKey == "" || !seenCalls[callKey] {
			// The call was not observed as a tool_use before this result, so
			// the pair cannot be proven. The result stays depth 0 with its own
			// key, no carrier, no part label, and its source tool id on the
			// direct channel: stored evidence whose served projection is
			// unchanged.
			block.Depth = 0
			block.CarrierNativeKey = ""
			block.PartType = nil
			block.ToolCallID = entry.ToolCallID
			return block, nil
		}
		block.ToolCallKey = callKey
	}
	if entry.EntryType == EntryTypeToolResult {
		if err := claudeAssignSubmission(&block, entry, false); err != nil {
			return ClassifiedBlock{}, err
		}
	}
	return block, nil
}

// claudeAssignContent copies the retained entry's content into the projection's
// content channel for its entry type.
func claudeAssignContent(block *ClassifiedBlock, entry schema.SessionEntry) {
	switch entry.EntryType {
	case EntryTypeToolUse:
		block.ToolArguments = derefString(entry.ToolInput)
	case EntryTypeToolResult:
		block.ToolResult = derefString(entry.ToolOutput)
	default:
		block.Content = derefString(entry.ContentPreview)
	}
}

// claudeAssignSubmission sets the six-dimension provenance and the acceptance
// key for the entries that are genuine user input. A depth-0 plain prompt (a
// user record whose content holds no tool result) is its own submission, keyed
// by the discriminating record identity. An AskUserQuestion answer -- a
// depth-1 user tool_result with a validated tool_use id -- is its own
// submission, keyed per record and answer. Mixed wrappers, harness content and
// tool activity carry no provenance. An over-bound answer key is refused, never
// dropped: every key this classifier assembles respects the persisted bound.
func claudeAssignSubmission(block *ClassifiedBlock, entry schema.SessionEntry, plainPrompt bool) error {
	if plainPrompt && entry.Role == RoleUser && entry.EntryType == EntryTypeText {
		block.Provenance = claudeSubmissionProvenance()
		block.SubmissionKey = block.NativeKey
		return nil
	}
	if entry.Role == RoleUser && entry.EntryType == EntryTypeToolResult {
		callKey := derefString(entry.ToolCallID)
		if callKey == "" {
			return nil
		}
		key, err := claudeAnswerKey(block.CarrierNativeKey, callKey)
		if err != nil {
			return err
		}
		block.Provenance = claudeSubmissionProvenance()
		block.SubmissionKey = key
	}
	return nil
}

// claudeSubmissionProvenance is the full six-dimension tuple every admitted
// Claude submission carries.
func claudeSubmissionProvenance() *schema.ContentProvenance {
	return &schema.ContentProvenance{
		Origin:        schema.ContentOriginSubmittedInput,
		Actor:         schema.ActorOriginUnknown,
		Delivery:      schema.DeliveryOriginSessionAdmission,
		Ownership:     schema.ContentOwnershipLocal,
		Evidence:      schema.EvidenceNativeTyped,
		InputModality: schema.InputModalityText,
	}
}

// claudeEntryExtra separates the retained entry's private extension document
// into the projection-owned channels and the adapter-owned remainder: opaque
// retained evidence, an observed model, and every unrelated key. The projection
// owns those keys, so they never reach the extension document.
func claudeEntryExtra(entry schema.SessionEntry) ([]RetainedUnknown, *string, string, error) {
	if entry.Extra == nil {
		return nil, nil, "", nil
	}
	retained, err := RetainedUnknownOf(entry)
	if err != nil {
		return nil, nil, "", err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*entry.Extra), &fields); err != nil {
		return nil, nil, "", fmt.Errorf("ingest.claudeEntryExtra: entry %d extra is not a JSON object; the extension cannot be preserved; restore the classified extension", entry.EntryIndex)
	}
	delete(fields, retainedUnknownKey)
	var observed *string
	if raw, present := fields["model_id"]; present {
		var id string
		if err := json.Unmarshal(raw, &id); err == nil && ValidObservedModel(id) {
			value := id
			observed = &value
		}
		delete(fields, "model_id")
	}
	if len(fields) == 0 {
		return retained, observed, "", nil
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, "", fmt.Errorf("ingest.claudeEntryExtra: entry %d extension could not be re-encoded; the private fields would be lost; report this with the session id: %w", entry.EntryIndex, err)
	}
	return retained, observed, string(encoded), nil
}

// claudeRecordUUID is the record's own uuid, when it has one. A classified
// record carries it directly; an opaque retained carrier does not, so the uuid
// is read from the retained record's payload. A record with no uuid anywhere is
// anchored by its stream ordinal instead.
func claudeRecordUUID(entry schema.SessionEntry) string {
	if entry.EntryID != nil && *entry.EntryID != "" {
		return *entry.EntryID
	}
	records, err := RetainedUnknownOf(entry)
	if err != nil || len(records) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(records[0].Payload, &fields) != nil {
		return ""
	}
	var uuid string
	if raw, present := fields["uuid"]; present {
		_ = json.Unmarshal(raw, &uuid)
	}
	return uuid
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func derefToolKind(value *schema.ToolCallKind) schema.ToolCallKind {
	if value == nil {
		return ""
	}
	return *value
}
