package ingest

import (
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
)

// RecordKindEntryMode states what a classified source node contributes. The
// mode is local reporting/lowering policy; it never admits parser input.
type RecordKindEntryMode uint8

const (
	RecordKindEntryModeNone RecordKindEntryMode = iota
	RecordKindEntryModeRepresented
	RecordKindEntryModeRetainedEvidence
)

func (m RecordKindEntryMode) String() string {
	switch m {
	case RecordKindEntryModeNone:
		return "none"
	case RecordKindEntryModeRepresented:
		return "represented"
	case RecordKindEntryModeRetainedEvidence:
		return "retained-evidence"
	default:
		return "invalid"
	}
}

// RecordKindCoordinateRequirement states whether retention must preserve the
// source coordinates needed to put opaque evidence back in transcript order.
type RecordKindCoordinateRequirement uint8

const (
	RecordKindCoordinatesNone RecordKindCoordinateRequirement = iota
	RecordKindCoordinatesRequired
)

func (r RecordKindCoordinateRequirement) String() string {
	if r == RecordKindCoordinatesRequired {
		return "required"
	}
	return "none"
}

type recordKindProfile struct {
	Anchor      string
	Outcome     indexformat.Outcome
	Status      RecordKindStatus
	Preview     RecordKindPreview
	Payload     string
	Reason      string
	EntryMode   RecordKindEntryMode
	Coordinates RecordKindCoordinateRequirement
}

var recordKindOutcomeProfiles = map[indexformat.Outcome]recordKindProfile{
	indexformat.OutcomeText: {
		Anchor: "text", Outcome: indexformat.OutcomeText,
		Status: RecordKindRepresented, Preview: RecordKindPreviewYes, Payload: "session entries",
		EntryMode: RecordKindEntryModeRepresented,
	},
	indexformat.OutcomeToolCall: {
		Anchor: "tool", Outcome: indexformat.OutcomeToolCall,
		Status: RecordKindRepresented, Preview: RecordKindPreviewNo, Payload: "tool name and arguments",
		EntryMode: RecordKindEntryModeRepresented,
	},
	indexformat.OutcomeToolResult: {
		Anchor: "result", Outcome: indexformat.OutcomeToolResult,
		Status: RecordKindRepresented, Preview: RecordKindPreviewYes, Payload: "tool output",
		EntryMode: RecordKindEntryModeRepresented,
	},
	indexformat.OutcomeControl: {
		Anchor: "control", Outcome: indexformat.OutcomeControl,
		Status: RecordKindRepresented, Preview: RecordKindPreviewYes, Payload: "bounded control extra (oversized known controls retain identity only)",
		EntryMode: RecordKindEntryModeRepresented,
	},
	indexformat.OutcomeIgnored: {
		Anchor: "ignored", Outcome: indexformat.OutcomeIgnored,
		Status: RecordKindIgnoredControl, Preview: RecordKindPreviewNo, Payload: "none", Reason: "Entryless control; unexpected conversation content remains a validation error.",
		EntryMode: RecordKindEntryModeNone,
	},
	indexformat.OutcomeOpaque: {
		Outcome: indexformat.OutcomeOpaque,
		Status:  RecordKindRetainedUnknown, Preview: RecordKindPreviewNo, Payload: "complete raw JSON and source coordinates in retainedUnknown", Reason: "Retain uninterpreted evidence and mark partial interpretation.",
		EntryMode: RecordKindEntryModeRetainedEvidence, Coordinates: RecordKindCoordinatesRequired,
	},
}

func structuralRecordKindProfile() recordKindProfile {
	return recordKindProfile{
		Anchor: "structural", Outcome: indexformat.OutcomeIgnored,
		Status: RecordKindRepresented, Preview: RecordKindPreviewNo, Payload: "state on owning entry; no independent row",
		EntryMode: RecordKindEntryModeNone,
	}
}

// recordKindAnchorShapes maps each shared generated-YAML anchor to the stored
// shape it always carries. The table is built from the same profiles that lower
// the rows, so it cannot drift from them. The generator declares an anchor on
// the first row that matches the anchor's shape and writes a row overriding any
// part of that shape in full: an anchor declared on an overriding row would
// silently retag every later row of the same harness that merges it.
var recordKindAnchorShapes = buildRecordKindAnchorShapes()

func buildRecordKindAnchorShapes() map[string]RecordKind {
	shapes := make(map[string]RecordKind)
	record := func(profile recordKindProfile) {
		if profile.Anchor == "" {
			return
		}
		shapes[profile.Anchor] = RecordKind{
			Status:  profile.Status,
			Preview: profile.Preview,
			Payload: profile.Payload,
			Reason:  profile.Reason,
		}
	}
	for _, profile := range recordKindOutcomeProfiles {
		record(profile)
	}
	record(structuralRecordKindProfile())
	record(piCarrierProfile(recordKindProfile{}))
	record(codexNativeItemProfile(recordKindProfile{}))
	record(codexMediaProfile(recordKindProfile{}))
	record(codexDiagnosticProfile(recordKindProfile{}))
	return shapes
}

// recordKindShapeMatches reports whether a lowered row carries exactly the shape
// its shared anchor is defined to carry.
func recordKindShapeMatches(kind, shape RecordKind) bool {
	return kind.Status == shape.Status && kind.Preview == shape.Preview && kind.Payload == shape.Payload && kind.Reason == shape.Reason
}

func lowerRecordKindRule(rule recordKindRule) RecordKind {
	profile := recordKindProfileFor(rule)
	return RecordKind{
		Context: rule.Context, Namespace: rule.Namespace, Kind: rule.Kind, Match: rule.Match,
		Status: profile.Status, Preview: profile.Preview, Payload: profile.Payload,
		Reason: profile.Reason, Outcome: profile.Outcome,
		EntryMode: profile.EntryMode, Coordinates: profile.Coordinates,
	}
}

func lowerRecordKindFallback(context RecordKindContext, namespace, kind string) RecordKind {
	profile := recordKindOutcomeProfiles[indexformat.OutcomeOpaque]
	return RecordKind{
		Context: context, Namespace: namespace, Kind: kind,
		Status: profile.Status, Preview: profile.Preview, Payload: profile.Payload,
		Reason: profile.Reason, Source: "internal/ingest/retained_unknown.go NewRetainedUnknown", Outcome: profile.Outcome,
		EntryMode: profile.EntryMode, Coordinates: profile.Coordinates,
	}
}

func recordKindProfileFor(rule recordKindRule) recordKindProfile {
	if rule.Outcome == indexformat.OutcomeOpaque {
		return recordKindOutcomeProfiles[indexformat.OutcomeOpaque]
	}
	if isStructuralRecordKind(rule) {
		return structuralRecordKindProfile()
	}
	base := recordKindOutcomeProfiles[rule.Outcome]
	key := RecordKindKey{Context: rule.Context, Namespace: rule.Namespace, Kind: rule.Kind, Match: rule.Match}
	switch {
	case key == (RecordKindKey{RecordKindRetained, "record", "attachment", RecordKindLiteral}):
		base.Status = RecordKindTrackedOnly
		base.Preview = RecordKindPreviewNo
		base.Payload = "bounded control extra"
		base.Anchor = ""
	case key == (RecordKindKey{RecordKindRetained, "system_subtype", "compact_boundary", RecordKindLiteral}):
		base.Payload = "compactMetadata"
	case rule.Namespace == "entry" && rule.Kind == "session":
		base.Reason = "Required v3 document header; no conversation row."
	case rule.Namespace == "entry" && rule.Kind == "message":
		base.Payload = "PiExtra state and usage; role/block dependent content"
	case rule.Namespace == "entry" && (rule.Kind == "compaction" || rule.Kind == "branch_summary"):
		base.Payload = "summary and PiExtra usage/native metadata"
	case rule.Namespace == "entry" && rule.Kind == "custom_message":
		base.Payload = "text/media and PiExtra native metadata"
	case rule.Namespace == "entry" && (rule.Kind == "thinking_level_change" || rule.Kind == "model_change" || rule.Kind == "label" || rule.Kind == "session_info"):
		base = piCarrierProfile(base)
	case rule.Namespace == "entry" && rule.Kind == "custom":
		base = piCustomCarrierProfile(base)
	case rule.Namespace == "message_role" && rule.Kind == "bashExecution":
		base.Payload = "shell parent and paired execute tool entries"
	case rule.Namespace == "content_block" && rule.Kind == "image":
		base.Payload = "textual media marker"
	case rule.Context == RecordKindNative && rule.Namespace == "item_body" && isCodexClassifiedNativeItemKind(rule.Kind):
		base = codexNativeItemProfile(base)
	case rule.Context == RecordKindNative && rule.Namespace == "message_block" && codexMediaContentTypes()[rule.Kind]:
		base = codexMediaProfile(base)
	case rule.Context == RecordKindNative && rule.Namespace == "content_kind":
		attribution := codexContentKindRegistry()[rule.Kind]
		switch {
		case attribution.mediaDiagnostic:
			base = codexDiagnosticProfile(base)
		case rule.Kind == "user.image" || rule.Kind == "user.audio":
			base = codexMediaProfile(base)
		}
	case rule.Namespace == "event" && (rule.Kind == "user_message" || rule.Kind == "agent_message" || rule.Kind == "agent_reasoning"):
		base.Reason = "Mirrored content is represented by response items."
	case rule.Context == RecordKindNative && rule.Namespace == "event" && rule.Kind == "token_count":
		base.Reason = "Metadata event; no conversation row."
	case rule.Namespace == "tool_content" && rule.Kind == "file":
		base.Preview = RecordKindPreviewNo
		base.Payload = "structured tool output URI/MIME"
		base.Anchor = ""
	}
	return base
}

func piCarrierProfile(base recordKindProfile) recordKindProfile {
	base.Anchor = "carrier"
	base.Status = RecordKindTrackedOnly
	base.Preview = RecordKindPreviewNo
	base.Payload = "PiExtra state carrier"
	return base
}

func piCustomCarrierProfile(base recordKindProfile) recordKindProfile {
	base.Anchor = "carrier"
	base.Status = RecordKindTrackedOnly
	base.Preview = RecordKindPreviewNo
	base.Payload = "PiExtra native metadata"
	return base
}

func isCodexClassifiedNativeItemKind(kind string) bool {
	switch kind {
	case "UserMessage", "AgentMessage", "Reasoning", "FunctionCallOutput":
		return false
	default:
		_, ok := codexCanonicalNativeItemTypes[kind]
		return ok
	}
}

func codexNativeItemProfile(base recordKindProfile) recordKindProfile {
	base.Anchor = "nativeitem"
	base.Status = RecordKindRepresented
	base.Preview = RecordKindPreviewYes
	base.Payload = "classified native item and provenance"
	return base
}

func codexMediaProfile(base recordKindProfile) recordKindProfile {
	base.Anchor = "media"
	base.Status = RecordKindRepresented
	base.Preview = RecordKindPreviewYes
	base.Payload = "media modality and textual marker"
	return base
}

func codexDiagnosticProfile(base recordKindProfile) recordKindProfile {
	base.Anchor = "diagnostic"
	base.Status = RecordKindRepresented
	base.Preview = RecordKindPreviewYes
	base.Payload = "diagnostic text and provenance; no standalone submitted-input count without a media sibling"
	return base
}

func isStructuralRecordKind(rule recordKindRule) bool {
	if rule.Outcome != indexformat.OutcomeIgnored {
		return false
	}
	if rule.Context == RecordKindNative {
		if rule.Namespace == "envelope" {
			return true
		}
		if rule.Namespace == "event" {
			switch rule.Kind {
			case "token_count", "user_message", "agent_message", "agent_reasoning":
				return false
			default:
				return true
			}
		}
	}
	return rule.Namespace == "record" && (rule.Kind == "turn.started" || rule.Kind == "turn.completed" || rule.Kind == "usage.reported") ||
		rule.Context == RecordKindRetained && rule.Namespace == "envelope" && (rule.Kind == "event_msg" || rule.Kind == "response_item")
}

// recordKindSourceLabel is used only in generated report metadata. The
// production census is compared structurally by the vocabulary tests instead.
func recordKindSourceLabel(inventory RecordKindInventory) string {
	labels := make([]string, 0, len(inventory.Sources))
	for _, source := range inventory.Sources {
		labels = append(labels, strings.TrimSpace(source.File+" "+source.Symbol))
	}
	return strings.Join(labels, "; ")
}
