// Package transcript holds the single canonical conversion path from indexed
// session_entries to the rendered SessionDetailPayload. It is imported by BOTH
// the local dashboard (internal/api) and the push pipeline (internal/push) so
// the village receives EXACTLY what the local session viewer shows. It lives in
// its own package (depending only on ingest, schema, and the harness-markup
// wrapper names exported by redact) to break the api->push import cycle:
// internal/api imports internal/push, so the builder cannot live in
// internal/api if push must also call it. Only redact's wrapper-name constants
// are used here, and redact itself imports only schema, so no cycle is
// possible.
//
// There is ONE conversion path. Changes here affect the web dashboard, the
// `peasant export sessions` output, AND the `peasant push` structured upload.
package transcript

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// toolResultData holds tool_result entry data for ToolCallID-based joining.
type toolResultData struct {
	Output    string
	IsError   bool
	Timestamp int64
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

// commandInvocationFromEntry projects the command an entry recorded at index
// time onto the wire type. The recorded name gets the leading slash the wire
// requires when the harness recorded it bare. It returns nil when the entry
// recorded no command and when the recorded name cannot form a valid
// invocation — which is also how a built-in harness command stays off the wire,
// because schema.NewCommandInvocation refuses one.
func commandInvocationFromEntry(entry schema.SessionEntry) *schema.CommandInvocation {
	if entry.Extra == nil {
		return nil
	}
	var stored struct {
		Name string          `json:"command_name"`
		Args json.RawMessage `json:"command_args"`
	}
	if err := json.Unmarshal([]byte(*entry.Extra), &stored); err != nil || stored.Name == "" {
		return nil
	}
	name := stored.Name
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	invocation, err := schema.NewCommandInvocation(name, commandArgsFromRaw(stored.Args))
	if err != nil {
		return nil
	}
	return &invocation
}

// commandArgsFromRaw reads a stored command_args value tolerantly: a JSON
// string becomes the args, and anything else — a number, an object, an array,
// a boolean, null, or the field being absent — is treated as empty args, so a
// non-string args value never drops the invocation (the name still reaches the
// wire).
func commandArgsFromRaw(raw json.RawMessage) string {
	var args string
	if len(raw) == 0 {
		return args
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return ""
	}
	return args
}

// entriesToTurns converts flat session_entries into the Turn model expected by
// the detail view. Depth=1 tool_use and tool_result entries are folded into
// their depth=0 parent Turn's ToolCalls, producing one card per assistant
// message instead of three. Old-style entries (ToolCallID on depth=0) are
// handled directly for backward compatibility.
//
// The fold uses a 3-pass algorithm:
//   - Pass 1: Collect tool data from depth=1 entries, join tool_use+tool_result
//     by ToolCallID, group by tool_use's ParentIndex.
//   - Pass 2: Build suppress set (folded depth=1 entries, user-wrapper depth=0,
//     text/thinking siblings of tool turns).
//   - Pass 3: Emit turns with folded ToolCalls attached to depth=0 parents.
func EntriesToTurns(entries []schema.SessionEntry) []ingest.Turn {
	projection, _ := entriesToProjection(entries, ProjectionOptions{}, false)
	return projection.Turns
}

// toolResultOutput is the text a tool_result entry contributes as a tool call's
// result on the served projection.
//
// It is the recorded output; and for an OMISSION PLACEHOLDER — the entry that
// stands where ingest left a source record out — it is the reader-facing note,
// which is the only text such an entry carries and the whole reason it exists.
// Without this the placeholder would be stored, published and served as an
// empty result, and a reader would see a tool call that silently returned
// nothing instead of being told what is missing and why.
func toolResultOutput(e schema.SessionEntry) string {
	if e.ToolOutput != nil {
		return *e.ToolOutput
	}
	if _, omitted := ingest.OmittedRecordOf(e); omitted && e.ContentPreview != nil {
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
	if _, omitted := ingest.OmittedRecordOf(entry); !omitted {
		return false
	}
	return entry.ToolCallID == nil || !toolUseIDs[*entry.ToolCallID]
}

func foldEntries(entries []schema.SessionEntry, evidence map[int]ingest.PiExtra) []ingest.Turn {
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
			rd := toolResultData{IsError: e.IsError, Output: toolResultOutput(e)}
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
				rd := toolResultData{IsError: e.IsError, Output: toolResultOutput(e)}
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
	foldedToolCalls := make(map[int][]ingest.ToolCall) // parentEntryIndex → ToolCalls
	foldedEntries := make(map[int]bool)                // entry indices to suppress

	for _, e := range entries {
		if e.Depth != 1 || e.ToolCallID == nil {
			continue
		}

		if e.EntryType == schema.EntryTypeToolUse {
			tc := ingest.ToolCall{
				ID: *e.ToolCallID,
			}
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
	depthZeroRoles := make(map[int]schema.Role)
	for _, e := range entries {
		if e.Depth == 0 {
			depthZeroRoles[e.EntryIndex] = e.Role
		}
	}
	for _, e := range entries {
		if e.Depth == 1 && e.EntryType == schema.EntryTypeToolResult && e.ParentIndex != nil {
			role := depthZeroRoles[*e.ParentIndex]
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
	for _, e := range entries {
		if e.Depth == 1 && e.ParentIndex != nil && parentsWithTools[*e.ParentIndex] {
			if e.EntryType == schema.EntryTypeText || e.EntryType == schema.EntryTypeThinking {
				suppress[e.EntryIndex] = true
			}
		}
	}

	// Pass 3: Emit turns.
	turns := make([]ingest.Turn, 0, len(entries))
	turnObservations := make(map[int]entryModelObservation)
	for _, e := range entries {
		_, pi := evidence[e.EntryIndex]
		if suppress[e.EntryIndex] || ingest.IsPiCarrier(e) || (pi && e.Depth > 0 && e.ParentIndex != nil && (e.EntryType == schema.EntryTypeThinking || e.EntryType == schema.EntryTypeText)) {
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

		command := commandInvocationFromEntry(e)
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

		t := ingest.Turn{
			SourceEntryRef: evidence[e.EntryIndex].SourceRef,
			Usage:          evidence[e.EntryIndex].Usage,
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
		observation := modelObservation(e)
		projectedObservation := projectModelObservation(observation)
		if projectedObservation != "" {
			assignProjectedModelObservation(&t, projectedObservation)
		}

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
			tc := ingest.ToolCall{
				ID: *e.ToolCallID,
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

			t.ToolCalls = []ingest.ToolCall{tc}
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
		hasObservation := turnObservations[t.Index].present
		if t.SourceEntryRef == "" && t.Command == nil && suppressEmptyTurn(hasContent, hasTools, hasObservation) {
			continue
		}
		filtered = append(filtered, t)
	}

	deduped := make([]ingest.Turn, 0, len(filtered))
	for _, curr := range filtered {
		if len(deduped) == 0 {
			deduped = append(deduped, curr)
			continue
		}
		prev := &deduped[len(deduped)-1]
		prevObservation := turnObservations[prev.Index]
		currObservation := turnObservations[curr.Index]
		observationsEqual := modelObservationsEquivalent(prevObservation, currObservation)
		if prev.SourceEntryRef == "" && curr.SourceEntryRef == "" && prev.Role == curr.Role && prev.Content == curr.Content && strings.TrimSpace(curr.Content) != "" && observationsEqual {
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
// that have not been analysed.
func qualityMetricsToScorecard(q *schema.QualityMetrics) *schema.SessionScorecard {
	if q == nil {
		return nil
	}
	sc := &schema.SessionScorecard{
		M2TokenOutcomeRatio:     q.M2TokenOutcomeRatio,
		M5ContextUtilizationPct: q.M5ContextUtilizationPct,
		M6OutputSurvivalPct:     q.M6OutputSurvivalPct,
		RetryTokensWasted:       q.RetryTokensWasted,
		TotalTokens:             q.TotalTokens,
		CostTotalUSD:            q.CostTotalUSD,
		SpecQualityScore:        q.SpecQualityScore,
		SignalDensity:           q.SignalDensity,
		M7SpecHasExamples:       q.M7SpecHasExamples,
		M7SpecHasConstraints:    q.M7SpecHasConstraints,
		M4ConsecutiveErrorMax:   q.M4ConsecutiveErrorMax,
		WithinSessionReverts:    q.WithinSessionReverts,
	}
	if q.Outcome != nil {
		sc.Outcome = *q.Outcome
	}
	return sc
}

// SessionToDetail converts a full Session to a SessionDetailPayload.
// Exported for use by the export package to ensure the exported transcript
// matches exactly what the session viewer shows.
func SessionToDetail(s *ingest.Session) *schema.SessionDetailPayload {
	if s.Harness == schema.HarnessPi {
		detail, _ := SessionToDetailValidated(s)
		return detail
	}
	return sessionToDetail(s)
}

// SessionToDetailValidated is the canonical producer trust boundary. Callers
// that can surface failures use it so invalid attribution never reaches a wire.
func SessionToDetailValidated(s *ingest.Session) (*schema.SessionDetailPayload, error) {
	if err := validateSessionObservedModelEvidence(s); err != nil {
		return nil, err
	}
	detail := sessionToDetail(s)
	if s.Harness == schema.HarnessPi {
		if err := piLegacyMirrors(detail); err != nil {
			return nil, err
		}
	}
	if err := schema.ValidateSessionDetailPayload(*detail); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return nil, err
	}
	validated, err := schema.DecodeSessionDetailPayloadRaw(raw)
	return &validated, err
}

// sessionToDetail converts a full Session to a SessionDetailPayload.
func sessionToDetail(s *ingest.Session) *schema.SessionDetailPayload {
	turns := make([]schema.TurnDetail, len(s.Turns))
	for i, t := range s.Turns {
		toolCalls := make([]schema.ToolCallDetail, len(t.ToolCalls))
		for j, tc := range t.ToolCalls {
			toolCalls[j] = schema.ToolCallDetail{
				CallEntryRef:   tc.CallEntryRef,
				ResultEntryRef: tc.ResultEntryRef,
				Usage:          tc.Usage,
				ID:             tc.ID,
				Name:           tc.Name,
				Namespace:      tc.Namespace,
				Arguments:      tc.Arguments,
				Result:         tc.Result,
				DurationMs:     tc.DurationMs,
				ExitCode:       tc.ExitCode,
				FilePath:       tc.FilePath,
				IsError:        tc.IsError,
				ToolKind:       tc.ToolKind,
			}
		}
		turns[i] = schema.TurnDetail{
			SourceEntryRef: t.SourceEntryRef,
			Usage:          t.Usage,
			Index:          t.Index,
			Role:           t.Role,
			Command:        t.Command,
			Content:        t.Content,
			ToolCalls:      toolCalls,
			Timestamp:      t.Timestamp.UTC(),
			Depth:          t.Depth,
			ParentIndex:    t.ParentIndex,
			EntryType:      t.EntryType,
			HasThinking:    t.HasThinking,
			StopReason:     t.StopReason,
			TokensIn:       t.TokensIn,
			TokensOut:      t.TokensOut,
			ObservedModel:  t.ObservedModel,
		}
	}

	model := sessionModelSeed(s)

	// Derive source and status from session fields.
	source := "imported"
	status := "local"
	if s.PushedAt != nil {
		status = "posted"
	}

	// Outcome is carried on the session's quality metrics when computed.
	var outcome schema.SessionOutcome
	if s.Metadata.Quality != nil && s.Metadata.Quality.Outcome != nil {
		outcome = *s.Metadata.Quality.Outcome
	}

	// Scorecard projects the per-session quality signals for the Highlights
	// self-assessment card; nil when the session has no computed metrics.
	scorecard := qualityMetricsToScorecard(s.Metadata.Quality)

	detail := &schema.SessionDetailPayload{
		NativeMetadata:   s.NativeMetadata,
		ID:               string(s.ID),
		Harness:          s.Harness,
		StartTime:        s.StartTime.UTC(),
		EndTime:          s.EndTime.UTC(),
		DurationMins:     s.Metadata.Duration.Minutes(),
		TotalTokens:      s.Metadata.TotalTokens,
		TokensIn:         s.Metadata.TokensIn,
		TokensOut:        s.Metadata.TokensOut,
		TurnCount:        s.Metadata.TurnCount,
		ToolCallCount:    s.Metadata.ToolCallCount,
		Turns:            turns,
		Source:           source,
		Status:           status,
		Project:          s.Project,
		Model:            model,
		WorkingDirectory: s.ProjectPath,
		GitBranch:        s.GitBranch,
		GitRemote:        s.GitRemote,
		Outcome:          outcome,
		Scorecard:        scorecard,
	}
	// Every served detail leaves this one producer bounded for display. A stored
	// record may be far larger than the contract's document policy allows the
	// served document to be, and a session is never refused for the size of its
	// tool outputs: the oversized text is shortened here, with a visible note,
	// and the store keeps the whole record. A session whose turn structure alone
	// exceeds the document budget is still refused by the contract's decoder;
	// that case is bounded by the hard document cap on purpose, and paged
	// serving is the planned answer to it. Every consumer of this projection,
	// the session_detail WebSocket, the kickstart preview, `peasant export` and
	// the publication content, inherits the same bound because they all come
	// through here.
	BoundServedDetail(detail, DefaultServedDocumentBudget())
	return detail
}
