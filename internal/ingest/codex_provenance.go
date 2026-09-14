package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/peasant-labs/schema"
)

// CodexThreadEvidence is the session-level native evidence the Codex
// provenance classifier reads from the captured session_meta record. It is
// decoded from the capture's own bounded bytes, never from a reopened mutable
// source, and it never invents a person author, a parent, or a root.
type CodexThreadEvidence struct {
	// HasSessionMeta reports whether a session_meta record was decoded from
	// the captured bytes. Without it every session-level fact stays omitted.
	HasSessionMeta bool
	// StableThreadID is the decoded session_meta.id, cross-checked against
	// the capture's stable identity.
	StableThreadID string
	// ThreadSource is the raw native thread_source purpose string. It is an
	// open analytics string, never a closed enum: only recognized values map
	// to a purpose, and nothing is inferred from an unfamiliar value.
	ThreadSource string
	// SourceInternalGuardian reports source={"internal":"guardian"}.
	SourceInternalGuardian bool
	// SourceSubagentOtherGuardian reports the older
	// source.subagent.other="guardian" form.
	SourceSubagentOtherGuardian bool
	// TopParentID is the top-level native parent thread when present.
	TopParentID string
	// NestedParentID is the older nested
	// source.subagent.thread_spawn.parent_thread_id fallback when present.
	NestedParentID string
	// ForkSourceID is the native forked_from_id context source when present.
	ForkSourceID string
	// RootSessionID is the explicit native session_id root grouping when
	// present. A missing value is not evidence, and a self value is not an
	// explicit root.
	RootSessionID string
}

// Recognized native thread_source purpose values. Unfamiliar values stay
// unmapped: purpose absence is preserved, never defaulted.
const (
	codexThreadSourceUser           = "user"
	codexThreadSourceSubagent       = "subagent"
	codexThreadSourceGuardianReview = "guardian_review"
)

// IsGuardianThread reports whether explicit native source or purpose evidence
// identifies a Guardian review thread. Title, content, nickname, model, or
// prompt similarity never identifies one: only these typed variants do.
func (e CodexThreadEvidence) IsGuardianThread() bool {
	return e.ThreadSource == codexThreadSourceGuardianReview ||
		e.SourceInternalGuardian ||
		e.SourceSubagentOtherGuardian
}

// Purpose maps recognized native purpose evidence to the shared purpose. An
// ordinary fork alone establishes neither helper purpose nor operator, and a
// missing or unfamiliar thread_source leaves purpose omitted.
func (e CodexThreadEvidence) Purpose() schema.SessionPurpose {
	switch {
	case e.IsGuardianThread():
		return schema.SessionPurposeHelperReview
	case e.ThreadSource == codexThreadSourceSubagent:
		return schema.SessionPurposeDelegatedWork
	case e.ThreadSource == codexThreadSourceUser:
		return schema.SessionPurposeInteraction
	default:
		return ""
	}
}

// DecodeCodexThreadEvidence decodes the session-level native evidence from the
// first session_meta record in bounded captured bytes. It reports
// HasSessionMeta=false when no session_meta record decodes, so the caller
// omits every session-level fact instead of inventing one. A stable identity
// disagreement is an error: the bytes do not belong to this thread.
func DecodeCodexThreadEvidence(rawBytes []byte, stableThreadID string) (CodexThreadEvidence, error) {
	evidence := CodexThreadEvidence{StableThreadID: stableThreadID}
	scanner := newJSONLRecordScanner(rawBytes, productionJSONLRecordLimit(0))
	for scanner.Scan() {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var env codexRolloutLine
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		if env.Type != codexTypeSessionMeta {
			continue
		}
		var wire struct {
			ID             string          `json:"id"`
			ThreadSource   string          `json:"thread_source"`
			Source         json.RawMessage `json:"source"`
			ParentThreadID string          `json:"parent_thread_id"`
			ForkedFromID   string          `json:"forked_from_id"`
			RootSessionID  string          `json:"session_id"`
		}
		if err := json.Unmarshal(env.Payload, &wire); err != nil {
			return CodexThreadEvidence{}, fmt.Errorf("ingest.DecodeCodexThreadEvidence: the captured session_meta payload for thread %q cannot be decoded; the thread evidence cannot be established; retain the capture and retry with intact source bytes", stableThreadID)
		}
		if wire.ID != "" && wire.ID != stableThreadID {
			return CodexThreadEvidence{}, fmt.Errorf("ingest.DecodeCodexThreadEvidence: captured session_meta.id disagrees with the stable thread identity; the bytes belong to another thread; retain the capture and retry with the matching source")
		}
		evidence.HasSessionMeta = true
		evidence.ThreadSource = wire.ThreadSource
		evidence.TopParentID = wire.ParentThreadID
		evidence.ForkSourceID = wire.ForkedFromID
		if wire.RootSessionID != "" && wire.RootSessionID != stableThreadID {
			evidence.RootSessionID = wire.RootSessionID
		}
		internal, other, nested := decodeCodexSourceVariants(wire.Source)
		evidence.SourceInternalGuardian = internal
		evidence.SourceSubagentOtherGuardian = other
		evidence.NestedParentID = nested
		return evidence, nil
	}
	return evidence, nil
}

// decodeCodexSourceVariants reads the launch-origin variants of a session_meta
// source value. A top-level CLI session carries a plain string such as "cli";
// a spawned session carries a nested object. Only the exact pinned Guardian
// spellings report true; unfamiliar shapes stay false without failing.
func decodeCodexSourceVariants(raw json.RawMessage) (internalGuardian, otherGuardian bool, nestedParent string) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false, false, ""
	}
	var plain string
	if err := json.Unmarshal(trimmed, &plain); err == nil {
		return false, false, ""
	}
	var source struct {
		Internal string `json:"internal"`
		Subagent struct {
			Other       string `json:"other"`
			ThreadSpawn struct {
				ParentThreadID string `json:"parent_thread_id"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	}
	if err := json.Unmarshal(trimmed, &source); err != nil {
		return false, false, ""
	}
	return source.Internal == "guardian", source.Subagent.Other == "guardian", source.Subagent.ThreadSpawn.ParentThreadID
}

// codexKindAttribution is one registered native content-item-kind meaning. The
// registry is an exact-match map over the pinned vocabulary: unfamiliar
// namespaces stay unknown, and no wildcard suffix ever claims context.
type codexKindAttribution struct {
	origin   schema.ContentOrigin
	actor    schema.ActorOrigin
	delivery schema.DeliveryOrigin
	// framing marks model-authored question framing that accompanies an
	// explicitly submitted answer. Framing never carries a submission of its
	// own; the sibling answer block can still qualify.
	framing bool
	// mediaDiagnostic marks a media-preparation diagnostic that counts with
	// the submitted media it replaces only when a sibling admitted media
	// block in the same message proves the association.
	mediaDiagnostic bool
	// userAction marks the typed shell user-command record: a native
	// user-initiated action that admits its own submission without claiming
	// person-authored chat prose.
	userAction bool
}

// codexSubmittedInput is the shared attribution for transported user input
// blocks. Actor stays unknown and delivery is resolved per block: transport
// origin never proves acceptance, ownership, or a person author.
func codexSubmittedInput() codexKindAttribution {
	return codexKindAttribution{
		origin:   schema.ContentOriginSubmittedInput,
		actor:    schema.ActorOriginUnknown,
		delivery: schema.DeliveryOriginUnknown,
	}
}

// codexHarnessContext is the shared attribution for harness-owned context
// blocks. The harness actor and system lifecycle delivery are the native
// meaning of these producer families, not an inference from their text.
func codexHarnessContext(delivery schema.DeliveryOrigin) codexKindAttribution {
	return codexKindAttribution{
		origin:   schema.ContentOriginHarnessContext,
		actor:    schema.ActorOriginHarness,
		delivery: delivery,
	}
}

// codexContentKindRegistry maps every supported native content-item kind to
// its positive attribution. Kinds absent here are unknown, including dynamic
// additional_content keys, extension instruction namespaces, and internal
// context namespaces without a fixture-backed registered producer family.
func codexContentKindRegistry() map[string]codexKindAttribution {
	registry := map[string]codexKindAttribution{
		"user.text":  codexSubmittedInput(),
		"user.image": {origin: schema.ContentOriginSubmittedInput, actor: schema.ActorOriginUnknown, delivery: schema.DeliveryOriginUnknown},
		"user.audio": {origin: schema.ContentOriginSubmittedInput, actor: schema.ActorOriginUnknown, delivery: schema.DeliveryOriginUnknown},

		"user.answered_question": {origin: schema.ContentOriginHarnessContext, actor: schema.ActorOriginHarness, delivery: schema.DeliveryOriginSessionAdmission, framing: true},

		"multi_agent.inter_agent_message":            {origin: schema.ContentOriginAgentCommunication, actor: schema.ActorOriginUnknown, delivery: schema.DeliveryOriginSubagentDelivery},
		"multi_agent.inter_agent_completion_message": {origin: schema.ContentOriginAgentCommunication, actor: schema.ActorOriginUnknown, delivery: schema.DeliveryOriginSubagentDelivery},

		"compaction.summary": {origin: schema.ContentOriginGeneratedSummary, actor: schema.ActorOriginHarness, delivery: schema.DeliveryOriginSystemLifecycle},

		"shell.user_command": {origin: schema.ContentOriginSubmittedInput, actor: schema.ActorOriginUnknown, delivery: schema.DeliveryOriginUnknown, userAction: true},

		"images.resize_notice":     {origin: schema.ContentOriginSubmittedInput, actor: schema.ActorOriginUnknown, delivery: schema.DeliveryOriginUnknown, mediaDiagnostic: true},
		"images.preparation_error": {origin: schema.ContentOriginSubmittedInput, actor: schema.ActorOriginUnknown, delivery: schema.DeliveryOriginUnknown, mediaDiagnostic: true},
		"images.unsupported":       {origin: schema.ContentOriginSubmittedInput, actor: schema.ActorOriginUnknown, delivery: schema.DeliveryOriginUnknown, mediaDiagnostic: true},
		"audio.unsupported":        {origin: schema.ContentOriginSubmittedInput, actor: schema.ActorOriginUnknown, delivery: schema.DeliveryOriginUnknown, mediaDiagnostic: true},
	}
	for _, kind := range []string{
		"agents_md.instructions",
		"environments.environment_context",
		"environments.instructions",
		"model.base_instructions",
		"generic.developer_instructions",
		"managed_config.developer_instructions",
		"permissions.instructions",
		"persistent_mode.instructions",
		"personality.spec_instructions",
		"collaboration_mode.instructions",
		"model_switch.instructions",
		"hooks.additional_context",
		"skills.catalog",
		"skills.selected_skill_instructions",
		"memories.instructions",
		"notes.thread_hint",
		"apps.instructions",
		"plugins.instructions",
		"plugins.usage_instructions",
		"plugins.recommendations",
		"tools.deferred_namespaces",
		"multi_agent.subagent_notification",
		"multi_agent.role_instructions",
		"multi_agent.mode_instructions",
		"multi_agent.usage_hint",
	} {
		delivery := schema.DeliveryOriginSystemLifecycle
		if strings.HasPrefix(kind, "multi_agent.") {
			delivery = schema.DeliveryOriginSubagentDelivery
		}
		registry[kind] = codexHarnessContext(delivery)
	}
	for _, kind := range []string{
		"guardian.policy",
		"guardian.node_repl_policy",
		"guardian.review_evidence",
		"guardian.node_repl_review_evidence",
		"guardian.trusted_tool",
		"guardian.trusted_skills",
		"guardian.approved_action",
		"guardian.followup_review_reminder",
	} {
		registry[kind] = codexKindAttribution{
			origin:   schema.ContentOriginHarnessContext,
			actor:    schema.ActorOriginHarness,
			delivery: schema.DeliveryOriginGuardianReview,
		}
	}
	for _, kind := range []string{
		"generic.turn_aborted",
		"current_time.reminder",
		"token_budget.context_window",
		"token_budget.context_window_guidance",
		"token_budget.remaining_tokens",
		"token_budget.reminder",
		"rollout_budget.remaining_tokens",
		"compaction.auto_fallback_prompt",
		"permissions.approved_command_prefix_saved",
		"network_proxy.rule_saved",
		"user_verification.notice",
	} {
		registry[kind] = codexKindAttribution{
			origin:   schema.ContentOriginSystemControl,
			actor:    schema.ActorOriginHarness,
			delivery: schema.DeliveryOriginSystemLifecycle,
		}
	}
	return registry
}

// codexMediaContentTypes are the native content discriminators compatible
// with submitted-media kinds. Any other pairing of a media kind with a
// text-only or output discriminator is a type/kind contradiction.
func codexMediaContentTypes() map[string]bool {
	return map[string]bool{
		"input_image": true,
		"image":       true,
		"image_url":   true,
		"input_audio": true,
		"audio":       true,
	}
}

// codexEnvelopeMetadata is the history-envelope metadata decoded from beside
// a record payload. Every field is optional: absent or malformed values leave
// the corresponding evidence unset, and independent vector, role, and
// lifecycle evidence still applies.
type codexEnvelopeMetadata struct {
	ClientAuthored               bool
	HasClientAuthored            bool
	HarnessAuthoredConfiguration bool
	HasHarnessAuthored           bool
	InheritedUserMessage         bool
	HasInheritedUserMessage      bool
	UserInputOrder               int64
	HasUserInputOrder            bool
}

// decodeCodexEnvelopeMetadata decodes one envelope metadata value. A missing,
// null, or malformed envelope yields zero metadata without failing: the
// caller classifies from independent evidence instead.
func decodeCodexEnvelopeMetadata(raw json.RawMessage) codexEnvelopeMetadata {
	var metadata codexEnvelopeMetadata
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return metadata
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return metadata
	}
	if rawValue, ok := fields["client_authored"]; ok {
		var value bool
		if err := json.Unmarshal(rawValue, &value); err == nil {
			metadata.ClientAuthored = value
			metadata.HasClientAuthored = true
		}
	}
	if rawValue, ok := fields["harness_authored_configuration"]; ok {
		var value bool
		if err := json.Unmarshal(rawValue, &value); err == nil {
			metadata.HarnessAuthoredConfiguration = value
			metadata.HasHarnessAuthored = true
		}
	}
	if rawValue, ok := fields["inherited_user_message"]; ok {
		var value bool
		if err := json.Unmarshal(rawValue, &value); err == nil {
			metadata.InheritedUserMessage = value
			metadata.HasInheritedUserMessage = true
		}
	}
	if rawValue, ok := fields["user_input_order"]; ok {
		var value int64
		decoder := json.NewDecoder(bytes.NewReader(rawValue))
		if err := decoder.Decode(&value); err == nil && value >= 0 {
			var trailing any
			if err := decoder.Decode(&trailing); err != nil {
				metadata.UserInputOrder = value
				metadata.HasUserInputOrder = true
			}
		}
	}
	return metadata
}

// codexMessageBlock is one decoded native content element of a message or
// reasoning item, with its positional kind when the aligned vector is usable.
type codexMessageBlock struct {
	ContentType string
	Text        string
	Kind        string
	HasKind     bool
}

// codexItemPayload is the classifier's view of one captured record payload:
// the typed discriminator, the role, the content elements, the passthrough
// kind vector, and any native inter-agent delivery correlation.
type codexItemPayload struct {
	Type         string
	Role         string
	ID           string
	Name         string
	CallID       string
	Author       string
	Recipient    string
	Content      []codexMessageBlock
	Summary      []codexMessageBlock
	Arguments    json.RawMessage
	Input        json.RawMessage
	Output       json.RawMessage
	Delivery     codexDeliveryCorrelation
	KindsUsable  bool
	KindsPresent bool
}

// decodeCodexItemPayload decodes one captured node payload for the
// classifier. Content elements keep their order; the kind vector is attached
// positionally only when it is an all-string vector of exactly the content
// length. Any other vector shape removes vector evidence only: the content
// elements, the typed discriminator, and the envelope metadata still apply.
func decodeCodexItemPayload(raw json.RawMessage) codexItemPayload {
	var payload codexItemPayload
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return payload
	}
	var wire struct {
		Type      string `json:"type"`
		Role      string `json:"role"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		CallID    string `json:"call_id"`
		Author    string `json:"author"`
		Recipient string `json:"recipient"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Summary []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
		Arguments   json.RawMessage           `json:"arguments"`
		Input       json.RawMessage           `json:"input"`
		Output      json.RawMessage           `json:"output"`
		Delivery    *codexDeliveryCorrelation `json:"delivery"`
		Passthrough *struct {
			Kinds json.RawMessage `json:"content_item_kinds"`
		} `json:"internal_chat_message_metadata_passthrough"`
	}
	if err := json.Unmarshal(trimmed, &wire); err != nil {
		return payload
	}
	payload.Type = wire.Type
	payload.Role = wire.Role
	payload.ID = wire.ID
	payload.Name = wire.Name
	payload.CallID = wire.CallID
	payload.Author = wire.Author
	payload.Recipient = wire.Recipient
	payload.Arguments = wire.Arguments
	payload.Input = wire.Input
	payload.Output = wire.Output
	if wire.Delivery != nil {
		payload.Delivery = *wire.Delivery
	}
	for _, element := range wire.Content {
		payload.Content = append(payload.Content, codexMessageBlock{ContentType: element.Type, Text: element.Text})
	}
	for _, element := range wire.Summary {
		payload.Summary = append(payload.Summary, codexMessageBlock{ContentType: element.Type, Text: element.Text})
	}
	if wire.Passthrough == nil {
		return payload
	}
	payload.KindsPresent = true
	var kinds []string
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(wire.Passthrough.Kinds)))
	if err := decoder.Decode(&kinds); err != nil {
		return payload
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return payload
	}
	if len(kinds) != len(payload.Content) {
		return payload
	}
	payload.KindsUsable = true
	for i := range kinds {
		payload.Content[i].Kind = kinds[i]
		payload.Content[i].HasKind = true
	}
	return payload
}

// codexBlockModality derives the structural input modality of one content
// element. It is a content fact, never a provenance claim: a shell action
// kind forces user_action, media kinds keep media, and text stays text.
func codexBlockModality(kind string, known bool, contentType string, userAction bool) schema.InputModality {
	if userAction {
		return schema.InputModalityUserAction
	}
	if known && (kind == "user.image" || kind == "user.audio") {
		return schema.InputModalityMedia
	}
	switch contentType {
	case "input_text", "output_text":
		return schema.InputModalityText
	default:
		if codexMediaContentTypes()[contentType] {
			return schema.InputModalityMedia
		}
		return schema.InputModalityUnknown
	}
}

// codexKindContentConflict reports whether a registered kind contradicts its
// content discriminator for one block. Context kinds ride text content
// normally, so only definite submitted-input contradictions conflict: a text
// claim on output content, or a media claim on non-media content.
func codexKindContentConflict(kind string, contentType string) bool {
	switch kind {
	case "user.text":
		return contentType != "" && contentType != "input_text"
	case "user.image", "user.audio":
		return contentType != "" && !codexMediaContentTypes()[contentType]
	default:
		return false
	}
}

// codexMediaDisplayContent is the canonical display content for a media block
// that carries no text. It names the structural discriminator only, never
// native bytes, so the block keeps a stable identity without leaking content.
func codexMediaDisplayContent(contentType string) string {
	if contentType == "" {
		return "[media]"
	}
	return "[media " + contentType + "]"
}

// ClassifyCodexBlocks converts captured native nodes into classified adapter
// blocks for the shared projection builder. It implements the positive
// native-evidence rules: per-block kind attribution, history-envelope
// metadata beside the payload, acceptance identity, delivery resolution, and
// ownership from the captured history. Text bytes never classify a block:
// without usable native evidence the block stays unknown, and wrapper-shaped
// pasted input is preserved exactly as the bytes prove.
//
// Nodes with reverted or excluded ownership produce no blocks: the active
// history no longer contains them. Uncertain earlier nodes move to the first
// declared earlier section with uncertain ownership. The returned earlier
// states are empty when no uncertain block exists.
func ClassifyCodexBlocks(stableThreadID string, nodes []CodexCapturedNode, thread CodexThreadEvidence, correlations []CodexCapturedCorrelation) ([]ClassifiedBlock, []schema.EarlierHistoryState, error) {
	registry := codexContentKindRegistry()
	classifier := codexBlockClassifier{
		stableThreadID: stableThreadID,
		thread:         thread,
		registry:       registry,
		correlated:     codexCorrelatedItems(correlations),
	}
	for _, node := range nodes {
		if err := classifier.classifyNode(node); err != nil {
			return nil, nil, err
		}
	}
	return classifier.finish()
}

// codexCorrelatedItems indexes the proven paired-response-event correlations
// by item so the classifier can credit lifecycle acceptance evidence without
// re-deriving the pairing from bytes or order.
func codexCorrelatedItems(correlations []CodexCapturedCorrelation) map[string]bool {
	correlated := make(map[string]bool, len(correlations))
	for _, correlation := range correlations {
		if correlation.Kind != CodexCorrelationPairedResponseEvent {
			continue
		}
		if correlation.ItemID != "" {
			correlated[correlation.ItemID] = true
		}
	}
	return correlated
}

// codexBlockClassifier holds the per-capture classification state.
type codexBlockClassifier struct {
	stableThreadID string
	thread         CodexThreadEvidence
	registry       map[string]codexKindAttribution
	correlated     map[string]bool
	blocks         []ClassifiedBlock
	useKeys        map[string]bool
	// callCarriers maps one native call identity to the depth-0 assistant
	// carrier its tool_use created. A matching tool_result attaches to that
	// SAME carrier instead of inventing a second empty carrier, so one folded
	// tool is one main record and the result never doubles the turn count.
	callCarriers map[string]string
	// carrierKey names the current assistant depth-0 carrier a following tool
	// call folds under. The first agent-output text/thinking block after a
	// non-assistant block becomes the carrier; a tool-only segment creates an
	// empty assistant carrier. A non-assistant block clears it, so tools never
	// attach to an unrelated earlier assistant message.
	carrierKey string
	uncertain  bool
}

// finish demotes orphan tool results to depth-0 entries and reports whether
// an uncertain earlier section is declared. A result without its call block
// in this capture keeps its content and identity at depth 0 instead of
// failing the candidate: the pairing evidence is absent, not the content.
func (c *codexBlockClassifier) finish() ([]ClassifiedBlock, []schema.EarlierHistoryState, error) {
	for i := range c.blocks {
		block := &c.blocks[i]
		if block.EntryType != schema.EntryTypeToolResult || block.Depth == 0 {
			continue
		}
		if block.ToolCallKey == "" || c.useKeys[block.ToolCallKey] {
			continue
		}
		block.Depth = 0
		block.CarrierNativeKey = ""
	}
	var earlier []schema.EarlierHistoryState
	if c.uncertain {
		earlier = append(earlier, schema.EarlierHistoryUncertainMigrated)
	}
	return c.blocks, earlier, nil
}

// classifyNode converts one captured node into zero or more classified
// blocks, preserving replay order.
func (c *codexBlockClassifier) classifyNode(node CodexCapturedNode) error {
	switch node.Ownership {
	case CodexOwnershipReverted, CodexOwnershipExcluded:
		return nil
	case CodexOwnershipOwn, CodexOwnershipInherited, CodexOwnershipUncertainEarlier:
	default:
		return fmt.Errorf("ingest.ClassifyCodexBlocks: captured node %q carries ownership %q outside the closed capture set; the block cannot be placed; replay the native history through the supported capture", node.NativeKey, node.Ownership)
	}
	metadata := decodeCodexEnvelopeMetadata(node.Metadata)
	payload := decodeCodexItemPayload(node.Payload)
	switch node.NativeType {
	case "message":
		if payload.Type == codexResponseAgentMessage {
			c.classifyAgentMessage(node, payload, metadata)
			return nil
		}
		if payload.Type == "UserMessage" {
			c.classifyCanonicalUserMessage(node, payload, metadata)
			return nil
		}
		c.classifyMessage(node, payload, metadata)
	case "reasoning":
		c.classifyReasoning(node, payload, metadata)
	case "function_call", "custom_tool_call":
		c.classifyToolUse(node, payload, metadata)
	case "function_call_output", "custom_tool_call_output":
		c.classifyToolResult(node, payload, metadata)
	default:
		c.classifyUnlistedNative(node, payload, metadata)
	}
	return nil
}

// ownershipOf resolves the shared ownership from the captured node ownership
// and the per-record inherited evidence. Explicit inherited metadata wins
// over the positional ownership: the copy is proven inherited even where the
// position alone looked local. Origin and original actor never change here.
func ownershipOf(node CodexCapturedNode, metadata codexEnvelopeMetadata) schema.ContentOwnership {
	if metadata.HasInheritedUserMessage && metadata.InheritedUserMessage {
		return schema.ContentOwnershipInherited
	}
	switch node.Ownership {
	case CodexOwnershipInherited:
		return schema.ContentOwnershipInherited
	case CodexOwnershipUncertainEarlier:
		return schema.ContentOwnershipUncertain
	default:
		return schema.ContentOwnershipLocal
	}
}

// sectionOf places uncertain blocks in the first declared earlier section and
// every other block in the main stream.
func sectionOf(ownership schema.ContentOwnership) ProjectionSection {
	if ownership == schema.ContentOwnershipUncertain {
		return ProjectionSection{Earlier: true, Index: 1}
	}
	return ProjectionSection{Index: 0}
}

// observeCarrier maintains the current assistant carrier a following tool call
// folds under. The first local agent-output text/thinking block after a
// non-assistant block becomes the carrier; every non-assistant block clears it,
// so a tool never attaches to an unrelated earlier assistant message. Inherited
// and uncertain blocks never become the local carrier.
func (c *codexBlockClassifier) observeCarrier(origin schema.ContentOrigin, nativeKey string, ownership schema.ContentOwnership) {
	if ownership != schema.ContentOwnershipLocal {
		c.carrierKey = ""
		return
	}
	if origin == schema.ContentOriginAgentOutput {
		if c.carrierKey == "" {
			c.carrierKey = nativeKey
		}
		return
	}
	c.carrierKey = ""
}

// classifyMessage splits one message item into one block per content element,
// each with a stable native key so refresh reuses its allocated reference.
func (c *codexBlockClassifier) classifyMessage(node CodexCapturedNode, payload codexItemPayload, metadata codexEnvelopeMetadata) {
	ownership := ownershipOf(node, metadata)
	if ownership == schema.ContentOwnershipUncertain {
		for i := range payload.Content {
			c.emitUncertainBlock(node, payload.Content[i], i)
		}
		if len(payload.Content) == 0 {
			c.emitUncertainBlock(node, codexMessageBlock{}, 0)
		}
		return
	}
	for i := range payload.Content {
		c.classifyContentBlock(node, payload, metadata, ownership, i)
	}
	if len(payload.Content) == 0 {
		c.classifyEmptyMessage(node, payload, metadata, ownership)
	}
}

// submissionKey allocates the native acceptance identity shared by every
// block of one admitted submission. Order identity pairs the thread with the
// native user_input_order; a typed shell action carries its own item
// identity. A bare role or kind with no acceptance identity gets no invented
// submission group, and inherited acceptance belongs to the parent thread, so
// inherited blocks carry no key here.
func (c *codexBlockClassifier) submissionKey(node CodexCapturedNode, payload codexItemPayload, metadata codexEnvelopeMetadata, block codexMessageBlock, kind codexKindAttribution) string {
	if kind.origin != schema.ContentOriginSubmittedInput || kind.framing {
		return ""
	}
	if ownershipOf(node, metadata) != schema.ContentOwnershipLocal {
		return ""
	}
	if kind.userAction {
		item := payload.ID
		if item == "" {
			item = node.ItemID
		}
		if item == "" {
			return ""
		}
		return "thread:" + c.stableThreadID + ":action:" + item
	}
	if metadata.HasUserInputOrder {
		return fmt.Sprintf("thread:%s:order:%d", c.stableThreadID, metadata.UserInputOrder)
	}
	if payload.Type == "UserMessage" || c.correlated[node.ItemID] {
		item := payload.ID
		if item == "" {
			item = node.ItemID
		}
		if item == "" {
			return ""
		}
		return "thread:" + c.stableThreadID + ":item:" + item
	}
	return ""
}

// classifyContentBlock classifies one content element with its positional
// kind, envelope metadata, thread delivery evidence, and media association.
func (c *codexBlockClassifier) classifyContentBlock(node CodexCapturedNode, payload codexItemPayload, metadata codexEnvelopeMetadata, ownership schema.ContentOwnership, index int) {
	block := payload.Content[index]
	nativeKey := fmt.Sprintf("%s/b%d", node.NativeKey, index)
	section := sectionOf(ownership)
	uncertain := ownership == schema.ContentOwnershipUncertain
	if context, ok := c.envelopeContext(node, metadata, ownership); ok {
		c.blocks = append(c.blocks, ClassifiedBlock{
			NativeKey: nativeKey,
			Section:   section,
			Uncertain: uncertain,
			Role:      RoleSystem,
			EntryType: EntryTypeSystem,
			Content:   block.Text,
			Provenance: &schema.ContentProvenance{
				Origin:        schema.ContentOriginHarnessContext,
				Actor:         context.actor,
				Delivery:      context.delivery,
				Ownership:     ownership,
				Evidence:      schema.EvidenceNativeTyped,
				InputModality: codexBlockModality("", false, block.ContentType, false),
			},
		})
		c.observeCarrier(schema.ContentOriginHarnessContext, nativeKey, ownership)
		return
	}
	kind, known := c.registry[block.Kind]
	if !block.HasKind {
		known = false
		kind = codexKindAttribution{}
	}
	if known && codexKindContentConflict(block.Kind, block.ContentType) {
		c.blocks = append(c.blocks, ClassifiedBlock{
			NativeKey: nativeKey,
			Section:   section,
			Uncertain: uncertain,
			Role:      RoleSystem,
			EntryType: EntryTypeSystem,
			Content:   block.Text,
			Provenance: &schema.ContentProvenance{
				Origin:        schema.ContentOriginUnknown,
				Actor:         schema.ActorOriginUnknown,
				Delivery:      schema.DeliveryOriginUnknown,
				Ownership:     ownership,
				Evidence:      schema.EvidenceConflict,
				InputModality: codexBlockModality("", false, block.ContentType, false),
			},
		})
		c.observeCarrier(schema.ContentOriginUnknown, nativeKey, ownership)
		return
	}
	if !known {
		origin, actor := codexRoleFallbackOrigin(node.NativeRole)
		evidence := schema.EvidenceUnknown
		if origin != schema.ContentOriginUnknown {
			evidence = schema.EvidenceNativeTyped
		}
		c.blocks = append(c.blocks, ClassifiedBlock{
			NativeKey: nativeKey,
			Section:   section,
			Uncertain: uncertain,
			Role:      codexPresentationRole(node.NativeRole),
			EntryType: EntryTypeText,
			Content:   block.Text,
			Provenance: &schema.ContentProvenance{
				Origin:        origin,
				Actor:         actor,
				Delivery:      schema.DeliveryOriginUnknown,
				Ownership:     ownership,
				Evidence:      evidence,
				InputModality: codexBlockModality("", false, block.ContentType, false),
			},
		})
		c.observeCarrier(origin, nativeKey, ownership)
		return
	}
	c.emitKnownBlock(node, payload, metadata, ownership, nativeKey, block, kind, index)
}

// codexRoleFallbackOrigin maps an unregistered content kind to the origin its
// valid native source role proves. An assistant message is agent output and a
// tool message is tool activity; anything else stays unknown. Text bytes never
// classify a block, so an unknown kind on a user message stays unknown.
func codexRoleFallbackOrigin(nativeRole string) (schema.ContentOrigin, schema.ActorOrigin) {
	switch nativeRole {
	case "assistant":
		return schema.ContentOriginAgentOutput, schema.ActorOriginUnknown
	case "tool":
		return schema.ContentOriginToolActivity, schema.ActorOriginUnknown
	default:
		return schema.ContentOriginUnknown, schema.ActorOriginUnknown
	}
}

// codexContextOverride is one harness-context override derived from a native
// role or an envelope configuration marker.
type codexContextOverride struct {
	actor    schema.ActorOrigin
	delivery schema.DeliveryOrigin
}

// envelopeContext reports whether a block is harness context because its native
// role is developer/system or an adjacent envelope marker identifies client or
// harness configuration. That native evidence outranks the kind vector: a
// developer-role configuration block stays harness_context even when its vector
// is absent, malformed or names an input kind. client_authored is a client
// developer's configuration, never a person; harness_authored_configuration is
// the harness itself.
func (c *codexBlockClassifier) envelopeContext(node CodexCapturedNode, metadata codexEnvelopeMetadata, ownership schema.ContentOwnership) (codexContextOverride, bool) {
	roleContext := node.NativeRole == "developer" || node.NativeRole == "system"
	harnessConfig := metadata.HasHarnessAuthored && metadata.HarnessAuthoredConfiguration
	clientConfig := metadata.HasClientAuthored && metadata.ClientAuthored
	if !roleContext && !harnessConfig && !clientConfig {
		return codexContextOverride{}, false
	}
	delivery := schema.DeliveryOriginSystemLifecycle
	if ownership == schema.ContentOwnershipInherited {
		delivery = schema.DeliveryOriginInheritedContext
	}
	actor := schema.ActorOriginHarness
	if clientConfig && !harnessConfig {
		actor = schema.ActorOriginUnknown
	}
	return codexContextOverride{actor: actor, delivery: delivery}, true
}

// emitKnownBlock emits one block for a registered kind, resolving delivery,
// submission identity, and presentation from the envelope and thread
// evidence. Injected instructions stay system entries: they never carry a
// submission key and therefore never seed titles or human-turn counts.
func (c *codexBlockClassifier) emitKnownBlock(node CodexCapturedNode, payload codexItemPayload, metadata codexEnvelopeMetadata, ownership schema.ContentOwnership, nativeKey string, block codexMessageBlock, kind codexKindAttribution, index int) {
	origin, actor, delivery := kind.origin, kind.actor, kind.delivery
	role, entryType := codexPresentation(node.NativeRole, origin)
	modality := codexBlockModality(block.Kind, true, block.ContentType, kind.userAction)
	content := block.Text
	if modality == schema.InputModalityMedia && content == "" {
		content = codexMediaDisplayContent(block.ContentType)
	}
	submission := ""
	if kind.origin == schema.ContentOriginSubmittedInput && !kind.framing {
		if kind.mediaDiagnostic {
			if sibling := c.siblingMediaSubmission(node, payload, metadata, index); sibling != "" {
				submission = sibling
				origin = schema.ContentOriginSubmittedInput
				delivery = schema.DeliveryOriginUnknown
			} else {
				origin = schema.ContentOriginSystemControl
				actor = schema.ActorOriginUnknown
				delivery = schema.DeliveryOriginUnknown
			}
		} else {
			submission = c.submissionKey(node, payload, metadata, block, kind)
		}
	}
	delivery = c.resolveDelivery(node, payload, metadata, ownership, kind, delivery, origin)
	if metadata.HasHarnessAuthored && metadata.HarnessAuthoredConfiguration {
		origin = schema.ContentOriginHarnessContext
		actor = schema.ActorOriginHarness
		delivery = schema.DeliveryOriginSystemLifecycle
		role, entryType = RoleSystem, EntryTypeSystem
		submission = ""
	}
	if metadata.HasClientAuthored && metadata.ClientAuthored {
		origin = schema.ContentOriginHarnessContext
		actor = schema.ActorOriginUnknown
		role, entryType = RoleSystem, EntryTypeSystem
		submission = ""
		if delivery != schema.DeliveryOriginInheritedContext {
			delivery = schema.DeliveryOriginSystemLifecycle
		}
	}
	evidence := schema.EvidenceNativeTyped
	c.blocks = append(c.blocks, ClassifiedBlock{
		NativeKey:     nativeKey,
		SubmissionKey: submission,
		Section:       sectionOf(ownership),
		Uncertain:     ownership == schema.ContentOwnershipUncertain,
		Role:          role,
		EntryType:     entryType,
		Content:       content,
		Provenance: &schema.ContentProvenance{
			Origin:        origin,
			Actor:         actor,
			Delivery:      delivery,
			Ownership:     ownership,
			Evidence:      evidence,
			InputModality: modality,
		},
	})
	c.observeCarrier(origin, nativeKey, ownership)
}

// siblingMediaSubmission finds the shared submission of an admitted sibling
// media block in the same message, proving a media diagnostic replaces
// genuine submitted media. Without such a sibling the diagnostic stands
// alone and counts nothing.
func (c *codexBlockClassifier) siblingMediaSubmission(node CodexCapturedNode, payload codexItemPayload, metadata codexEnvelopeMetadata, skip int) string {
	for i := range payload.Content {
		if i == skip {
			continue
		}
		block := payload.Content[i]
		kind, known := c.registry[block.Kind]
		if !block.HasKind || !known {
			continue
		}
		if kind.mediaDiagnostic || kind.framing || kind.userAction {
			continue
		}
		if kind.origin != schema.ContentOriginSubmittedInput {
			continue
		}
		if modality := codexBlockModality(block.Kind, true, block.ContentType, false); modality != schema.InputModalityMedia {
			continue
		}
		if key := c.submissionKey(node, payload, metadata, block, kind); key != "" {
			return key
		}
	}
	return ""
}

// resolveDelivery applies the delivery precedence over the kind default:
// inherited evidence first, then the Guardian veto, then correlated
// inter-agent delivery, then the typed shell admission, then the user-thread
// session admission. Anything else stays unknown: delivery without proof is
// retained visibly but excluded from own counts and titles.
func (c *codexBlockClassifier) resolveDelivery(node CodexCapturedNode, payload codexItemPayload, metadata codexEnvelopeMetadata, ownership schema.ContentOwnership, kind codexKindAttribution, delivery schema.DeliveryOrigin, origin schema.ContentOrigin) schema.DeliveryOrigin {
	if ownership == schema.ContentOwnershipInherited {
		return schema.DeliveryOriginInheritedContext
	}
	if origin != schema.ContentOriginSubmittedInput {
		return delivery
	}
	if c.thread.IsGuardianThread() {
		return schema.DeliveryOriginGuardianReview
	}
	if payload.Delivery.isCorrelated() {
		return schema.DeliveryOriginSubagentDelivery
	}
	if kind.userAction {
		return schema.DeliveryOriginSessionAdmission
	}
	if c.thread.ThreadSource == codexThreadSourceUser {
		if metadata.HasUserInputOrder || payload.Type == "UserMessage" || c.correlated[node.ItemID] {
			return schema.DeliveryOriginSessionAdmission
		}
	}
	return schema.DeliveryOriginUnknown
}

// classifyEmptyMessage preserves a content-less message item as one traceable
// block so positions survive even when the native item carried no content
// elements. Without content there is no kind to attribute, so the block stays
// unknown and counts nothing.
func (c *codexBlockClassifier) classifyEmptyMessage(node CodexCapturedNode, payload codexItemPayload, metadata codexEnvelopeMetadata, ownership schema.ContentOwnership) {
	origin, actor, delivery := schema.ContentOriginUnknown, schema.ActorOriginUnknown, schema.DeliveryOriginUnknown
	role, entryType := codexPresentation(node.NativeRole, origin)
	if payload.Role == "developer" || payload.Role == "system" || metadata.HasHarnessAuthored && metadata.HarnessAuthoredConfiguration || metadata.HasClientAuthored && metadata.ClientAuthored {
		origin = schema.ContentOriginHarnessContext
		actor = schema.ActorOriginHarness
		delivery = schema.DeliveryOriginSystemLifecycle
		if metadata.HasClientAuthored && metadata.ClientAuthored {
			actor = schema.ActorOriginUnknown
		}
		role, entryType = RoleSystem, EntryTypeSystem
	}
	if ownership == schema.ContentOwnershipInherited {
		delivery = schema.DeliveryOriginInheritedContext
	}
	nativeKey := fmt.Sprintf("%s/b0", node.NativeKey)
	c.blocks = append(c.blocks, ClassifiedBlock{
		NativeKey: nativeKey,
		Section:   sectionOf(ownership),
		Uncertain: ownership == schema.ContentOwnershipUncertain,
		Role:      role,
		EntryType: entryType,
		Provenance: &schema.ContentProvenance{
			Origin:        origin,
			Actor:         actor,
			Delivery:      delivery,
			Ownership:     ownership,
			Evidence:      schema.EvidenceUnknown,
			InputModality: schema.InputModalityUnknown,
		},
	})
	c.observeCarrier(origin, nativeKey, ownership)
}

// classifyCanonicalUserMessage classifies a typed canonical UserMessage
// item or event. The lifecycle type proves transport origin even when the
// kind vector is absent or unusable; acceptance still needs the order, the
// event correlation, or a user-thread admission before delivery is claimed.
func (c *codexBlockClassifier) classifyCanonicalUserMessage(node CodexCapturedNode, payload codexItemPayload, metadata codexEnvelopeMetadata) {
	ownership := ownershipOf(node, metadata)
	if ownership == schema.ContentOwnershipUncertain {
		c.emitUncertainBlock(node, codexMessageBlock{ContentType: "input_text", Text: codexJoinBlockTexts(payload.Content)}, 0)
		return
	}
	text := codexJoinBlockTexts(payload.Content)
	nativeKey := fmt.Sprintf("%s/b0", node.NativeKey)
	submission := ""
	delivery := schema.DeliveryOriginUnknown
	if ownership == schema.ContentOwnershipInherited {
		delivery = schema.DeliveryOriginInheritedContext
	} else if c.thread.IsGuardianThread() {
		delivery = schema.DeliveryOriginGuardianReview
		submission = c.orderSubmissionKey(metadata)
	} else if payload.Delivery.isCorrelated() {
		delivery = schema.DeliveryOriginSubagentDelivery
	} else if c.thread.ThreadSource == codexThreadSourceUser && (metadata.HasUserInputOrder || c.correlated[node.ItemID]) {
		delivery = schema.DeliveryOriginSessionAdmission
		submission = c.orderSubmissionKey(metadata)
		if submission == "" {
			submission = c.itemSubmissionKey(node, payload)
		}
	} else if metadata.HasUserInputOrder || c.correlated[node.ItemID] {
		submission = c.orderSubmissionKey(metadata)
		if submission == "" {
			submission = c.itemSubmissionKey(node, payload)
		}
	}
	actor := schema.ActorOriginUnknown
	if payload.Delivery.isCorrelated() {
		actor = schema.ActorOriginAgentDelegate
	}
	c.blocks = append(c.blocks, ClassifiedBlock{
		NativeKey:     nativeKey,
		SubmissionKey: submission,
		Section:       sectionOf(ownership),
		Role:          RoleUser,
		EntryType:     EntryTypeText,
		Content:       text,
		Provenance: &schema.ContentProvenance{
			Origin:        schema.ContentOriginSubmittedInput,
			Actor:         actor,
			Delivery:      delivery,
			Ownership:     ownership,
			Evidence:      schema.EvidenceLifecycleTyped,
			InputModality: schema.InputModalityText,
		},
	})
	c.observeCarrier(schema.ContentOriginSubmittedInput, nativeKey, ownership)
}

// orderSubmissionKey returns the thread acceptance identity for a native
// user_input_order, or empty when no order proves acceptance.
func (c *codexBlockClassifier) orderSubmissionKey(metadata codexEnvelopeMetadata) string {
	if !metadata.HasUserInputOrder {
		return ""
	}
	return fmt.Sprintf("thread:%s:order:%d", c.stableThreadID, metadata.UserInputOrder)
}

// itemSubmissionKey returns the lifecycle acceptance identity for a typed
// UserMessage item or event, or empty when the item carries no identity.
func (c *codexBlockClassifier) itemSubmissionKey(node CodexCapturedNode, payload codexItemPayload) string {
	item := payload.ID
	if item == "" {
		item = node.ItemID
	}
	if item == "" {
		return ""
	}
	return "thread:" + c.stableThreadID + ":item:" + item
}

// classifyAgentMessage classifies a typed inter-agent message. The typed
// author and recipient structure proves agent communication; a named sender
// establishes the agent delegate, otherwise the actor stays unknown. Agent
// delivery never seeds own counts or titles.
func (c *codexBlockClassifier) classifyAgentMessage(node CodexCapturedNode, payload codexItemPayload, metadata codexEnvelopeMetadata) {
	ownership := ownershipOf(node, metadata)
	actor := schema.ActorOriginUnknown
	if payload.Author != "" {
		actor = schema.ActorOriginAgentDelegate
	}
	delivery := schema.DeliveryOriginSubagentDelivery
	if ownership == schema.ContentOwnershipInherited {
		delivery = schema.DeliveryOriginInheritedContext
	}
	text := codexJoinBlockTexts(payload.Content)
	nativeKey := fmt.Sprintf("%s/b0", node.NativeKey)
	c.blocks = append(c.blocks, ClassifiedBlock{
		NativeKey: nativeKey,
		Section:   sectionOf(ownership),
		Uncertain: ownership == schema.ContentOwnershipUncertain,
		Role:      RoleSystem,
		EntryType: EntryTypeSystem,
		Content:   text,
		Provenance: &schema.ContentProvenance{
			Origin:        schema.ContentOriginAgentCommunication,
			Actor:         actor,
			Delivery:      delivery,
			Ownership:     ownership,
			Evidence:      schema.EvidenceNativeTyped,
			InputModality: schema.InputModalityText,
		},
	})
	c.observeCarrier(schema.ContentOriginAgentCommunication, nativeKey, ownership)
}

// classifyReasoning preserves each reasoning summary and content element as
// its own thinking block. Distinct refs never merge, even for byte-equal
// text: thinking siblings stay visible side by side.
func (c *codexBlockClassifier) classifyReasoning(node CodexCapturedNode, payload codexItemPayload, metadata codexEnvelopeMetadata) {
	ownership := ownershipOf(node, metadata)
	elements := append(append([]codexMessageBlock{}, payload.Summary...), payload.Content...)
	if len(elements) == 0 {
		elements = append(elements, codexMessageBlock{})
	}
	for i, element := range elements {
		nativeKey := fmt.Sprintf("%s/b%d", node.NativeKey, i)
		if ownership == schema.ContentOwnershipUncertain {
			c.emitUncertainBlock(node, element, i)
			continue
		}
		delivery := schema.DeliveryOriginUnknown
		if ownership == schema.ContentOwnershipInherited {
			delivery = schema.DeliveryOriginInheritedContext
		}
		c.blocks = append(c.blocks, ClassifiedBlock{
			NativeKey:   nativeKey,
			Section:     sectionOf(ownership),
			Role:        RoleAssistant,
			EntryType:   EntryTypeThinking,
			Content:     element.Text,
			HasThinking: true,
			Provenance: &schema.ContentProvenance{
				Origin:        schema.ContentOriginAgentOutput,
				Actor:         schema.ActorOriginUnknown,
				Delivery:      delivery,
				Ownership:     ownership,
				Evidence:      schema.EvidenceNativeTyped,
				InputModality: schema.InputModalityText,
			},
		})
		c.observeCarrier(schema.ContentOriginAgentOutput, nativeKey, ownership)
	}
}

// classifyToolUse emits the depth-1 tool_use block under the assistant carrier
// its segment belongs to. The first local agent-output text/thinking block
// after a non-assistant block is that carrier; a tool-only segment creates an
// empty assistant carrier with its own ref. The carrier counts as the tool-only
// turn without claiming content, and one folded tool adds exactly one main
// record. Correlation is the native call identity, never text equality.
func (c *codexBlockClassifier) classifyToolUse(node CodexCapturedNode, payload codexItemPayload, metadata codexEnvelopeMetadata) {
	ownership := ownershipOf(node, metadata)
	if ownership == schema.ContentOwnershipUncertain {
		c.uncertain = true
	}
	carrierKey := ""
	if ownership == schema.ContentOwnershipLocal {
		carrierKey = c.carrierKey
	}
	if carrierKey == "" {
		carrierKey = node.NativeKey + "/carrier"
		c.blocks = append(c.blocks, ClassifiedBlock{
			NativeKey: carrierKey,
			Section:   sectionOf(ownership),
			Uncertain: ownership == schema.ContentOwnershipUncertain,
			Role:      RoleAssistant,
			EntryType: EntryTypeText,
		})
		if ownership == schema.ContentOwnershipLocal {
			c.carrierKey = carrierKey
		}
	}
	callKey := payload.CallID
	if callKey == "" {
		item := payload.ID
		if item == "" {
			item = node.ItemID
		}
		callKey = "item:" + item
	}
	delivery := schema.DeliveryOriginToolDelivery
	if ownership == schema.ContentOwnershipInherited {
		delivery = schema.DeliveryOriginInheritedContext
	}
	partType := payload.Type
	if c.callCarriers == nil {
		c.callCarriers = map[string]string{}
	}
	c.callCarriers[callKey] = carrierKey
	toolInput := codexToolBytes(payload.Arguments, payload.Input)
	c.blocks = append(c.blocks, ClassifiedBlock{
		NativeKey:        node.NativeKey + "/call",
		Section:          sectionOf(ownership),
		Uncertain:        ownership == schema.ContentOwnershipUncertain,
		Role:             RoleAssistant,
		EntryType:        EntryTypeToolUse,
		Depth:            1,
		CarrierNativeKey: carrierKey,
		ToolCallKey:      callKey,
		ToolName:         payload.Name,
		ToolArguments:    toolInput,
		PartType:         &partType,
		Provenance: &schema.ContentProvenance{
			Origin:        schema.ContentOriginToolActivity,
			Actor:         schema.ActorOriginUnknown,
			Delivery:      delivery,
			Ownership:     ownership,
			Evidence:      schema.EvidenceNativeTyped,
			InputModality: schema.InputModalityNone,
		},
	})
	if c.useKeys == nil {
		c.useKeys = map[string]bool{}
	}
	c.useKeys[callKey] = true
}

// classifyToolResult emits the depth-1 tool_result block under the carrier its
// tool_use created, so the folded pair is one main record. A result whose call
// never appears in this capture keeps its content and identity at depth 0 with
// no invented carrier: the pairing evidence is absent, not the content.
func (c *codexBlockClassifier) classifyToolResult(node CodexCapturedNode, payload codexItemPayload, metadata codexEnvelopeMetadata) {
	ownership := ownershipOf(node, metadata)
	if ownership == schema.ContentOwnershipUncertain {
		c.uncertain = true
	}
	callKey := payload.CallID
	if callKey == "" {
		item := payload.ID
		if item == "" {
			item = node.ItemID
		}
		callKey = "item:" + item
	}
	delivery := schema.DeliveryOriginToolDelivery
	if ownership == schema.ContentOwnershipInherited {
		delivery = schema.DeliveryOriginInheritedContext
	}
	partType := payload.Type
	toolResult := codexToolBytes(payload.Output, nil)
	block := ClassifiedBlock{
		NativeKey:   node.NativeKey + "/result",
		Section:     sectionOf(ownership),
		Uncertain:   ownership == schema.ContentOwnershipUncertain,
		Role:        RoleTool,
		EntryType:   EntryTypeToolResult,
		ToolCallKey: callKey,
		ToolResult:  toolResult,
		PartType:    &partType,
		Provenance: &schema.ContentProvenance{
			Origin:        schema.ContentOriginToolActivity,
			Actor:         schema.ActorOriginUnknown,
			Delivery:      delivery,
			Ownership:     ownership,
			Evidence:      schema.EvidenceNativeTyped,
			InputModality: schema.InputModalityNone,
		},
	}
	if carrierKey, ok := c.callCarriers[callKey]; ok {
		block.Depth = 1
		block.CarrierNativeKey = carrierKey
	} else {
		block.Depth = 0
	}
	c.blocks = append(c.blocks, block)
}

// classifyUnlistedNative preserves a recognized native item type the kind
// table does not name. Content and the valid source role survive with
// unknown provenance: no text-derived repair, no invented authorship, and no
// count or title contribution.
func (c *codexBlockClassifier) classifyUnlistedNative(node CodexCapturedNode, payload codexItemPayload, metadata codexEnvelopeMetadata) {
	ownership := ownershipOf(node, metadata)
	text := codexJoinBlockTexts(payload.Content)
	if text == "" {
		text = codexJoinBlockTexts(payload.Summary)
	}
	role := codexPresentationRole(node.NativeRole)
	nativeKey := fmt.Sprintf("%s/b0", node.NativeKey)
	c.blocks = append(c.blocks, ClassifiedBlock{
		NativeKey: nativeKey,
		Section:   sectionOf(ownership),
		Uncertain: ownership == schema.ContentOwnershipUncertain,
		Role:      role,
		EntryType: EntryTypeText,
		Content:   text,
		Provenance: &schema.ContentProvenance{
			Origin:        schema.ContentOriginUnknown,
			Actor:         schema.ActorOriginUnknown,
			Delivery:      schema.DeliveryOriginUnknown,
			Ownership:     ownership,
			Evidence:      schema.EvidenceUnknown,
			InputModality: schema.InputModalityUnknown,
		},
	})
	c.observeCarrier(schema.ContentOriginUnknown, nativeKey, ownership)
}

// emitUncertainBlock retains one uncertain earlier-history block with its
// content and source role. Ownership uncertainty is explicit, so the block
// can never satisfy the strict own-input predicate even when its bytes look
// like ordinary chat.
func (c *codexBlockClassifier) emitUncertainBlock(node CodexCapturedNode, block codexMessageBlock, index int) {
	c.uncertain = true
	nativeKey := fmt.Sprintf("%s/b%d", node.NativeKey, index)
	c.blocks = append(c.blocks, ClassifiedBlock{
		NativeKey: nativeKey,
		Section:   ProjectionSection{Earlier: true, Index: 1},
		Uncertain: true,
		Role:      codexPresentationRole(node.NativeRole),
		EntryType: EntryTypeText,
		Content:   block.Text,
		Provenance: &schema.ContentProvenance{
			Origin:        schema.ContentOriginUnknown,
			Actor:         schema.ActorOriginUnknown,
			Delivery:      schema.DeliveryOriginUnknown,
			Ownership:     schema.ContentOwnershipUncertain,
			Evidence:      schema.EvidenceUnknown,
			InputModality: codexBlockModality("", false, block.ContentType, false),
		},
	})
	c.observeCarrier(schema.ContentOriginUnknown, nativeKey, schema.ContentOwnershipUncertain)
}

// codexPresentation maps a native role and a classified origin to the
// normalized presentation role and entry type. Submitted input renders as
// the user; harness, summary, control, and agent communication render as
// system; agent output renders as the assistant. Unknown content keeps its
// valid source role so the bytes stay readable without a provenance claim.
func codexPresentation(nativeRole string, origin schema.ContentOrigin) (schema.Role, schema.EntryType) {
	switch origin {
	case schema.ContentOriginSubmittedInput:
		return RoleUser, EntryTypeText
	case schema.ContentOriginHarnessContext, schema.ContentOriginGeneratedSummary, schema.ContentOriginSystemControl, schema.ContentOriginAgentCommunication:
		return RoleSystem, EntryTypeSystem
	case schema.ContentOriginAgentOutput:
		return RoleAssistant, EntryTypeText
	default:
		return codexPresentationRole(nativeRole), EntryTypeText
	}
}

// codexPresentationRole retains a valid native source role for unknown
// content, defaulting to system when the native role itself is unusable.
// Developer-role payloads are harness configuration, never user chat.
func codexPresentationRole(nativeRole string) schema.Role {
	switch nativeRole {
	case "user":
		return RoleUser
	case "assistant":
		return RoleAssistant
	case "tool":
		return RoleTool
	case "system", "developer":
		return RoleSystem
	default:
		return RoleSystem
	}
}

// codexJoinBlockTexts joins the text of decoded content elements in order.
// It preserves positions for display; it never classifies.
func codexJoinBlockTexts(blocks []codexMessageBlock) string {
	var parts []string
	for _, block := range blocks {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// codexToolBytes preserves the verbatim native tool bytes, preferring the
// primary encoding and falling back to the alternate field. Empty and null
// stay absent so callers can tell missing input from empty input.
func codexToolBytes(primary, fallback json.RawMessage) string {
	if value := codexRawJSONToString(primary); value != nil {
		return *value
	}
	if value := codexRawJSONToString(fallback); value != nil {
		return *value
	}
	return ""
}
