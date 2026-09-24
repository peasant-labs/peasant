package ingest

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// CaptureCoverage is the single write-selection authority for V1 captures.
// The zero value never certifies.
type CaptureCoverage uint8

const (
	CaptureCoverageUnknown CaptureCoverage = iota
	CaptureCoverageFull
	CaptureCoveragePreview
)

// CaptureReadPolicy selects the coordinate policy for one assessment.
// Fresh candidates require explicit non-null public coordinates.
// LegacyPreview permits only wholly absent coordinates after all other
// evidence validates, and never certifies a fresh full capture.
type CaptureReadPolicy uint8

const (
	CaptureFreshCandidate CaptureReadPolicy = iota
	CaptureLegacyPreview
)

// CaptureFacts are adapters over current captured input and parser result,
// not new success claims. They are derived from the captured parse, never
// from an unknown count.
type CaptureFacts struct {
	Harness       Harness
	Result        indexformat.Result
	Policy        CaptureReadPolicy
	Authoritative bool
	SourceOmitted bool
	Unaccounted   bool
}

// CaptureAssessment is the one owner of the mapping from validated evidence
// and existing parser coverage to the existing capture status/format/code.
// Fields stay private. A result is useful only when AssessCapture returns nil
// error. It is not permission to skip validation at a storage boundary.
// Keep the value request-scoped and immutable. Do not cache it by session ID
// or serialize it as a permanent trust token. Zero assessment is non-full.
type CaptureAssessment struct {
	coverage CaptureCoverage
	policy   CaptureReadPolicy
	partial  bool
	failure  ContentCaptureFailureCode
	retained []RetainedUnknownKindCount
}

// Coverage reports the assessed write-selection authority.
// A nil receiver reports Unknown and never certifies.
func (a *CaptureAssessment) Coverage() CaptureCoverage {
	if a == nil {
		return CaptureCoverageUnknown
	}
	switch a.coverage {
	case CaptureCoverageFull, CaptureCoveragePreview:
		return a.coverage
	default:
		return CaptureCoverageUnknown
	}
}

// CandidateCounts returns a copy of candidate occurrences. Counts stay named
// as candidates until the store outcome confirms the commit for that
// candidate with a full assessment; only then may a caller report them as
// committed. A nil receiver returns nil.
func (a *CaptureAssessment) CandidateCounts() []RetainedUnknownKindCount {
	if a == nil || len(a.retained) == 0 {
		return nil
	}
	out := make([]RetainedUnknownKindCount, len(a.retained))
	copy(out, a.retained)
	return out
}

// ContentCapture converts an assessed capture to a store write. It refuses
// unknown, invalid, or zero conversion before persistence: an unknown
// coverage never silently becomes a valid preview write through the store's
// absent-value defaults. A nil or typed-nil receiver, an unknown coverage,
// and any unsupported coverage or read-policy enum are refused with an error.
func (a *CaptureAssessment) ContentCapture(authority ContentSourceAuthority, origin TranscriptOrigin, capturedAt int64) (SessionContentCaptureWrite, error) {
	if a == nil {
		return SessionContentCaptureWrite{}, fmt.Errorf("ingest.ContentCapture: nil capture assessment cannot be converted to a store write; no persistence was authorized; assess the captured evidence before writing")
	}
	// Typed-nil detection: a non-nil interface holding a nil pointer must not
	// certify. Callers pass *CaptureAssessment; a typed-nil pointer arrives
	// here as a non-nil receiver only when the method set permits it, so
	// defend with reflection as well as the nil comparison above.
	if reflect.ValueOf(a).Kind() == reflect.Pointer && reflect.ValueOf(a).IsNil() {
		return SessionContentCaptureWrite{}, fmt.Errorf("ingest.ContentCapture: nil capture assessment cannot be converted to a store write; no persistence was authorized; assess the captured evidence before writing")
	}
	coverage := a.Coverage()
	if coverage == CaptureCoverageUnknown {
		return SessionContentCaptureWrite{}, fmt.Errorf("ingest.ContentCapture: unknown capture coverage cannot be converted to a store write; no persistence was authorized; assess the captured evidence and resolve the refusal before writing")
	}
	if a.policy != CaptureFreshCandidate && a.policy != CaptureLegacyPreview {
		return SessionContentCaptureWrite{}, fmt.Errorf("ingest.ContentCapture: unknown read policy %d cannot be converted to a store write; no persistence was authorized; assess with a supported policy before writing", uint8(a.policy))
	}
	if _, err := NewContentSourceAuthority(string(authority)); err != nil {
		return SessionContentCaptureWrite{}, err
	}
	if err := origin.Validate(); err != nil {
		return SessionContentCaptureWrite{}, err
	}
	if capturedAt == 0 {
		capturedAt = time.Now().UnixMilli()
	}
	switch coverage {
	case CaptureCoverageFull:
		if a.partial {
			// Full stored coverage with partial interpretation: an accounted
			// incompleteness that still holds every entry. The stored code
			// names the accounted reason; mixed unknown plus omission keeps
			// the omission code with all counts preserved.
			if a.failure != ContentCaptureUnknownDataRetained && a.failure != ContentCaptureSourceRecordsOmitted {
				return SessionContentCaptureWrite{}, fmt.Errorf("ingest.ContentCapture: full partial capture carries unsupported failure code %q; no persistence was authorized; assess the captured evidence before writing", string(a.failure))
			}
			return SessionContentCaptureWrite{
				Status: ContentCaptureIncomplete, SourceAuthority: authority,
				TranscriptOrigin: origin, CaptureFormat: ContentCaptureFormatFull,
				CapturedAtMs: capturedAt, FailureCode: a.failure,
				FailureMessage: captureFailureMessage(a.failure),
			}, nil
		}
		if a.failure != ContentCaptureNoFailure {
			return SessionContentCaptureWrite{}, fmt.Errorf("ingest.ContentCapture: complete full capture carries failure code %q; no persistence was authorized; assess the captured evidence before writing", string(a.failure))
		}
		return SessionContentCaptureWrite{
			Status: ContentCaptureComplete, SourceAuthority: authority,
			TranscriptOrigin: origin, CaptureFormat: ContentCaptureFormatFull,
			CapturedAtMs: capturedAt,
		}, nil
	case CaptureCoveragePreview:
		// Preview-only activation is reported as a preview operation, never as
		// a successful full or retained capture. The store's absent-value
		// defaults must never promote this to full: the format is explicit.
		code := a.failure
		if code == ContentCaptureNoFailure {
			// A preview with no recorded failure is first-discovery
			// incompleteness (for example native incomplete_new), not a
			// refusal. Keep the empty code so the selector can tell
			// not-certified-yet from refused.
			code = ContentCaptureNoFailure
		}
		if _, err := NewContentCaptureFailureCode(string(code)); err != nil {
			return SessionContentCaptureWrite{}, err
		}
		return SessionContentCaptureWrite{
			Status: ContentCaptureIncomplete, SourceAuthority: authority,
			TranscriptOrigin: origin, CaptureFormat: ContentCaptureFormatPreviewOnly,
			CapturedAtMs: capturedAt, FailureCode: code,
			FailureMessage: captureFailureMessage(code),
		}, nil
	default:
		return SessionContentCaptureWrite{}, fmt.Errorf("ingest.ContentCapture: unknown capture coverage %d cannot be converted to a store write; no persistence was authorized; assess the captured evidence before writing", uint8(coverage))
	}
}

func captureFailureMessage(code ContentCaptureFailureCode) string {
	switch code {
	case ContentCaptureNoFailure:
		return ""
	case ContentCaptureUnknownDataRetained:
		return "uninterpreted source data was retained with complete payload and source coordinates; interpretation is partial and outbound transfer limits apply"
	case ContentCaptureSourceRecordsOmitted:
		return "oversized source records were omitted with positional placeholders"
	case ContentCaptureStrictRefused:
		return "strict parser refused the transcript and the tolerant projection was stored instead; previews show it and nothing certifies it"
	case ContentCaptureLegacyPreviewOnly:
		return "legacy capture predates content certification; bounded preview only; re-index the source for a certified capture"
	default:
		return "capture is incomplete; restore a supported intact transcript and rerun harvest index --force"
	}
}

// AssessCapture maps validated evidence and parser coverage to one coherent
// capture tuple. It validates the declared concrete result and the entire
// evidence set before permitting any certification, preserves mixed
// accounted reasons without overwriting, and never promotes an unaccounted
// reason merely because a placeholder or a retained record also exists.
func AssessCapture(facts CaptureFacts) (CaptureAssessment, error) {
	if facts.Policy != CaptureFreshCandidate && facts.Policy != CaptureLegacyPreview {
		return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: unknown read policy %d for harness %q; no capture was certified; assess with a supported policy", uint8(facts.Policy), string(facts.Harness))
	}
	if facts.Harness == "" || !slices.Contains(schema.Harnesses(), facts.Harness) {
		return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: unknown harness %q; no capture was certified; assess with a supported harness", string(facts.Harness))
	}
	if facts.Result == nil {
		return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: nil parser result for harness %q; no capture was certified; return a concrete result only when parsing completed", string(facts.Harness))
	}
	if _, err := indexformat.VersionOf(facts.Result); err != nil {
		return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: %w; no capture was certified", err)
	}
	switch result := facts.Result.(type) {
	case indexformat.V1:
		return assessV1Capture(facts, result)
	case *indexformat.V1:
		if result == nil {
			return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: nil parser result for harness %q; no capture was certified; return a concrete result only when parsing completed", string(facts.Harness))
		}
		return assessV1Capture(facts, *result)
	case indexformat.V2:
		return assessV2Capture(facts, result)
	case *indexformat.V2:
		if result == nil {
			return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: nil parser result for harness %q; no capture was certified; return a concrete result only when parsing completed", string(facts.Harness))
		}
		return assessV2Capture(facts, *result)
	default:
		return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: unsupported result format %T for harness %q; no capture was certified; return a concrete V1 or V2 result", facts.Result, string(facts.Harness))
	}
}

func assessV1Capture(facts CaptureFacts, v1 indexformat.V1) (CaptureAssessment, error) {
	entries := v1.Entries
	hasOmissions := outputRecordsItsOmissions(facts.Result)

	// Validate the entire evidence set regardless of desired coverage.
	// Evidence validates raw at rest; there is no redaction step here.
	collectedErr := validateV1Evidence(entries, facts.Harness)
	if collectedErr != nil {
		if errors.Is(collectedErr, ErrUnknownPositionUnavailable) {
			if facts.Policy == CaptureLegacyPreview {
				// Legacy exception: only wholly absent coordinates after all
				// other evidence validates. Corruption outranks compatibility.
				if legacyCoordinatesWhollyAbsent(entries) {
					if err := validateV1LegacyEvidence(entries, facts.Harness); err != nil {
						return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: legacy evidence invalid for harness %q: %w; no capture was certified", string(facts.Harness), err)
					}
					legacyRetained, legacyErr := retainedUnknownEntries(entries)
					if legacyErr != nil {
						return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: legacy evidence invalid for harness %q: %w; no capture was certified", string(facts.Harness), legacyErr)
					}
					return CaptureAssessment{
						coverage: CaptureCoveragePreview, policy: facts.Policy,
						partial: true, failure: ContentCaptureLegacyPreviewOnly,
						retained: retainedUnknownCounts(legacyRetained),
					}, nil
				}
			}
			return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: %w; no capture was certified; re-index the original source with a position-aware adapter, then retry", collectedErr)
		}
		return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: invalid retained evidence for harness %q: %w; no capture was certified", string(facts.Harness), collectedErr)
	}

	// Candidate counts come only from validated selected evidence.
	var retained []RetainedUnknown
	var err error
	retained, err = retainedUnknownEntries(entries)
	if err != nil {
		return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: invalid retained evidence for harness %q: %w; no capture was certified", string(facts.Harness), err)
	}
	counts := retainedUnknownCounts(retained)
	hasUnknown := len(retained) > 0

	// A legacy policy never certifies fresh full coverage, even when the
	// evidence would otherwise be complete.
	if facts.Policy == CaptureLegacyPreview {
		return CaptureAssessment{
			coverage: CaptureCoveragePreview, policy: facts.Policy,
			partial: true, failure: ContentCaptureLegacyPreviewOnly, retained: counts,
		}, nil
	}

	// Unaccounted loss yields preview regardless of carrier count. A mixed
	// result cannot erase an unaccounted reason merely because it also has
	// one placeholder or one retained record.
	if facts.Unaccounted {
		return CaptureAssessment{
			coverage: CaptureCoveragePreview, policy: facts.Policy,
			partial: true, failure: ContentCaptureStrictRefused, retained: counts,
		}, nil
	}
	if !facts.Authoritative {
		return CaptureAssessment{
			coverage: CaptureCoveragePreview, policy: facts.Policy,
			partial: true, failure: ContentCaptureStrictRefused, retained: counts,
		}, nil
	}
	// SourceOmitted alone never grants full coverage. An omission without a
	// placeholder is unaccounted and stays preview.
	if facts.SourceOmitted && !hasOmissions {
		return CaptureAssessment{
			coverage: CaptureCoveragePreview, policy: facts.Policy,
			partial: true, failure: ContentCaptureSourceRecordsOmitted, retained: counts,
		}, nil
	}

	switch {
	case hasUnknown && hasOmissions:
		// Both facts checked; full only because every gap is accounted.
		// Retain both internally with all counts; the stored code stays the
		// existing omission code. Never overwrite one reason to erase the other.
		return CaptureAssessment{
			coverage: CaptureCoverageFull, policy: facts.Policy,
			partial: true, failure: ContentCaptureSourceRecordsOmitted, retained: counts,
		}, nil
	case hasUnknown:
		return CaptureAssessment{
			coverage: CaptureCoverageFull, policy: facts.Policy,
			partial: true, failure: ContentCaptureUnknownDataRetained, retained: counts,
		}, nil
	case hasOmissions:
		return CaptureAssessment{
			coverage: CaptureCoverageFull, policy: facts.Policy,
			partial: true, failure: ContentCaptureSourceRecordsOmitted, retained: counts,
		}, nil
	default:
		return CaptureAssessment{
			coverage: CaptureCoverageFull, policy: facts.Policy,
			partial: false, failure: ContentCaptureNoFailure, retained: counts,
		}, nil
	}
}

func assessV2Capture(facts CaptureFacts, v2 indexformat.V2) (CaptureAssessment, error) {
	if err := v2.Generation.Validate(); err != nil {
		return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: invalid managed generation for harness %q: %w; no capture was certified", string(facts.Harness), err)
	}
	// Select Main + all selected Earlier entries as one set. Validate owned
	// evidence syntax, payload integrity, harness ownership, coordinate
	// presence, order, and overlaps for the entire set, regardless of desired
	// coverage. Evidence validates raw at rest; there is no redaction step.
	selected := append([]schema.SessionEntry(nil), v2.Generation.Main.Entries...)
	for _, earlier := range v2.Generation.Earlier {
		selected = append(selected, earlier.Content.Entries...)
	}
	collectedErr := validateV2Evidence(selected, facts.Harness)
	if collectedErr != nil {
		if errors.Is(collectedErr, ErrUnknownPositionUnavailable) {
			if facts.Policy == CaptureLegacyPreview && legacyCoordinatesWhollyAbsent(selected) {
				if err := validateV1LegacyEvidence(selected, facts.Harness); err != nil {
					return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: legacy evidence invalid for harness %q: %w; no capture was certified", string(facts.Harness), err)
				}
				legacyRetained, legacyErr := retainedUnknownEntries(selected)
				if legacyErr != nil {
					return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: legacy evidence invalid for harness %q: %w; no capture was certified", string(facts.Harness), legacyErr)
				}
				return CaptureAssessment{
					coverage: CaptureCoveragePreview, policy: facts.Policy,
					partial: true, failure: ContentCaptureLegacyPreviewOnly,
					retained: retainedUnknownCounts(legacyRetained),
				}, nil
			}
			return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: %w; no capture was certified; re-index the original source with a position-aware adapter, then retry", collectedErr)
		}
		return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: invalid retained evidence for harness %q: %w; no capture was certified", string(facts.Harness), collectedErr)
	}
	retained, err := retainedUnknownEntries(selected)
	if err != nil {
		return CaptureAssessment{}, fmt.Errorf("ingest.AssessCapture: invalid retained evidence for harness %q: %w; no capture was certified", string(facts.Harness), err)
	}
	counts := retainedUnknownCounts(retained)
	hasUnknown := len(retained) > 0
	hasOmissions := outputRecordsItsOmissions(facts.Result)
	// V2 completeness is read directly from the result. Incomplete_new uses an
	// honest preview regardless of carrier count and never invents a
	// publication agreement. Carrier-independent: native completeness never
	// depends on retained carrier count.
	if v2.Generation.Completeness == indexformat.GenerationCompletenessIncompleteNew {
		return CaptureAssessment{
			coverage: CaptureCoveragePreview, policy: facts.Policy,
			partial: true, failure: ContentCaptureNoFailure,
		}, nil
	}
	if facts.Policy == CaptureLegacyPreview {
		return CaptureAssessment{
			coverage: CaptureCoveragePreview, policy: facts.Policy,
			partial: true, failure: ContentCaptureLegacyPreviewOnly, retained: counts,
		}, nil
	}
	if facts.Unaccounted || !facts.Authoritative {
		return CaptureAssessment{
			coverage: CaptureCoveragePreview, policy: facts.Policy,
			partial: true, failure: ContentCaptureStrictRefused, retained: counts,
		}, nil
	}
	if facts.SourceOmitted && !hasOmissions {
		return CaptureAssessment{
			coverage: CaptureCoveragePreview, policy: facts.Policy,
			partial: true, failure: ContentCaptureSourceRecordsOmitted, retained: counts,
		}, nil
	}
	switch {
	case hasUnknown && hasOmissions:
		return CaptureAssessment{
			coverage: CaptureCoverageFull, policy: facts.Policy,
			partial: true, failure: ContentCaptureSourceRecordsOmitted, retained: counts,
		}, nil
	case hasUnknown:
		return CaptureAssessment{
			coverage: CaptureCoverageFull, policy: facts.Policy,
			partial: true, failure: ContentCaptureUnknownDataRetained, retained: counts,
		}, nil
	case hasOmissions:
		return CaptureAssessment{
			coverage: CaptureCoverageFull, policy: facts.Policy,
			partial: true, failure: ContentCaptureSourceRecordsOmitted, retained: counts,
		}, nil
	default:
		return CaptureAssessment{
			coverage: CaptureCoverageFull, policy: facts.Policy,
			partial: false, failure: ContentCaptureNoFailure, retained: counts,
		}, nil
	}
}

func validateV2Evidence(entries []schema.SessionEntry, harness Harness) error {
	_, err := CollectRetainedUnknown(entries, harness)
	return err
}

func validateV1Evidence(entries []schema.SessionEntry, harness Harness) error {
	_, err := CollectRetainedUnknown(entries, harness)
	return err
}

// legacyCoordinatesWhollyAbsent reports whether every retained record lacks
// public traversal coordinates. It is the narrow legacy exception: wholly
// absent coordinates may still read as a bounded preview, but any present
// coordinate set must validate as a whole.
func legacyCoordinatesWhollyAbsent(entries []schema.SessionEntry) bool {
	found := false
	for _, entry := range entries {
		records, err := RetainedUnknownOf(entry)
		if err != nil || len(records) == 0 {
			continue
		}
		for _, record := range records {
			found = true
			if record.Position.Public != nil {
				return false
			}
		}
	}
	return found
}

func validateV1LegacyEvidence(entries []schema.SessionEntry, harness Harness) error {
	for _, entry := range entries {
		records, err := RetainedUnknownOf(entry)
		if err != nil {
			return err
		}
		for _, record := range records {
			if (harness != "" && record.Harness != harness) || (entry.Harness != "" && record.Harness != entry.Harness) {
				return fmt.Errorf("stored harness disagrees with its owner; no detail was emitted; re-index the original source")
			}
			if record.Position.Public != nil {
				return fmt.Errorf("legacy evidence carries public coordinates; no legacy preview was certified; re-index the source with a position-aware adapter")
			}
		}
	}
	return nil
}
