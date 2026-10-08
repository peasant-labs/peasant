package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// preparedHarmonized is one candidate's lock-free prepare output (design
// §4.1 P1–P4): the validated generation plus every digest the staging and
// commit phases need, computed once from the in-memory fields. The commit
// phase recomputes nothing from stored state except the skip comparison and
// the compare-and-swap stamp.
type preparedHarmonized struct {
	sessionID  schema.SessionID
	generation indexformat.Generation
	// bodies holds every main and earlier entry as its body row, in
	// (partition, index) order; partitions[i] names the body partition.
	bodies      []EntryRecord
	bodyDigests []BodyDigest
	partitions  []int
	// emitted holds every source ref an entry emits (main and earlier).
	emitted map[schema.SourceEntryRef]struct{}
	// blobs holds the non-emitted content only (P3): refs no emitted body
	// holds, with the exact bytes the blob store keeps. Emitted refs never
	// reach the blob store, even when their bytes match some entry field.
	blobs []preparedBlob
	// binding is the activation binding (P4): candidate_digest.
	binding string
	// record is the mapped generation catalog row without the
	// staging-derived stamps (installed/activated/candidate digest); children
	// holds its ordered 1:N rows.
	record   GenerationRecord
	children GenerationChildren
	// aliases compares as a map (native key to ref), not as an ordered
	// slice: insertion order carries no meaning.
	aliases map[string]schema.SourceEntryRef
	// mainEntries is the generation's main entries for the entries hash and
	// the capture certificate (the unchanged hash domain).
	mainEntries []schema.SessionEntry
	// captureHash is fullCaptureHash over the main entries: the generation's
	// content_hash and the capture certificate share one write-time domain.
	captureHash string
}

// preparedBlob is one non-emitted content object: the descriptor's ref with
// the exact bytes the header and chunks persist.
type preparedBlob struct {
	ref    schema.SourceEntryRef
	digest ContentDigest
	data   []byte
}

// prepareHarmonizedCandidate runs P1–P4 over one candidate without touching
// the database and without taking any lock, so callers may prepare
// candidates concurrently. P1 parses and validates (the generation plus the
// shared storage validator — hostile input stops here); P2 encodes every
// entry once and digests it; P3 classifies inline versus blob; P4 binds the
// candidate digest over the same inputs the commit and the migration use.
func prepareHarmonizedCandidate(sessionID schema.SessionID, generation indexformat.Generation, blobs map[schema.SourceEntryRef][]byte) (*preparedHarmonized, error) {
	emitted := emittedRefs(generation)
	if blobs != nil {
		for _, record := range generation.Content {
			if _, ok := emitted[record.Ref]; ok {
				continue
			}
			if _, ok := blobs[record.Ref]; !ok {
				return nil, fmt.Errorf("store: a content record of session %s names no emitted entry and carries no staged bytes; no generation was prepared; supply the ref bytes or drop the record", sessionID)
			}
		}
		generation.Content = fillContentRecords(generation.Content, blobs)
	}
	if err := generation.Validate(); err != nil {
		// The validator text is untrusted: a candidate title ref or another
		// field can carry a private path, so refuse with a fixed category
		// against the already-validated session identity and never wrap
		// the raw validator error.
		return nil, fmt.Errorf("store: refuse a managed generation for session %s: the candidate failed managed generation validation; no generation was activated and the prior generation is unchanged; re-index the source for a fresh candidate", sessionID)
	}
	if generation.Metadata.SessionID != sessionID {
		return nil, fmt.Errorf("store: managed generation for session %s names session %s; no generation was prepared; build the generation for its own captured metadata", sessionID, generation.Metadata.SessionID)
	}
	entries := allGenerationEntries(generation)
	if err := validateEntriesForStorage(sessionID, entries); err != nil {
		return nil, err
	}
	if err := validateEntriesUTF8(sessionID, entries); err != nil {
		return nil, err
	}
	prepared := &preparedHarmonized{
		sessionID:   sessionID,
		generation:  generation,
		emitted:     emitted,
		aliases:     map[string]schema.SourceEntryRef{},
		mainEntries: append([]schema.SessionEntry(nil), generation.Main.Entries...),
	}
	if err := prepared.encodeBodies(generation); err != nil {
		return nil, err
	}
	if err := prepared.classifyContent(generation, blobs); err != nil {
		return nil, err
	}
	record, children, err := mapGenerationRecord(sessionID, generation)
	if err != nil {
		return nil, err
	}
	prepared.record = record
	prepared.children = children
	for _, alias := range generation.Aliases {
		prepared.aliases[alias.NativeKey] = alias.Ref
	}
	captureHash, err := fullCaptureHash(prepared.mainEntries)
	if err != nil {
		return nil, fmt.Errorf("store: hash main entries for session %s: %w; no generation was prepared", sessionID, err)
	}
	prepared.captureHash = captureHash
	prepared.binding = computeActivationBinding(record, prepared.bodyDigests, prepared.blobDigests())
	prepared.record.CandidateDigest = prepared.binding
	return prepared, nil
}

// emittedRefs returns every source ref an entry emits (main and earlier).
func emittedRefs(generation indexformat.Generation) map[schema.SourceEntryRef]struct{} {
	emitted := map[schema.SourceEntryRef]struct{}{}
	for i := range generation.Main.Entries {
		if ref := generation.Main.Entries[i].SourceEntryRef; ref != "" {
			emitted[ref] = struct{}{}
		}
	}
	for i := range generation.Earlier {
		for j := range generation.Earlier[i].Content.Entries {
			if ref := generation.Earlier[i].Content.Entries[j].SourceEntryRef; ref != "" {
				emitted[ref] = struct{}{}
			}
		}
	}
	return emitted
}

// fillContentRecords completes content records from the staged bytes: a
// record missing its blob path, length, or digest adopts the deterministic
// values of the bytes it names, so producers that never addressed files
// still validate. Records that arrive complete keep their values, and the
// digest check in classification still refuses bytes that do not verify.
func fillContentRecords(records []indexformat.ContentRecord, blobs map[schema.SourceEntryRef][]byte) []indexformat.ContentRecord {
	filled := append([]indexformat.ContentRecord(nil), records...)
	for i := range filled {
		data, ok := blobs[filled[i].Ref]
		if !ok {
			continue
		}
		sum := sha256.Sum256(data)
		digest := hex.EncodeToString(sum[:])
		if filled[i].RelativeBlob == "" {
			filled[i].RelativeBlob = "c_" + digest + ".blob"
		}
		if filled[i].ByteLength == 0 {
			filled[i].ByteLength = int64(len(data))
		}
		if filled[i].Digest == "" {
			filled[i].Digest = digest
		}
	}
	return filled
}

// validateEntriesForStorage runs the ingest-owned shared storage validator:
// the harmonized prepare path calls it at P1 and again at the store
// boundary (S0).
func validateEntriesForStorage(sessionID schema.SessionID, entries []schema.SessionEntry) error {
	return ingest.ValidateEntriesForStorage(ingest.SessionID(sessionID), entries)
}

// validateEntriesUTF8 refuses entries with invalid UTF-8 in any text
// column before anything is staged: SQLite TEXT carrying invalid UTF-8
// would serialize to replacement bytes and break the digest binding.
func validateEntriesUTF8(sessionID schema.SessionID, entries []schema.SessionEntry) error {
	texts := func(entry *schema.SessionEntry) []*string {
		return []*string{
			entry.ContentPreview, entry.ToolInput, entry.ToolOutput,
			entry.ToolNamesCSV, entry.ToolCallID, entry.EntryID,
			entry.ParentEntryID, entry.PartType, entry.Extra,
		}
	}
	for i := range entries {
		for _, text := range texts(&entries[i]) {
			if text != nil && !utf8.ValidString(*text) {
				return fmt.Errorf("store: entry %d of session %s carries invalid UTF-8; no object was staged; repair source encoding and recapture", entries[i].EntryIndex, sessionID)
			}
		}
	}
	return nil
}

// allGenerationEntries returns the main entries followed by every earlier
// partition's entries, in partition order.
func allGenerationEntries(generation indexformat.Generation) []schema.SessionEntry {
	out := make([]schema.SessionEntry, 0, len(generation.Main.Entries))
	out = append(out, generation.Main.Entries...)
	for i := range generation.Earlier {
		out = append(out, generation.Earlier[i].Content.Entries...)
	}
	return out
}

// encodeBodies maps every entry to its body row and digests the canonical
// text once (P2). The digest is computed over the row the writer stores, so
// the integrity anchor can never describe bytes the store does not hold.
func (p *preparedHarmonized) encodeBodies(generation indexformat.Generation) error {
	encode := func(partition int, entries []schema.SessionEntry) error {
		for i := range entries {
			record, err := entryRecordFromEntry(entries[i])
			if err != nil {
				return err
			}
			p.bodies = append(p.bodies, record)
			p.bodyDigests = append(p.bodyDigests, BodyDigest(bodyDigestForRecord(record)))
			p.partitions = append(p.partitions, partition)
		}
		return nil
	}
	if err := encode(0, generation.Main.Entries); err != nil {
		return fmt.Errorf("store: encode main entries for session %s: %w; no generation was prepared", p.sessionID, err)
	}
	for i := range generation.Earlier {
		if err := encode(i+1, generation.Earlier[i].Content.Entries); err != nil {
			return fmt.Errorf("store: encode earlier[%d] entries for session %s: %w; no generation was prepared", i, p.sessionID, err)
		}
	}
	return nil
}

// classifyContent splits the generation's content records into inline and
// blob (P3): a ref an emitted entry holds stays inline in its body and
// never reaches the blob store; every other ref becomes a blob object from
// the supplied bytes. Object kinds never deduplicate against each other: a
// non-emitted ref whose bytes equal some emitted entry's field still gets
// its own blob row. A non-emitted ref without supplied bytes refuses the
// candidate before anything is staged — except inside the commit, where no
// bytes travel: there the record's validated digest addresses the staged
// blob, and the descriptor's foreign key proves it was staged, so an
// unstaged candidate refuses at the commit instead of writing partial rows.
func (p *preparedHarmonized) classifyContent(generation indexformat.Generation, blobs map[schema.SourceEntryRef][]byte) error {
	for i := range generation.Content {
		record := generation.Content[i]
		if _, ok := p.emitted[record.Ref]; ok {
			continue
		}
		if blobs == nil {
			if record.Digest == "" {
				return fmt.Errorf("store: a content record of session %s names no emitted entry and carries no digest; no generation was prepared; the commit stages nothing, so every non-emitted ref must arrive with its validated digest", p.sessionID)
			}
			p.blobs = append(p.blobs, preparedBlob{
				ref:    record.Ref,
				digest: ContentDigest(record.Digest),
			})
			continue
		}
		data, ok := blobs[record.Ref]
		if !ok {
			return fmt.Errorf("store: a content record of session %s names no emitted entry and carries no staged bytes; no generation was prepared; supply the ref bytes or drop the record", p.sessionID)
		}
		sum := sha256.Sum256(data)
		digest := hex.EncodeToString(sum[:])
		if !equalDigests(digest, record.Digest) {
			return fmt.Errorf("store: a content record of session %s carries an integrity digest its bytes do not verify; no generation was prepared; re-index the source so the record describes the bytes it names", p.sessionID)
		}
		p.blobs = append(p.blobs, preparedBlob{
			ref:    record.Ref,
			digest: ContentDigest(digest),
			data:   append([]byte(nil), data...),
		})
	}
	sort.Slice(p.blobs, func(i, j int) bool { return p.blobs[i].ref < p.blobs[j].ref })
	return nil
}

// equalDigests compares two hex digests case-insensitively: indexers emit
// lowercase, but the comparison must not mistake case for corruption.
func equalDigests(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := 0; i < len(left); i++ {
		a, b := left[i], right[i]
		if a >= 'A' && a <= 'F' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'F' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

// blobDigests returns the staged blob digests in ref order: the binding's
// blob operand.
func (p *preparedHarmonized) blobDigests() []BlobDigest {
	out := make([]BlobDigest, 0, len(p.blobs))
	for _, blob := range p.blobs {
		out = append(out, BlobDigest(blob.digest))
	}
	return out
}

// BlobDigest is hex sha256 over a staged blob's exact bytes: the descriptor
// operand the binding hashes. It aliases the store's content-digest shape.
type BlobDigest = ContentDigest

// computeActivationBinding binds one candidate over the same inputs for the
// write path and the migration: the generation's read-visible catalog
// values with the identity, the read-invisible provenance, the derived
// anchors, and the staging-derived fields removed, every body digest in
// (partition, index) order, and every non-emitted blob digest by ref. The
// generation identifier is excluded so identical content binds identically
// across refreshes (a refresh carries a fresh identifier over unchanged
// bytes); the identifier still scopes the immutable-identity check. The
// adapter revision is excluded for the same reason it is excluded from the
// skip comparison: no reader sees it, and the bookkeeping stamps it forward
// on a skip. The metadata hash is excluded as a derived anchor: it is a
// deterministic function of the compared metadata, so it adds no signal
// and would couple the binding to the anchor it anchors. Two candidates
// that differ anywhere readers see bind differently, and an installed
// generation whose digest matches the candidate is the identical complete
// candidate (AlreadyCommitted).
func computeActivationBinding(gen GenerationRecord, bodies []BodyDigest, blobs []BlobDigest) string {
	normalized := gen
	normalized.GenerationID = ""
	normalized.AdapterVersion = nil
	normalized.MetadataHash = ""
	normalized.InstalledAtMs = 0
	normalized.ActivatedAtMs = nil
	normalized.CandidateDigest = ""
	payload, err := json.Marshal(struct {
		Generation GenerationRecord `json:"generation"`
		Bodies     []BodyDigest     `json:"bodies"`
		Blobs      []BlobDigest     `json:"blobs"`
	}{Generation: normalized, Bodies: bodies, Blobs: blobs})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// mapGenerationRecord flattens one generation into its catalog row (without
// the staging-derived stamps) and its ordered 1:N children (design §3.3
// tables 3 and 3b plus the reshaped table-6 rows). The measured stats stay
// out: they are mutable and live in the captured-stats row, and the
// metadata hash below is the stats-excluded capture anchor.
func mapGenerationRecord(sessionID schema.SessionID, generation indexformat.Generation) (GenerationRecord, GenerationChildren, error) {
	metadata := generation.Metadata
	record := GenerationRecord{
		SessionID:            sessionID,
		GenerationID:         generation.ID,
		SchemaVersion:        metadata.SchemaVersion,
		Harness:              metadata.ModelHarness,
		Model:                metadata.Model,
		Version:              metadata.Version,
		TimestampStartMs:     metadata.Timestamp.Start,
		TimestampEndMs:       metadata.Timestamp.End,
		TimestampIngestedMs:  metadata.Timestamp.Ingested,
		SourceFormat:         metadata.Source.Format,
		ProjectHash:          metadata.Project.Hash,
		ProjectName:          metadata.Project.Name,
		HostSlug:             metadata.HostSlug,
		ContentHash:          metadata.ContentHash,
		MetadataHash:         statsExcludedMetadataHash(metadata),
		RedactionApplied:     metadata.Redaction.Applied,
		Completeness:         generation.Completeness,
		SourceEvidenceDigest: generation.SourceEvidenceDigest,
		IndexFormatVersion:   2,
	}
	if metadata.Source.FilePath != "" {
		path := metadata.Source.FilePath
		record.SourceFilePath = &path
	}
	if metadata.Git.Branch != nil {
		record.GitBranch = metadata.Git.Branch
	}
	if metadata.Git.Remote != nil {
		record.GitRemote = metadata.Git.Remote
	}
	if metadata.Git.Worktree != nil {
		record.GitWorktree = metadata.Git.Worktree
	}
	if metadata.Git.Tracking != nil {
		record.GitTracking = metadata.Git.Tracking
	}
	if metadata.Project.FilePath != "" {
		path := metadata.Project.FilePath
		record.ProjectFilePath = &path
	}
	if metadata.RootSessionID != nil {
		record.RootSessionID = metadata.RootSessionID
	}
	if metadata.Purpose != "" {
		purpose := metadata.Purpose
		record.Purpose = &purpose
	}
	if metadata.CWD != "" {
		cwd := metadata.CWD
		record.CWD = &cwd
	}
	record.DerivedAtMs = metadata.DerivedAt
	if metadata.Redaction.Level != "" {
		level := metadata.Redaction.Level
		record.RedactionLevel = &level
	}
	if metadata.Redaction.RuleSetVersion != "" {
		version := metadata.Redaction.RuleSetVersion
		record.RedactionRuleSetVersion = &version
	}
	record.RedactionAtMs = metadata.Redaction.RedactedAtMs
	if metadata.Redaction.ContentHashAtRedact != "" {
		hash := metadata.Redaction.ContentHashAtRedact
		record.RedactionContentHashAtRedact = &hash
	}
	record.AdapterVersion = metadata.AdapterVersion
	record.DiagnosticsPartial = metadata.Diagnostics.Partial
	var children GenerationChildren
	for i := range metadata.Subagents {
		children.Subagents = append(children.Subagents, GenerationSubagent{
			Ordinal:           i,
			SubagentSessionID: metadata.Subagents[i].SessionID,
			ParentUUID:        metadata.Subagents[i].ParentUUID,
		})
	}
	for i := range metadata.Git.Commits {
		commit := metadata.Git.Commits[i]
		children.Commits = append(children.Commits, GenerationCommit{
			Ordinal: i, Hash: commit.Hash, Message: commit.Message,
			AuthorName: commit.AuthorName, AuthorEmail: commit.AuthorEmail,
			CommitTime: commit.CommitTime, AuthorTime: commit.AuthorTime,
		})
	}
	for i := range metadata.Git.Associations {
		association := metadata.Git.Associations[i]
		children.Associations = append(children.Associations, GenerationAssociation{
			Ordinal: i, AssociationID: string(association.ID),
			ObservedCommitHash: association.ObservedCommitHash,
		})
	}
	for i := range metadata.Diagnostics.Warnings {
		warning := metadata.Diagnostics.Warnings[i]
		children.Diagnostics = append(children.Diagnostics, GenerationDiagnostic{
			Ordinal: i, ErrorType: warning.ErrorType, Location: warning.Location,
			Message: warning.Message, Remediation: warning.Remediation,
		})
	}
	children.TitleRefs = append(children.TitleRefs, generation.TitleRefs...)
	for _, relationship := range metadata.Relationships {
		mapped := GenerationRelationship{
			Kind:        relationship.Kind,
			TargetState: relationship.TargetState,
			Evidence:    nullableEvidence(relationship.Evidence),
		}
		if relationship.TargetLocalID != nil {
			mapped.TargetLocalID = relationship.TargetLocalID
		}
		if relationship.Anchor != nil {
			kind := relationship.Anchor.Kind
			mapped.AnchorKind = &kind
			mapped.AnchorSourceEntryRef = &relationship.Anchor.SourceEntryRef
			if relationship.Anchor.SourceRevisionRef != "" {
				revision := string(relationship.Anchor.SourceRevisionRef)
				mapped.AnchorSourceRevisionRef = &revision
			}
		}
		children.Relationships = append(children.Relationships, mapped)
	}
	for _, segment := range generation.Segments {
		mapped := GenerationSegment{
			Ordinal:                 segment.Ordinal,
			LogicalSessionID:        segment.LogicalSessionID,
			PhysicalSourceID:        segment.PhysicalSourceID,
			CoordinateKind:          segment.Coordinates.Kind,
			StartCoordinate:         segment.Coordinates.Start,
			EndExclusive:            segment.Coordinates.EndExclusive,
			DecodedByteStart:        segment.Coordinates.DecodedByteStart,
			DecodedByteEndExclusive: segment.Coordinates.DecodedByteEndExclusive,
			Inclusion:               segment.Inclusion,
		}
		children.Segments = append(children.Segments, mapped)
		for i, ref := range segment.CapturedRefs {
			children.SegmentRefs = append(children.SegmentRefs, GenerationSegmentRef{
				SegmentOrdinal: segment.Ordinal, Ordinal: i, SourceEntryRef: ref,
			})
		}
	}
	children.Sections = append(children.Sections, GenerationSection{PartitionID: 0})
	for i := range generation.Main.NativeMetadata {
		mapped, err := mapNativeMetadata(0, i, generation.Main.NativeMetadata[i])
		if err != nil {
			return GenerationRecord{}, GenerationChildren{}, err
		}
		children.NativeMetadata = append(children.NativeMetadata, mapped)
	}
	for i := range generation.Earlier {
		section := generation.Earlier[i]
		mappedSection := GenerationSection{PartitionID: i + 1}
		if section.State != "" {
			state := section.State
			mappedSection.EarlierState = &state
		}
		children.Sections = append(children.Sections, mappedSection)
		for j := range section.Content.NativeMetadata {
			mapped, err := mapNativeMetadata(i+1, j, section.Content.NativeMetadata[j])
			if err != nil {
				return GenerationRecord{}, GenerationChildren{}, err
			}
			children.NativeMetadata = append(children.NativeMetadata, mapped)
		}
	}
	return record, children, nil
}

// nullableEvidence maps the relationship evidence kind: the empty kind reads
// as NULL, every other value as its string.
func nullableEvidence(evidence schema.EvidenceKind) *string {
	if evidence == "" {
		return nil
	}
	out := string(evidence)
	return &out
}

// mapNativeMetadata maps one native metadata record to its ordered child row
// (partition.NativeMetadata, ordered). The data payload stays opaque: the
// native usage-metadata document, schema-less by the harness's own design,
// bound as text to the STRICT TEXT column. Absent optionals read as NULL.
func mapNativeMetadata(partition, ordinal int, record schema.NativeMetadataRecord) (GenerationNativeMetadata, error) {
	mapped := GenerationNativeMetadata{
		PartitionID:    partition,
		Ordinal:        ordinal,
		NativeID:       record.ID,
		Kind:           record.Kind,
		SourceEntryRef: record.Source.EntryRef,
		SourceType:     record.Source.SourceType,
		Data:           string(record.Data),
	}
	if record.Source.MessageRole != "" {
		role := record.Source.MessageRole
		mapped.SourceMessageRole = &role
	}
	if record.Attachment != nil {
		mapped.AttachmentTurnIndex = record.Attachment.TurnIndex
		if record.Attachment.ToolCallID != "" {
			toolCallID := record.Attachment.ToolCallID
			mapped.AttachmentToolCallID = &toolCallID
		}
	}
	if record.CustomType != "" {
		customType := record.CustomType
		mapped.CustomType = &customType
	}
	return mapped, nil
}

// statsExcludedMetadataHash is the stats-excluded capture anchor (design
// §3.3): the schema's ComputeMetadataHash over the captured document with
// the mutable stats subtree zeroed. It stays verifiable after any stats
// update, and a stats-only resume can never force a new generation because
// P5 never compares it and C2 never re-derives it from the stats row.
func statsExcludedMetadataHash(metadata schema.UnifiedMetadata) string {
	withoutStats := metadata
	withoutStats.Stats = schema.SessionStats{}
	return schema.ComputeMetadataHash(&withoutStats)
}
