package indexformat

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/peasant-labs/schema"
)

// MaxNativeAliasKeyBytes bounds one opaque native alias key. Native identity
// is encoded into this key before it is persisted; the raw private path stays
// in root-confined artifact bookkeeping and never in the alias key.
const MaxNativeAliasKeyBytes = 512

// MaxNativeMetadataIDBytes and MaxNativeMetadataCustomTypeBytes mirror the
// public bounds the schema enforces on attached native metadata records.
const (
	MaxNativeMetadataIDBytes         = 96
	MaxNativeMetadataCustomTypeBytes = 128
)

// Partition is one ordered content partition: the main turn stream, or one
// earlier-history section. Native metadata always attaches to the partition
// that owns its source entry.
type Partition struct {
	Entries        []schema.SessionEntry
	NativeMetadata []schema.NativeMetadataRecord
}

// Validate checks one partition's entries and native attachment. path names
// the partition in an error so a caller can tell main from earlier.
// sessionID and harness name the owning session from the generation or
// snapshot metadata that owns this partition; every entry must carry that
// same ownership so a reader never mixes blocks across sessions.
func (p Partition) Validate(path string, sessionID schema.SessionID, harness schema.Harness) error {
	entryIndexes := make(map[int]struct{}, len(p.Entries))
	indexDepth := make(map[int]int, len(p.Entries))
	refs := make(map[schema.SourceEntryRef]struct{}, len(p.Entries))
	for i := range p.Entries {
		entry := p.Entries[i]
		if err := validatePartitionEntry(entry, sessionID, harness, path, i); err != nil {
			return err
		}
		if _, duplicate := entryIndexes[entry.EntryIndex]; duplicate {
			return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: entryIndex %d repeats inside one partition; a reader cannot order the blocks; keep one entry per index", path, i, entry.EntryIndex)
		}
		entryIndexes[entry.EntryIndex] = struct{}{}
		indexDepth[entry.EntryIndex] = entry.Depth
		if entry.SourceEntryRef == "" {
			continue
		}
		if err := entry.SourceEntryRef.Validate(); err != nil {
			return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: source entry ref is invalid; the partition cannot be reconstructed; emit a bounded opaque ref: %w", path, i, err)
		}
		if _, duplicate := refs[entry.SourceEntryRef]; duplicate {
			return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: source entry ref %q repeats inside one partition; a reader cannot tell the blocks apart; keep one entry per ref", path, i, entry.SourceEntryRef)
		}
		refs[entry.SourceEntryRef] = struct{}{}
	}
	for i := range p.Entries {
		entry := p.Entries[i]
		if entry.ParentIndex == nil {
			continue
		}
		parentDepth, ok := indexDepth[*entry.ParentIndex]
		if !ok {
			return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: parentIndex %d does not target an entry in this partition; folded attachment ownership is ambiguous; remap the parent after partition layout", path, i, *entry.ParentIndex)
		}
		if parentDepth != 0 {
			return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: parentIndex %d targets a depth-%d entry; only a depth-0 assistant carrier can own a tool subtree; remap the parent to the carrier", path, i, *entry.ParentIndex, parentDepth)
		}
	}
	return validatePartitionNativeMetadata(p.NativeMetadata, refs, path)
}

// validatePartitionEntry checks one staged entry's role, type, provenance,
// owning session and harness, and depth/parent shape. It uses the schema's
// own closed sets so the managed domain never drifts from the wire contract.
// An absent provenance stays permitted for legacy rows; a present one must be
// fully valid.
func validatePartitionEntry(entry schema.SessionEntry, sessionID schema.SessionID, harness schema.Harness, path string, index int) error {
	if !entry.Role.IsValid() {
		return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: role %q is outside the closed role set; a reader cannot classify the block; use a published role", path, index, entry.Role)
	}
	if !entry.EntryType.IsValid() {
		return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: entryType %q is outside the closed entry-type set; a reader cannot classify the block; use a published entry type", path, index, entry.EntryType)
	}
	if entry.Provenance != nil {
		if err := entry.Provenance.Validate(); err != nil {
			return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: provenance is malformed; the block cannot be attributed; emit complete provenance or omit it: %w", path, index, err)
		}
	}
	if entry.SessionID != sessionID {
		return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: sessionId %q disagrees with the owning session %q; blocks from another session cannot be staged here; keep one owning session per generation", path, index, entry.SessionID, sessionID)
	}
	if !entry.Harness.IsKnown() {
		return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: harness %q is not a known harness; the block cannot be attributed; use a known harness", path, index, entry.Harness)
	}
	if entry.Harness != harness {
		return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: harness %q disagrees with the owning harness %q; blocks from another harness cannot be staged here; keep one owning harness per generation", path, index, entry.Harness, harness)
	}
	if entry.EntryIndex < 0 {
		return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: entryIndex %d is negative; partition order cannot be reconstructed; record the nonnegative entry index", path, index, entry.EntryIndex)
	}
	if entry.Depth < 0 {
		return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: depth %d is negative; attachment ancestry cannot be reconstructed; record depth 0 for a message or 1 for a content part", path, index, entry.Depth)
	}
	if entry.Depth == 0 && entry.ParentIndex != nil {
		return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: depth 0 carries parentIndex %d; a message is a root, not a tool part; keep parentIndex nil at depth 0 and use the message-chain link for ancestry", path, index, *entry.ParentIndex)
	}
	if entry.Depth > 0 && entry.ParentIndex == nil {
		return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: depth %d has no parentIndex; a content part cannot be attached; record the carrier entry index in the same partition", path, index, entry.Depth)
	}
	if entry.ToolKind != nil && !entry.ToolKind.IsValid() {
		return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: toolKind %q is outside the closed set; the tool cannot be classified; use a published tool kind or omit it", path, index, *entry.ToolKind)
	}
	if entry.StopReason != nil && !entry.StopReason.IsValid() {
		return fmt.Errorf("indexformat.Partition.Validate %s entries[%d]: stopReason %q is outside the closed set; the turn end cannot be classified; use a published stop reason or omit it", path, index, *entry.StopReason)
	}
	return nil
}

// entryRefs returns the partition's non-empty source entry refs.
func (p Partition) entryRefs() map[schema.SourceEntryRef]struct{} {
	refs := make(map[schema.SourceEntryRef]struct{}, len(p.Entries))
	for i := range p.Entries {
		if p.Entries[i].SourceEntryRef != "" {
			refs[p.Entries[i].SourceEntryRef] = struct{}{}
		}
	}
	return refs
}

// EarlierPartition is one earlier-history section with its content partition.
type EarlierPartition struct {
	State   schema.EarlierHistoryState
	Content Partition
}

// Validate checks one earlier section's state and content partition. The
// owner names the generation or snapshot session every entry must carry.
func (e EarlierPartition) Validate(index int, sessionID schema.SessionID, harness schema.Harness) error {
	if !e.State.IsValid() {
		return fmt.Errorf("indexformat.EarlierPartition.Validate earlier[%d]: state %q is outside the closed earlier-history set; retained history cannot be explained; use a published state", index, e.State)
	}
	return e.Content.Validate(fmt.Sprintf("earlier[%d]", index), sessionID, harness)
}

// ContentRecord is one immutable managed content blob for a source entry ref.
// RelativeBlob is a validated owned-generation relative path; the byte length
// and digest are integrity evidence, and all three stay local.
type ContentRecord struct {
	Ref          schema.SourceEntryRef
	RelativeBlob string
	ByteLength   int64
	Digest       string
}

// Validate checks one content record's ref, managed relative path, length and
// integrity digest.
func (c ContentRecord) Validate() error {
	if err := c.Ref.Validate(); err != nil {
		return fmt.Errorf("indexformat.ContentRecord.Validate: ref is invalid; full content cannot be addressed; emit a bounded opaque ref: %w", err)
	}
	if err := ValidateOwnedRelativeBlob(c.RelativeBlob); err != nil {
		return fmt.Errorf("indexformat.ContentRecord.Validate ref %q: %w", c.Ref, err)
	}
	if c.ByteLength < 0 {
		return fmt.Errorf("indexformat.ContentRecord.Validate ref %q: byteLength %d is negative; a reader cannot bound the content; record the exact nonnegative byte length", c.Ref, c.ByteLength)
	}
	if strings.TrimSpace(c.Digest) == "" {
		return fmt.Errorf("indexformat.ContentRecord.Validate ref %q: digest is empty; managed content cannot be verified before use; record the integrity digest", c.Ref)
	}
	return nil
}

// ValidateOwnedRelativeBlob rejects a blob path that is absolute, uses a
// backslash separator, or can escape the owned generation directory. It is a
// local path guard, not a filesystem operation.
func ValidateOwnedRelativeBlob(relative string) error {
	if relative == "" {
		return fmt.Errorf("managed content path is empty; the blob cannot be located; provide the owned-generation relative path")
	}
	if path.IsAbs(relative) || strings.HasPrefix(relative, "/") || strings.Contains(relative, "\\") {
		return fmt.Errorf("managed content path %q is absolute or uses a non-portable separator; it could escape the owned generation directory; use a relative forward-slash path", relative)
	}
	cleaned := path.Clean(relative)
	if cleaned != relative {
		return fmt.Errorf("managed content path %q is not canonical (%q); different spellings could address different files; store the cleaned relative path", relative, cleaned)
	}
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("managed content path %q escapes the owned generation directory; no committed content is verifiable; use a path inside the generation directory", relative)
	}
	return nil
}

// NativeAlias is one opaque native-identity key mapped to a source entry ref.
// The key is bounded and encoded; it never carries a raw private path.
type NativeAlias struct {
	NativeKey string
	Ref       schema.SourceEntryRef
}

// Validate checks one alias key's bound and its target ref.
func (a NativeAlias) Validate() error {
	if a.NativeKey == "" {
		return fmt.Errorf("indexformat.NativeAlias.Validate: nativeKey is empty; a native identity cannot be matched on reuse; encode a non-empty opaque key")
	}
	if !utf8.ValidString(a.NativeKey) {
		return fmt.Errorf("indexformat.NativeAlias.Validate: nativeKey is not valid UTF-8; a reader cannot match the native identity; encode a valid opaque key")
	}
	if len(a.NativeKey) > MaxNativeAliasKeyBytes {
		return fmt.Errorf("indexformat.NativeAlias.Validate: nativeKey is %d bytes, over the %d-byte bound; a raw native path may be leaking into persisted identity; encode an opaque key within the bound", len(a.NativeKey), MaxNativeAliasKeyBytes)
	}
	if err := a.Ref.Validate(); err != nil {
		return fmt.Errorf("indexformat.NativeAlias.Validate key %q: ref is invalid; the alias cannot be resolved; emit a bounded opaque ref: %w", a.NativeKey, err)
	}
	return nil
}

// SegmentCoordinates carries native coordinates for one context segment.
// Start/EndExclusive are native coordinates, never UI indices. Snapshot-only
// and unknown segments carry nil numeric bounds.
type SegmentCoordinates struct {
	Kind                    CoordinateKind
	Start                   *int64
	EndExclusive            *int64
	DecodedByteStart        *int64
	DecodedByteEndExclusive *int64
}

// Validate checks the coordinate kind and its bounds.
func (c SegmentCoordinates) Validate() error {
	if !c.Kind.IsValid() {
		return fmt.Errorf("indexformat.SegmentCoordinates.Validate: kind %q is outside the closed coordinate-kind set; a reader cannot interpret the captured range; use a published coordinate kind", c.Kind)
	}
	switch c.Kind {
	case CoordinateKindCodexOrdinalRange, CoordinateKindCodexReferenceRange, CoordinateKindOpenCodeSequenceRange:
		if c.Start == nil || c.EndExclusive == nil {
			return fmt.Errorf("indexformat.SegmentCoordinates.Validate kind %q: start and endExclusive are required native coordinates; the captured range has no boundary; provide both or use snapshot_only", c.Kind)
		}
		if *c.Start < 0 {
			return fmt.Errorf("indexformat.SegmentCoordinates.Validate kind %q: start %d is negative; the captured range cannot be ordered; record the native coordinate", c.Kind, *c.Start)
		}
		if *c.EndExclusive <= *c.Start {
			return fmt.Errorf("indexformat.SegmentCoordinates.Validate kind %q: endExclusive %d is not after start %d; the range is empty or inverted; record the checked exclusive upper bound", c.Kind, *c.EndExclusive, *c.Start)
		}
	case CoordinateKindSnapshotOnly, CoordinateKindUnknown:
		if c.Start != nil || c.EndExclusive != nil {
			return fmt.Errorf("indexformat.SegmentCoordinates.Validate kind %q: snapshot_only and unknown segments carry nil numeric bounds; a native range cannot be invented; clear start and endExclusive", c.Kind)
		}
	}
	if (c.DecodedByteStart == nil) != (c.DecodedByteEndExclusive == nil) {
		return fmt.Errorf("indexformat.SegmentCoordinates.Validate kind %q: decoded byte bounds must be both present or both absent; a half-open byte range cannot be checked; provide both or neither", c.Kind)
	}
	if c.DecodedByteStart != nil {
		if *c.DecodedByteStart < 0 {
			return fmt.Errorf("indexformat.SegmentCoordinates.Validate kind %q: decodedByteStart %d is negative; the byte range cannot be ordered; record the nonnegative offset", c.Kind, *c.DecodedByteStart)
		}
		if *c.DecodedByteEndExclusive <= *c.DecodedByteStart {
			return fmt.Errorf("indexformat.SegmentCoordinates.Validate kind %q: decodedByteEndExclusive %d is not after decodedByteStart %d; the byte range is empty or inverted; record the checked exclusive upper bound", c.Kind, *c.DecodedByteEndExclusive, *c.DecodedByteStart)
		}
	}
	return nil
}

// ContextSegment is one ordered captured context segment. A generation holds
// an ordered slice of these; a single boundary scalar is not enough because a
// context relation can have more than one segment of construction.
type ContextSegment struct {
	Ordinal          int
	LogicalSessionID *schema.SessionID
	PhysicalSourceID string
	Coordinates      SegmentCoordinates
	Inclusion        SegmentInclusion
	CapturedRefs     []schema.SourceEntryRef
}

// Validate checks one segment's ordinal, inclusion, coordinates and captured
// refs.
func (s ContextSegment) Validate() error {
	if s.Ordinal < 0 {
		return fmt.Errorf("indexformat.ContextSegment.Validate: ordinal %d is negative; segment order cannot be reconstructed; record the nonnegative ordinal", s.Ordinal)
	}
	if !s.Inclusion.IsValid() {
		return fmt.Errorf("indexformat.ContextSegment.Validate ordinal %d: inclusion %q is outside the closed set; a reader cannot classify the captured history; use a published inclusion", s.Ordinal, s.Inclusion)
	}
	if s.LogicalSessionID != nil {
		if _, err := schema.NewSessionID(string(*s.LogicalSessionID)); err != nil {
			return fmt.Errorf("indexformat.ContextSegment.Validate ordinal %d: logicalSessionId is malformed; the segment cannot be tied to a session; provide a canonical session identifier: %w", s.Ordinal, err)
		}
	}
	if !utf8.ValidString(s.PhysicalSourceID) {
		return fmt.Errorf("indexformat.ContextSegment.Validate ordinal %d: physicalSourceId is not valid UTF-8; the native source identity cannot be preserved; record a valid opaque identifier", s.Ordinal)
	}
	if err := s.Coordinates.Validate(); err != nil {
		return fmt.Errorf("indexformat.ContextSegment.Validate ordinal %d: %w", s.Ordinal, err)
	}
	seen := make(map[schema.SourceEntryRef]struct{}, len(s.CapturedRefs))
	for i := range s.CapturedRefs {
		ref := s.CapturedRefs[i]
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("indexformat.ContextSegment.Validate ordinal %d capturedRefs[%d]: ref is invalid; the captured segment cannot be reconstructed; emit a bounded opaque ref: %w", s.Ordinal, i, err)
		}
		if _, duplicate := seen[ref]; duplicate {
			return fmt.Errorf("indexformat.ContextSegment.Validate ordinal %d: captured ref %q repeats in one segment; a reader could double-count the block; keep each captured ref once", s.Ordinal, ref)
		}
		seen[ref] = struct{}{}
	}
	return nil
}

// Generation is one immutable managed projection. Metadata.Stats is the ONLY
// count authority: there is no separate submission count and no fourth version
// axis. Segments are ordered, and every content/alias/title ref is checked
// against the generation's own partitions so the generation is self-contained.
type Generation struct {
	ID                   string
	Completeness         GenerationCompleteness
	Metadata             schema.UnifiedMetadata
	Main                 Partition
	Earlier              []EarlierPartition
	Segments             []ContextSegment
	Content              []ContentRecord
	Aliases              []NativeAlias
	SourceEvidenceDigest string
	TitleRefs            []schema.SourceEntryRef
}

// Validate checks the whole generation: identity, completeness, metadata
// counts, partitions and native attachments, ordered segments, content and
// alias bounds, and title/content ref consistency.
func (g Generation) Validate() error {
	if strings.TrimSpace(g.ID) == "" {
		return fmt.Errorf("indexformat.Generation.Validate: id is empty; a generation cannot be addressed or activated; assign the installed generation ID")
	}
	if !g.Completeness.IsValid() {
		return fmt.Errorf("indexformat.Generation.Validate id %q: completeness %q is outside the closed set; a reader cannot tell whether the generation is complete; use complete or incomplete_new", g.ID, g.Completeness)
	}
	if strings.TrimSpace(g.SourceEvidenceDigest) == "" {
		return fmt.Errorf("indexformat.Generation.Validate id %q: sourceEvidenceDigest is empty; the captured source evidence cannot be verified; record the evidence digest", g.ID)
	}
	if err := validateUnifiedMetadata(g.Metadata, "generation.metadata"); err != nil {
		return err
	}
	ownerSession := g.Metadata.SessionID
	ownerHarness := g.Metadata.ModelHarness
	if err := g.Main.Validate("main", ownerSession, ownerHarness); err != nil {
		return err
	}
	for i := range g.Earlier {
		if err := g.Earlier[i].Validate(i, ownerSession, ownerHarness); err != nil {
			return err
		}
	}
	if err := validateGlobalEntryRefUniqueness(g.Main, g.Earlier, g.ID); err != nil {
		return err
	}
	if err := validateGlobalNativeMetadataIDUniqueness(g.Main, g.Earlier, g.ID); err != nil {
		return err
	}
	if err := validateSegments(g.Segments); err != nil {
		return err
	}

	entryRefs := g.Main.entryRefs()
	for i := range g.Earlier {
		for ref := range g.Earlier[i].Content.entryRefs() {
			entryRefs[ref] = struct{}{}
		}
	}
	retainedRefs := segmentCapturedRefs(g.Segments)

	contentRefs := make(map[schema.SourceEntryRef]struct{}, len(g.Content))
	for i := range g.Content {
		record := g.Content[i]
		if err := record.Validate(); err != nil {
			return fmt.Errorf("indexformat.Generation.Validate id %q content[%d]: %w", g.ID, i, err)
		}
		if _, duplicate := contentRefs[record.Ref]; duplicate {
			return fmt.Errorf("indexformat.Generation.Validate id %q: content ref %q repeats; a reader could apply the wrong blob; keep one content record per ref", g.ID, record.Ref)
		}
		contentRefs[record.Ref] = struct{}{}
	}
	for ref := range contentRefs {
		if _, ok := entryRefs[ref]; !ok {
			if _, retained := retainedRefs[ref]; !retained {
				return fmt.Errorf("indexformat.Generation.Validate id %q: content ref %q appears in neither an emitted partition nor a captured context segment; the generation is not self-contained; attach content only to refs this generation emits or retains as inherited evidence", g.ID, ref)
			}
		}
	}

	aliasKeys := make(map[string]struct{}, len(g.Aliases))
	for i := range g.Aliases {
		alias := g.Aliases[i]
		if err := alias.Validate(); err != nil {
			return fmt.Errorf("indexformat.Generation.Validate id %q aliases[%d]: %w", g.ID, i, err)
		}
		if _, duplicate := aliasKeys[alias.NativeKey]; duplicate {
			return fmt.Errorf("indexformat.Generation.Validate id %q: native alias key %q repeats; a later mapping would silently replace an earlier one; keep one alias per opaque key", g.ID, alias.NativeKey)
		}
		aliasKeys[alias.NativeKey] = struct{}{}
	}
	for _, alias := range g.Aliases {
		if _, ok := entryRefs[alias.Ref]; !ok {
			if _, retained := retainedRefs[alias.Ref]; !retained {
				return fmt.Errorf("indexformat.Generation.Validate id %q: native alias key %q targets ref %q outside the emitted partitions and captured segments; the alias cannot be resolved after reopen; map aliases only to retained generation refs", g.ID, alias.NativeKey, alias.Ref)
			}
		}
	}

	for i := range g.TitleRefs {
		ref := g.TitleRefs[i]
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("indexformat.Generation.Validate id %q titleRefs[%d]: ref is invalid; a title cannot be attributed; emit a bounded opaque ref: %w", g.ID, i, err)
		}
		if _, ok := g.Main.entryRefs()[ref]; !ok {
			return fmt.Errorf("indexformat.Generation.Validate id %q: title ref %q is not a main entry; a title must be seeded by a main owned block; move the ref or drop it", g.ID, ref)
		}
	}
	return validateCompletenessCountPresence(g.Completeness, g.Metadata.Stats.InputSubmissionCount, g.ID)
}

// validateGlobalEntryRefUniqueness requires every non-empty source entry ref
// to have exactly one owning partition across main and all earlier sections.
// A ref emitted twice would give two blocks one identity after reopen.
func validateGlobalEntryRefUniqueness(main Partition, earlier []EarlierPartition, generationID string) error {
	owners := make(map[schema.SourceEntryRef]string, len(main.Entries))
	for i := range main.Entries {
		ref := main.Entries[i].SourceEntryRef
		if ref == "" {
			continue
		}
		if owner, duplicate := owners[ref]; duplicate {
			return fmt.Errorf("indexformat.Generation.Validate id %q: source entry ref %q is owned by both %s and main entries[%d]; one block cannot live in two partitions; keep one owner per ref across the whole generation", generationID, ref, owner, i)
		}
		owners[ref] = fmt.Sprintf("main entries[%d]", i)
	}
	for section := range earlier {
		entries := earlier[section].Content.Entries
		for i := range entries {
			ref := entries[i].SourceEntryRef
			if ref == "" {
				continue
			}
			if owner, duplicate := owners[ref]; duplicate {
				return fmt.Errorf("indexformat.Generation.Validate id %q: source entry ref %q is owned by both %s and earlier[%d] entries[%d]; one block cannot live in two partitions; keep one owner per ref across the whole generation", generationID, ref, owner, section, i)
			}
			owners[ref] = fmt.Sprintf("earlier[%d] entries[%d]", section, i)
		}
	}
	return nil
}

// validateGlobalNativeMetadataIDUniqueness requires every native metadata id
// to be unique across the whole generation so an attachment cannot alias two
// blocks in different partitions.
func validateGlobalNativeMetadataIDUniqueness(main Partition, earlier []EarlierPartition, generationID string) error {
	owners := make(map[string]string)
	for i := range main.NativeMetadata {
		id := main.NativeMetadata[i].ID
		if owner, duplicate := owners[id]; duplicate {
			return fmt.Errorf("indexformat.Generation.Validate id %q: native metadata id %q is attached by both %s and main nativeMetadata[%d]; attachment is ambiguous across partitions; keep unique ids across the whole generation", generationID, id, owner, i)
		}
		owners[id] = fmt.Sprintf("main nativeMetadata[%d]", i)
	}
	for section := range earlier {
		records := earlier[section].Content.NativeMetadata
		for i := range records {
			id := records[i].ID
			if owner, duplicate := owners[id]; duplicate {
				return fmt.Errorf("indexformat.Generation.Validate id %q: native metadata id %q is attached by both %s and earlier[%d] nativeMetadata[%d]; attachment is ambiguous across partitions; keep unique ids across the whole generation", generationID, id, owner, section, i)
			}
			owners[id] = fmt.Sprintf("earlier[%d] nativeMetadata[%d]", section, i)
		}
	}
	return nil
}

// segmentCapturedRefs returns every non-empty ref a validated segment
// captures. Inherited context is retained locally under these refs without
// being emitted as main or earlier entries.
func segmentCapturedRefs(segments []ContextSegment) map[schema.SourceEntryRef]struct{} {
	retained := make(map[schema.SourceEntryRef]struct{})
	for i := range segments {
		for _, ref := range segments[i].CapturedRefs {
			if ref != "" {
				retained[ref] = struct{}{}
			}
		}
	}
	return retained
}

// validateCompletenessCountPresence enforces the count-presence contract: a
// complete generation has measured its input submissions, even when the
// measure is zero, while an incomplete first-discovery generation omits the
// scalar until a complete inspection exists. Absent stays unknown; present
// zero stays measured none.
func validateCompletenessCountPresence(completeness GenerationCompleteness, count *int64, generationID string) error {
	switch completeness {
	case GenerationCompletenessComplete:
		if count == nil {
			return fmt.Errorf("indexformat.Generation.Validate id %q: completeness is complete but stats.inputSubmissionCount is absent; a complete generation has measured its submissions; record the measured count, including zero", generationID)
		}
	case GenerationCompletenessIncompleteNew:
		if count != nil {
			return fmt.Errorf("indexformat.Generation.Validate id %q: completeness is incomplete_new but stats.inputSubmissionCount is present; a first-discovery generation without complete inspection omits the scalar; clear it until a complete capture exists", generationID)
		}
	}
	return nil
}

// validatePartitionNativeMetadata requires every native metadata record to
// attach to an entry in the same partition and to be individually bounded.
// The schema owns the full kind/source/attachment matrix against cooked
// TurnDetail; a partition holds raw SessionEntry rows, so this check only
// enforces the partition-attachment invariant the read path depends on.
func validatePartitionNativeMetadata(records []schema.NativeMetadataRecord, entryRefs map[schema.SourceEntryRef]struct{}, partition string) error {
	ids := make(map[string]struct{}, len(records))
	sources := make(map[schema.SourceEntryRef]struct{}, len(records))
	for i := range records {
		record := records[i]
		if err := record.Source.EntryRef.Validate(); err != nil {
			return fmt.Errorf("indexformat.Partition.Validate %s nativeMetadata[%d]: source entry ref is invalid; attached evidence cannot be attributed; emit a bounded opaque ref: %w", partition, i, err)
		}
		if _, ok := entryRefs[record.Source.EntryRef]; !ok {
			return fmt.Errorf("indexformat.Partition.Validate %s nativeMetadata[%d]: source entry ref %q is not an entry in this partition; main metadata must attach to main and earlier metadata to its named partition; move the record to the owning partition", partition, i, record.Source.EntryRef)
		}
		if !record.Kind.IsValid() {
			return fmt.Errorf("indexformat.Partition.Validate %s nativeMetadata[%d]: kind %q is outside the closed set; attached evidence cannot be classified; use a published kind", partition, i, record.Kind)
		}
		if !record.Source.SourceType.IsValid() {
			return fmt.Errorf("indexformat.Partition.Validate %s nativeMetadata[%d]: source type %q is outside the closed set; attached evidence cannot be classified; use a published source type", partition, i, record.Source.SourceType)
		}
		if record.ID == "" || !utf8.ValidString(record.ID) || len(record.ID) > MaxNativeMetadataIDBytes {
			return fmt.Errorf("indexformat.Partition.Validate %s nativeMetadata[%d]: id is empty, invalid UTF-8, or over %d bytes; attached evidence has no bounded identity; emit a bounded opaque id", partition, i, MaxNativeMetadataIDBytes)
		}
		if len(record.Data) == 0 || isJSONNull(record.Data) {
			return fmt.Errorf("indexformat.Partition.Validate %s nativeMetadata[%d]: data is missing or null; attached evidence cannot be checked; provide a non-null JSON value", partition, i)
		}
		if record.CustomType != "" && (!utf8.ValidString(record.CustomType) || len(record.CustomType) > MaxNativeMetadataCustomTypeBytes) {
			return fmt.Errorf("indexformat.Partition.Validate %s nativeMetadata[%d]: customType is invalid UTF-8 or over %d bytes; consumers cannot identify the extension; emit a shorter valid value", partition, i, MaxNativeMetadataCustomTypeBytes)
		}
		if _, duplicate := ids[record.ID]; duplicate {
			return fmt.Errorf("indexformat.Partition.Validate %s: native metadata id %q repeats; attachment is ambiguous; keep unique ids", partition, record.ID)
		}
		if _, duplicate := sources[record.Source.EntryRef]; duplicate {
			return fmt.Errorf("indexformat.Partition.Validate %s: native metadata source ref %q repeats; attachment is ambiguous; attach one record per source ref", partition, record.Source.EntryRef)
		}
		ids[record.ID] = struct{}{}
		sources[record.Source.EntryRef] = struct{}{}
	}
	return nil
}

func isJSONNull(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}

// validateSegments requires the generation's segment slice to be ordered by a
// strictly ascending ordinal so the order is unambiguous.
func validateSegments(segments []ContextSegment) error {
	previous := -1
	for i := range segments {
		segment := segments[i]
		if err := segment.Validate(); err != nil {
			return fmt.Errorf("indexformat.Generation.Validate segments[%d]: %w", i, err)
		}
		if segment.Ordinal <= previous {
			return fmt.Errorf("indexformat.Generation.Validate segments[%d]: ordinal %d does not increase past %d; segments are not ordered and unique; sort them by strictly ascending ordinal", i, segment.Ordinal, previous)
		}
		previous = segment.Ordinal
	}
	return nil
}

// validateUnifiedMetadata checks the durable metadata that carries the single
// count authority. It never invents a second count.
func validateUnifiedMetadata(metadata schema.UnifiedMetadata, path string) error {
	if metadata.SchemaVersion <= 0 {
		return fmt.Errorf("indexformat.%s: schemaVersion %d is not positive; the durable metadata cannot be interpreted; record the source schema version", path, metadata.SchemaVersion)
	}
	if _, err := schema.NewSessionID(string(metadata.SessionID)); err != nil {
		return fmt.Errorf("indexformat.%s: sessionId is malformed; the generation has no durable identity; provide a canonical session identifier: %w", path, err)
	}
	if !isKnownHarness(metadata.ModelHarness) {
		return fmt.Errorf("indexformat.%s: modelHarness %q is not a known harness; the generation cannot be attributed; use a known harness", path, metadata.ModelHarness)
	}
	if err := schema.ValidateInputSubmissionCount(metadata.Stats.InputSubmissionCount, path+".stats.inputSubmissionCount"); err != nil {
		return fmt.Errorf("indexformat.%s: %w", path, err)
	}
	if metadata.Stats.TurnCount < 0 {
		return fmt.Errorf("indexformat.%s: stats.turnCount %d is negative; the count cannot be trusted; record the emitted main record count", path, metadata.Stats.TurnCount)
	}
	if metadata.RootSessionID != nil {
		if _, err := schema.NewSessionID(string(*metadata.RootSessionID)); err != nil {
			return fmt.Errorf("indexformat.%s: rootSessionId is malformed; graph identity cannot be preserved; provide a canonical session identifier: %w", path, err)
		}
	}
	if !metadata.Purpose.IsValid() {
		return fmt.Errorf("indexformat.%s: purpose %q is outside the closed set; the session cannot be classified; use a published purpose or omit it", path, metadata.Purpose)
	}
	if err := schema.ValidateSessionRelationships(metadata.Relationships); err != nil {
		return fmt.Errorf("indexformat.%s: %w", path, err)
	}
	return nil
}

func isKnownHarness(harness schema.Harness) bool {
	for _, known := range schema.Harnesses() {
		if harness == known {
			return true
		}
	}
	return false
}

// V2 is the managed generation index result. Returning a concrete V2 is the
// only way a producer advertises format 2: the format target stays disabled
// until a matching writer and snapshot reader are both integrated.
type V2 struct {
	Generation Generation
}

// IndexVersion reports the concrete format version.
func (V2) IndexVersion() int { return 2 }

// Validate checks the wrapped generation.
func (v V2) Validate() error {
	return v.Generation.Validate()
}

var _ Result = V2{}

// LegacySource is the local harness/path pair used only by a legacy V1 read.
// It carries no filesystem behavior and never reaches the public wire.
type LegacySource struct {
	Harness schema.Harness
	Path    string
}

// Validate checks an optional legacy source. A zero value is not legacy.
func (l LegacySource) Validate() error {
	if l.Harness == "" && l.Path == "" {
		return nil
	}
	if !isKnownHarness(l.Harness) {
		return fmt.Errorf("indexformat.LegacySource.Validate: harness %q is not a known harness; a legacy read cannot choose a parser; use a known harness", l.Harness)
	}
	if !utf8.ValidString(l.Path) {
		return fmt.Errorf("indexformat.LegacySource.Validate: path is not valid UTF-8; the legacy source cannot be located; record a valid path")
	}
	return nil
}

// IsZero reports whether the value carries no legacy source.
func (l LegacySource) IsZero() bool { return l.Harness == "" && l.Path == "" }

// ReadSnapshot is the immutable in-memory read captured under one shared
// session lock. Session carries the flat durable metadata/stats with empty
// turns; Main/Earlier carry the generation's content partitions; GenerationID
// is empty only for a legacy V1 read.
type ReadSnapshot struct {
	Session      schema.SessionDetailPayload
	Metadata     schema.UnifiedMetadata
	TitleRefs    []schema.SourceEntryRef
	GenerationID string
	Completeness GenerationCompleteness
	IndexVersion int
	Main         Partition
	Earlier      []EarlierPartition
	Content      []ContentRecord
	LegacySource LegacySource
}

// Validate checks the snapshot's metadata/session equality and its generation
// state. A legacy read carries index version 1 and a legacy source; a
// generation read carries index version 2 and no mutable-source fallback.
func (s ReadSnapshot) Validate() error {
	if err := schema.ValidateSessionDetailPayload(s.Session); err != nil {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate session: %w", err)
	}
	if err := validateUnifiedMetadata(s.Metadata, "snapshot.metadata"); err != nil {
		return err
	}
	if err := validateSnapshotMirrors(s.Session, s.Metadata); err != nil {
		return err
	}
	if err := s.LegacySource.Validate(); err != nil {
		return err
	}
	switch s.IndexVersion {
	case 1:
		return s.validateLegacy()
	case 2:
		return s.validateGeneration()
	default:
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: indexVersion %d is not 1 or 2; a reader cannot choose a hydration path; record the captured format", s.IndexVersion)
	}
}

func (s ReadSnapshot) validateLegacy() error {
	if s.GenerationID != "" {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: generationId %q is set at index version 1; the same content would have two authorities; clear it for a legacy read", s.GenerationID)
	}
	if s.Completeness != "" {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: completeness %q is set at index version 1; a legacy read has no managed generation; clear it", s.Completeness)
	}
	if s.LegacySource.IsZero() {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: index version 1 carries no legacy source; the reader cannot locate the retained transcript; set the local harness and path")
	}
	if len(s.Main.Entries) > 0 || len(s.Main.NativeMetadata) > 0 || len(s.Earlier) > 0 || len(s.Content) > 0 {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: index version 1 carries generation partitions; the same content would have two authorities; clear main, earlier and content for a legacy read")
	}
	if len(s.TitleRefs) > 0 {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: index version 1 carries title refs; a legacy read has no managed generation; clear titleRefs")
	}
	return nil
}

func (s ReadSnapshot) validateGeneration() error {
	if s.GenerationID == "" {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: index version 2 has an empty generationId; the reader cannot identify the committed generation; record the active generation ID")
	}
	if !s.Completeness.IsValid() {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: index version 2 has completeness %q outside the closed set; the reader cannot warn about incompleteness; use complete or incomplete_new", s.Completeness)
	}
	if !s.LegacySource.IsZero() {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: index version 2 carries a legacy source; a generation read must never fall back to the mutable original; clear it")
	}
	ownerSession := s.Metadata.SessionID
	ownerHarness := s.Metadata.ModelHarness
	if err := s.Main.Validate("main", ownerSession, ownerHarness); err != nil {
		return err
	}
	for i := range s.Earlier {
		if err := s.Earlier[i].Validate(i, ownerSession, ownerHarness); err != nil {
			return err
		}
	}
	if err := validateSnapshotEntryRefUniqueness(s.Main, s.Earlier); err != nil {
		return err
	}
	entryRefs := s.Main.entryRefs()
	for i := range s.Earlier {
		for ref := range s.Earlier[i].Content.entryRefs() {
			entryRefs[ref] = struct{}{}
		}
	}
	contentRefs := make(map[schema.SourceEntryRef]struct{}, len(s.Content))
	for i := range s.Content {
		record := s.Content[i]
		if err := record.Validate(); err != nil {
			return fmt.Errorf("indexformat.ReadSnapshot.Validate content[%d]: %w", i, err)
		}
		if _, duplicate := contentRefs[record.Ref]; duplicate {
			return fmt.Errorf("indexformat.ReadSnapshot.Validate: content ref %q repeats; a reader could apply the wrong blob; keep one content record per ref", record.Ref)
		}
		contentRefs[record.Ref] = struct{}{}
	}
	for ref := range contentRefs {
		if _, ok := entryRefs[ref]; !ok {
			return fmt.Errorf("indexformat.ReadSnapshot.Validate: content ref %q does not appear in any partition; the snapshot is not self-contained; attach content only to refs this snapshot emits", ref)
		}
	}
	for i := range s.TitleRefs {
		ref := s.TitleRefs[i]
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("indexformat.ReadSnapshot.Validate titleRefs[%d]: ref is invalid; a title cannot be attributed; emit a bounded opaque ref: %w", i, err)
		}
		if _, ok := s.Main.entryRefs()[ref]; !ok {
			return fmt.Errorf("indexformat.ReadSnapshot.Validate: title ref %q is not a main entry; a title must be seeded by a main owned block; move the ref or drop it", ref)
		}
	}
	return validateSnapshotCompletenessCountPresence(s.Completeness, s.Metadata.Stats.InputSubmissionCount)
}

// validateSnapshotEntryRefUniqueness requires every non-empty source entry ref
// in a generation snapshot to have exactly one owning partition.
func validateSnapshotEntryRefUniqueness(main Partition, earlier []EarlierPartition) error {
	owners := make(map[schema.SourceEntryRef]string, len(main.Entries))
	for i := range main.Entries {
		ref := main.Entries[i].SourceEntryRef
		if ref == "" {
			continue
		}
		if owner, duplicate := owners[ref]; duplicate {
			return fmt.Errorf("indexformat.ReadSnapshot.Validate: source entry ref %q is owned by both %s and main entries[%d]; one block cannot live in two partitions; keep one owner per ref across the whole snapshot", ref, owner, i)
		}
		owners[ref] = fmt.Sprintf("main entries[%d]", i)
	}
	for section := range earlier {
		entries := earlier[section].Content.Entries
		for i := range entries {
			ref := entries[i].SourceEntryRef
			if ref == "" {
				continue
			}
			if owner, duplicate := owners[ref]; duplicate {
				return fmt.Errorf("indexformat.ReadSnapshot.Validate: source entry ref %q is owned by both %s and earlier[%d] entries[%d]; one block cannot live in two partitions; keep one owner per ref across the whole snapshot", ref, owner, section, i)
			}
			owners[ref] = fmt.Sprintf("earlier[%d] entries[%d]", section, i)
		}
	}
	return nil
}

// validateSnapshotCompletenessCountPresence enforces the same count-presence
// contract as a stored generation: complete measures, incomplete omits.
func validateSnapshotCompletenessCountPresence(completeness GenerationCompleteness, count *int64) error {
	switch completeness {
	case GenerationCompletenessComplete:
		if count == nil {
			return fmt.Errorf("indexformat.ReadSnapshot.Validate: completeness is complete but stats.inputSubmissionCount is absent; a complete snapshot has measured its submissions; record the measured count, including zero")
		}
	case GenerationCompletenessIncompleteNew:
		if count != nil {
			return fmt.Errorf("indexformat.ReadSnapshot.Validate: completeness is incomplete_new but stats.inputSubmissionCount is present; a first-discovery snapshot without complete inspection omits the scalar; clear it until a complete capture exists")
		}
	}
	return nil
}

// validateSnapshotMirrors requires the flat durable session and the durable
// metadata to describe the same committed snapshot.
func validateSnapshotMirrors(session schema.SessionDetailPayload, metadata schema.UnifiedMetadata) error {
	if session.ID != string(metadata.SessionID) {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: session.id %q disagrees with metadata.sessionId %q; the reader could hydrate the wrong session; reload one committed snapshot", session.ID, metadata.SessionID)
	}
	if session.Harness != metadata.ModelHarness {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: session.harness %q disagrees with metadata.modelHarness %q; the reader could use the wrong parser; reload one committed snapshot", session.Harness, metadata.ModelHarness)
	}
	if session.TurnCount != metadata.Stats.TurnCount {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: session.turnCount %d disagrees with metadata.stats.turnCount %d; two count authorities are present; derive both from Metadata.Stats", session.TurnCount, metadata.Stats.TurnCount)
	}
	if !equalOptionalInt64(session.InputSubmissionCount, metadata.Stats.InputSubmissionCount) {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: session.inputSubmissionCount and metadata.stats.inputSubmissionCount disagree, including absent versus measured zero; the unknown/measured distinction would be lost; derive both from Metadata.Stats")
	}
	if !equalOptionalSessionID(session.RootSessionID, metadata.RootSessionID) {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: session.rootSessionId and metadata.rootSessionId disagree; graph identity would differ between the session and its metadata; reload one committed snapshot")
	}
	if session.Purpose != metadata.Purpose {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: session.purpose %q disagrees with metadata.purpose %q; consumers could classify the session differently; derive both from the durable metadata", session.Purpose, metadata.Purpose)
	}
	if !reflect.DeepEqual(session.Relationships, metadata.Relationships) {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: session.relationships and metadata.relationships disagree; authorized navigation would differ from durable evidence; derive both from one committed snapshot")
	}
	return validateParentMirrors(session, metadata)
}

// validateParentMirrors keeps the two legacy parent projections aligned with
// each other and with the durable started_by relationship when one exists.
func validateParentMirrors(session schema.SessionDetailPayload, metadata schema.UnifiedMetadata) error {
	durable := durableStartedByTarget(session.Relationships)
	if session.ParentSessionID != nil {
		if durable != nil && *session.ParentSessionID != *durable {
			return fmt.Errorf("indexformat.ReadSnapshot.Validate: session.parentSessionId disagrees with the durable started_by target; navigation could point at the wrong parent; derive the legacy parent from the relationship")
		}
		if len(session.Relationships) == 0 && metadata.ParentUUID != nil && *metadata.ParentUUID != *session.ParentSessionID {
			return fmt.Errorf("indexformat.ReadSnapshot.Validate: session.parentSessionId and metadata.parentUuid disagree; the legacy parent has two values; reconcile them from one committed snapshot")
		}
	}
	if metadata.ParentUUID != nil && durable != nil && *metadata.ParentUUID != *durable {
		return fmt.Errorf("indexformat.ReadSnapshot.Validate: metadata.parentUuid disagrees with the durable started_by target; the legacy parent projection could navigate to the wrong session; derive it from the relationship")
	}
	return nil
}

func durableStartedByTarget(relationships []schema.SessionRelationship) *schema.SessionID {
	for i := range relationships {
		relationship := relationships[i]
		if relationship.Kind != schema.SessionRelationshipStartedBy {
			continue
		}
		if relationship.TargetState == schema.RelationshipTargetKnown || relationship.TargetState == schema.RelationshipTargetKnownRetained {
			return relationship.TargetLocalID
		}
	}
	return nil
}

func equalOptionalInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func equalOptionalSessionID(left, right *schema.SessionID) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// SnapshotReader loads ONE immutable read snapshot for a session and keeps the
// session's shared lock held for the entire callback, through hydration and
// serialization. Implementations acquire the shared OS lock before the SQLite
// read transaction and must not re-read the active pointer during hydration.
type SnapshotReader interface {
	WithSessionSnapshot(context.Context, schema.SessionID, func(ReadSnapshot) error) error
}

// ContentResolver reads one immutable managed content blob addressed by the
// captured snapshot identity. It must never reparse the native source for V2.
type ContentResolver interface {
	ReadFullContent(context.Context, schema.SessionID, string, ContentRecord) ([]byte, error)
}
