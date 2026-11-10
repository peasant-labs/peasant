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

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// EntriesToTurns delegates to the one ingest definition of a turn, so the
// viewer, export and publication cannot disagree with the indexing count.
func EntriesToTurns(entries []schema.SessionEntry) []ingest.Turn {
	return ingest.EntriesToTurns(entries)
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
	if err := validateSessionThinkingLevelEvidence(s); err != nil {
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
	turns := turnsToDetail(s.Turns)

	model := sessionModelSeed(s)
	thinkingLevel, thinkingLevelRaw := sessionThinkingSeed(s)

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
		NativeMetadata:       s.NativeMetadata,
		RetainedUnknown:      s.RetainedUnknown,
		Diagnostics:          s.Diagnostics,
		ID:                   string(s.ID),
		Harness:              s.Harness,
		StartTime:            s.StartTime.UTC(),
		EndTime:              s.EndTime.UTC(),
		DurationMins:         s.Metadata.Duration.Minutes(),
		TotalTokens:          s.Metadata.TotalTokens,
		TokensIn:             s.Metadata.TokensIn,
		TokensOut:            s.Metadata.TokensOut,
		TurnCount:            s.Metadata.TurnCount,
		ToolCallCount:        s.Metadata.ToolCallCount,
		Turns:                turns,
		Source:               source,
		Status:               status,
		Project:              s.Project,
		Model:                model,
		ThinkingLevel:        thinkingLevel,
		ThinkingLevelRaw:     thinkingLevelRaw,
		ThinkingLevelHistory: sessionThinkingHistory(s),
		WorkingDirectory:     s.ProjectPath,
		GitBranch:            s.GitBranch,
		GitRemote:            s.GitRemote,
		Outcome:              outcome,
		Scorecard:            scorecard,
		Relationships:        s.Relationships,
		Purpose:              s.Purpose,
		RootSessionID:        s.RootSessionID,
		ParentSessionID:      s.ParentSessionID,
		InputSubmissionCount: s.InputSubmissionCount,
		EarlierHistory:       earlierHistoryToDetail(s.EarlierHistory),
	}
	if len(detail.RetainedUnknown) > 0 {
		detail.Diagnostics = &schema.InterpretationDiagnostics{Partial: true}
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

func contentPartsFromTurn(turn ingest.Turn) []schema.TurnContentPart {
	if len(turn.Parts) == 0 {
		return nil
	}
	parts := make([]schema.TurnContentPart, 0, len(turn.Parts))
	for _, part := range turn.Parts {
		switch part.Kind {
		case ingest.TurnPartText:
			parts = append(parts, schema.TurnContentPart{Type: schema.TurnPartTypeText, Text: part.Text})
		case ingest.TurnPartImage:
			image := part.Image
			if image == nil {
				image = &schema.ImagePart{Ref: part.ImageRef}
			}
			copied := *image
			parts = append(parts, schema.TurnContentPart{Type: schema.TurnPartTypeImage, Image: &copied})
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// turnsToDetail converts folded turns to wire turns. It is the single
// turn-projection site for main turns and every earlier-history partition, so
// the two cannot drift in what a turn carries.
func turnsToDetail(folded []ingest.Turn) []schema.TurnDetail {
	turns := make([]schema.TurnDetail, len(folded))
	for i, t := range folded {
		toolCalls := make([]schema.ToolCallDetail, len(t.ToolCalls))
		for j, tc := range t.ToolCalls {
			toolCalls[j] = schema.ToolCallDetail{
				CallEntryRef:     schema.SourceEntryRef(tc.CallEntryRef),
				ResultEntryRef:   schema.SourceEntryRef(tc.ResultEntryRef),
				CallProvenance:   tc.CallProvenance,
				ResultProvenance: tc.ResultProvenance,
				Usage:            tc.Usage,
				ID:               tc.ID,
				Name:             tc.Name,
				Namespace:        tc.Namespace,
				Arguments:        tc.Arguments,
				Result:           tc.Result,
				DurationMs:       tc.DurationMs,
				ExitCode:         tc.ExitCode,
				FilePath:         tc.FilePath,
				IsError:          tc.IsError,
				ToolKind:         tc.ToolKind,
			}
		}
		turns[i] = schema.TurnDetail{
			ContentParts:     contentPartsFromTurn(t),
			ControlKind:      t.ControlKind,
			SourceEntryRef:   schema.SourceEntryRef(t.SourceEntryRef),
			Provenance:       t.Provenance,
			Usage:            t.Usage,
			Index:            t.Index,
			Role:             t.Role,
			Command:          t.Command,
			Content:          t.Content,
			ToolCalls:        toolCalls,
			Timestamp:        t.Timestamp.UTC(),
			Depth:            t.Depth,
			ParentIndex:      t.ParentIndex,
			EntryType:        t.EntryType,
			HasThinking:      t.HasThinking,
			StopReason:       t.StopReason,
			TokensIn:         t.TokensIn,
			TokensOut:        t.TokensOut,
			ObservedModel:    t.ObservedModel,
			ThinkingLevel:    t.ThinkingLevel,
			ThinkingLevelRaw: t.ThinkingLevelRaw,
		}
	}
	return turns
}

// earlierHistoryToDetail converts retained earlier-history partitions to wire
// sections. Nil input stays nil so sessions without stored history omit the
// field; an explicitly empty retained set is preserved as-is by the caller.
func earlierHistoryToDetail(sections []ingest.EarlierHistorySection) []schema.EarlierHistorySection {
	if sections == nil {
		return nil
	}
	out := make([]schema.EarlierHistorySection, len(sections))
	for i, section := range sections {
		out[i] = schema.EarlierHistorySection{
			State:          section.State,
			Turns:          turnsToDetail(section.Turns),
			NativeMetadata: section.NativeMetadata,
		}
		if out[i].Turns == nil {
			out[i].Turns = []schema.TurnDetail{}
		}
	}
	return out
}
