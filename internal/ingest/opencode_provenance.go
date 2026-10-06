package ingest

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// OpenCodeProvenanceShape names the native shape a classified OpenCode message
// was decoded from. The shape decides the evidence strength the classifier may
// claim: only the current storage shapes carry the typed discriminator,
// sequence identity and explicit parent null the admission rule requires. The
// legacy and semantic shapes are decoded by their existing readers and keep
// that reader's authority.
type OpenCodeProvenanceShape string

const (
	// OpenCodeProvenanceCurrent is one session_message row with the pinned
	// current data schema (type, id, seq columns plus data payload).
	OpenCodeProvenanceCurrent OpenCodeProvenanceShape = "current"
	// OpenCodeProvenanceV2 is one session_message row with the newer V2 data
	// schema (shellID envelopes, skill messages, structured tool state).
	OpenCodeProvenanceV2 OpenCodeProvenanceShape = "v2"
	// OpenCodeProvenanceLegacy is one legacy message/part row pair without a
	// type or sequence column.
	OpenCodeProvenanceLegacy OpenCodeProvenanceShape = "legacy"
	// OpenCodeProvenanceSemantic is one file-tree message with its part files.
	OpenCodeProvenanceSemantic OpenCodeProvenanceShape = "semantic"
)

// OpenCodeProvenanceFile is one native file attached to a user message. The
// display text follows the same precedence the row normalizer uses (name,
// then URI, then description) so the classified media block shows what the
// retained transcript shows.
type OpenCodeProvenanceFile struct {
	Name        string
	URI         string
	Description string
}

// DisplayText reports the media label the retained transcript shows.
func (f OpenCodeProvenanceFile) DisplayText() string {
	if f.Name != "" {
		return f.Name
	}
	if f.URI != "" {
		return f.URI
	}
	return f.Description
}

// OpenCodeProvenanceAssistantPart is one decoded assistant content part with
// its native identity preserved. The part ID is the tool correlation identity
// the shared projection remaps after layout; it is never derived from text.
type OpenCodeProvenanceAssistantPart struct {
	ID            string
	Kind          string // text, reasoning, or tool
	Text          string
	ToolName      string
	ToolInput     string // full serialized arguments
	ToolResult    string // full result bytes
	ToolCompleted bool
}

// OpenCodeProvenanceMessage is one natively decoded OpenCode message plus the
// envelope facts the section 3.2 table decides on. Decoders for each shape
// populate it; the classifier never reparses native bytes itself, so the
// envelope always names the evidence the decoder actually proved.
type OpenCodeProvenanceMessage struct {
	RetainedUnknown []RetainedUnknown `json:"retainedUnknown,omitempty"`
	MessageID       string
	SessionID       string
	Shape           OpenCodeProvenanceShape
	// NativeType is the current storage discriminator (user, assistant, shell,
	// synthetic, system, skill, compaction, agent-switched, model-switched, or
	// a newer value). Empty for legacy and semantic shapes, which have no
	// discriminator column.
	NativeType    string
	Seq           int64
	HasSeq        bool
	TimeCreated   int64
	TimeCompleted int64
	// ParentNullProven is true only when the native session row explicitly
	// carries a null parent_id. A missing parent column, an unread row, or a
	// file-tree session leaves it false: admission needs proven null, never
	// absent evidence.
	ParentNullProven bool
	// HasParent names a present native parent_id. It proves started_by only;
	// it never proves a copied prefix, an author, or a delivery.
	HasParent bool
	// AgentDelivered is true only when the caller correlated this message to
	// persisted subagent tool target/prompt delivery. Session-level agent
	// labels alone never set it: delivery needs per-input proof.
	AgentDelivered bool
	// User payload (NativeType user).
	Text          string
	Files         []OpenCodeProvenanceFile
	AgentMentions []string
	// SkillTexts carries native skill attachment prose. Skill instructions are
	// harness context even when they ride inside a user message; the flag
	// exists so the classifier never mistakes them for user prose.
	SkillTexts []string
	// Shell payload (NativeType shell without a V2 shellID envelope).
	ShellCallID    string
	ShellCommand   string
	ShellOutput    string
	ShellCompleted bool
	// HasShellID marks the V2 tool-form shell envelope. A tool-form shell is a
	// tool invocation, not a native user action.
	HasShellID bool
	// Parts carries assistant content in source order.
	Parts []OpenCodeProvenanceAssistantPart
	// Compaction payload (NativeType compaction).
	CompactionSummary   string
	CompactionCompleted bool
	// SystemText carries synthetic, system, skill, and control prose.
	SystemText string
	// AllPartsSynthetic marks a legacy or semantic message whose every part
	// carries the native synthetic flag: harness annotation, not a person's
	// words, even when the role column reads user.
	AllPartsSynthetic bool
	// Role is the legacy/semantic role fallback when no discriminator exists.
	Role schema.Role
}

// OpenCodeMessageAttribution overlays fork-history ownership on one message.
// The history materializer computes it from settled copy proof; the classifier
// applies it without changing the independently established origin or actor.
type OpenCodeMessageAttribution struct {
	Ownership schema.ContentOwnership
	// Inherited marks a settled copied row: delivery becomes inherited_context
	// while origin and actor stay as the native rule established.
	Inherited bool
	// UncertainCopy marks a retained copy whose proof did not survive (the
	// parent payload is gone). The caller must place such blocks in a declared
	// earlier section; the classifier flags them so the builder can enforce it.
	UncertainCopy bool
}

// LocalOpenCodeAttribution is the default attribution for a session's own
// rows with no fork evidence.
func LocalOpenCodeAttribution() OpenCodeMessageAttribution {
	return OpenCodeMessageAttribution{Ownership: schema.ContentOwnershipLocal}
}

// openCodeProvenanceEvidence maps a shape to the evidence kind its decoder may
// claim. Current shapes prove typed admission facts; legacy and semantic
// shapes keep their existing reader's authority; anything else is unknown.
func openCodeProvenanceEvidence(shape OpenCodeProvenanceShape) schema.EvidenceKind {
	switch shape {
	case OpenCodeProvenanceCurrent, OpenCodeProvenanceV2:
		return schema.EvidenceNativeTyped
	case OpenCodeProvenanceLegacy, OpenCodeProvenanceSemantic:
		return schema.EvidenceExistingAdapter
	default:
		return schema.EvidenceUnknown
	}
}

// openCodeNativeKey builds the bounded opaque alias key for one classified
// block. Keys encode identity and slot only; raw private values never enter.
func openCodeNativeKey(shape OpenCodeProvenanceShape, messageID, slot string) (string, error) {
	if strings.TrimSpace(messageID) == "" {
		return "", fmt.Errorf("ingest.ClassifyOpenCodeMessage: message identity is empty; the block cannot be aliased across refreshes; supply the native message id")
	}
	key := "opencode:" + string(shape) + ":" + messageID + ":" + slot
	if len(key) > indexformat.MaxNativeAliasKeyBytes {
		return "", fmt.Errorf("ingest.ClassifyOpenCodeMessage: native key for message %q slot %q is %d bytes over the %d-byte bound; alias keys must stay bounded; shorten the native identity", messageID, slot, len(key), indexformat.MaxNativeAliasKeyBytes)
	}
	return key, nil
}

// openCodeSubmissionKey builds the native acceptance identity for one admitted
// message. Text and media blocks of the same message share it, so one prompt
// with text and an image counts once.
func openCodeSubmissionKey(sessionID, messageID string) string {
	return "opencode-accept:" + sessionID + ":" + messageID
}

// ClassifyOpenCodeMessage maps one natively decoded OpenCode message to its
// classified blocks following the section 3.2 table. It proves admission from
// an explicit parent null on a typed user row, never from a role, a timestamp,
// or a bare type; it vetoes counting on correlated subagent delivery; and it
// leaves child delivery unknown without per-input proof. Injected skill text
// inside a user message classifies as harness context, never as user prose.
// Unsupported discriminators keep their content with unknown provenance; the
// classifier never repairs them by text inference and never invents a
// synthetic=false flag.
func ClassifyOpenCodeMessage(msg OpenCodeProvenanceMessage, attr OpenCodeMessageAttribution) ([]ClassifiedBlock, error) {
	if strings.TrimSpace(msg.MessageID) == "" {
		return nil, fmt.Errorf("ingest.ClassifyOpenCodeMessage: message identity is empty; the block cannot be attributed; supply the native message id")
	}
	if strings.TrimSpace(msg.SessionID) == "" {
		return nil, fmt.Errorf("ingest.ClassifyOpenCodeMessage: session identity is empty for message %q; the block cannot be scoped; supply the native session id", msg.MessageID)
	}
	if !attr.Ownership.IsValid() {
		return nil, fmt.Errorf("ingest.ClassifyOpenCodeMessage: message %q ownership %q is outside the closed set; retained history cannot be explained; use a published ownership", msg.MessageID, attr.Ownership)
	}
	if msg.Shape != OpenCodeProvenanceCurrent && msg.Shape != OpenCodeProvenanceV2 &&
		msg.Shape != OpenCodeProvenanceLegacy && msg.Shape != OpenCodeProvenanceSemantic {
		return nil, fmt.Errorf("ingest.ClassifyOpenCodeMessage: message %q shape %q is outside the closed set; the evidence strength is unknown; use current, v2, legacy, or semantic", msg.MessageID, msg.Shape)
	}
	var blocks []ClassifiedBlock
	var err error
	switch msg.Shape {
	case OpenCodeProvenanceCurrent, OpenCodeProvenanceV2:
		blocks, err = classifyOpenCodeTypedMessage(msg, attr)
	default:
		blocks, err = classifyOpenCodeHistoricalMessage(msg, attr)
	}
	if err != nil {
		return nil, err
	}
	if len(msg.RetainedUnknown) > 0 {
		if err := validateOpenCodeUnknown(msg.RetainedUnknown); err != nil {
			return nil, err
		}
		key, err := openCodeNativeKey(msg.Shape, msg.MessageID, "retained-unknown")
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, ClassifiedBlock{NativeKey: key, Uncertain: attr.UncertainCopy, Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem, RetainedUnknown: msg.RetainedUnknown, TimestampMs: nonEmptyTimestamp(msg.TimeCreated), Provenance: &schema.ContentProvenance{Origin: schema.ContentOriginUnknown, Actor: schema.ActorOriginUnknown, Delivery: schema.DeliveryOriginUnknown, Ownership: attr.Ownership, Evidence: schema.EvidenceNativeTyped, InputModality: schema.InputModalityNone}})
	}
	return blocks, nil
}

// classifyOpenCodeTypedMessage applies the current-shape rows of the section
// 3.2 table: explicit discriminators with sequence identity.
func classifyOpenCodeTypedMessage(msg OpenCodeProvenanceMessage, attr OpenCodeMessageAttribution) ([]ClassifiedBlock, error) {
	if len(msg.RetainedUnknown) > 0 && !knownOpenCodeCurrentRow(msg.NativeType) {
		return nil, nil
	}
	switch msg.NativeType {
	case "user":
		return classifyOpenCodeTypedUser(msg, attr)
	case "shell":
		if msg.HasShellID {
			return classifyOpenCodeToolFormShell(msg, attr)
		}
		return classifyOpenCodeShellAction(msg, attr)
	case "synthetic", "system", "skill":
		return singleOpenCodeBlock(msg, attr, schema.RoleSystem, schema.EntryTypeSystem, msg.SystemText, openCodeHarnessContext())
	case "compaction":
		if msg.CompactionCompleted {
			return singleOpenCodeBlock(msg, attr, schema.RoleSystem, schema.EntryTypeSystem, msg.CompactionSummary, openCodeGeneratedSummary())
		}
		return singleOpenCodeBlock(msg, attr, schema.RoleSystem, schema.EntryTypeSystem, msg.CompactionSummary, openCodeSystemControl())
	case "agent-switched", "model-switched":
		return singleOpenCodeBlock(msg, attr, schema.RoleSystem, schema.EntryTypeSystem, msg.SystemText, openCodeSystemControl())
	case "assistant":
		return classifyOpenCodeAssistant(msg, attr)
	default:
		return singleOpenCodeBlock(msg, attr, schema.RoleSystem, schema.EntryTypeSystem, msg.SystemText, openCodeUnknownProvenance())
	}
}

// openCodeAdmission decides the delivery and submission identity for one typed
// user or shell block. Admission is a proven direct session input route, never
// a person proof: it needs an explicit parent null with no conflicting typed
// delivery evidence. Correlated subagent delivery vetoes it; any other child
// position without per-input proof stays unknown.
func openCodeAdmission(msg OpenCodeProvenanceMessage, attr OpenCodeMessageAttribution) (schema.DeliveryOrigin, schema.ActorOrigin, string) {
	if attr.Inherited || attr.Ownership != schema.ContentOwnershipLocal {
		return schema.DeliveryOriginInheritedContext, schema.ActorOriginUnknown, ""
	}
	if msg.AgentDelivered {
		return schema.DeliveryOriginSubagentDelivery, schema.ActorOriginAgentDelegate, ""
	}
	if msg.ParentNullProven && !msg.HasParent {
		return schema.DeliveryOriginSessionAdmission, schema.ActorOriginUnknown, openCodeSubmissionKey(msg.SessionID, msg.MessageID)
	}
	return schema.DeliveryOriginUnknown, schema.ActorOriginUnknown, ""
}

// openCodeShellAdmission decides the delivery and submission identity for one
// native shell action. A settled local shell row carries its own positive
// user-action evidence (section 3.2): it counts once without needing the chat
// parent-null admission, even in a child session. Inherited copy evidence and
// correlated subagent delivery still veto the count, and the actor stays
// unknown because a native action is not a person proof.
func openCodeShellAdmission(msg OpenCodeProvenanceMessage, attr OpenCodeMessageAttribution) (schema.DeliveryOrigin, schema.ActorOrigin, string) {
	if attr.Inherited || attr.Ownership != schema.ContentOwnershipLocal {
		return schema.DeliveryOriginInheritedContext, schema.ActorOriginUnknown, ""
	}
	if msg.AgentDelivered {
		return schema.DeliveryOriginSubagentDelivery, schema.ActorOriginAgentDelegate, ""
	}
	return schema.DeliveryOriginSessionAdmission, schema.ActorOriginUnknown, openCodeSubmissionKey(msg.SessionID, msg.MessageID)
}

// classifyOpenCodeTypedUser classifies one typed user row: text and media keep
// separate refs under one message submission ref, while native skill and agent
// attachments stay harness context with no submission of their own.
func classifyOpenCodeTypedUser(msg OpenCodeProvenanceMessage, attr OpenCodeMessageAttribution) ([]ClassifiedBlock, error) {
	delivery, actor, submission := openCodeAdmission(msg, attr)
	evidence := openCodeProvenanceEvidence(msg.Shape)
	var blocks []ClassifiedBlock
	if msg.Text != "" || (len(msg.Files) == 0 && len(msg.SkillTexts) == 0 && len(msg.AgentMentions) == 0) {
		key, err := openCodeNativeKey(msg.Shape, msg.MessageID, "text")
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, ClassifiedBlock{
			NativeKey:     key,
			SubmissionKey: submission,
			Role:          schema.RoleUser,
			EntryType:     schema.EntryTypeText,
			Content:       msg.Text,
			Provenance: &schema.ContentProvenance{
				Origin:        schema.ContentOriginSubmittedInput,
				Actor:         actor,
				Delivery:      delivery,
				Ownership:     attr.Ownership,
				Evidence:      evidence,
				InputModality: schema.InputModalityText,
			},
			TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
		})
	}
	for i, file := range msg.Files {
		key, err := openCodeNativeKey(msg.Shape, msg.MessageID, fmt.Sprintf("file-%d", i))
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, ClassifiedBlock{
			NativeKey:     key,
			SubmissionKey: submission,
			Role:          schema.RoleUser,
			EntryType:     schema.EntryTypeText,
			Content:       file.DisplayText(),
			Provenance: &schema.ContentProvenance{
				Origin:        schema.ContentOriginSubmittedInput,
				Actor:         actor,
				Delivery:      delivery,
				Ownership:     attr.Ownership,
				Evidence:      evidence,
				InputModality: schema.InputModalityMedia,
			},
			TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
		})
	}
	for i, skill := range msg.SkillTexts {
		key, err := openCodeNativeKey(msg.Shape, msg.MessageID, fmt.Sprintf("skill-%d", i))
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, ClassifiedBlock{
			NativeKey: key,
			Role:      schema.RoleSystem,
			EntryType: schema.EntryTypeSystem,
			Content:   skill,
			Provenance: &schema.ContentProvenance{
				Origin:        schema.ContentOriginHarnessContext,
				Actor:         schema.ActorOriginHarness,
				Delivery:      schema.DeliveryOriginSystemLifecycle,
				Ownership:     attr.Ownership,
				Evidence:      evidence,
				InputModality: schema.InputModalityText,
			},
			TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
		})
	}
	for i, agent := range msg.AgentMentions {
		key, err := openCodeNativeKey(msg.Shape, msg.MessageID, fmt.Sprintf("agent-%d", i))
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, ClassifiedBlock{
			NativeKey: key,
			Role:      schema.RoleSystem,
			EntryType: schema.EntryTypeSystem,
			Content:   agent,
			Provenance: &schema.ContentProvenance{
				Origin:        schema.ContentOriginHarnessContext,
				Actor:         schema.ActorOriginHarness,
				Delivery:      schema.DeliveryOriginSystemLifecycle,
				Ownership:     attr.Ownership,
				Evidence:      evidence,
				InputModality: schema.InputModalityText,
			},
			TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
		})
	}
	if attr.UncertainCopy {
		markOpenCodeUncertain(blocks)
	}
	return blocks, nil
}

// classifyOpenCodeShellAction classifies one native shell row as a user action:
// the command counts once when admission holds, and output prose never seeds a
// title. The output rides a second block with no submission of its own so the
// action still counts exactly once while no output byte is lost.
func classifyOpenCodeShellAction(msg OpenCodeProvenanceMessage, attr OpenCodeMessageAttribution) ([]ClassifiedBlock, error) {
	delivery, actor, submission := openCodeShellAdmission(msg, attr)
	evidence := openCodeProvenanceEvidence(msg.Shape)
	key, err := openCodeNativeKey(msg.Shape, msg.MessageID, "shell")
	if err != nil {
		return nil, err
	}
	blocks := []ClassifiedBlock{{
		NativeKey:     key,
		SubmissionKey: submission,
		Role:          schema.RoleUser,
		EntryType:     schema.EntryTypeText,
		Content:       msg.ShellCommand,
		Provenance: &schema.ContentProvenance{
			Origin:        schema.ContentOriginSubmittedInput,
			Actor:         actor,
			Delivery:      delivery,
			Ownership:     attr.Ownership,
			Evidence:      evidence,
			InputModality: schema.InputModalityUserAction,
		},
		TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
	}}
	if msg.ShellOutput != "" {
		outputKey, err := openCodeNativeKey(msg.Shape, msg.MessageID, "shell-output")
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, ClassifiedBlock{
			NativeKey: outputKey,
			Role:      schema.RoleAssistant,
			EntryType: schema.EntryTypeText,
			Content:   msg.ShellOutput,
			Provenance: &schema.ContentProvenance{
				Origin:        schema.ContentOriginAgentOutput,
				Actor:         schema.ActorOriginUnknown,
				Delivery:      delivery,
				Ownership:     attr.Ownership,
				Evidence:      evidence,
				InputModality: schema.InputModalityNone,
			},
			TimestampMs: nonEmptyTimestamp(msg.TimeCompleted),
		})
	}
	if attr.UncertainCopy {
		markOpenCodeUncertain(blocks)
	}
	return blocks, nil
}

// classifyOpenCodeToolFormShell classifies the V2 shellID envelope as a tool
// invocation pair under an empty assistant carrier: it is tool activity, not a
// native user action, so it never carries a submission.
func classifyOpenCodeToolFormShell(msg OpenCodeProvenanceMessage, attr OpenCodeMessageAttribution) ([]ClassifiedBlock, error) {
	evidence := openCodeProvenanceEvidence(msg.Shape)
	ownership := attr.Ownership
	delivery := openCodeLifecycleDelivery(attr)
	carrierKey, err := openCodeNativeKey(msg.Shape, msg.MessageID, "carrier")
	if err != nil {
		return nil, err
	}
	callKey := msg.ShellCallID
	if callKey == "" {
		callKey = msg.MessageID
	}
	useKey, err := openCodeNativeKey(msg.Shape, msg.MessageID, "tool-"+callKey)
	if err != nil {
		return nil, err
	}
	blocks := []ClassifiedBlock{
		{
			NativeKey: carrierKey,
			Role:      schema.RoleAssistant,
			EntryType: schema.EntryTypeText,
			Provenance: &schema.ContentProvenance{
				Origin:        schema.ContentOriginToolActivity,
				Actor:         schema.ActorOriginUnknown,
				Delivery:      delivery,
				Ownership:     ownership,
				Evidence:      evidence,
				InputModality: schema.InputModalityNone,
			},
			TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
		},
		{
			NativeKey:        useKey,
			Depth:            1,
			CarrierNativeKey: carrierKey,
			ToolCallKey:      "opencode-shell:" + callKey,
			ToolName:         "shell",
			Role:             schema.RoleAssistant,
			EntryType:        schema.EntryTypeToolUse,
			ToolArguments:    msg.ShellCommand,
			Provenance: &schema.ContentProvenance{
				Origin:        schema.ContentOriginToolActivity,
				Actor:         schema.ActorOriginUnknown,
				Delivery:      delivery,
				Ownership:     ownership,
				Evidence:      evidence,
				InputModality: schema.InputModalityNone,
			},
			TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
		},
	}
	if msg.ShellCompleted || msg.ShellOutput != "" {
		resultKey, err := openCodeNativeKey(msg.Shape, msg.MessageID, "result-"+callKey)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, ClassifiedBlock{
			NativeKey:        resultKey,
			Depth:            1,
			CarrierNativeKey: carrierKey,
			ToolCallKey:      "opencode-shell:" + callKey,
			ToolName:         "shell",
			Role:             schema.RoleTool,
			EntryType:        schema.EntryTypeToolResult,
			ToolResult:       msg.ShellOutput,
			Provenance: &schema.ContentProvenance{
				Origin:        schema.ContentOriginToolActivity,
				Actor:         schema.ActorOriginUnknown,
				Delivery:      delivery,
				Ownership:     ownership,
				Evidence:      evidence,
				InputModality: schema.InputModalityNone,
			},
			TimestampMs: nonEmptyTimestamp(msg.TimeCompleted),
		})
	}
	if attr.UncertainCopy {
		markOpenCodeUncertain(blocks)
	}
	return blocks, nil
}

// classifyOpenCodeAssistant classifies one assistant row with its native part
// IDs preserved: the first text or thinking block carries later tool parts,
// further text or thinking blocks stay separate depth-0 entries so identical
// prose in both survives, and tool parts hang below the carrier by native call
// identity, never by text equality.
func classifyOpenCodeAssistant(msg OpenCodeProvenanceMessage, attr OpenCodeMessageAttribution) ([]ClassifiedBlock, error) {
	evidence := openCodeProvenanceEvidence(msg.Shape)
	ownership := attr.Ownership
	delivery := openCodeLifecycleDelivery(attr)
	var blocks []ClassifiedBlock
	carrierKey := ""
	for i, part := range msg.Parts {
		partID := part.ID
		if partID == "" {
			partID = fmt.Sprintf("slot-%d", i)
		}
		switch part.Kind {
		case "reasoning", "text":
			entryType := schema.EntryTypeText
			if part.Kind == "reasoning" {
				entryType = schema.EntryTypeThinking
			}
			key, err := openCodeNativeKey(msg.Shape, msg.MessageID, "part-"+partID)
			if err != nil {
				return nil, err
			}
			if carrierKey == "" {
				carrierKey = key
			}
			blocks = append(blocks, ClassifiedBlock{
				NativeKey: key,
				Role:      schema.RoleAssistant,
				EntryType: entryType,
				Content:   part.Text,
				Provenance: &schema.ContentProvenance{
					Origin:        schema.ContentOriginAgentOutput,
					Actor:         schema.ActorOriginUnknown,
					Delivery:      delivery,
					Ownership:     ownership,
					Evidence:      evidence,
					InputModality: schema.InputModalityNone,
				},
				TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
			})
		case "tool":
			if carrierKey == "" {
				key, err := openCodeNativeKey(msg.Shape, msg.MessageID, "carrier")
				if err != nil {
					return nil, err
				}
				carrierKey = key
				blocks = append(blocks, ClassifiedBlock{
					NativeKey: carrierKey,
					Role:      schema.RoleAssistant,
					EntryType: schema.EntryTypeText,
					Provenance: &schema.ContentProvenance{
						Origin:        schema.ContentOriginToolActivity,
						Actor:         schema.ActorOriginUnknown,
						Delivery:      delivery,
						Ownership:     ownership,
						Evidence:      evidence,
						InputModality: schema.InputModalityNone,
					},
					TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
				})
			}
			useKey, err := openCodeNativeKey(msg.Shape, msg.MessageID, "tool-"+partID)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, ClassifiedBlock{
				NativeKey:        useKey,
				Depth:            1,
				CarrierNativeKey: carrierKey,
				ToolCallKey:      "opencode-part:" + partID,
				ToolName:         part.ToolName,
				Role:             schema.RoleAssistant,
				EntryType:        schema.EntryTypeToolUse,
				ToolArguments:    part.ToolInput,
				Provenance: &schema.ContentProvenance{
					Origin:        schema.ContentOriginToolActivity,
					Actor:         schema.ActorOriginUnknown,
					Delivery:      delivery,
					Ownership:     ownership,
					Evidence:      evidence,
					InputModality: schema.InputModalityNone,
				},
				TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
			})
			if part.ToolCompleted || part.ToolResult != "" {
				resultKey, err := openCodeNativeKey(msg.Shape, msg.MessageID, "result-"+partID)
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, ClassifiedBlock{
					NativeKey:        resultKey,
					Depth:            1,
					CarrierNativeKey: carrierKey,
					ToolCallKey:      "opencode-part:" + partID,
					ToolName:         part.ToolName,
					Role:             schema.RoleTool,
					EntryType:        schema.EntryTypeToolResult,
					ToolResult:       part.ToolResult,
					Provenance: &schema.ContentProvenance{
						Origin:        schema.ContentOriginToolActivity,
						Actor:         schema.ActorOriginUnknown,
						Delivery:      delivery,
						Ownership:     ownership,
						Evidence:      evidence,
						InputModality: schema.InputModalityNone,
					},
					TimestampMs: nonEmptyTimestamp(msg.TimeCompleted),
				})
			}
		default:
			key, err := openCodeNativeKey(msg.Shape, msg.MessageID, "part-"+partID)
			if err != nil {
				return nil, err
			}
			// An unrecognized native part kind keeps its bytes with unknown
			// provenance throughout: no text-derived repair, no lifecycle
			// claim, no count.
			blocks = append(blocks, ClassifiedBlock{
				NativeKey: key,
				Role:      schema.RoleAssistant,
				EntryType: schema.EntryTypeText,
				Content:   part.Text,
				Provenance: &schema.ContentProvenance{
					Origin:        schema.ContentOriginUnknown,
					Actor:         schema.ActorOriginUnknown,
					Delivery:      schema.DeliveryOriginUnknown,
					Ownership:     ownership,
					Evidence:      schema.EvidenceUnknown,
					InputModality: schema.InputModalityUnknown,
				},
				TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
			})
		}
	}
	if len(blocks) == 0 {
		key, err := openCodeNativeKey(msg.Shape, msg.MessageID, "empty")
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, ClassifiedBlock{
			NativeKey: key,
			Role:      schema.RoleAssistant,
			EntryType: schema.EntryTypeText,
			Provenance: &schema.ContentProvenance{
				Origin:        schema.ContentOriginAgentOutput,
				Actor:         schema.ActorOriginUnknown,
				Delivery:      delivery,
				Ownership:     ownership,
				Evidence:      evidence,
				InputModality: schema.InputModalityNone,
			},
			TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
		})
	}
	if attr.UncertainCopy {
		markOpenCodeUncertain(blocks)
	}
	return blocks, nil
}

// classifyOpenCodeHistoricalMessage classifies legacy and semantic shapes with
// their existing reader's authority: roles and synthetic flags decide, and
// delivery stays unknown because those shapes prove no admission route. A
// message whose every part is natively synthetic is harness context even when
// its role reads user.
func classifyOpenCodeHistoricalMessage(msg OpenCodeProvenanceMessage, attr OpenCodeMessageAttribution) ([]ClassifiedBlock, error) {
	evidence := openCodeProvenanceEvidence(msg.Shape)
	if msg.AllPartsSynthetic {
		return singleOpenCodeBlock(msg, attr, schema.RoleSystem, schema.EntryTypeSystem, msg.SystemText, openCodeHistoricalContext(evidence))
	}
	role := msg.Role
	if !role.IsValid() {
		role = schema.RoleSystem
	}
	switch role {
	case schema.RoleUser:
		delivery := schema.DeliveryOriginUnknown
		if attr.Inherited || attr.Ownership != schema.ContentOwnershipLocal {
			delivery = schema.DeliveryOriginInheritedContext
		}
		key, err := openCodeNativeKey(msg.Shape, msg.MessageID, "text")
		if err != nil {
			return nil, err
		}
		blocks := []ClassifiedBlock{{
			NativeKey: key,
			Role:      schema.RoleUser,
			EntryType: schema.EntryTypeText,
			Content:   msg.Text,
			Provenance: &schema.ContentProvenance{
				Origin:        schema.ContentOriginSubmittedInput,
				Actor:         schema.ActorOriginUnknown,
				Delivery:      delivery,
				Ownership:     attr.Ownership,
				Evidence:      evidence,
				InputModality: schema.InputModalityText,
			},
			TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
		}}
		if attr.UncertainCopy {
			markOpenCodeUncertain(blocks)
		}
		return blocks, nil
	case schema.RoleAssistant:
		return classifyOpenCodeAssistant(msg, attr)
	default:
		return singleOpenCodeBlock(msg, attr, role, schema.EntryTypeSystem, msg.SystemText, openCodeHistoricalContext(evidence))
	}
}

// openCodeLifecycleDelivery maps attribution to the delivery of non-input
// blocks: inherited copies keep their inherited delivery, everything else is
// system lifecycle.
func openCodeLifecycleDelivery(attr OpenCodeMessageAttribution) schema.DeliveryOrigin {
	if attr.Inherited || attr.Ownership != schema.ContentOwnershipLocal {
		return schema.DeliveryOriginInheritedContext
	}
	return schema.DeliveryOriginSystemLifecycle
}

func openCodeHarnessContext() *schema.ContentProvenance {
	return &schema.ContentProvenance{
		Origin:        schema.ContentOriginHarnessContext,
		Actor:         schema.ActorOriginHarness,
		Delivery:      schema.DeliveryOriginSystemLifecycle,
		Ownership:     schema.ContentOwnershipLocal,
		Evidence:      schema.EvidenceNativeTyped,
		InputModality: schema.InputModalityText,
	}
}

func openCodeGeneratedSummary() *schema.ContentProvenance {
	return &schema.ContentProvenance{
		Origin:        schema.ContentOriginGeneratedSummary,
		Actor:         schema.ActorOriginHarness,
		Delivery:      schema.DeliveryOriginSystemLifecycle,
		Ownership:     schema.ContentOwnershipLocal,
		Evidence:      schema.EvidenceNativeTyped,
		InputModality: schema.InputModalityText,
	}
}

func openCodeSystemControl() *schema.ContentProvenance {
	return &schema.ContentProvenance{
		Origin:        schema.ContentOriginSystemControl,
		Actor:         schema.ActorOriginHarness,
		Delivery:      schema.DeliveryOriginSystemLifecycle,
		Ownership:     schema.ContentOwnershipLocal,
		Evidence:      schema.EvidenceNativeTyped,
		InputModality: schema.InputModalityNone,
	}
}

func openCodeUnknownProvenance() *schema.ContentProvenance {
	return &schema.ContentProvenance{
		Origin:        schema.ContentOriginUnknown,
		Actor:         schema.ActorOriginUnknown,
		Delivery:      schema.DeliveryOriginUnknown,
		Ownership:     schema.ContentOwnershipLocal,
		Evidence:      schema.EvidenceUnknown,
		InputModality: schema.InputModalityUnknown,
	}
}

func openCodeHistoricalContext(evidence schema.EvidenceKind) *schema.ContentProvenance {
	return &schema.ContentProvenance{
		Origin:        schema.ContentOriginHarnessContext,
		Actor:         schema.ActorOriginHarness,
		Delivery:      schema.DeliveryOriginSystemLifecycle,
		Ownership:     schema.ContentOwnershipLocal,
		Evidence:      evidence,
		InputModality: schema.InputModalityText,
	}
}

// singleOpenCodeBlock emits one block, applying the fork-history attribution
// overlay so inherited and uncertain copies keep the caller's ownership.
func singleOpenCodeBlock(msg OpenCodeProvenanceMessage, attr OpenCodeMessageAttribution, role schema.Role, entryType schema.EntryType, content string, provenance *schema.ContentProvenance) ([]ClassifiedBlock, error) {
	key, err := openCodeNativeKey(msg.Shape, msg.MessageID, "text")
	if err != nil {
		return nil, err
	}
	provenance.Ownership = attr.Ownership
	if attr.Inherited || attr.Ownership != schema.ContentOwnershipLocal {
		provenance.Delivery = schema.DeliveryOriginInheritedContext
	}
	blocks := []ClassifiedBlock{{
		NativeKey:   key,
		Role:        role,
		EntryType:   entryType,
		Content:     content,
		Provenance:  provenance,
		TimestampMs: nonEmptyTimestamp(msg.TimeCreated),
	}}
	if attr.UncertainCopy {
		markOpenCodeUncertain(blocks)
	}
	return blocks, nil
}

// markOpenCodeUncertain flags retained copies whose proof did not survive. The
// shared builder only accepts uncertain blocks in a declared earlier section,
// so the capture builder must route them there.
func markOpenCodeUncertain(blocks []ClassifiedBlock) {
	for i := range blocks {
		blocks[i].Uncertain = true
		if blocks[i].Provenance != nil {
			blocks[i].Provenance.Ownership = schema.ContentOwnershipUncertain
		}
	}
}

func nonEmptyTimestamp(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}
