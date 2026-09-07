package transcript

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// ProjectedTarget uses canonical entry indices, never array positions.
type ProjectedTarget struct {
	TurnIndex  int
	ToolCallID string
}

// Projection is the one fold result for local viewing and all outbound content.
type Projection struct {
	Turns          []ingest.Turn
	NativeMetadata []schema.NativeMetadataRecord
	UsageOwners    []schema.UsageDetail
	SourceMap      map[string]ProjectedTarget
}

// ProjectionOptions can require carrier agreement with the session harness.
type ProjectionOptions struct{ Harness schema.Harness }

// EntriesToProjectionValidated reconstructs evidence after SQLite reopen.
func EntriesToProjectionValidated(entries []schema.SessionEntry, opts ProjectionOptions) (Projection, error) {
	return entriesToProjection(entries, opts, true)
}

func entriesToProjection(entries []schema.SessionEntry, opts ProjectionOptions, validate bool) (Projection, error) {
	p := Projection{SourceMap: make(map[string]ProjectedTarget)}
	evidence := make(map[int]ingest.PiExtra)
	indexes := make(map[int]bool)
	for _, entry := range entries {
		extra, pi, err := ingest.DecodePiEntryExtra(entry)
		if err != nil {
			return p, err
		}
		if ingest.IsPiCarrier(entry) && (!pi || entry.Role != schema.RoleSystem || entry.EntryType != schema.EntryTypeSystem) {
			return p, projectionError("invalid carrier row")
		}
		if !pi {
			if opts.Harness == schema.HarnessPi {
				return p, projectionError("Pi session contains a row without typed source/usage evidence")
			}
			continue
		}
		if opts.Harness != "" && opts.Harness != schema.HarnessPi {
			return p, projectionError("Pi evidence on a different harness")
		}
		if entry.Harness != "" && entry.Harness != schema.HarnessPi {
			return p, projectionError("indexed row harness disagrees with Pi evidence")
		}
		if !ingest.IsPiCarrier(entry) && extra.SourceRef == "" {
			return p, projectionError("conversational Pi row lacks its source reference")
		}
		if entry.ToolCallID != nil {
			if err := ingest.ValidatePiPublicRef(*entry.ToolCallID); err != nil {
				return p, err
			}
		}
		if indexes[entry.EntryIndex] {
			return p, projectionError("duplicate indexed row")
		}
		indexes[entry.EntryIndex] = true
		evidence[entry.EntryIndex] = extra
		p.NativeMetadata = append(p.NativeMetadata, extra.Metadata...)
	}
	if validate || len(evidence) > 0 {
		if err := ValidateObservedModelEntries(entries); err != nil {
			return p, err
		}
	}
	p.Turns = foldEntries(entries, evidence)
	tools := make(map[string]*ingest.ToolCall)
	toolTargets := make(map[string]int)
	for i := range p.Turns {
		t := &p.Turns[i]
		if t.SourceEntryRef != "" {
			if _, exists := p.SourceMap[t.SourceEntryRef]; exists {
				return p, projectionError("duplicate visible source")
			}
			p.SourceMap[t.SourceEntryRef] = ProjectedTarget{TurnIndex: t.Index}
		}
		for j := range t.ToolCalls {
			tc := &t.ToolCalls[j]
			if tools[tc.ID] != nil && len(evidence) > 0 {
				return p, projectionError("duplicate tool ID")
			}
			tools[tc.ID] = tc
			toolTargets[tc.ID] = t.Index
		}
	}
	seenResults := make(map[string]bool)
	callIndexes := make(map[string]int)
	for _, entry := range entries {
		if entry.EntryType == schema.EntryTypeToolUse && entry.ToolCallID != nil {
			callIndexes[*entry.ToolCallID] = entry.EntryIndex
		}
	}
	for _, entry := range entries {
		extra, pi := evidence[entry.EntryIndex]
		if !pi {
			continue
		}
		if entry.EntryType == schema.EntryTypeThinking && entry.ParentIndex != nil {
			for i := range p.Turns {
				t := &p.Turns[i]
				if t.Index == *entry.ParentIndex && entry.ContentPreview != nil {
					if t.Content != "" {
						t.Content += "\n"
					}
					t.Content += *entry.ContentPreview
					t.HasThinking = true
				}
			}
		}
		if entry.ToolCallID == nil {
			continue
		}
		tc := tools[*entry.ToolCallID]
		if tc == nil {
			return p, projectionError("tool evidence has no surviving call")
		}
		switch entry.EntryType {
		case schema.EntryTypeToolUse:
			if entry.ParentIndex == nil || *entry.ParentIndex != toolTargets[tc.ID] {
				return p, projectionError("tool call parent disagrees with its surviving owner")
			}
			parent, ok := evidence[*entry.ParentIndex]
			if !ok || parent.SourceRef != extra.SourceRef {
				return p, projectionError("tool call source disagrees with its assistant source")
			}
			tc.CallEntryRef = extra.SourceRef
			tc.Namespace = extra.Namespace
		case schema.EntryTypeToolResult:
			callIndex, ok := callIndexes[tc.ID]
			if !ok || callIndex >= entry.EntryIndex || (entry.ParentIndex != nil && *entry.ParentIndex != toolTargets[tc.ID]) {
				return p, projectionError("backward or reattributed tool result")
			}
			if seenResults[tc.ID] {
				return p, projectionError("duplicate tool result")
			}
			seenResults[tc.ID] = true
			tc.ResultEntryRef = extra.SourceRef
			tc.Usage = extra.Usage
			if _, exists := p.SourceMap[extra.SourceRef]; exists {
				return p, projectionError("duplicate result source")
			}
			p.SourceMap[extra.SourceRef] = ProjectedTarget{TurnIndex: toolTargets[tc.ID], ToolCallID: tc.ID}
		}
	}
	metadataBytes := 0
	for _, record := range p.NativeMetadata {
		metadataBytes += len(record.Data)
	}
	if len(p.NativeMetadata) > 256 || metadataBytes > 1<<20 {
		return p, projectionError("metadata exceeds the raw aggregate budget before sanitization")
	}
	for i := range p.NativeMetadata {
		m := &p.NativeMetadata[i]
		data, err := ingest.SanitizePiMetadataData(m.Data, nil)
		if err != nil {
			return p, err
		}
		m.Data = data
		if m.Kind == schema.NativeMetadataPiCustomData {
			if _, exists := p.SourceMap[m.Source.EntryRef]; exists {
				return p, projectionError("plain custom metadata collides with a conversational source")
			}
			continue
		}
		target, ok := p.SourceMap[m.Source.EntryRef]
		if !ok {
			return p, projectionError("metadata source has no surviving target")
		}
		m.Attachment = &schema.NativeAttachmentRef{TurnIndex: &target.TurnIndex, ToolCallID: target.ToolCallID}
	}
	// Evidence can live on a suppressed row or a trailing carrier. Resolve its
	// source to the surviving turn/result rather than dropping that owner.
	byIndex := make(map[int]*ingest.Turn, len(p.Turns))
	for i := range p.Turns {
		byIndex[p.Turns[i].Index] = &p.Turns[i]
	}
	for _, extra := range evidence {
		if extra.Usage == nil {
			continue
		}
		target, ok := p.SourceMap[extra.Usage.SourceEntryRef]
		if !ok {
			return p, projectionError("usage carrier source has no surviving owner target")
		}
		if target.ToolCallID != "" {
			tools[target.ToolCallID].Usage = extra.Usage
		} else {
			byIndex[target.TurnIndex].Usage = extra.Usage
		}
	}
	for _, entry := range entries {
		extra, pi := evidence[entry.EntryIndex]
		if !pi || ingest.IsPiCarrier(entry) {
			continue
		}
		if (entry.Role == schema.RoleAssistant && entry.Depth == 0) || entry.EntryType == schema.EntryTypeToolResult {
			target, ok := p.SourceMap[extra.SourceRef]
			if !ok {
				return p, projectionError("eligible usage source was suppressed")
			}
			if target.ToolCallID != "" {
				if tools[target.ToolCallID].Usage == nil {
					return p, projectionError("tool result lacks an unknown-or-recorded usage owner")
				}
			} else if byIndex[target.TurnIndex].Usage == nil {
				return p, projectionError("assistant lacks an unknown-or-recorded usage owner")
			}
		}
	}
	for _, turn := range p.Turns {
		if turn.Usage != nil {
			p.UsageOwners = append(p.UsageOwners, *turn.Usage)
		}
		for _, tool := range turn.ToolCalls {
			if tool.Usage != nil {
				p.UsageOwners = append(p.UsageOwners, *tool.Usage)
			}
		}
	}
	ownerCount := 0
	for _, extra := range evidence {
		if extra.Usage != nil {
			ownerCount++
		}
	}
	if ownerCount != len(p.UsageOwners) {
		return p, projectionError("fold would discard or duplicate a usage owner")
	}
	// Validate the full public matrix once all target references are resolved.
	s := &ingest.Session{Harness: schema.HarnessPi, Turns: p.Turns, NativeMetadata: p.NativeMetadata}
	if len(evidence) > 0 {
		if err := schema.ValidateNativeMetadataRecords(p.NativeMetadata, sessionToDetail(s).Turns); err != nil {
			return p, err
		}
	}
	return p, nil
}

// SessionToDetailValidatedWithProjection retains nonconversational evidence.
func SessionToDetailValidatedWithProjection(session *ingest.Session, p Projection) (*schema.SessionDetailPayload, error) {
	copy := *session
	copy.Turns = p.Turns
	copy.NativeMetadata = p.NativeMetadata
	return SessionToDetailValidated(&copy)
}

func projectionError(reason string) error {
	return fmt.Errorf("transcript projection failed: %s; indexed Pi evidence is inconsistent in transcript.EntriesToProjectionValidated during storage read; nothing was emitted; re-index the session from a valid source and retry", reason)
}
