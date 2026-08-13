// Package transcript holds the single canonical conversion path from indexed
// session_entries to the rendered SessionDetailPayload. It is imported by BOTH
// the local dashboard (internal/api) and the push pipeline (internal/push) so
// the village receives EXACTLY what the local session viewer shows. It lives in
// its own package (depending only on ingest + schema) to break the api->push
// import cycle: internal/api imports internal/push, so the builder cannot live
// in internal/api if push must also call it.
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
	const (
		nameOpen     = "<command-name>"
		nameClose    = "</command-name>"
		messageOpen  = "<command-message>"
		messageClose = "</command-message>"
		argsOpen     = "<command-args>"
		argsClose    = "</command-args>"
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
	if len(entries) == 0 {
		return nil
	}

	// Pass 1: Collect tool_result data keyed by ToolCallID for joining.
	// Also collects depth=0 entries with ToolOutput for backward compat
	// (old-style entries where both tool_use and tool_result are at depth=0).
	resultMap := make(map[string]toolResultData)
	for _, e := range entries {
		if e.ToolCallID == nil {
			continue
		}
		// Depth=1 tool_result entries are the primary source.
		if e.Depth == 1 && e.EntryType == schema.EntryTypeToolResult {
			rd := toolResultData{IsError: e.IsError}
			if e.ToolOutput != nil {
				rd.Output = *e.ToolOutput
			}
			if e.TimestampMs != nil {
				rd.Timestamp = *e.TimestampMs
			}
			resultMap[*e.ToolCallID] = rd
			continue
		}
		// Backward compat: depth=0 entries with ToolOutput (old-style).
		// Only used for duration computation on old-style entries.
		if e.ToolOutput != nil && e.TimestampMs != nil {
			if _, exists := resultMap[*e.ToolCallID]; !exists {
				resultMap[*e.ToolCallID] = toolResultData{
					Output:    *e.ToolOutput,
					IsError:   e.IsError,
					Timestamp: *e.TimestampMs,
				}
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
		if suppress[e.EntryIndex] {
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

		t := ingest.Turn{
			Index:       e.EntryIndex,
			Role:        injectedCommandRole(e, content),
			Content:     content,
			Timestamp:   ts,
			Depth:       e.Depth,
			ParentIndex: e.ParentIndex,
			EntryType:   e.EntryType,
			HasThinking: e.HasThinking,
			StopReason:  e.StopReason,
			TokensIn:    e.TokensIn,
			TokensOut:   e.TokensOut,
			PartType:    e.PartType,
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
		if e.ToolCallID != nil && len(t.ToolCalls) == 0 {
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
			if e.ToolOutput != nil {
				tc.Result = *e.ToolOutput
			}
			if e.ToolKind != nil {
				tc.ToolKind = *e.ToolKind
			}
			tc.IsError = e.IsError

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
		if suppressEmptyTurn(hasContent, hasTools, hasObservation) {
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
		if prev.Role == curr.Role && prev.Content == curr.Content && strings.TrimSpace(curr.Content) != "" && observationsEqual {
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
	return sessionToDetail(s)
}

// SessionToDetailValidated is the canonical producer trust boundary. Callers
// that can surface failures use it so invalid attribution never reaches a wire.
func SessionToDetailValidated(s *ingest.Session) (*schema.SessionDetailPayload, error) {
	if err := validateSessionObservedModelEvidence(s); err != nil {
		return nil, err
	}
	return sessionToDetail(s), nil
}

// sessionToDetail converts a full Session to a SessionDetailPayload.
func sessionToDetail(s *ingest.Session) *schema.SessionDetailPayload {
	turns := make([]schema.TurnDetail, len(s.Turns))
	for i, t := range s.Turns {
		toolCalls := make([]schema.ToolCallDetail, len(t.ToolCalls))
		for j, tc := range t.ToolCalls {
			toolCalls[j] = schema.ToolCallDetail{
				ID:         tc.ID,
				Name:       tc.Name,
				Arguments:  tc.Arguments,
				Result:     tc.Result,
				DurationMs: tc.DurationMs,
				ExitCode:   tc.ExitCode,
				FilePath:   tc.FilePath,
				IsError:    tc.IsError,
				ToolKind:   tc.ToolKind,
			}
		}
		turns[i] = schema.TurnDetail{
			Index:         t.Index,
			Role:          t.Role,
			Content:       t.Content,
			ToolCalls:     toolCalls,
			Timestamp:     t.Timestamp,
			Depth:         t.Depth,
			ParentIndex:   t.ParentIndex,
			EntryType:     t.EntryType,
			HasThinking:   t.HasThinking,
			StopReason:    t.StopReason,
			TokensIn:      t.TokensIn,
			TokensOut:     t.TokensOut,
			ObservedModel: t.ObservedModel,
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

	return &schema.SessionDetailPayload{
		ID:               string(s.ID),
		Harness:          s.Harness,
		StartTime:        s.StartTime,
		EndTime:          s.EndTime,
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
}
