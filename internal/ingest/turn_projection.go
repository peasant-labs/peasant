package ingest

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// EntriesToTurns folds flat session_entries into the rendered Turn model. It
// is the one definition of a turn in the producer: local viewing, export,
// publication, the CLI previews and the conversation-turn count all come
// through this fold, so no consumer re-derives what a turn is.
//
// Depth=1 tool_use and tool_result entries fold into their depth=0 parent
// Turn's ToolCalls, producing one card per assistant message instead of three.
// Old-style entries (ToolCallID on depth=0) are handled directly for backward
// compatibility.
func EntriesToTurns(entries []schema.SessionEntry) []Turn {
	evidence := make(map[int]PiExtra)
	for _, entry := range entries {
		extra, pi, err := DecodePiEntryExtra(entry)
		if err != nil {
			// The caller has no error channel here. A malformed stored row is
			// already refused by the validated projection path; the plain fold
			// returns no turns rather than a partial one it cannot vouch for.
			return nil
		}
		if pi {
			evidence[entry.EntryIndex] = extra
		}
	}
	return foldEntries(entries, evidence)
}

// FoldEntries is the fold entry point for a caller that already decoded the
// Pi evidence for the same entry set. The transcript projection decodes it for
// native metadata, so it passes that map here instead of decoding twice and
// then treats the fold's answer as the single turn projection.
func FoldEntries(entries []schema.SessionEntry, evidence map[int]PiExtra) []Turn {
	return foldEntries(entries, evidence)
}

// ConversationTurnCount counts the projected turns that are conversation: every
// turn whose rendered role is not system. System rows stay served (a reader can
// reveal them and search can land on them), but they are harness state, not
// turns of the conversation, so every count surface excludes them. It is
// computed once during indexing and carried to the store and the wire, so the
// local list, detail, export and publication cannot disagree about a turn.
func ConversationTurnCount(turns []Turn) int {
	count := 0
	for _, turn := range turns {
		if turn.Role != schema.RoleSystem {
			count++
		}
	}
	return count
}

// ObservedModelEvidence returns the fold's raw per-entry model observation
// (present, value). It lets a negative-build test assert the evidence rule
// independently of whether the projection keeps the observation.
func ObservedModelEvidence(entry schema.SessionEntry) (bool, string) {
	observation := modelObservation(entry)
	return observation.present, observation.value
}

type entryModelObservation struct {
	present bool
	value   string
}

func modelObservation(entry schema.SessionEntry) entryModelObservation {
	if entry.Role != schema.RoleAssistant || entry.Extra == nil {
		return entryModelObservation{}
	}
	var extra map[string]json.RawMessage
	if json.Unmarshal([]byte(*entry.Extra), &extra) != nil {
		return entryModelObservation{}
	}
	raw, ok := extra["model_id"]
	if !ok {
		return entryModelObservation{}
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || !ValidObservedModel(value) {
		return entryModelObservation{}
	}
	return entryModelObservation{present: true, value: value}
}

// thinkingLevelObservation reads the per-entry thinking-level evidence from an
// assistant entry's Extra. Non-assistant entries never carry the evidence, and
// a malformed or out-of-contract value is dropped rather than normalized: the
// malformed half is served absent while a well-formed surviving half is still
// served, so this turn's level is unknown only when the canonical level was
// dropped. An explicit JSON null decodes to nil here and is served absent, a
// non-string half fails the typed decode and drops the whole pair, and
// ThinkingLevelEntryWarnings reports each drop so the source can be repaired.
func thinkingLevelObservation(entry schema.SessionEntry) (schema.ThinkingLevel, schema.ThinkingLevelRaw) {
	if entry.Role != schema.RoleAssistant || entry.Extra == nil {
		return "", ""
	}
	var extra struct {
		Level *string `json:"thinking_level"`
		Raw   *string `json:"thinking_level_raw"`
	}
	if json.Unmarshal([]byte(*entry.Extra), &extra) != nil {
		return "", ""
	}
	var level schema.ThinkingLevel
	if extra.Level != nil && schema.ThinkingLevel(*extra.Level).IsValid() {
		level = schema.ThinkingLevel(*extra.Level)
	}
	var raw schema.ThinkingLevelRaw
	if extra.Raw != nil {
		raw, _ = schema.NewThinkingLevelRaw(*extra.Raw)
	}
	return level, raw
}

type toolResultData struct {
	Output    string
	IsError   bool
	Timestamp int64
	// ResultRef is the durable source ref carried on the tool_result entry.
	ResultRef string
	// Provenance is the durable evidence carried on the tool_result entry.
	Provenance *schema.ContentProvenance
}

// copyProvenance returns an independent copy of a source-entry provenance
// record so folded turns never alias stored entry memory.
func copyProvenance(p *schema.ContentProvenance) *schema.ContentProvenance {
	if p == nil {
		return nil
	}
	dupe := *p
	return &dupe
}

type commandWrapperKind uint8

const (
	commandWrapperName commandWrapperKind = 1 << iota
	commandWrapperMessage
	commandWrapperArguments
)

// injectedCommandRole keeps historical harness-generated command markup from
// claiming the user's authorship. It is deliberately conservative: only a
// complete sequence of known wrappers with one valid slash-command name
// qualifies. Any prose, unknown or malformed markup, duplicate wrapper, or
// possibly truncated preview leaves the stored role unchanged.
//
// This projection repairs stored rows that carry no authoritative injection
// signal. It is not complete injected-turn detection: provider metadata such as
// Claude's isMeta belongs at ingest, where the original event is still present.
func injectedCommandRole(entry schema.SessionEntry, content string) schema.Role {
	if entry.Role != schema.RoleUser || !isInjectedCommandWrapperOnly(content) {
		return entry.Role
	}
	return schema.RoleSystem
}

func isInjectedCommandWrapperOnly(content string) bool {
	// ContentPreview is the classification input. At the preview limit, a
	// syntactically complete wrapper could still have user prose beyond the stored
	// bytes. Match the existing overlay gate and fail safe to user authorship.
	if content == "" || len(content) >= defaults.ContentPreviewLimit {
		return false
	}

	rest := strings.TrimSpace(content)
	seen := commandWrapperKind(0)
	for rest != "" {
		kind, openTag, closeTag, ok := commandWrapperAtStart(rest)
		if !ok || seen&kind != 0 {
			return false
		}

		bodyAndRest := rest[len(openTag):]
		closeIndex := strings.Index(bodyAndRest, closeTag)
		if closeIndex < 0 {
			return false
		}
		body := bodyAndRest[:closeIndex]
		if strings.ContainsAny(body, "<>") || !validCommandWrapperBody(kind, body) {
			return false
		}

		seen |= kind
		rest = strings.TrimSpace(bodyAndRest[closeIndex+len(closeTag):])
	}

	return seen&commandWrapperName != 0
}

func commandWrapperAtStart(content string) (commandWrapperKind, string, string, bool) {
	// The wrapper names come from redact, which owns the one catalog of
	// harness-injected markup names, so this gate and the title pipeline can
	// never recognize different command wrappers.
	const (
		nameOpen     = "<" + redact.WrapperCommandName + ">"
		nameClose    = "</" + redact.WrapperCommandName + ">"
		messageOpen  = "<" + redact.WrapperCommandMessage + ">"
		messageClose = "</" + redact.WrapperCommandMessage + ">"
		argsOpen     = "<" + redact.WrapperCommandArgs + ">"
		argsClose    = "</" + redact.WrapperCommandArgs + ">"
	)

	switch {
	case strings.HasPrefix(content, nameOpen):
		return commandWrapperName, nameOpen, nameClose, true
	case strings.HasPrefix(content, messageOpen):
		return commandWrapperMessage, messageOpen, messageClose, true
	case strings.HasPrefix(content, argsOpen):
		return commandWrapperArguments, argsOpen, argsClose, true
	default:
		return 0, "", "", false
	}
}

func validCommandWrapperBody(kind commandWrapperKind, body string) bool {
	trimmed := strings.TrimSpace(body)
	switch kind {
	case commandWrapperName:
		return len(trimmed) > 1 && strings.HasPrefix(trimmed, "/") && !strings.ContainsAny(trimmed, " \t\r\n")
	case commandWrapperMessage:
		return trimmed != ""
	case commandWrapperArguments:
		return true
	default:
		return false
	}
}

func toolResultOutput(e schema.SessionEntry) string {
	if e.ToolOutput != nil {
		return *e.ToolOutput
	}
	if _, omitted := OmittedRecordOf(e); omitted && e.ContentPreview != nil {
		return *e.ContentPreview
	}
	return ""
}

// unjoinedOmissionPlaceholder reports whether an entry is an omission
// placeholder, the entry that stands where ingest left a source record out,
// that NO tool call will show to a reader.
//
// A placeholder is shown as a tool call's result only when it carries the id
// of a tool call the transcript also holds. It carries no id at all when the
// omitted record opened without one of the correlation id keys: every Pi
// omission, an oversized assistant message, and a Cursor or Strike record
// whose id sits outside the read prefix. It carries an id nothing answers when
// the tool call itself was the omitted record. In both of those cases the
// placeholder is the only trace of the missing record, so it has to be emitted
// as a turn of its own; suppressing it as a depth-0 tool wrapper would drop
// the note from every served surface (the detail socket, the previews, the
// export and the publication) and the reader would see the conversation jump.
func unjoinedOmissionPlaceholder(entry schema.SessionEntry, toolUseIDs map[string]bool) bool {
	if _, omitted := OmittedRecordOf(entry); !omitted {
		return false
	}
	return entry.ToolCallID == nil || !toolUseIDs[*entry.ToolCallID]
}

// representedClaudeTextBlock identifies a decomposed block whose entire text
// its assistant carrier already displays. A source-referenced block bypasses
// consecutive dedup; unrelated children and their own evidence must survive.
func representedClaudeTextBlock(child, parent schema.SessionEntry) bool {
	if child.Harness != schema.HarnessClaudeCode || parent.Harness != child.Harness {
		return false
	}
	if child.Depth != 1 || parent.Depth != 0 {
		return false
	}
	if child.Role != schema.RoleAssistant || parent.Role != schema.RoleAssistant {
		return false
	}
	if child.EntryType != schema.EntryTypeText || child.PartType == nil || *child.PartType != "text" {
		return false
	}
	if child.HasToolUse || child.HasThinking || child.ToolCallID != nil {
		return false
	}
	if child.ContentPreview == nil || parent.ContentPreview == nil {
		return false
	}
	if *child.ContentPreview == "" || *child.ContentPreview != *parent.ContentPreview || modelObservation(child).present {
		return false
	}
	level, rawLevel := thinkingLevelObservation(child)
	return level == "" && rawLevel == ""
}

func foldEntries(entries []schema.SessionEntry, evidence map[int]PiExtra) []Turn {
	if len(entries) == 0 {
		return nil
	}

	// The tool calls this transcript actually holds. An omission placeholder
	// is folded into one of them only when its id is in this set.
	toolUseIDs := make(map[string]bool)
	for _, e := range entries {
		if e.EntryType == schema.EntryTypeToolUse && e.ToolCallID != nil {
			toolUseIDs[*e.ToolCallID] = true
		}
	}

	// Pass 1: Collect tool_result data keyed by ToolCallID for joining.
	// Inline ToolOutput remains a fallback at any depth, including tool_use
	// children whose harness stores the completed result on the call itself.
	resultMap := make(map[string]toolResultData)
	for _, e := range entries {
		if e.ToolCallID == nil {
			continue
		}
		// Depth=1 tool_result entries are the primary source.
		if e.Depth == 1 && e.EntryType == schema.EntryTypeToolResult {
			rd := toolResultData{IsError: e.IsError, Output: toolResultOutput(e), ResultRef: string(e.SourceEntryRef), Provenance: copyProvenance(e.Provenance)}
			if e.TimestampMs != nil {
				rd.Timestamp = *e.TimestampMs
			}
			resultMap[*e.ToolCallID] = rd
			continue
		}
		// Preserve inline output as well as untimed flat result/error records.
		// Explicit depth-1 results above take precedence in either entry order.
		if e.ToolOutput != nil || (e.Depth == 0 && e.EntryType == schema.EntryTypeToolResult) {
			if _, exists := resultMap[*e.ToolCallID]; !exists {
				rd := toolResultData{IsError: e.IsError, Output: toolResultOutput(e), ResultRef: string(e.SourceEntryRef), Provenance: copyProvenance(e.Provenance)}
				if e.TimestampMs != nil {
					rd.Timestamp = *e.TimestampMs
				}
				resultMap[*e.ToolCallID] = rd
			}
		}
	}

	// Pass 1b: Collect depth=1 tool_use entries, build complete ToolCalls by
	// joining with tool_result data via ToolCallID, and group by ParentIndex.
	// Both tool_use and tool_result depth=1 entries share the same ParentIndex
	// (the depth=0 assistant entry) — but we join by ToolCallID for correctness
	// since tool_result entries in Claude JSONL may be wrapped in separate
	// depth=0 user messages with their own ParentIndex.
	foldedToolCalls := make(map[int][]ToolCall) // parentEntryIndex → ToolCalls
	foldedEntries := make(map[int]bool)         // entry indices to suppress

	for _, e := range entries {
		if e.Depth != 1 || e.ToolCallID == nil {
			continue
		}

		if e.EntryType == schema.EntryTypeToolUse {
			tc := ToolCall{
				ID: *e.ToolCallID,
			}
			// A managed generation carries the durable source ref on the
			// entry itself; Pi rows carry it in typed Extra instead and leave
			// the entry ref empty, so the entry value is only a fallback.
			if e.SourceEntryRef != "" {
				tc.CallEntryRef = string(e.SourceEntryRef)
			}
			tc.CallProvenance = copyProvenance(e.Provenance)
			if e.ToolNamesCSV != nil {
				tc.Name = *e.ToolNamesCSV
			}
			if e.ToolInput != nil {
				tc.Arguments = *e.ToolInput
				tc.FilePath = extractFilePath(*e.ToolInput)
			}
			if e.ToolKind != nil {
				tc.ToolKind = *e.ToolKind
			}

			// Merge result data from the matching tool_result entry.
			if rd, ok := resultMap[*e.ToolCallID]; ok {
				tc.Result = rd.Output
				tc.IsError = rd.IsError
				if tc.ResultEntryRef == "" {
					tc.ResultEntryRef = rd.ResultRef
				}
				if tc.ResultProvenance == nil {
					tc.ResultProvenance = copyProvenance(rd.Provenance)
				}
				// Compute duration from tool_use → tool_result timestamps.
				if e.TimestampMs != nil && rd.Timestamp > 0 {
					dur := int(rd.Timestamp - *e.TimestampMs)
					if dur >= 0 {
						tc.DurationMs = &dur
					}
				}
				// Extract exit code for Bash tool calls.
				if tc.Name == "Bash" && tc.Result != "" {
					tc.ExitCode = extractExitCode(tc.Result)
				}
			}

			if e.ParentIndex != nil {
				foldedToolCalls[*e.ParentIndex] = append(foldedToolCalls[*e.ParentIndex], tc)
			}
			foldedEntries[e.EntryIndex] = true
		} else if e.EntryType == schema.EntryTypeToolResult {
			foldedEntries[e.EntryIndex] = true
		}
	}

	// Pass 2: Build the full suppress set.
	suppress := make(map[int]bool, len(foldedEntries))
	for idx := range foldedEntries {
		suppress[idx] = true
	}

	// Suppress depth=0 tool wrappers.
	// v10+: indexer canonicalizes wrapper role to tool (R2).
	for _, e := range entries {
		if e.Depth == 0 && e.Role == schema.RoleTool {
			if unjoinedOmissionPlaceholder(e, toolUseIDs) {
				continue
			}
			suppress[e.EntryIndex] = true
		}
	}
	// Fallback for pre-v10 sessions: depth-0 user/tool wrappers whose children
	// are tool_result. A provider may attach tool_use and tool_result children
	// directly to one assistant parent, which must remain visible.
	depthZeroEntries := make(map[int]schema.SessionEntry)
	for _, e := range entries {
		if e.Depth == 0 {
			depthZeroEntries[e.EntryIndex] = e
		}
	}
	for _, e := range entries {
		if e.Depth == 1 && e.EntryType == schema.EntryTypeToolResult && e.ParentIndex != nil {
			role := depthZeroEntries[*e.ParentIndex].Role
			if role == schema.RoleUser || role == schema.RoleTool {
				suppress[*e.ParentIndex] = true
			}
		}
	}

	// Suppress depth=1 text/thinking siblings of folded tool entries.
	// When a depth=0 assistant message has both text and tool_use children, the
	// depth=0 parent's ContentPreview already contains the text summary. Emitting
	// the text sibling as a separate card would duplicate content.
	parentsWithTools := make(map[int]bool) // depth=0 entries that have folded ToolCalls
	for parentIdx := range foldedToolCalls {
		parentsWithTools[parentIdx] = true
	}
	representedTextParents := make(map[int]bool)
	hydratedCarrierText := make(map[int]*string)
	for _, e := range entries {
		if e.ParentIndex != nil && !representedTextParents[*e.ParentIndex] {
			parent := depthZeroEntries[*e.ParentIndex]
			// Managed tool carriers retain their prose in text children rather
			// than their tool-argument channel. Display the first plain block
			// on that carrier, preserving its model, time and folded tools.
			// Work on the local entry copy so stored evidence stays unchanged.
			if parentsWithTools[parent.EntryIndex] && parent.EntryType == schema.EntryTypeToolUse && parent.ToolCallID == nil &&
				(parent.ContentPreview == nil || *parent.ContentPreview == "") {
				represented := parent
				represented.ContentPreview = e.ContentPreview
				if representedClaudeTextBlock(e, represented) {
					parent = represented
					depthZeroEntries[parent.EntryIndex] = parent
					hydratedCarrierText[parent.EntryIndex] = parent.ContentPreview
				}
			}
			if representedClaudeTextBlock(e, parent) {
				suppress[e.EntryIndex] = true
				// The carrier represents one occurrence, not every equal block.
				// Two native "ok" blocks must still display twice in total.
				representedTextParents[*e.ParentIndex] = true
			}
		}
		if e.Depth == 1 && e.ParentIndex != nil && parentsWithTools[*e.ParentIndex] {
			// A Claude carrier contains only its first text block. Its other
			// text blocks remain conversation even when tools share the record.
			legacyTextSummary := e.EntryType == schema.EntryTypeText && e.Harness != schema.HarnessClaudeCode
			if legacyTextSummary || e.EntryType == schema.EntryTypeThinking {
				suppress[e.EntryIndex] = true
			}
		}
	}

	// Image-bearing messages keep every text and image child as an ordered part
	// of the depth-0 carrier. The children are not their own turns.
	imageParts := imagePartsByParent(entries)
	for _, entry := range entries {
		if entry.ParentIndex == nil {
			continue
		}
		if _, ok := imageParts[*entry.ParentIndex]; !ok {
			continue
		}
		if entry.EntryType == schema.EntryTypeImage || (entry.EntryType == schema.EntryTypeText && entry.PartType != nil && *entry.PartType == "text") {
			suppress[entry.EntryIndex] = true
		}
	}

	// Pass 3: Emit turns.
	turns := make([]Turn, 0, len(entries))
	turnObservations := make(map[int]entryModelObservation)
	for _, e := range entries {
		if content, hydrated := hydratedCarrierText[e.EntryIndex]; hydrated && e.Depth == 0 {
			e.ContentPreview = content
		}
		_, pi := evidence[e.EntryIndex]
		if suppress[e.EntryIndex] || IsPiCarrier(e) || IsRetainedUnknownCarrier(e) || (pi && e.Depth > 0 && e.ParentIndex != nil && (e.EntryType == schema.EntryTypeThinking || e.EntryType == schema.EntryTypeText)) {
			continue
		}

		content := ""
		if e.ContentPreview != nil {
			content = *e.ContentPreview
		}

		var ts time.Time
		if e.TimestampMs != nil {
			ts = time.UnixMilli(*e.TimestampMs)
		}

		command := CommandInvocationFromEntry(e)
		role := injectedCommandRole(e, content)
		// A USER turn whose only content is the invocation is harness-injected
		// markup too, exactly like the command wrappers the gate above
		// recognizes. A harness that records the invocation structurally leaves
		// no text for that gate to match, so the role is settled here instead.
		// The stored role is part of the condition because not every harness
		// records a command on a user entry: one records a skill call on the
		// ASSISTANT message that made it, and that turn is already correctly
		// attributed to its author, so it keeps the assistant role (and with it
		// the only role on which its model observation is valid evidence).
		if command != nil && e.Role == schema.RoleUser && strings.TrimSpace(content) == "" {
			role = schema.RoleSystem
		}

		// The producer's control classification is set on the depth-0 turn it
		// describes and nowhere else. A depth-1 block part type is conversation
		// structure, never a control kind, so the guard is part of the rule.
		var controlKind *string
		if e.Depth == 0 && e.PartType != nil {
			if kind := ControlRecordKindFor(e.Harness, *e.PartType); kind != "" {
				controlKind = &kind
			}
		}

		parts := imageParts[e.EntryIndex]
		if e.ContentPreview == nil || *e.ContentPreview == "" {
			if joined := joinedPartText(parts); joined != "" {
				content = joined
			}
		}
		t := Turn{
			Parts:          parts,
			ControlKind:    controlKind,
			SourceEntryRef: evidence[e.EntryIndex].SourceRef,
			Usage:          evidence[e.EntryIndex].Usage,
			Provenance:     copyProvenance(e.Provenance),
			Index:          e.EntryIndex,
			Role:           role,
			Command:        command,
			Content:        content,
			Timestamp:      ts,
			Depth:          e.Depth,
			ParentIndex:    e.ParentIndex,
			EntryType:      e.EntryType,
			HasThinking:    e.HasThinking,
			StopReason:     e.StopReason,
			TokensIn:       e.TokensIn,
			TokensOut:      e.TokensOut,
			PartType:       e.PartType,
		}
		// A managed generation carries the durable source ref on the entry
		// itself. Pi rows keep it in typed Extra and leave the entry ref
		// empty, so the entry value only fills the gap Pi evidence leaves.
		if t.SourceEntryRef == "" {
			t.SourceEntryRef = string(e.SourceEntryRef)
		}
		observation := modelObservation(e)
		projectedObservation := projectModelObservation(observation)
		if projectedObservation != "" {
			assignProjectedModelObservation(&t, projectedObservation)
		}
		t.ThinkingLevel, t.ThinkingLevelRaw = thinkingLevelObservation(e)

		// Attach folded ToolCalls from depth=1 children.
		if folded, ok := foldedToolCalls[e.EntryIndex]; ok {
			t.ToolCalls = folded
		}

		// Backward compatibility: old-style single-level entries carry ToolCallID
		// directly on depth=0. These are not collected by the fold pre-pass
		// (which only processes depth=1), so handle them here.
		//
		// An omission placeholder no tool call will show is emitted as its own
		// turn carrying the note, so it gets no synthetic tool call here: that
		// would show the reader the same note twice, once as the turn and once
		// as the result of a call that answers nothing.
		if e.ToolCallID != nil && len(t.ToolCalls) == 0 && !unjoinedOmissionPlaceholder(e, toolUseIDs) {
			tc := ToolCall{
				ID: *e.ToolCallID,
			}
			// Old-style single-level rows share one entry for call and
			// result. A tool_result row owns the result ref; any other row
			// owns the call ref.
			if e.EntryType == schema.EntryTypeToolResult {
				if e.SourceEntryRef != "" {
					tc.ResultEntryRef = string(e.SourceEntryRef)
				}
				tc.ResultProvenance = copyProvenance(e.Provenance)
			} else {
				if e.SourceEntryRef != "" {
					tc.CallEntryRef = string(e.SourceEntryRef)
				}
				tc.CallProvenance = copyProvenance(e.Provenance)
			}
			if e.ToolNamesCSV != nil {
				tc.Name = *e.ToolNamesCSV
			}
			if e.ToolInput != nil {
				tc.Arguments = *e.ToolInput
				tc.FilePath = extractFilePath(*e.ToolInput)
			}
			tc.Result = toolResultOutput(e)
			if e.ToolKind != nil {
				tc.ToolKind = *e.ToolKind
			}
			tc.IsError = e.IsError
			if rd, ok := resultMap[*e.ToolCallID]; ok && e.EntryType == schema.EntryTypeToolUse {
				tc.Result = rd.Output
				tc.IsError = rd.IsError
				if tc.ResultEntryRef == "" {
					tc.ResultEntryRef = rd.ResultRef
				}
				if tc.ResultProvenance == nil {
					tc.ResultProvenance = copyProvenance(rd.Provenance)
				}
			}

			// Compute duration from tool_use → tool_result timestamps.
			if e.TimestampMs != nil {
				if rd, ok := resultMap[*e.ToolCallID]; ok && rd.Timestamp > 0 {
					dur := int(rd.Timestamp - *e.TimestampMs)
					if dur >= 0 {
						tc.DurationMs = &dur
					}
				}
			}
			if tc.Name == "Bash" && tc.Result != "" {
				tc.ExitCode = extractExitCode(tc.Result)
			}

			t.ToolCalls = []ToolCall{tc}
		}

		turns = append(turns, t)
		if observation.present {
			turnObservations[t.Index] = observation
		}
	}

	// Post-processing: empty entry suppression and consecutive dedup.
	//
	// Empty entry suppression: remove turns with no displayable content and no
	// tool calls. These are artefacts (e.g. bare role markers) that add no value
	// to the session viewer. Note: system entries with short content are kept
	// intentionally because they can carry legitimate control context.
	//
	// Consecutive dedup: when two adjacent turns share the same role, non-empty
	// content, and valid observation presence/value, keep one. Observation is part
	// of equivalence because a source-evidence boundary must survive even when the
	// visible text repeats. If one has tool calls and the other does not, prefer it.
	filtered := turns[:0]
	for _, t := range turns {
		hasContent := strings.TrimSpace(t.Content) != ""
		hasTools := len(t.ToolCalls) > 0
		hasParts := len(t.Parts) > 0
		hasObservation := turnObservations[t.Index].present
		// An empty depth-0 system turn is never a turn, whatever record kind
		// produced it: a managed generation puts a source ref on every entry,
		// and Claude stores control records as entry_type=text plus a part type,
		// so neither the source ref nor entry_type can stand in for "this is
		// conversation". The command and suppressEmptyTurn gates still decide
		// whether a turn that carries content, a tool call, a command or an
		// observation survives.
		suppressionCandidate := t.SourceEntryRef == "" || (t.Role == schema.RoleSystem && t.Depth == 0)
		if suppressionCandidate && t.Command == nil && !hasParts && suppressEmptyTurn(hasContent, hasTools, hasObservation) {
			continue
		}
		filtered = append(filtered, t)
	}

	deduped := make([]Turn, 0, len(filtered))
	for _, curr := range filtered {
		if len(deduped) == 0 {
			deduped = append(deduped, curr)
			continue
		}
		prev := &deduped[len(deduped)-1]
		prevObservation := turnObservations[prev.Index]
		currObservation := turnObservations[curr.Index]
		observationsEqual := modelObservationsEquivalent(prevObservation, currObservation)
		if prev.SourceEntryRef == "" && curr.SourceEntryRef == "" && prev.Role == curr.Role && prev.Content == curr.Content && strings.TrimSpace(curr.Content) != "" && observationsEqual && prev.ThinkingLevel == curr.ThinkingLevel && prev.ThinkingLevelRaw == curr.ThinkingLevelRaw {
			prevHasTools := len(prev.ToolCalls) > 0
			currHasTools := len(curr.ToolCalls) > 0
			if currHasTools && !prevHasTools {
				deduped[len(deduped)-1] = curr
			}
			// Either way, skip the duplicate.
			continue
		}
		deduped = append(deduped, curr)
	}

	return deduped
}

func suppressEmptyTurn(hasContent, hasTools, hasObservation bool) bool {
	return shouldSuppressEmptyTurn(hasContent, hasTools, hasObservation)
}

func modelObservationsEquivalent(previous, current entryModelObservation) bool {
	return observationsEquivalent(previous, current)
}

// extractFilePath parses tool input JSON for common file path keys.
func extractFilePath(toolInput string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(toolInput), &args); err != nil {
		return ""
	}
	for _, key := range []string{"file_path", "notebook_path", "path"} {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// extractExitCode attempts to parse an exit code from Bash tool result content.
// Returns nil if no exit code pattern is found (which means success/exit 0).
func extractExitCode(result string) *int {
	// Check for the common "Exit code N" prefix that the Bash tool produces on error.
	if len(result) > 10 && result[:10] == "Exit code " {
		code := 0
		for i := 10; i < len(result) && result[i] >= '0' && result[i] <= '9'; i++ {
			code = code*10 + int(result[i]-'0')
		}
		if code != 0 {
			return &code
		}
	}
	return nil
}

// qualityMetricsToScorecard projects a schema.QualityMetrics into the flat
// SessionScorecard consumed by the Highlights self-assessment card. Returns
// nil when the metrics are nil so the payload omits the scorecard for sessions
