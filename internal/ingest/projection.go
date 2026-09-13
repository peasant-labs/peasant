package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// ProjectionSection names the partition a classified block belongs to. Index 0
// is the main turn stream; 1..N name the earlier-history sections the capture
// declares in order.
type ProjectionSection struct {
	Earlier bool
	Index   int
}

// ClassifiedUsage is the per-block token usage a native classifier observed.
// Nil means the source recorded no usage; a present zero is measured none.
type ClassifiedUsage struct {
	TokensIn  *int
	TokensOut *int
}

// ClassifiedNativeAttachment is one typed native-metadata enrichment attached
// to a single classified block. The projection remaps its owner to the
// allocated block ref in the owner's final partition and remaps an optional
// tool attachment from the native call key to the allocated tool-use ref.
type ClassifiedNativeAttachment struct {
	// ID is the bounded opaque metadata identity.
	ID string
	// Kind and SourceType name the closed native-metadata classification.
	Kind       schema.NativeMetadataKind
	SourceType schema.NativeMetadataSourceType
	// CustomType carries the extension identifier for custom kinds.
	CustomType string
	// Data is the non-null JSON evidence payload.
	Data string
	// AttachmentToolCallKey optionally names the native call identity whose
	// allocated tool-use ref the record attaches to. Empty means no tool
	// attachment.
	AttachmentToolCallKey string
	// MessageRole optionally carries the pi message role for message sources.
	MessageRole schema.NativePiMessageRole
}

// ClassifiedBlock is one classified native content block produced by an
// adapter's native classifier. The adapter owns native decoding and the
// positive attribution of each block; the shared projection owns reference
// allocation, layout, tool folding, content records and counting.
type ClassifiedBlock struct {
	// NativeKey is the bounded opaque local native identity or coordinate for
	// this block. It is persisted as an alias so a later capture reuses the
	// same reference.
	NativeKey string
	// SubmissionKey is the native acceptance identity shared by every block of
	// one admitted submission. Empty means the block proves no submission group.
	SubmissionKey string
	// AmbiguousPairKey marks an ID-less response/event pair whose pairing the
	// native reducer could not prove. Every representation is retained with its
	// own ref in uncertain earlier history; byte or evidence equality is never
	// proof of one native item, so an unproved pair never collapses.
	AmbiguousPairKey string
	// NativeCorrelationKey is explicit positive native proof, supplied by the
	// native reducer, that blocks carrying the same non-empty value are one
	// native item (a proven mirror). The projection collapses those blocks to
	// one entry and aliases the mirrors to the owner. The proof is never
	// inferred from equal bytes, equal evidence or first-wins order; a
	// withdrawn proof leaves every representation with its own ref.
	NativeCorrelationKey string
	// Section is the adapter's requested partition.
	Section ProjectionSection
	// Uncertain marks evidence whose ownership is not local and certain. It is
	// only legal in a declared earlier-history section.
	Uncertain bool
	// UncertainSubtree moves the whole tool subtree rooted at this carrier into
	// the first earlier-history section when the parent/child split is unknown.
	UncertainSubtree bool
	Role             schema.Role
	EntryType        schema.EntryType
	Depth            int
	// CarrierNativeKey names the depth-0 carrier a depth-1 tool entry hangs
	// from, by NativeKey. A tool subtree is partition-closed: the projection
	// moves a depth-1 entry to its carrier's partition.
	CarrierNativeKey string
	// ToolCallKey is the native call identity that correlates a tool_use with
	// its tool_result inside one partition. Correlation is never text equality.
	ToolCallKey string
	// ToolName and ToolKind are optional adapter-supplied enrichment.
	ToolName string
	ToolKind schema.ToolCallKind
	// Content is the full block content for text/thinking/system/media records.
	Content string
	// ToolArguments and ToolResult are the full serialized tool bytes.
	ToolArguments string
	ToolResult    string
	// Provenance is the classified evidence. Nil is a legacy block with no
	// positive provenance.
	Provenance  *schema.ContentProvenance
	TimestampMs *int64
	PartType    *string
	HasThinking bool
	// Usage is the optional per-block token usage the classifier observed.
	// Nil means the source recorded no usage; a present zero is measured none.
	Usage *ClassifiedUsage
	// ObservedModel is the exact model identifier the source observed while
	// producing this block. Nil means no observation. When present it must
	// name an assistant block and survive layout on the owning entry.
	ObservedModel *string
	// NativeAttachments carries optional typed native-metadata enrichment
	// owned by this block. Each record is remapped to the allocated block ref
	// in the owner's final partition; a tool attachment is remapped from its
	// native call key to the allocated tool-use ref in the same partition.
	NativeAttachments []ClassifiedNativeAttachment
}

// ClassifiedCapture is one classified adapter capture plus the prior alias
// state the projection consults before allocating. It is the typed seam a
// native adapter's classifier produces and the transcript hydration converts.
type ClassifiedCapture struct {
	// ID is the generation identity the caller installs. The projection does
	// not invent one, because activation owns generation addressing.
	ID                   string
	SessionID            SessionID
	Harness              Harness
	Metadata             schema.UnifiedMetadata
	SourceEvidenceDigest string
	Completeness         indexformat.GenerationCompleteness
	// EarlierStates declares earlier-history sections 1..N in order.
	EarlierStates []schema.EarlierHistoryState
	Blocks        []ClassifiedBlock
	Segments      []indexformat.ContextSegment
	Prior         ProjectionPriorState
}

// resolvedBlock is one classified block with its effective partition and its
// allocated identities.
type resolvedBlock struct {
	block      ClassifiedBlock
	section    int
	ref        schema.SourceEntryRef
	dropped    bool
	dropTo     *resolvedBlock
	entryIndex int
}

// BuildGeneration projects one classified capture into a validated managed
// generation. It allocates stable opaque references (reusing the supplied
// prior alias state), lays out partitions, folds tool subtrees, remaps
// ParentIndex after the final order, records full content once per ref, and
// derives the strict input-submission count and prose-title refs.
//
// The builder performs no source I/O and decodes no native format: the adapter
// supplies already-classified blocks and full content bytes.
func BuildGeneration(capture ClassifiedCapture, allocator RefAllocator) (indexformat.Generation, error) {
	if err := validateCapture(capture, allocator); err != nil {
		return indexformat.Generation{}, err
	}
	prior := capture.Prior.clone()

	resolved, err := resolveProjectionBlocks(capture)
	if err != nil {
		return indexformat.Generation{}, err
	}
	if err := allocateBlockRefs(resolved, prior, allocator); err != nil {
		return indexformat.Generation{}, err
	}
	if err := allocateSubmissionRefs(resolved, prior, allocator); err != nil {
		return indexformat.Generation{}, err
	}
	partitions, err := layoutProjectionPartitions(capture, resolved)
	if err != nil {
		return indexformat.Generation{}, err
	}
	if err := remapToolParents(partitions, resolved); err != nil {
		return indexformat.Generation{}, err
	}
	if err := attachProjectionNativeMetadata(&partitions, resolved); err != nil {
		return indexformat.Generation{}, err
	}
	if err := validateProjectionProvenance(partitions); err != nil {
		return indexformat.Generation{}, err
	}
	if err := validateUniqueProjectionRefs(partitions); err != nil {
		return indexformat.Generation{}, err
	}
	content, err := buildProjectionContent(partitions)
	if err != nil {
		return indexformat.Generation{}, err
	}
	aliases, err := buildProjectionAliases(resolved)
	if err != nil {
		return indexformat.Generation{}, err
	}

	generation := indexformat.Generation{
		ID:                   capture.ID,
		Completeness:         capture.Completeness,
		Metadata:             capture.Metadata,
		Main:                 partitions.main,
		Earlier:              partitions.earlier,
		Segments:             capture.Segments,
		Content:              content,
		Aliases:              aliases,
		SourceEvidenceDigest: capture.SourceEvidenceDigest,
	}
	applyStrictCounts(&generation, capture.Completeness)
	if err := generation.Validate(); err != nil {
		return indexformat.Generation{}, err
	}
	return generation, nil
}

// BuildV2 projects one classified capture and wraps the validated generation in
// the concrete index result a native adapter returns for format 2.
func BuildV2(capture ClassifiedCapture, allocator RefAllocator) (indexformat.V2, error) {
	generation, err := BuildGeneration(capture, allocator)
	if err != nil {
		return indexformat.V2{}, err
	}
	return indexformat.V2{Generation: generation}, nil
}

// projectionPartitions holds the built main and earlier partitions.
type projectionPartitions struct {
	main    indexformat.Partition
	earlier []indexformat.EarlierPartition
}

func validateCapture(capture ClassifiedCapture, allocator RefAllocator) error {
	if allocator == nil {
		return fmt.Errorf("ingest.BuildGeneration: no reference allocator was supplied; the projection cannot allocate stable identities; supply a RefAllocator")
	}
	if strings.TrimSpace(capture.ID) == "" {
		return fmt.Errorf("ingest.BuildGeneration: generation id is empty; the candidate cannot be addressed or activated; assign the installed generation id")
	}
	if _, err := schema.NewSessionID(string(capture.SessionID)); err != nil {
		return fmt.Errorf("ingest.BuildGeneration: session id is malformed; the candidate cannot be stored; provide a canonical session identifier: %w", err)
	}
	if capture.Metadata.SessionID != capture.SessionID {
		return fmt.Errorf("ingest.BuildGeneration: capture session %q disagrees with metadata session %q; the candidate could be stored under the wrong session; rebuild the capture from one session", capture.SessionID, capture.Metadata.SessionID)
	}
	if capture.Metadata.ModelHarness != schema.Harness(capture.Harness) {
		return fmt.Errorf("ingest.BuildGeneration: capture harness %q disagrees with metadata harness %q; the candidate could be parsed as another harness; rebuild the capture from one source", capture.Harness, capture.Metadata.ModelHarness)
	}
	if strings.TrimSpace(capture.SourceEvidenceDigest) == "" {
		return fmt.Errorf("ingest.BuildGeneration: source evidence digest is empty; the captured source cannot be verified; record the evidence digest")
	}
	if !capture.Completeness.IsValid() {
		return fmt.Errorf("ingest.BuildGeneration: completeness %q is outside the closed set; a reader cannot tell whether the candidate is usable; use complete or incomplete_new", capture.Completeness)
	}
	for i, state := range capture.EarlierStates {
		if !state.IsValid() {
			return fmt.Errorf("ingest.BuildGeneration: earlier[%d] state %q is outside the closed set; retained history cannot be explained; use a published state", i+1, state)
		}
	}
	for i := range capture.Blocks {
		block := capture.Blocks[i]
		if block.Section.Index < 0 || block.Section.Index > len(capture.EarlierStates) {
			return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q names partition %d, outside 0..%d; the block has no declared partition; declare the earlier section or use index 0", i, block.NativeKey, block.Section.Index, len(capture.EarlierStates))
		}
		if block.Section.Index > 0 && !block.Section.Earlier {
			return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q names earlier index %d without the earlier flag; the partition is ambiguous; set earlier=true or use index 0", i, block.NativeKey, block.Section.Index)
		}
		if block.Section.Index == 0 && block.Section.Earlier {
			return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q marks the main stream as earlier; the block would lose its main identity; use index 0 without the earlier flag", i, block.NativeKey)
		}
		if block.Uncertain && block.Section.Index == 0 {
			return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q is uncertain but placed in the main stream; uncertain evidence cannot count as local main content; place it in a declared earlier section", i, block.NativeKey)
		}
		if err := validateClassifiedUsage(i, block); err != nil {
			return err
		}
		if err := validateClassifiedObservedModel(i, block); err != nil {
			return err
		}
		if err := validateClassifiedAttachments(i, block); err != nil {
			return err
		}
	}
	return nil
}

func validateClassifiedUsage(index int, block ClassifiedBlock) error {
	if block.Usage == nil {
		return nil
	}
	if block.Usage.TokensIn != nil && *block.Usage.TokensIn < 0 {
		return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q usage tokensIn %d is negative; token usage cannot be negative; record a nonnegative count or omit the usage", index, block.NativeKey, *block.Usage.TokensIn)
	}
	if block.Usage.TokensOut != nil && *block.Usage.TokensOut < 0 {
		return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q usage tokensOut %d is negative; token usage cannot be negative; record a nonnegative count or omit the usage", index, block.NativeKey, *block.Usage.TokensOut)
	}
	return nil
}

func validateClassifiedObservedModel(index int, block ClassifiedBlock) error {
	if block.ObservedModel == nil {
		return nil
	}
	observed := *block.ObservedModel
	if _, err := schema.NewObservedModelID(observed); err != nil {
		return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q observedModel is invalid; the observation cannot be preserved exactly; supply the exact source identifier or omit it: %w", index, block.NativeKey, err)
	}
	if block.Role != schema.RoleAssistant {
		return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q carries observedModel on role %q; only assistant output can carry an observed model; omit it from user, system, and tool blocks", index, block.NativeKey, block.Role)
	}
	return nil
}

func validateClassifiedAttachments(index int, block ClassifiedBlock) error {
	if len(block.NativeAttachments) == 0 {
		return nil
	}
	if len(block.NativeAttachments) > 1 {
		return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q carries %d native attachments; one source ref owns at most one metadata record; keep one attachment per block", index, block.NativeKey, len(block.NativeAttachments))
	}
	for j := range block.NativeAttachments {
		attachment := block.NativeAttachments[j]
		if attachment.ID == "" || len(attachment.ID) > indexformat.MaxNativeMetadataIDBytes {
			return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q native attachment[%d] id is empty or over %d bytes; attached evidence has no bounded identity; emit a bounded opaque id", index, block.NativeKey, j, indexformat.MaxNativeMetadataIDBytes)
		}
		if !attachment.Kind.IsValid() {
			return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q native attachment %q kind %q is outside the closed set; attached evidence cannot be classified; use a published kind", index, block.NativeKey, attachment.ID, attachment.Kind)
		}
		if !attachment.SourceType.IsValid() {
			return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q native attachment %q source type %q is outside the closed set; attached evidence cannot be classified; use a published source type", index, block.NativeKey, attachment.ID, attachment.SourceType)
		}
		if attachment.CustomType != "" && len(attachment.CustomType) > indexformat.MaxNativeMetadataCustomTypeBytes {
			return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q native attachment %q customType is over %d bytes; consumers cannot identify the extension; emit a shorter valid value", index, block.NativeKey, attachment.ID, indexformat.MaxNativeMetadataCustomTypeBytes)
		}
		if strings.TrimSpace(attachment.Data) == "" || projectionAttachmentDataIsNull([]byte(attachment.Data)) {
			return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q native attachment %q data is missing or null; attached evidence cannot be checked; provide a non-null JSON value", index, block.NativeKey, attachment.ID)
		}
		var probe any
		if err := json.Unmarshal([]byte(attachment.Data), &probe); err != nil {
			return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q native attachment %q data is not valid JSON; attached evidence cannot be checked; provide a JSON value: %w", index, block.NativeKey, attachment.ID, err)
		}
		if attachment.MessageRole != "" && !attachment.MessageRole.IsValid() {
			return fmt.Errorf("ingest.BuildGeneration: block[%d] native key %q native attachment %q message role %q is outside the closed set; attached evidence cannot be classified; use a published message role or omit it", index, block.NativeKey, attachment.ID, attachment.MessageRole)
		}
	}
	return nil
}

// resolveProjectionBlocks computes each block's effective partition: it
// resolves uncertainty across each connected carrier/call subtree before final
// layout, keeps certain tool subtrees with their carrier, and retains every
// unique unproved ambiguous representation with its own ref in uncertain
// earlier history.
func resolveProjectionBlocks(capture ClassifiedCapture) ([]*resolvedBlock, error) {
	resolved := make([]*resolvedBlock, len(capture.Blocks))
	byKey := make(map[string]*resolvedBlock, len(capture.Blocks))
	for i := range capture.Blocks {
		block := capture.Blocks[i]
		if strings.TrimSpace(block.NativeKey) == "" {
			return nil, fmt.Errorf("ingest.BuildGeneration: block[%d] has an empty native key; the block cannot be matched on reuse; encode a non-empty opaque native key", i)
		}
		if _, duplicate := byKey[block.NativeKey]; duplicate {
			return nil, fmt.Errorf("ingest.BuildGeneration: native key %q repeats in one capture; a later capture could not tell the blocks apart; keep one block per native key", block.NativeKey)
		}
		rb := &resolvedBlock{block: block, section: block.Section.Index}
		resolved[i] = rb
		byKey[block.NativeKey] = rb
	}

	firstEarlier := 1
	hasEarlier := len(capture.EarlierStates) > 0

	for _, rb := range resolved {
		if rb.block.AmbiguousPairKey != "" {
			if !hasEarlier {
				return nil, fmt.Errorf("ingest.BuildGeneration: native key %q is an ambiguous pair but the capture declares no earlier section; the pair cannot be retained honestly; declare an uncertain earlier section", rb.block.NativeKey)
			}
			rb.section = firstEarlier
			rb.block.Uncertain = true
		}
		if rb.block.UncertainSubtree {
			if !hasEarlier {
				return nil, fmt.Errorf("ingest.BuildGeneration: carrier %q has an uncertain subtree but the capture declares no earlier section; the subtree cannot be placed honestly; declare an uncertain earlier section", rb.block.NativeKey)
			}
			rb.section = firstEarlier
			rb.block.Uncertain = true
		}
	}

	// Resolve uncertainty across each connected carrier/call/result subtree
	// before final layout. Uncertainty anywhere that would split the subtree
	// relocates the entire subtree into one declared earlier section; certain
	// tool children follow their own carrier. Connected means sharing a
	// carrier edge or a native call identity.
	if err := resolveUncertainSubtrees(resolved, byKey, hasEarlier, firstEarlier); err != nil {
		return nil, err
	}

	// Retain every unproved ambiguous representation with its own ref in
	// uncertain earlier history. Only an explicit native correlation proof
	// collapses representations to one entry; byte or evidence equality never
	// does, and an ambiguous first-wins selection never discards content.
	collapseProvedMirrors(resolved)
	return resolved, nil
}

// resolveUncertainSubtrees moves every connected carrier/call/result component
// that carries uncertainty into one declared earlier section. A component is
// connected through carrier edges and shared native call identities, so a
// tool result explicitly marked uncertain in earlier history pulls its carrier
// and siblings with it instead of being promoted into the carrier's main
// partition.
func resolveUncertainSubtrees(resolved []*resolvedBlock, byKey map[string]*resolvedBlock, hasEarlier bool, firstEarlier int) error {
	indexOf := make(map[*resolvedBlock]int, len(resolved))
	for i, rb := range resolved {
		indexOf[rb] = i
	}
	parent := make([]int, len(resolved))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}
	// Carrier edges.
	for i, rb := range resolved {
		if rb.block.Depth == 0 {
			continue
		}
		carrier, ok := byKey[rb.block.CarrierNativeKey]
		if !ok {
			return fmt.Errorf("ingest.BuildGeneration: depth-1 block %q names carrier %q that is not in this capture; the tool subtree would be split; supply the carrier block", rb.block.NativeKey, rb.block.CarrierNativeKey)
		}
		if carrier.block.Depth != 0 {
			return fmt.Errorf("ingest.BuildGeneration: depth-1 block %q names carrier %q whose depth is %d, not 0; the tool parent cannot be resolved; point the block at its depth-0 carrier", rb.block.NativeKey, carrier.block.NativeKey, carrier.block.Depth)
		}
		union(i, indexOf[carrier])
	}
	// Shared native call identities.
	byCall := make(map[string][]int)
	for i, rb := range resolved {
		if rb.block.ToolCallKey != "" {
			byCall[rb.block.ToolCallKey] = append(byCall[rb.block.ToolCallKey], i)
		}
	}
	for _, members := range byCall {
		for k := 1; k < len(members); k++ {
			union(members[0], members[k])
		}
	}
	// Explicitly proved native correlations are one item and must never be split
	// across partitions: uncertainty anywhere in a proved mirror moves the whole
	// group into earlier history before the collapse keeps the owner's entry.
	byCorrelation := make(map[string][]int)
	for i, rb := range resolved {
		if rb.block.NativeCorrelationKey != "" {
			byCorrelation[rb.block.NativeCorrelationKey] = append(byCorrelation[rb.block.NativeCorrelationKey], i)
		}
	}
	for _, members := range byCorrelation {
		for k := 1; k < len(members); k++ {
			union(members[0], members[k])
		}
	}
	components := make(map[int][]*resolvedBlock)
	for i, rb := range resolved {
		root := find(i)
		components[root] = append(components[root], rb)
	}
	for _, members := range components {
		uncertain := false
		for _, rb := range members {
			if rb.block.Uncertain || rb.block.UncertainSubtree || rb.block.AmbiguousPairKey != "" {
				uncertain = true
				break
			}
		}
		if !uncertain {
			// Certain subtrees stay partition-closed to their own carrier.
			for _, rb := range members {
				if rb.block.Depth == 0 {
					continue
				}
				carrier := byKey[rb.block.CarrierNativeKey]
				rb.section = carrier.section
			}
			continue
		}
		if !hasEarlier {
			return fmt.Errorf("ingest.BuildGeneration: a connected carrier/call subtree carries uncertain evidence but the capture declares no earlier section; the subtree cannot be placed honestly; declare an uncertain earlier section")
		}
		for _, rb := range members {
			rb.section = firstEarlier
			rb.block.Uncertain = true
		}
	}
	return nil
}

// collapseProvedMirrors collapses only blocks the native reducer has positively
// proved are one native item, by carrying the same non-empty
// NativeCorrelationKey. The proof is explicit native correlation; the
// projection never infers it from equal bytes, equal evidence or first-wins
// order. Every unproved representation keeps its own ref, and the mirror's
// native key aliases the retained owner so a later capture cannot reallocate.
func collapseProvedMirrors(resolved []*resolvedBlock) {
	groups := make(map[string][]*resolvedBlock)
	for _, rb := range resolved {
		if rb.block.NativeCorrelationKey != "" {
			groups[rb.block.NativeCorrelationKey] = append(groups[rb.block.NativeCorrelationKey], rb)
		}
	}
	for _, group := range groups {
		owner := group[0]
		for _, rb := range group[1:] {
			rb.dropped = true
			rb.dropTo = owner
			rb.section = owner.section
		}
	}
}

// allocateBlockRefs assigns one opaque block ref per retained block, reusing a
// prior alias when one exists. When two distinct native keys inherited the same
// prior ref because an earlier generation merged them (an unproved pair since
// proven distinct, or a proved mirror whose proof was withdrawn), the first
// retained owner keeps the ref and the newly distinct representation gets a
// fresh one. A persisted alias is never silently cleared or re-merged.
func allocateBlockRefs(resolved []*resolvedBlock, prior ProjectionPriorState, allocator RefAllocator) error {
	used := make(map[schema.SourceEntryRef]string, len(resolved))
	for _, rb := range resolved {
		if rb.dropped {
			continue
		}
		ref := schema.SourceEntryRef("")
		if priorRef, ok := prior.Entries[rb.block.NativeKey]; ok {
			if err := priorRef.Validate(); err != nil {
				return fmt.Errorf("ingest.BuildGeneration: prior alias for native key %q holds an invalid ref; the identity cannot be reused; re-index the session from a valid prior generation: %w", rb.block.NativeKey, err)
			}
			ref = priorRef
		}
		if ref == "" || used[ref] != "" {
			// No prior identity, or two distinct native keys resolved to one
			// prior ref. Apply safe reconciliation: keep the owner's ref, then
			// allocate a fresh identity for the newly distinct representation
			// so neither representation is lost and no alias needs clearing.
			fresh, err := allocator.NewEntryRef()
			if err != nil {
				return fmt.Errorf("ingest.BuildGeneration: allocating a block ref for native key %q failed; the block has no identity; retry the ingest run: %w", rb.block.NativeKey, err)
			}
			if err := fresh.Validate(); err != nil {
				return fmt.Errorf("ingest.BuildGeneration: the allocator returned an invalid block ref for native key %q; the block has no publicly valid identity; fix the allocator: %w", rb.block.NativeKey, err)
			}
			ref = fresh
		}
		if other, duplicate := used[ref]; duplicate {
			return fmt.Errorf("ingest.BuildGeneration: native keys %q and %q resolved to the same block ref %q; the allocator reused an identity; fix the allocator", other, rb.block.NativeKey, ref)
		}
		rb.ref = ref
		used[ref] = rb.block.NativeKey
	}
	for _, rb := range resolved {
		if rb.dropped {
			rb.ref = rb.dropTo.ref
		}
	}
	return nil
}

// allocateSubmissionRefs assigns one opaque submission ref per native
// acceptance key and writes it into the block provenance, the single authority
// for the value.
func allocateSubmissionRefs(resolved []*resolvedBlock, prior ProjectionPriorState, allocator RefAllocator) error {
	byKey := make(map[string]schema.SubmissionRef)
	for _, rb := range resolved {
		if rb.dropped || rb.block.SubmissionKey == "" {
			continue
		}
		if rb.block.Provenance == nil {
			return fmt.Errorf("ingest.BuildGeneration: native key %q carries acceptance key %q but no provenance; a submission cannot be attributed; classify the block before projecting it", rb.block.NativeKey, rb.block.SubmissionKey)
		}
		if rb.block.Provenance.Origin != schema.ContentOriginSubmittedInput {
			return fmt.Errorf("ingest.BuildGeneration: native key %q carries acceptance key %q but origin %q is not submitted_input; a submission group would be attributed to the wrong content; set the origin or drop the acceptance key", rb.block.NativeKey, rb.block.SubmissionKey, rb.block.Provenance.Origin)
		}
		if ref, ok := prior.Submissions[rb.block.SubmissionKey]; ok {
			byKey[rb.block.SubmissionKey] = ref
		} else if _, ok := byKey[rb.block.SubmissionKey]; !ok {
			ref, err := allocator.NewSubmissionRef()
			if err != nil {
				return fmt.Errorf("ingest.BuildGeneration: allocating a submission ref for acceptance key %q failed; the submission has no identity; retry the ingest run: %w", rb.block.SubmissionKey, err)
			}
			if err := ref.Validate(); err != nil {
				return fmt.Errorf("ingest.BuildGeneration: the allocator returned an invalid submission ref for acceptance key %q; fix the allocator: %w", rb.block.SubmissionKey, err)
			}
			byKey[rb.block.SubmissionKey] = ref
		}
	}
	for _, rb := range resolved {
		if rb.dropped || rb.block.SubmissionKey == "" {
			continue
		}
		ref := byKey[rb.block.SubmissionKey]
		provenance := *rb.block.Provenance
		provenance.SubmissionRef = ref
		rb.block.Provenance = &provenance
	}
	return nil
}

// layoutProjectionPartitions builds the entry rows per partition and assigns
// EntryIndex within each partition.
func layoutProjectionPartitions(capture ClassifiedCapture, resolved []*resolvedBlock) (projectionPartitions, error) {
	out := projectionPartitions{
		main:    indexformat.Partition{},
		earlier: make([]indexformat.EarlierPartition, len(capture.EarlierStates)),
	}
	for i, state := range capture.EarlierStates {
		out.earlier[i] = indexformat.EarlierPartition{State: state}
	}
	for _, rb := range resolved {
		if rb.dropped {
			continue
		}
		entry, err := projectionEntry(capture, rb)
		if err != nil {
			return projectionPartitions{}, err
		}
		if rb.section == 0 {
			rb.entryIndex = len(out.main.Entries)
			entry.EntryIndex = rb.entryIndex
			out.main.Entries = append(out.main.Entries, entry)
			continue
		}
		partition := &out.earlier[rb.section-1]
		rb.entryIndex = len(partition.Content.Entries)
		entry.EntryIndex = rb.entryIndex
		partition.Content.Entries = append(partition.Content.Entries, entry)
	}
	return out, nil
}

// projectionEntry builds one durable entry row from a resolved block.
func projectionEntry(capture ClassifiedCapture, rb *resolvedBlock) (schema.SessionEntry, error) {
	block := rb.block
	if !block.Role.IsValid() {
		return schema.SessionEntry{}, fmt.Errorf("ingest.BuildGeneration: native key %q role %q is outside the closed role set; the entry cannot be rendered; classify the block with a published role", block.NativeKey, block.Role)
	}
	if !block.EntryType.IsValid() {
		return schema.SessionEntry{}, fmt.Errorf("ingest.BuildGeneration: native key %q entryType %q is outside the closed entry-type set; the entry cannot be rendered; classify the block with a published entry type", block.NativeKey, block.EntryType)
	}
	if block.Depth < 0 {
		return schema.SessionEntry{}, fmt.Errorf("ingest.BuildGeneration: native key %q depth %d is negative; the entry has no valid nesting; record a nonnegative depth", block.NativeKey, block.Depth)
	}
	entry := schema.SessionEntry{
		SessionID:      capture.SessionID,
		Harness:        schema.Harness(capture.Harness),
		Role:           block.Role,
		EntryType:      block.EntryType,
		Depth:          block.Depth,
		SourceEntryRef: rb.ref,
		Provenance:     block.Provenance,
		TimestampMs:    block.TimestampMs,
		PartType:       block.PartType,
		HasThinking:    block.HasThinking,
	}
	if block.Usage != nil {
		if block.Usage.TokensIn != nil {
			value := *block.Usage.TokensIn
			entry.TokensIn = &value
		}
		if block.Usage.TokensOut != nil {
			value := *block.Usage.TokensOut
			entry.TokensOut = &value
		}
	}
	if block.ObservedModel != nil {
		encoded, err := json.Marshal(map[string]string{"model_id": *block.ObservedModel})
		if err != nil {
			return schema.SessionEntry{}, fmt.Errorf("ingest.BuildGeneration: native key %q observedModel cannot be encoded; the observation cannot be preserved; supply the exact source identifier: %w", block.NativeKey, err)
		}
		value := string(encoded)
		entry.Extra = &value
	}
	switch block.EntryType {
	case schema.EntryTypeToolUse:
		entry.HasToolUse = true
		entry.ToolInput = nonEmptyString(block.ToolArguments)
		entry.ContentPreview = nonEmptyString(block.ToolArguments)
	case schema.EntryTypeToolResult:
		entry.ToolOutput = nonEmptyString(block.ToolResult)
		entry.ContentPreview = nonEmptyString(block.ToolResult)
	default:
		entry.ContentPreview = nonEmptyString(block.Content)
	}
	if block.ToolName != "" {
		name := block.ToolName
		entry.ToolNamesCSV = &name
	}
	if block.ToolKind != "" {
		kind := block.ToolKind
		entry.ToolKind = &kind
	}
	return entry, nil
}

// remapToolParents assigns each depth-1 entry its ToolCallID and ParentIndex
// after the final partition order is known. It pairs a tool_result to its
// tool_use by native call identity, never by content.
func remapToolParents(partitions projectionPartitions, resolved []*resolvedBlock) error {
	type callKey struct {
		partition int
		native    string
	}
	type callOwner struct {
		entry *schema.SessionEntry
	}
	calls := make(map[callKey]callOwner)
	indexByRef := make(map[schema.SourceEntryRef]*resolvedBlock, len(resolved))
	byNative := make(map[string]*resolvedBlock, len(resolved))

	assign := func(entries []schema.SessionEntry, section int) error {
		for i := range entries {
			entry := &entries[i]
			rb := indexByRef[entry.SourceEntryRef]
			if rb == nil {
				return fmt.Errorf("ingest.BuildGeneration: entry ref %q is not in the resolved block set; the projection is inconsistent; rebuild the capture", entry.SourceEntryRef)
			}
			if rb.block.Depth == 0 {
				continue
			}
			carrier := byNative[rb.block.CarrierNativeKey]
			if carrier == nil {
				return fmt.Errorf("ingest.BuildGeneration: depth-1 block %q has no carrier %q; the tool subtree parent cannot be resolved; supply the carrier block", rb.block.NativeKey, rb.block.CarrierNativeKey)
			}
			parent := carrier.entryIndex
			entry.ParentIndex = &parent
			switch entry.EntryType {
			case schema.EntryTypeToolUse:
				if rb.block.ToolCallKey == "" {
					return fmt.Errorf("ingest.BuildGeneration: tool_use native key %q has no native call identity; a later result could not be paired; supply the native call key", rb.block.NativeKey)
				}
				id := string(rb.ref)
				entry.ToolCallID = &id
				calls[callKey{partition: section, native: rb.block.ToolCallKey}] = callOwner{entry: entry}
			case schema.EntryTypeToolResult:
				owner, ok := calls[callKey{partition: section, native: rb.block.ToolCallKey}]
				if !ok {
					return fmt.Errorf("ingest.BuildGeneration: tool_result native key %q has no preceding tool_use with call identity %q in the same partition; the result would be orphaned or split; supply the call block first in its partition", rb.block.NativeKey, rb.block.ToolCallKey)
				}
				id := *owner.entry.ToolCallID
				entry.ToolCallID = &id
			}
		}
		return nil
	}

	for i := range resolved {
		if !resolved[i].dropped {
			indexByRef[resolved[i].ref] = resolved[i]
			byNative[resolved[i].block.NativeKey] = resolved[i]
		}
	}
	if err := assign(partitions.main.Entries, 0); err != nil {
		return err
	}
	for i := range partitions.earlier {
		if err := assign(partitions.earlier[i].Content.Entries, i+1); err != nil {
			return err
		}
	}
	return nil
}

// attachProjectionNativeMetadata places each typed native-metadata enrichment
// on its owner's final partition and remaps its owner and tool attachment to
// the allocated opaque refs. A record never leaves its owner's partition, so
// call/result/usage/native ownership survives layout and an uncertain
// relocation carries its enrichment with it.
func attachProjectionNativeMetadata(partitions *projectionPartitions, resolved []*resolvedBlock) error {
	type callKey struct {
		partition int
		native    string
	}
	toolIDs := make(map[callKey]string)
	for _, rb := range resolved {
		if rb.dropped || rb.block.Depth != 1 || rb.block.EntryType != schema.EntryTypeToolUse {
			continue
		}
		if rb.block.ToolCallKey == "" {
			continue
		}
		toolIDs[callKey{partition: rb.section, native: rb.block.ToolCallKey}] = string(rb.ref)
	}
	for _, rb := range resolved {
		if rb.dropped || len(rb.block.NativeAttachments) == 0 {
			continue
		}
		for j := range rb.block.NativeAttachments {
			supplied := rb.block.NativeAttachments[j]
			record := schema.NativeMetadataRecord{
				ID:   supplied.ID,
				Kind: supplied.Kind,
				Source: schema.NativeSourceRef{
					EntryRef:   rb.ref,
					SourceType: supplied.SourceType,
				},
				CustomType: supplied.CustomType,
				Data:       json.RawMessage(supplied.Data),
			}
			if supplied.MessageRole != "" {
				record.Source.MessageRole = supplied.MessageRole
			}
			if supplied.AttachmentToolCallKey != "" {
				toolID, ok := toolIDs[callKey{partition: rb.section, native: supplied.AttachmentToolCallKey}]
				if !ok {
					return fmt.Errorf("ingest.BuildGeneration: native key %q native attachment %q names tool call %q with no tool_use in the same partition; the attachment would be split; supply the call block in its partition", rb.block.NativeKey, supplied.ID, supplied.AttachmentToolCallKey)
				}
				record.Attachment = &schema.NativeAttachmentRef{ToolCallID: toolID}
			}
			if rb.section == 0 {
				partitions.main.NativeMetadata = append(partitions.main.NativeMetadata, record)
				continue
			}
			partitions.earlier[rb.section-1].Content.NativeMetadata = append(partitions.earlier[rb.section-1].Content.NativeMetadata, record)
		}
	}
	return nil
}

// validateProjectionProvenance validates each entry's provenance against the
// schema closed sets before the projection is accepted.
func validateProjectionProvenance(partitions projectionPartitions) error {
	check := func(entries []schema.SessionEntry, path string) error {
		for i := range entries {
			entry := entries[i]
			if entry.Provenance == nil {
				continue
			}
			if err := entry.Provenance.Validate(); err != nil {
				return fmt.Errorf("ingest.BuildGeneration %s entries[%d] ref %q: %w; the candidate provenance is invalid; correct the classified block", path, i, entry.SourceEntryRef, err)
			}
		}
		return nil
	}
	if err := check(partitions.main.Entries, "main"); err != nil {
		return err
	}
	for i := range partitions.earlier {
		if err := check(partitions.earlier[i].Content.Entries, fmt.Sprintf("earlier[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

// validateUniqueProjectionRefs enforces that a source ref appears once across
// main and every earlier section.
func validateUniqueProjectionRefs(partitions projectionPartitions) error {
	seen := make(map[schema.SourceEntryRef]string)
	check := func(entries []schema.SessionEntry, path string) error {
		for i := range entries {
			ref := entries[i].SourceEntryRef
			if other, duplicate := seen[ref]; duplicate {
				return fmt.Errorf("ingest.BuildGeneration: source ref %q appears in %s and %s; two blocks would share one identity; keep each ref in one partition", ref, other, path)
			}
			seen[ref] = path
		}
		return nil
	}
	if err := check(partitions.main.Entries, "main"); err != nil {
		return err
	}
	for i := range partitions.earlier {
		if err := check(partitions.earlier[i].Content.Entries, fmt.Sprintf("earlier[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

// buildProjectionContent records the full bytes once per emitted ref. The
// relative blob path is a validated owned-generation path; the digest is local
// integrity evidence only.
func buildProjectionContent(partitions projectionPartitions) ([]indexformat.ContentRecord, error) {
	var records []indexformat.ContentRecord
	add := func(entries []schema.SessionEntry) error {
		for i := range entries {
			entry := entries[i]
			content := projectionEntryContent(entry)
			if content == "" {
				continue
			}
			relative := projectionContentPath(entry.SourceEntryRef)
			sum := sha256.Sum256([]byte(content))
			record := indexformat.ContentRecord{
				Ref:          entry.SourceEntryRef,
				RelativeBlob: relative,
				ByteLength:   int64(len(content)),
				Digest:       hex.EncodeToString(sum[:]),
			}
			if err := record.Validate(); err != nil {
				return fmt.Errorf("ingest.BuildGeneration: content for ref %q is not a valid managed record; the candidate cannot hydrate it; fix the ref or content: %w", entry.SourceEntryRef, err)
			}
			records = append(records, record)
		}
		return nil
	}
	if err := add(partitions.main.Entries); err != nil {
		return nil, err
	}
	for i := range partitions.earlier {
		if err := add(partitions.earlier[i].Content.Entries); err != nil {
			return nil, err
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Ref < records[j].Ref })
	return records, nil
}

// projectionContentPath names the owned-generation blob for one ref.
func projectionContentPath(ref schema.SourceEntryRef) string {
	return "content/" + string(ref)
}

// projectionEntryContent returns the full canonical content bytes one entry
// owns, matching the record the builder persists.
func projectionEntryContent(entry schema.SessionEntry) string {
	switch entry.EntryType {
	case schema.EntryTypeToolUse:
		if entry.ToolInput != nil {
			return *entry.ToolInput
		}
	case schema.EntryTypeToolResult:
		if entry.ToolOutput != nil {
			return *entry.ToolOutput
		}
	default:
		if entry.ContentPreview != nil {
			return *entry.ContentPreview
		}
	}
	return ""
}

// buildProjectionAliases persists one block alias per classified key and one
// acceptance alias per submission key pointing at the first submitted input
// block. Dropped ambiguous keys alias the retained entry so a reopen cannot
// reallocate.
func buildProjectionAliases(resolved []*resolvedBlock) ([]indexformat.NativeAlias, error) {
	byAlias := make(map[string]schema.SourceEntryRef)
	firstSubmissionRef := make(map[string]schema.SourceEntryRef)
	for _, rb := range resolved {
		if rb.dropped {
			continue
		}
		key, err := encodeBlockAliasKey(rb.block.NativeKey)
		if err != nil {
			return nil, fmt.Errorf("ingest.BuildGeneration: %w", err)
		}
		byAlias[key] = rb.ref
		if rb.block.SubmissionKey != "" {
			if _, seen := firstSubmissionRef[rb.block.SubmissionKey]; !seen {
				firstSubmissionRef[rb.block.SubmissionKey] = rb.ref
			}
		}
	}
	for _, rb := range resolved {
		if !rb.dropped {
			continue
		}
		key, err := encodeBlockAliasKey(rb.block.NativeKey)
		if err != nil {
			return nil, fmt.Errorf("ingest.BuildGeneration: %w", err)
		}
		if rb.dropTo == nil {
			return nil, fmt.Errorf("ingest.BuildGeneration: ambiguous block %q has no retained entry; the pair cannot be represented; rebuild the capture", rb.block.NativeKey)
		}
		byAlias[key] = rb.dropTo.ref
	}
	for submissionKey, ref := range firstSubmissionRef {
		key, err := encodeSubmissionAliasKey(submissionKey)
		if err != nil {
			return nil, fmt.Errorf("ingest.BuildGeneration: %w", err)
		}
		if existing, ok := byAlias[key]; ok && existing != ref {
			return nil, fmt.Errorf("ingest.BuildGeneration: submission alias key %q collides with a block alias; the identities are ambiguous; encode distinct native keys", submissionKey)
		}
		byAlias[key] = ref
	}
	keys := make([]string, 0, len(byAlias))
	for key := range byAlias {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	aliases := make([]indexformat.NativeAlias, 0, len(keys))
	for _, key := range keys {
		alias := indexformat.NativeAlias{NativeKey: key, Ref: byAlias[key]}
		if err := alias.Validate(); err != nil {
			return nil, fmt.Errorf("ingest.BuildGeneration: alias %q is invalid; the candidate cannot persist it; fix the native key or ref: %w", key, err)
		}
		aliases = append(aliases, alias)
	}
	return aliases, nil
}

// applyStrictCounts writes the single count authority and the prose-title refs.
// TurnCount is the emitted main entry cardinality after folding; the input
// count is the distinct submission refs that satisfy the strict predicate.
func applyStrictCounts(generation *indexformat.Generation, completeness indexformat.GenerationCompleteness) {
	main := generation.Main.Entries
	turnCount := 0
	submissions := make(map[schema.SubmissionRef]struct{})
	var titleRefs []schema.SourceEntryRef
	for i := range main {
		entry := main[i]
		if entry.Depth == 0 {
			turnCount++
		}
		if !eligibleInputProvenance(entry.Provenance) {
			continue
		}
		submissions[entry.Provenance.SubmissionRef] = struct{}{}
		if entry.Provenance.InputModality == schema.InputModalityText && strings.TrimSpace(projectionEntryContent(entry)) != "" {
			titleRefs = append(titleRefs, entry.SourceEntryRef)
		}
	}
	generation.Metadata.Stats.TurnCount = turnCount
	generation.TitleRefs = titleRefs
	if completeness == indexformat.GenerationCompletenessComplete {
		count := int64(len(submissions))
		generation.Metadata.Stats.InputSubmissionCount = &count
	} else {
		generation.Metadata.Stats.InputSubmissionCount = nil
	}
}

// eligibleInputProvenance is the strict admitted-submission predicate. Actor
// unknown is permitted; harness and agent-delegate actors are not. It is an
// admitted-submission count, not a claim of human authorship.
func eligibleInputProvenance(provenance *schema.ContentProvenance) bool {
	if provenance == nil || provenance.SubmissionRef == "" {
		return false
	}
	if provenance.Origin != schema.ContentOriginSubmittedInput {
		return false
	}
	if provenance.Ownership != schema.ContentOwnershipLocal {
		return false
	}
	if provenance.Delivery != schema.DeliveryOriginSessionAdmission {
		return false
	}
	if provenance.Evidence != schema.EvidenceNativeTyped && provenance.Evidence != schema.EvidenceLifecycleTyped {
		return false
	}
	if provenance.Actor == schema.ActorOriginHarness || provenance.Actor == schema.ActorOriginAgentDelegate {
		return false
	}
	return true
}

func nonEmptyString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func projectionAttachmentDataIsNull(data []byte) bool {
	trimmed := strings.TrimSpace(string(data))
	return trimmed == "" || trimmed == "null"
}
