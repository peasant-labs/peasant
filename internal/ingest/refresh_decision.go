package ingest

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"time"
)

// RefreshReason is the closed set of reasons a refresh decision can name.
//
// The refreshing reasons spell the same codes as the run-outcome
// refresh reasons in outcome_reason.go; their values are derived from those
// constants so a code has one spelling. SourceCompare and None are decisions
// that write no refresh outcome row.
type RefreshReason string

// The values are variables rather than typed constants because their spelling
// is owned by outcome_reason.go. A switch on them is an ordinary expression
// switch and stays exhaustive-checkable through AllRefreshReasons.
var (
	RefreshReasonNew                                     = RefreshReason(OutcomeReasonRefreshNew.Code())
	RefreshReasonSourceChanged                           = RefreshReason(OutcomeReasonRefreshSourceChanged.Code())
	RefreshReasonPairMissing                             = RefreshReason(OutcomeReasonRefreshPairMissing.Code())
	RefreshReasonMetadataSchema                          = RefreshReason(OutcomeReasonRefreshMetadataSchema.Code())
	RefreshReasonForced                                  = RefreshReason(OutcomeReasonRefreshForced.Code())
	RefreshReasonAdapterBehind                           = RefreshReason(OutcomeReasonRefreshAdapterBehind.Code())
	RefreshReasonAdapterBehindNativeOnly                 = RefreshReason(OutcomeReasonRefreshAdapterBehindNativeOnly.Code())
	RefreshReasonAdapterStampAbsent                      = RefreshReason(OutcomeReasonRefreshAdapterStampAbsent.Code())
	RefreshReasonIndexerBehind                           = RefreshReason(OutcomeReasonRefreshIndexerBehind.Code())
	RefreshReasonRefusalReopened                         = RefreshReason(OutcomeReasonRefreshRefusalReopened.Code())
	RefreshReasonPublicationUnbound                      = RefreshReason(OutcomeReasonRefreshPublicationUnbound.Code())
	RefreshReasonPublicationCaptureMissing               = RefreshReason(OutcomeReasonRefreshPublicationCaptureMissing.Code())
	RefreshReasonSourceCompare             RefreshReason = "source_compare"
	RefreshReasonNone                      RefreshReason = "none"
)

// AllRefreshReasons lists every decision reason in design order: the
// refreshing reasons, then the two decisions that write no outcome row.
var AllRefreshReasons = []RefreshReason{
	RefreshReasonNew,
	RefreshReasonSourceChanged,
	RefreshReasonPairMissing,
	RefreshReasonMetadataSchema,
	RefreshReasonForced,
	RefreshReasonAdapterBehind,
	RefreshReasonAdapterBehindNativeOnly,
	RefreshReasonAdapterStampAbsent,
	RefreshReasonIndexerBehind,
	RefreshReasonRefusalReopened,
	RefreshReasonPublicationUnbound,
	RefreshReasonPublicationCaptureMissing,
	RefreshReasonSourceCompare,
	RefreshReasonNone,
}

// NewRefreshReason validates a raw reason name at an input boundary.
func NewRefreshReason(raw string) (RefreshReason, error) {
	reason := RefreshReason(raw)
	if slices.Contains(AllRefreshReasons, reason) {
		return reason, nil
	}
	return "", fmt.Errorf("ingest: read refresh reason %q: this build defines no such decision reason; nothing was refreshed; use one of the named refresh reasons", raw)
}

var refreshOutcomeReasons = func() map[RefreshReason]OutcomeReason {
	reasons := make(map[RefreshReason]OutcomeReason, len(AllRefreshReasons))
	for _, reason := range AllRefreshReasons {
		outcome, err := NewOutcomeReason(string(reason))
		if err == nil && outcome.Kind() == OutcomeKindRefresh {
			reasons[reason] = outcome
		}
	}
	return reasons
}()

// OutcomeReason returns the run-outcome reason this decision records, or 0 for
// source_compare and none, which write no outcome row.
func (r RefreshReason) OutcomeReason() OutcomeReason { return refreshOutcomeReasons[r] }

// Refreshing reports whether the reason writes a refresh outcome row.
func (r RefreshReason) Refreshing() bool { return r.OutcomeReason().Defined() }

// RefreshDecision is the one answer to "what does this stored session need?".
// The lane is the path the work takes; the reason names why.
type RefreshDecision struct {
	Reason RefreshReason
	Lane   RefreshLane
}

// IsRefresh reports whether the decision records a refresh.
func (d RefreshDecision) IsRefresh() bool { return d.Reason.Refreshing() }

// OutcomeReason returns the reason's outcome row, or 0 for a decision that
// writes none.
func (d RefreshDecision) OutcomeReason() OutcomeReason { return d.Reason.OutcomeReason() }

// Status is the public DiffStatus summary derived from the decision. A stored
// index refresh is a pending-work update that the index lane settles without a
// native capture, so it reports unchanged: the summary counts what the run
// rewrote, not what it re-indexed. A native source compare is unchanged when
// its bytes match; the caller re-decides with the captured bytes.
func (d RefreshDecision) Status() DiffStatus {
	switch d.Reason {
	case RefreshReasonNew, RefreshReasonForced:
		return DiffNew
	case RefreshReasonSourceChanged, RefreshReasonPairMissing, RefreshReasonMetadataSchema,
		RefreshReasonAdapterBehind, RefreshReasonAdapterBehindNativeOnly, RefreshReasonAdapterStampAbsent,
		RefreshReasonPublicationCaptureMissing:
		return DiffUpdated
	default:
		return DiffUnchanged
	}
}

// refreshEvidence is the closed set of facts a refresh decision reads. Each
// caller fills the facts it can observe; every fact it cannot observe keeps its
// zero value, so a caller never invents evidence.
type refreshEvidence struct {
	// StoredRow reports that a stored row exists. A session with no row is new.
	StoredRow bool
	// PairMissing reports that the stored pair is absent or damaged.
	PairMissing bool
	// Forced reports an explicit --force run.
	Forced bool
	// SourceChanged reports that the captured source bytes, clock or cursor
	// moved past the stored evidence.
	SourceChanged bool
	// PublicationCannotBind and StructuralMismatch require native repair.
	PublicationCannotBind bool
	StructuralMismatch    bool
	// OpenCodeClockMoved requires native comparison before a stored-index or no-op decision.
	OpenCodeClockMoved bool
	// Discovery freshness is a preliminary clock, schema and readiness hint.
	FreshnessFutureSchema      bool
	FreshnessClockMoved        bool
	FreshnessSchemaOld         bool
	FreshnessPublicationNeeded bool
	// SourceClockMoved reports a source clock newer than the stored ingest
	// clock. The bytes must be read before any stored lane may claim the source
	// unchanged, so this outranks a behind producer.
	SourceClockMoved bool
	// SourceComparePending reports a settled stored row that still holds a
	// source fingerprint. The ordinary native comparison preflight reads it to
	// prove an append did not land between the capture and its ingest stamp.
	SourceComparePending bool
	// SourceCompared reports that captured bytes matched the stored fingerprint.
	SourceCompared bool
	// SourceUnproven reports a stored row whose index holds no input proof.
	// The stored copy is not known to describe the discovered source, so the
	// source must be compared natively instead of trusting the stored index.
	SourceUnproven bool
	// MetadataSchemaOld reports a stored metadata schema this build re-extracts
	// natively.
	MetadataSchemaOld bool
	// CaptureRecoveryNeeded reports a migrated preview-only capture with
	// recorded source evidence. It predates the refusal vocabulary, so only a
	// native capture can restore its full content.
	CaptureRecoveryNeeded bool
	// PublicationCaptureMissing marks a fingerprint-bearing row saved before
	// publication metadata was captured. Only native acquisition can create it.
	// A permanent refusal stays settled unless the store has certified its
	// saved content as publishable partial. In that case a native capture can
	// supply the missing publication metadata without re-deciding eligibility.
	PublicationCaptureMissing bool
	// AdapterVersion, AdapterTarget and AdapterStampPresent describe the adapter
	// producer. An absent stamp reads as revision 1.
	AdapterVersion      int
	AdapterTarget       int
	AdapterStampPresent bool
	// AdapterCanExtract reports that the adapter implements stored-transcript
	// metadata extraction.
	AdapterCanExtract bool
	// IndexerVersion and IndexerTarget describe the stored index producer.
	IndexerVersion int
	IndexerTarget  int
	// IndexProofPresent reports that the stored index records the input it
	// consumed. Without it a refusal is not settled.
	IndexProofPresent bool
	// PermanentRefusal reports a stored refusal this build can only lift with a
	// newer parser or new bytes.
	PermanentRefusal bool
	// PublicationUnbound reports a current metadata capture the stored index is
	// not bound to.
	PublicationUnbound bool
	// ContentPending reports incomplete content this build may still certify.
	ContentPending bool
	// LatestAttemptSettled reports a latest attempt that already consumed the
	// current pair under this build's targets.
	LatestAttemptSettled bool
}

// decideRefresh is the one home of the refresh decision. It orders the facts
// the way the accepted design does: a capture-side fact (forced, new, missing
// pair, changed source, old schema) wins a native capture; otherwise the
// producer that is actually behind decides the lane. An indexer reads the
// stored transcript, so its bump never needs the native source. An adapter
// produces the pair, so its bump needs the native source only when the adapter
// cannot re-derive from the stored transcript.
func decideRefresh(in refreshEvidence) (decision RefreshDecision) {
	defer func() {
		if in.OpenCodeClockMoved && (decision.Lane == RefreshLaneStoredIndex || decision.Reason == RefreshReasonNone) {
			decision = RefreshDecision{Reason: RefreshReasonSourceCompare, Lane: RefreshLaneNative}
		}
	}()
	switch {
	case in.Forced:
		return RefreshDecision{Reason: RefreshReasonForced, Lane: RefreshLaneNative}
	case !in.StoredRow:
		return RefreshDecision{Reason: RefreshReasonNew, Lane: RefreshLaneNative}
	case in.PairMissing:
		return RefreshDecision{Reason: RefreshReasonPairMissing, Lane: RefreshLaneNative}
	case in.SourceChanged, in.StructuralMismatch:
		return RefreshDecision{Reason: RefreshReasonSourceChanged, Lane: RefreshLaneNative}
	case in.SourceClockMoved:
		return RefreshDecision{Reason: RefreshReasonSourceCompare, Lane: RefreshLaneNative}
	case in.CaptureRecoveryNeeded:
		return RefreshDecision{Reason: RefreshReasonSourceChanged, Lane: RefreshLaneNative}
	case in.SourceUnproven:
		return RefreshDecision{Reason: RefreshReasonSourceCompare, Lane: RefreshLaneNative}
	case in.MetadataSchemaOld:
		return RefreshDecision{Reason: RefreshReasonMetadataSchema, Lane: RefreshLaneNative}
	case in.PublicationCaptureMissing:
		return RefreshDecision{Reason: RefreshReasonPublicationCaptureMissing, Lane: RefreshLaneNative}
	case in.AdapterVersion < in.AdapterTarget:
		if !in.AdapterCanExtract {
			return RefreshDecision{Reason: RefreshReasonAdapterBehindNativeOnly, Lane: RefreshLaneNative}
		}
		if !in.AdapterStampPresent {
			return RefreshDecision{Reason: RefreshReasonAdapterStampAbsent, Lane: RefreshLaneStoredMetadata}
		}
		return RefreshDecision{Reason: RefreshReasonAdapterBehind, Lane: RefreshLaneStoredMetadata}
	case in.LatestAttemptSettled:
		return RefreshDecision{Reason: RefreshReasonNone, Lane: RefreshLaneNone}
	case in.PermanentRefusal && (in.IndexerVersion < in.IndexerTarget || !in.IndexProofPresent):
		return RefreshDecision{Reason: RefreshReasonRefusalReopened, Lane: RefreshLaneStoredIndex}
	case in.IndexerVersion < in.IndexerTarget:
		return RefreshDecision{Reason: RefreshReasonIndexerBehind, Lane: RefreshLaneStoredIndex}
	case !in.IndexProofPresent:
		// A missing input proof anchors nothing to the bytes it claims to
		// describe. The index write restores it from the stored transcript.
		return RefreshDecision{Reason: RefreshReasonIndexerBehind, Lane: RefreshLaneStoredIndex}
	case in.ContentPending:
		return RefreshDecision{Reason: RefreshReasonIndexerBehind, Lane: RefreshLaneStoredIndex}
	case in.PublicationUnbound:
		if in.PublicationCannotBind {
			return RefreshDecision{Reason: RefreshReasonSourceChanged, Lane: RefreshLaneNative}
		}
		return RefreshDecision{Reason: RefreshReasonPublicationUnbound, Lane: RefreshLaneStoredIndex}
	case in.SourceCompared, in.SourceComparePending:
		return RefreshDecision{Reason: RefreshReasonSourceCompare, Lane: RefreshLaneNative}
	default:
		return RefreshDecision{Reason: RefreshReasonNone, Lane: RefreshLaneNone}
	}
}

// storedAdapterEvidence returns the effective stored adapter revision and
// whether a stamp was recorded. The row is the durability point and the one
// stamp maintenance selects on, so it is authoritative whenever it exists. A
// NULL row stamp reads as baseline 1, exactly as it does in the selection SQL;
// a file stamp is only a fallback for a legacy pair with no row.
func storedAdapterEvidence(loc SessionLocation, hasLocation bool, metadata *UnifiedMetadata) (version int, present bool) {
	if hasLocation {
		if loc.AdapterVersion != nil {
			return *loc.AdapterVersion, true
		}
		return storedAdapterRevision(nil), false
	}
	if metadata != nil && metadata.AdapterVersion != nil {
		return *metadata.AdapterVersion, true
	}
	return storedAdapterRevision(nil), false
}

// adapterSupportsStoredExtraction reports whether the registered adapter can
// re-derive its metadata from the stored transcript. It asks the real interface
// rather than naming a harness list, so a new native-only adapter is covered
// without a second definition of the set.
func (p *Pipeline) adapterSupportsStoredExtraction(harness Harness) bool {
	factory, ok := p.adapters[harness]
	if !ok {
		return false
	}
	_, ok = factory(p.fs, p.git, p.salt).(TranscriptMetadataExtractor)
	return ok
}

func (p *Pipeline) adapterDeclaresOutputPreserving(harness Harness) bool {
	factory, ok := p.adapters[harness]
	if !ok {
		return false
	}
	declaration, ok := factory(p.fs, p.git, p.salt).(OutputPreservingDeclaration)
	return ok && declaration.DeclaresOutputPreserving()
}

// classificationDecision is the decision the discovery classifier derives
// from the stored row. It reads no file: the row carries the adapter stamp,
// index producer, refusal and publication facts the decision needs, and the
// source clock and fingerprint it compares against.
func (p *Pipeline) classificationDecision(session DiscoveredSession, captured *MaterializedTranscript) RefreshDecision {
	in, ok := p.refreshEvidenceForClassification(session, captured)
	if !ok {
		return RefreshDecision{}
	}
	return decideRefresh(in)
}

// refreshEvidenceForClassification reads the stored row the discovery
// classifier already holds. captured is nil before a native read and carries
// the captured bytes after it.
func (p *Pipeline) refreshEvidenceForClassification(session DiscoveredSession, captured *MaterializedTranscript) (refreshEvidence, bool) {
	loc, ok := p.locationCache[session.SessionID]
	if !ok {
		// A row the cache cannot name is not authoritatively stored. A metadata
		// file may still exist for pre-migration data, but only a captured
		// comparison can decide that case; leave it to the caller's file path.
		return refreshEvidence{}, false
	}
	in := p.refreshEvidenceForLocation(session, loc, nil)
	in.Forced = p.config.Force
	in.StoredRow = loc.IngestedMs != nil
	in.PermanentRefusal = permanentCaptureRefusal(loc.ContentFailureCode)
	in.IndexProofPresent = loc.IndexedInputHash != nil
	in.SourceUnproven = captured == nil && loc.SourceEvidenceSupported && loc.IndexerVersion > 0 && loc.IndexedInputHash == nil
	in.PublicationUnbound = loc.CaptureRevision > 0 && !loc.PublicationBound &&
		!metadataNeedsNativeRefresh(loc.SchemaVersion)
	if captured != nil && loc.SourceEvidenceSupported {
		if len(loc.SourceFingerprint) == 0 {
			// No stored fingerprint has ever been recorded: the first capture
			// acquires the evidence and is a source change by definition.
			in.SourceChanged = true
		} else if len(captured.SourceFingerprint) > 0 && !bytes.Equal(loc.SourceFingerprint, captured.SourceFingerprint) {
			in.SourceChanged = true
		} else {
			in.SourceCompared = true
		}
	}
	if storedSeq, tracked := p.seqCursorCache[session.SessionID]; tracked && session.EventSeq > storedSeq {
		// An in-place rewrite can move the event cursor without moving any
		// time column. It is source change evidence regardless of the lane.
		in.SourceChanged = true
	}
	if captured == nil && loc.IngestedMs != nil {
		if !session.ModTime.IsZero() && session.ModTime.After(time.UnixMilli(*loc.IngestedMs)) {
			// A newer source clock cannot be proven unchanged without reading
			// it. The captured comparison re-decides with the bytes.
			in.SourceClockMoved = true
		} else if loc.SourceEvidenceSupported && len(loc.SourceFingerprint) > 0 &&
			!permanentCaptureRefusal(loc.ContentFailureCode) {
			// The ordinary compare preflight: an append can land between the
			// previous capture and its ingest stamp, so a settled row with a
			// fingerprint is read once to prove it unchanged. A zero discovery
			// clock is not evidence; the stored fingerprint still is. This is
			// not a refresh and writes no outcome row.
			in.SourceComparePending = true
		}
	}
	return in, true
}

// refreshEvidenceForLocation fills the facts held by the stored row itself.
func (p *Pipeline) refreshEvidenceForLocation(session DiscoveredSession, loc SessionLocation, metadata *UnifiedMetadata) refreshEvidence {
	target := p.versionTargets()[session.Harness]
	version, present := storedAdapterEvidence(loc, true, metadata)
	return refreshEvidence{
		StoredRow:             loc.IngestedMs != nil,
		Forced:                p.config.Force,
		MetadataSchemaOld:     metadataNeedsNativeRefresh(loc.SchemaVersion),
		CaptureRecoveryNeeded: loc.ContentFailureCode == ContentCaptureLegacyPreviewOnly && len(loc.SourceFingerprint) > 0,
		PublicationCaptureMissing: loc.CaptureRevision == 0 && loc.SourceEvidenceSupported &&
			len(loc.SourceFingerprint) > 0 && (!permanentCaptureRefusal(loc.ContentFailureCode) || loc.ContentPublishable),
		AdapterVersion:      version,
		AdapterTarget:       target.AdapterVersion,
		AdapterStampPresent: present,
		AdapterCanExtract:   p.adapterSupportsStoredExtraction(session.Harness),
		IndexerVersion:      loc.IndexerVersion,
		IndexerTarget:       target.IndexerVersion,
		IndexProofPresent:   loc.IndexedInputHash != nil,
		PermanentRefusal:    permanentCaptureRefusal(loc.ContentFailureCode),
	}
}

// storedMetadataForDecision reads the saved metadata half of the pair without
// opening the transcript. It is the structural comparison the stored-index
// early return needs before it may claim the stored pair still describes the
// discovered session.
func (p *Pipeline) storedMetadataForDecision(ctx context.Context, session DiscoveredSession) *UnifiedMetadata {
	path, err := p.findMetadataPath(ctx, session)
	if err != nil || path == "" {
		return nil
	}
	data, err := p.fs.ReadFile(path)
	if err != nil {
		return nil
	}
	meta, err := decodeManagedMetadata(data, p.managedRelativePath(path))
	if err != nil || meta == nil {
		return nil
	}
	return meta
}

// publicationBindable reports that an unbound publication capture can be
// settled by the stored pair: the pair is readable and a current capture row
// exists. Otherwise the native capture is the repair.
func (p *Pipeline) publicationBindable(ctx context.Context, sid SessionID) bool {
	if repair, readFault := p.pairNeedsRepairIn(ctx, sid, nil); repair || readFault {
		return false
	}
	reader, ok := p.metricsStore.(SessionIndexStateReader)
	if !ok {
		return false
	}
	state, err := reader.ReadIndexState(ctx, sid)
	return err == nil && state != nil && state.PublicationCaptureRevision > 0
}

// openCodeSessionClockMoved compares the discovered OpenCode changed clock
// with the clock the stored metadata capture records. The audit clock is not
// this clock: an OpenCode session row can move its time_updated while still
// sorting older than the ingest time.
func (p *Pipeline) openCodeSessionClockMoved(ctx context.Context, session DiscoveredSession) bool {
	if session.ModTime.IsZero() {
		return false
	}
	stored := p.storedMetadataForDecision(ctx, session)
	if stored == nil {
		return false
	}
	storedClock := stored.Timestamp.End
	if storedClock == 0 {
		storedClock = stored.Timestamp.Start
	}
	if storedClock == 0 {
		return false
	}
	return session.ModTime.After(time.UnixMilli(storedClock))
}

// refreshDecisionForFile is the store-free / legacy file fallback's decision.
// The pair is the metadata file; a stored file-only harvest proves the source
// unchanged by reading it once and comparing the owned capture marker.
func (p *Pipeline) refreshDecisionForFile(session DiscoveredSession, metadata *UnifiedMetadata, sourceCompare bool) RefreshDecision {
	return decideRefresh(p.refreshEvidenceForFile(session, metadata, sourceCompare))
}

func (p *Pipeline) refreshEvidenceForFile(session DiscoveredSession, metadata *UnifiedMetadata, sourceCompare bool) refreshEvidence {
	version, present := storedAdapterEvidence(SessionLocation{}, false, metadata)
	in := refreshEvidence{
		StoredRow:         metadata != nil,
		Forced:            p.config.Force,
		MetadataSchemaOld: metadata != nil && metadataNeedsNativeRefresh(metadata.SchemaVersion),
		// A file-only pair has no stored index producer to settle; its
		// evidence is the marker and the saved metadata. Do not read the
		// absent row fields as pending index work.
		IndexProofPresent:    true,
		SourceComparePending: sourceCompare,
	}
	if sourceCompare {
		// The owned marker or a current-schema artifact makes the source
		// worth proving again. An old artifact with neither evidence is left
		// on the clock hint, exactly as the file fallback leaves it.
		in.AdapterVersion = version
		in.AdapterTarget = p.versionTargets()[session.Harness].AdapterVersion
		in.AdapterStampPresent = present
		in.AdapterCanExtract = p.adapterSupportsStoredExtraction(session.Harness)
	}
	if metadata != nil {
		if metadata.Timestamp.Ingested != nil && !session.ModTime.IsZero() &&
			session.ModTime.After(time.UnixMilli(*metadata.Timestamp.Ingested)) {
			in.SourceClockMoved = true
		}
		if metadata.Source.FilePath != "" && metadata.Source.FilePath != session.SourcePath.String() {
			in.SourceChanged = true
		}
		if metadata.Source.Format != "" && metadata.Source.Format != session.SourceFormat {
			in.SourceChanged = true
		}
		if !sameParentSession(metadata.ParentUUID, session.ParentUUID) {
			in.SourceChanged = true
		}
	}
	return in
}

func sameParentSession(a, b *SessionID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// refreshDecisionForEntry returns the decision a worker routes from. A decision
// the diff phase carried is authoritative; a directly constructed entry (a
// repair or a test) gets the same decision computed from its stored row and
// metadata.
func (p *Pipeline) refreshDecisionForEntry(session DiscoveredSession, metadata *UnifiedMetadata, carried RefreshDecision) RefreshDecision {
	// The zero value means no decision was carried, distinct from the valid
	// none lane of an explicit decision.
	const unspecifiedLane RefreshLane = ""
	if carried.Lane != unspecifiedLane {
		return carried
	}
	loc, ok := p.locationCache[session.SessionID]
	if !ok {
		return p.refreshDecisionForFile(session, metadata, true)
	}
	in := p.refreshEvidenceForLocation(session, loc, metadata)
	in.PublicationUnbound = loc.CaptureRevision > 0 && !loc.PublicationBound &&
		!metadataNeedsNativeRefresh(loc.SchemaVersion)
	if loc.IngestedMs != nil && !session.ModTime.IsZero() && session.ModTime.After(time.UnixMilli(*loc.IngestedMs)) {
		in.SourceClockMoved = true
	}
	return decideRefresh(in)
}

// refreshDecisionForLocation is the stored-row decision used by the stored
// maintenance passes, which have no captured source.
func (p *Pipeline) refreshDecisionForLocation(session DiscoveredSession, loc SessionLocation, metadata *UnifiedMetadata) RefreshDecision {
	in := p.refreshEvidenceForLocation(session, loc, metadata)
	in.PublicationUnbound = loc.CaptureRevision > 0 && !loc.PublicationBound &&
		!metadataNeedsNativeRefresh(loc.SchemaVersion)
	return decideRefresh(in)
}

// refreshDecisionForIndexState is the stored-index half of the decision. It
// reads only stored SQL state and the captured input proof, so a caller can
// decide without opening the pair.
func (p *Pipeline) refreshDecisionForIndexState(state *SessionIndexState, target HarvesterVersions, inputHashMatches bool, bindsPublication bool) RefreshDecision {
	if state == nil {
		return decideRefresh(refreshEvidence{StoredRow: true})
	}
	in := refreshEvidence{
		StoredRow:           true,
		Forced:              p.config.Force,
		SourceChanged:       !inputHashMatches,
		AdapterVersion:      target.AdapterVersion,
		AdapterTarget:       target.AdapterVersion,
		AdapterStampPresent: true,
		IndexerVersion:      state.IndexerVersion,
		IndexerTarget:       target.IndexerVersion,
		IndexProofPresent:   state.IndexedInputHash != nil,
		PermanentRefusal:    permanentCaptureRefusal(state.ContentFailureCode),
		ContentPending: state.ContentStatus != ContentCaptureComplete &&
			p.certifiesContent(state.Harness) && !permanentRefusalIsSettled(state, target),
		PublicationUnbound:   state.PublicationCaptureRevision > 0 && !state.PublicationBound && bindsPublication,
		LatestAttemptSettled: !p.config.Reindex && latestAttemptPreviewSettles(state, target),
	}
	return decideRefresh(in)
}
