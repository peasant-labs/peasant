package ingest

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// CodexGraphMapping is the canonical logical graph derived from current
// native evidence: the purpose, the started_by and context_from relations,
// the root grouping, and the legacy parent cache. Spawn parentage and
// context derivation stay independent relations even when they name
// different sessions, and a physical rollout identity never becomes a
// logical edge.
type CodexGraphMapping struct {
	Purpose       schema.SessionPurpose
	Relationships []schema.SessionRelationship
	RootSessionID *schema.SessionID
	// ParentUUID is the legacy compatibility cache of the current known
	// started_by target only. It is nil for retained, conflicting,
	// explicit-none, and unknown states: null never proves a root and never
	// hides a known logical relation, which lives in Relationships.
	ParentUUID *SessionID
}

// MapCodexGraph maps thread evidence and optional prior metadata to the
// canonical logical graph. The current native parent supersedes an older
// projection; two disagreeing current native parents clear the target with
// a conflict state instead of guessing; context_from is derived from the
// fork source independently and never substituted with started_by.
func MapCodexGraph(thread CodexThreadEvidence, prior *schema.UnifiedMetadata) (CodexGraphMapping, error) {
	mapping := CodexGraphMapping{Purpose: thread.Purpose()}
	if root, ok := validatedGraphSessionID(thread.RootSessionID); ok {
		identifier := schema.SessionID(root)
		mapping.RootSessionID = &identifier
	}
	startedBy, parent := mapStartedBy(thread, prior)
	if startedBy != nil {
		mapping.Relationships = append(mapping.Relationships, *startedBy)
		mapping.ParentUUID = parent
	}
	if contextFrom := mapContextFrom(thread, prior); contextFrom != nil {
		mapping.Relationships = append(mapping.Relationships, *contextFrom)
	}
	if err := schema.ValidateSessionRelationships(mapping.Relationships); err != nil {
		return CodexGraphMapping{}, &codexCandidateRefusal{
			Operation: "MapCodexGraph",
			Reason:    "the mapped Codex relationships are not a valid navigation graph",
			Effect:    "no candidate was emitted and the retained generation is unchanged",
			Recovery:  "retain the capture, correct the native parent evidence, and retry harvest",
		}
	}
	return mapping, nil
}

// validatedGraphSessionID reports whether a raw native session reference is
// a usable logical target. Malformed values are absent evidence, not a
// candidate failure: the relation is omitted and the content is preserved.
func validatedGraphSessionID(raw string) (SessionID, bool) {
	if raw == "" {
		return "", false
	}
	identifier, err := NewSessionID(raw)
	if err != nil {
		return "", false
	}
	return identifier, true
}

// mapStartedBy resolves the started_by relation. Disagreeing current native
// parents conflict and clear the target; one current parent is known; a
// decoded session_meta naming no parent is explicit none, which clears any
// stale cache; only an unavailable session_meta (no native parent evidence at
// all) retains a prior known target; otherwise the relation is omitted.
func mapStartedBy(thread CodexThreadEvidence, prior *schema.UnifiedMetadata) (*schema.SessionRelationship, *SessionID) {
	top, topOK := validatedGraphSessionID(thread.TopParentID)
	nested, nestedOK := validatedGraphSessionID(thread.NestedParentID)
	switch {
	case topOK && nestedOK && top != nested:
		return &schema.SessionRelationship{
			Kind:        schema.SessionRelationshipStartedBy,
			TargetState: schema.RelationshipTargetConflictingCurrentNativeEvidence,
			Evidence:    schema.EvidenceConflict,
		}, nil
	case topOK:
		parent := SessionID(top)
		return &schema.SessionRelationship{
			Kind:          schema.SessionRelationshipStartedBy,
			TargetState:   schema.RelationshipTargetKnown,
			TargetLocalID: (*schema.SessionID)(&parent),
			Evidence:      schema.EvidenceNativeTyped,
		}, &parent
	case nestedOK:
		parent := SessionID(nested)
		return &schema.SessionRelationship{
			Kind:          schema.SessionRelationshipStartedBy,
			TargetState:   schema.RelationshipTargetKnown,
			TargetLocalID: (*schema.SessionID)(&parent),
			Evidence:      schema.EvidenceNativeTyped,
		}, &parent
	}
	if thread.HasSessionMeta {
		return &schema.SessionRelationship{
			Kind:        schema.SessionRelationshipStartedBy,
			TargetState: schema.RelationshipTargetExplicitNone,
			Evidence:    schema.EvidenceNativeTyped,
		}, nil
	}
	if retained := retainedGraphTarget(prior, schema.SessionRelationshipStartedBy); retained != nil {
		target := *retained
		return &schema.SessionRelationship{
			Kind:          schema.SessionRelationshipStartedBy,
			TargetState:   schema.RelationshipTargetKnownRetained,
			TargetLocalID: &target,
			Evidence:      schema.EvidenceRetainedLastGood,
		}, nil
	}
	return nil, nil
}

// mapContextFrom resolves the context_from relation from the fork source,
// independently of started_by. The general anchor links the source session
// without claiming a verified entry boundary: exact anchors need redacted
// public refs the candidate cannot verify.
func mapContextFrom(thread CodexThreadEvidence, prior *schema.UnifiedMetadata) *schema.SessionRelationship {
	if source, ok := validatedGraphSessionID(thread.ForkSourceID); ok {
		target := schema.SessionID(source)
		return &schema.SessionRelationship{
			Kind:          schema.SessionRelationshipContextFrom,
			TargetState:   schema.RelationshipTargetKnown,
			TargetLocalID: &target,
			Evidence:      schema.EvidenceNativeTyped,
			Anchor:        &schema.PublicSourceAnchor{Kind: schema.PublicSourceAnchorGeneral},
		}
	}
	if retained := retainedGraphTarget(prior, schema.SessionRelationshipContextFrom); retained != nil {
		target := *retained
		return &schema.SessionRelationship{
			Kind:          schema.SessionRelationshipContextFrom,
			TargetState:   schema.RelationshipTargetKnownRetained,
			TargetLocalID: &target,
			Evidence:      schema.EvidenceRetainedLastGood,
		}
	}
	return nil
}

// retainedGraphTarget recovers the last good target of one relation kind
// from prior metadata. Only known states carry a reusable target; a prior
// conflict, explicit none, or unknown state retains nothing.
func retainedGraphTarget(prior *schema.UnifiedMetadata, kind schema.SessionRelationshipKind) *schema.SessionID {
	if prior == nil {
		return nil
	}
	for i := range prior.Relationships {
		relation := prior.Relationships[i]
		if relation.Kind != kind {
			continue
		}
		if relation.TargetLocalID == nil {
			return nil
		}
		switch relation.TargetState {
		case schema.RelationshipTargetKnown, schema.RelationshipTargetKnownRetained:
			target := *relation.TargetLocalID
			return &target
		default:
			return nil
		}
	}
	return nil
}

// CodexProvenanceIndexerConfig wires the native provenance candidate path into
// a CodexIndexer. Prior supplies the last good graph/alias state (nil before a
// first install), GenerationID assigns the installed generation identity, and
// Allocator mints or replays opaque references. The path stays disabled until
// the maintenance activation enables it; while disabled every retained V1 flow
// is untouched.
type CodexProvenanceIndexerConfig struct {
	Enabled      bool
	Prior        func(session DiscoveredSession) (*schema.UnifiedMetadata, ProjectionPriorState, error)
	GenerationID func(session DiscoveredSession) string
	Allocator    RefAllocator
}

// IndexCodexCandidateV2 runs the production Codex provenance exit: read-only
// current-history capture with recheck, metadata extraction over the verified
// incarnation, native classification, canonical graph mapping, and the shared
// validated generation projection. It returns the concrete format-2 candidate
// the maintenance activation consumes and enables no format target itself.
func (idx *CodexIndexer) IndexCodexCandidateV2(ctx context.Context, session DiscoveredSession) (indexformat.V2, error) {
	config := idx.provenanceCapture
	if !config.Enabled {
		return indexformat.V2{}, fmt.Errorf("ingest.CodexIndexer.IndexCodexCandidateV2: the provenance candidate path is disabled for session %s; the retained entry path is unchanged; enable it with a real generation identity before requesting a candidate", session.SessionID)
	}
	if config.GenerationID == nil {
		return indexformat.V2{}, fmt.Errorf("ingest.CodexIndexer.IndexCodexCandidateV2: no generation identity source was supplied for session %s; activation owns generation addressing and the projection invents none; wire the generation identity before requesting a candidate", session.SessionID)
	}
	var prior *schema.UnifiedMetadata
	priorState := NewProjectionPriorState()
	if config.Prior != nil {
		loadedPrior, loadedState, err := config.Prior(session)
		if err != nil {
			return indexformat.V2{}, err
		}
		prior = loadedPrior
		priorState = loadedState
	}
	generationID := config.GenerationID(session)
	candidate, err := idx.BuildCodexCandidateForSession(ctx, session, prior, priorState, config.Allocator, generationID)
	if err != nil {
		return indexformat.V2{}, err
	}
	return candidate.V2, nil
}

// codexCandidateRefusal is the safe refusal for the Codex candidate boundary.
// It names a fixed operation, reason, caller effect, and recovery, and never
// carries a native locator, a raw validator value, or a wrapped filesystem
// error, so a refusal can be logged or surfaced without leaking private
// locations.
type codexCandidateRefusal struct {
	Operation string
	Reason    string
	Effect    string
	Recovery  string
}

func (e *codexCandidateRefusal) Error() string {
	return fmt.Sprintf("ingest.%s: %s; %s; %s", e.Operation, e.Reason, e.Effect, e.Recovery)
}

// CodexSourceProof is the source evidence a Codex candidate carries for
// maintenance activation. It names the stable thread, the authoritative
// pointer, the physical incarnation, the capture fingerprint, and the
// completeness the candidate proves. The fingerprint is local evidence only,
// never a public anchor.
type CodexSourceProof struct {
	StableThreadID   string
	Pointer          string
	PhysicalSourceID string
	Fingerprint      string
	Completeness     indexformat.GenerationCompleteness
	GenerationID     string
}

// CodexCandidate is one validated Codex managed-generation candidate with
// its source proof and the full content bytes its generation names. A complete
// candidate measures its input submissions; an incomplete_new candidate omits
// the count until a complete inspection exists. Activation, file staging, and
// database writes belong to maintenance activation, not to this candidate.
type CodexCandidate struct {
	V2    indexformat.V2
	Proof CodexSourceProof
	// Content holds the full bytes for every content record the generation
	// names, including retained inherited evidence, so an activation can stage
	// a self-contained candidate without reopening the native source.
	Content     map[schema.SourceEntryRef][]byte
	Diagnostics []DiagnosticEntry
}

// CodexCandidateInput is the typed seam for Codex candidate emission: the
// captured native history, the extracted metadata base, the optional prior
// metadata for retained relations, the prior alias state, and the install
// identity the caller assigns.
type CodexCandidateInput struct {
	History      CodexCapturedHistory
	Base         schema.UnifiedMetadata
	Prior        *schema.UnifiedMetadata
	PriorState   ProjectionPriorState
	Allocator    RefAllocator
	GenerationID string
}

// BuildCodexCandidate projects one captured Codex history into a validated
// managed-generation candidate. It classifies every captured node with
// native evidence only, maps the canonical logical graph, and builds the
// generation through the shared projection builder, so references, layout,
// counts, and titles follow the one shared algorithm. The mutable native
// source is never reopened: every byte the classifier reads comes from the
// capture.
func BuildCodexCandidate(input CodexCandidateInput) (CodexCandidate, error) {
	if input.History.StableThreadID == "" {
		return CodexCandidate{}, fmt.Errorf("ingest.BuildCodexCandidate: the captured history carries no stable thread identity; the candidate cannot be addressed; capture the native history before projecting it")
	}
	if _, err := NewSessionID(input.History.StableThreadID); err != nil {
		return CodexCandidate{}, &codexCandidateRefusal{
			Operation: "BuildCodexCandidate",
			Reason:    "the captured stable thread identity is not a canonical identifier",
			Effect:    "no candidate was emitted and the retained generation is unchanged",
			Recovery:  "recapture the native history and retry harvest",
		}
	}
	if input.GenerationID == "" {
		return CodexCandidate{}, fmt.Errorf("ingest.BuildCodexCandidate: no generation id was supplied for thread %q; activation owns generation addressing and the projection invents none; assign the installed generation id", input.History.StableThreadID)
	}
	if input.History.Fingerprint == "" {
		return CodexCandidate{}, fmt.Errorf("ingest.BuildCodexCandidate: the captured history for thread %q carries no source evidence digest; the candidate cannot be verified; capture the native history before projecting it", input.History.StableThreadID)
	}
	thread, err := DecodeCodexThreadEvidence(input.History.RawBytes, input.History.StableThreadID)
	if err != nil {
		return CodexCandidate{}, err
	}
	blocks, earlierStates, err := ClassifyCodexBlocks(input.History.StableThreadID, input.History.Nodes, thread, input.History.Correlations)
	if err != nil {
		return CodexCandidate{}, err
	}
	graph, err := MapCodexGraph(thread, input.Prior)
	if err != nil {
		return CodexCandidate{}, err
	}
	metadata := input.Base
	sessionID := SessionID(input.History.StableThreadID)
	metadata.SessionID = sessionID
	metadata.ModelHarness = HarnessCodex
	metadata.Purpose = graph.Purpose
	metadata.Relationships = graph.Relationships
	metadata.RootSessionID = (*schema.SessionID)(graph.RootSessionID)
	metadata.ParentUUID = (*SessionID)(graph.ParentUUID)
	allocator := input.Allocator
	if allocator == nil {
		allocator = RandomRefAllocator{}
	}
	capture := ClassifiedCapture{
		ID:                   input.GenerationID,
		SessionID:            sessionID,
		Harness:              HarnessCodex,
		Metadata:             metadata,
		SourceEvidenceDigest: input.History.Fingerprint,
		Completeness:         input.History.Completeness,
		EarlierStates:        earlierStates,
		Blocks:               blocks,
		Segments:             input.History.Segments,
		Prior:                input.PriorState,
	}
	result, content, err := BuildV2WithContent(capture, allocator)
	if err != nil {
		return CodexCandidate{}, err
	}
	return CodexCandidate{
		V2: result,
		Proof: CodexSourceProof{
			StableThreadID:   input.History.StableThreadID,
			Pointer:          input.History.Pointer,
			PhysicalSourceID: input.History.PhysicalSourceID,
			Fingerprint:      input.History.Fingerprint,
			Completeness:     input.History.Completeness,
			GenerationID:     input.GenerationID,
		},
		Content:     content,
		Diagnostics: input.History.Diagnostics,
	}, nil
}

// BuildCodexCandidateForSession runs the real Codex extraction-to-candidate
// path: read-only current-history capture with retry, metadata extraction
// over the verified incarnation, native classification, graph mapping, and
// shared generation projection. It returns complete and incomplete_new
// candidates with their source proof for maintenance activation. It enables
// no format target and performs no file or database activation: those belong
// to the harvester registry path.
func (idx *CodexIndexer) BuildCodexCandidateForSession(ctx context.Context, session DiscoveredSession, prior *schema.UnifiedMetadata, priorState ProjectionPriorState, allocator RefAllocator, generationID string) (CodexCandidate, error) {
	if generationID == "" {
		return CodexCandidate{}, fmt.Errorf("ingest.CodexIndexer.BuildCodexCandidateForSession: no generation id was supplied for session %s; activation owns generation addressing and the projection invents none; assign the installed generation id", session.SessionID)
	}
	var options []CodexFileSourceOption
	if idx.pointerStore != nil {
		options = append(options, WithCodexNativePointerStore(idx.pointerStore))
	}
	history, err := CaptureCodexHistoryWithRetry(ctx, NewCodexFileSource(idx.fs, options...), session, nil)
	if err != nil {
		return CodexCandidate{}, err
	}
	identified := session
	if stableID, idErr := NewSessionID(history.StableThreadID); idErr == nil && history.StableThreadID != "" {
		identified.SessionID = stableID
	}
	if history.Pointer != "" {
		identified.SourcePath = ResolvedPath(history.Pointer)
	}
	base, err := idx.extractCandidateMetadata(ctx, identified, history.RawBytes)
	if err != nil {
		return CodexCandidate{}, err
	}
	return BuildCodexCandidate(CodexCandidateInput{
		History:      history,
		Base:         *base,
		Prior:        prior,
		PriorState:   priorState,
		Allocator:    allocator,
		GenerationID: generationID,
	})
}

// extractCandidateMetadata extracts the metadata base from the verified
// capture bytes using the retained extraction kernel: model, version,
// timestamps, token totals, and recorded working-directory and git context.
// Counts, purpose, relationships, and identity come from the candidate
// projection, never from this legacy extraction. It never reopens the mutable
// native source, so a replacement, append, or delete after verification cannot
// mix one incarnation's entries with another incarnation's metadata. Project
// identity derivation needs the harvester's salt and git resolver, so it stays
// with the retained path and maintenance activation.
func (idx *CodexIndexer) extractCandidateMetadata(ctx context.Context, session DiscoveredSession, data []byte) (*UnifiedMetadata, error) {
	meta := NewUnifiedMetadata()
	meta.SessionID = session.SessionID
	meta.ModelHarness = HarnessCodex
	meta.Source = SourceInfo{
		FilePath: string(session.SourcePath),
		Format:   SourceFormatJSONL,
	}
	sessionMeta, err := parseCodexTranscriptMetadata(ctx, data, &meta)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// The extraction kernel text can name the captured identity. Refuse with
		// a fixed safe reason instead of echoing the validator value.
		return nil, &codexCandidateRefusal{
			Operation: "CodexIndexer.extractCandidateMetadata",
			Reason:    "the captured metadata is not self-consistent with the captured identity",
			Effect:    "no candidate was emitted and the retained generation is unchanged",
			Recovery:  "recapture the native history and retry harvest",
		}
	}
	if sessionMeta != nil {
		meta.CWD = sessionMeta.CWD
		if sessionMeta.Git != nil {
			if sessionMeta.Git.Branch != "" {
				branch := sessionMeta.Git.Branch
				meta.Git.Branch = &branch
			}
			if sessionMeta.Git.RepositoryURL != "" {
				remote := sessionMeta.Git.RepositoryURL
				meta.Git.Remote = &remote
			}
		}
	}
	return &meta, nil
}
