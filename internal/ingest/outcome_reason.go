package ingest

import "fmt"

// OutcomeKind is the closed set of run-outcome kinds. Its values are the ids
// of the ingest_outcome_kinds rows.
type OutcomeKind uint8

const (
	OutcomeKindWorkerError OutcomeKind = 1
	OutcomeKindDiagnostic  OutcomeKind = 2
	OutcomeKindRefresh     OutcomeKind = 3
)

// AllOutcomeKinds lists every kind in id order.
var AllOutcomeKinds = []OutcomeKind{OutcomeKindWorkerError, OutcomeKindDiagnostic, OutcomeKindRefresh}

// String returns the stored name of the kind.
func (k OutcomeKind) String() string {
	switch k {
	case OutcomeKindWorkerError:
		return "worker_error"
	case OutcomeKindDiagnostic:
		return "diagnostic"
	case OutcomeKindRefresh:
		return "refresh"
	default:
		return "unknown"
	}
}

// NewOutcomeKind validates a stored kind name.
func NewOutcomeKind(raw string) (OutcomeKind, error) {
	for _, kind := range AllOutcomeKinds {
		if kind.String() == raw {
			return kind, nil
		}
	}
	return 0, fmt.Errorf("ingest: read run outcome kind %q during audit: this build defines no such kind; no outcome was recorded; use worker_error, diagnostic or refresh", raw)
}

// OutcomeReasonLocalIDBase is the first id a store may assign on its own to a
// stored code no build lists. Ids below it are fixed by the constants in this
// file and equal in every store.
const OutcomeReasonLocalIDBase int64 = 1000000

// OutcomeReason is the payload-free vocabulary of run outcomes: worker
// failures, diagnostic ErrorType spellings, and refresh reasons. Its value is
// the reason's stable id in ingest_outcome_reasons. A code names exactly one
// reason and one kind. The zero value is not a reason.
type OutcomeReason int64

type outcomeReasonDef struct {
	reason  OutcomeReason
	kind    OutcomeKind
	code    string
	retired bool
}

const (
	OutcomeReasonError                              OutcomeReason = 1   // worker_error "error"
	OutcomeReasonAdapterCaptureFailed               OutcomeReason = 2   // worker_error "adapter_capture_failed"
	OutcomeReasonAdapterRefreshUnavailable          OutcomeReason = 100 // diagnostic "adapter_refresh_unavailable"
	OutcomeReasonArtifactCleanup                    OutcomeReason = 101 // diagnostic "artifact_cleanup"
	OutcomeReasonArtifactMirrorPending              OutcomeReason = 102 // diagnostic "artifact_mirror_pending"
	OutcomeReasonArtifactRelocation                 OutcomeReason = 103 // diagnostic "artifact_relocation"
	OutcomeReasonCodexCheckpointMetadataMisaligned  OutcomeReason = 104 // diagnostic "codex_checkpoint_metadata_misaligned"
	OutcomeReasonCodexCopiedPrefixIncomplete        OutcomeReason = 105 // diagnostic "codex_copied_prefix_incomplete"
	OutcomeReasonCodexCopyBoundaryMalformed         OutcomeReason = 106 // diagnostic "codex_copy_boundary_malformed"
	OutcomeReasonCodexHistoryEnvelopeMalformed      OutcomeReason = 107 // diagnostic "codex_history_envelope_malformed"
	OutcomeReasonCodexHistoryModeUnsupported        OutcomeReason = 108 // diagnostic "codex_history_mode_unsupported"
	OutcomeReasonCodexItemBodyMalformed             OutcomeReason = 109 // diagnostic "codex_item_body_malformed"
	OutcomeReasonCodexItemVariantUnknown            OutcomeReason = 110 // diagnostic "codex_item_variant_unknown"
	OutcomeReasonCodexLifecycleUnbalanced           OutcomeReason = 111 // diagnostic "codex_lifecycle_unbalanced"
	OutcomeReasonCodexNativePointerIdentityMismatch OutcomeReason = 112 // diagnostic "codex_native_pointer_identity_mismatch"
	OutcomeReasonCodexNativePointerUnreadable       OutcomeReason = 113 // diagnostic "codex_native_pointer_unreadable"
	OutcomeReasonCodexOrdinalRegression             OutcomeReason = 114 // diagnostic "codex_ordinal_regression"
	OutcomeReasonCodexPaginatedRollbackAnomaly      OutcomeReason = 115 // diagnostic "codex_paginated_rollback_anomaly"
	OutcomeReasonCodexRecordMalformed               OutcomeReason = 116 // diagnostic "codex_record_malformed"
	OutcomeReasonCodexReferenceAmbiguous            OutcomeReason = 117 // diagnostic "codex_reference_ambiguous"
	OutcomeReasonCodexReferenceBoundsInvalid        OutcomeReason = 118 // diagnostic "codex_reference_bounds_invalid"
	OutcomeReasonCodexReferenceByteBoundaryInvalid  OutcomeReason = 119 // diagnostic "codex_reference_byte_boundary_invalid"
	OutcomeReasonCodexReferenceCoverageIncomplete   OutcomeReason = 120 // diagnostic "codex_reference_coverage_incomplete"
	OutcomeReasonCodexReferenceCycle                OutcomeReason = 121 // diagnostic "codex_reference_cycle"
	OutcomeReasonCodexReferenceDepthExceeded        OutcomeReason = 122 // diagnostic "codex_reference_depth_exceeded"
	OutcomeReasonCodexReferenceModeUnsupported      OutcomeReason = 123 // diagnostic "codex_reference_mode_unsupported"
	OutcomeReasonCodexReferenceSegmentsBounded      OutcomeReason = 124 // diagnostic "codex_reference_segments_bounded"
	OutcomeReasonCodexReferenceUnavailable          OutcomeReason = 125 // diagnostic "codex_reference_unavailable"
	OutcomeReasonCodexReferenceUnreadable           OutcomeReason = 126 // diagnostic "codex_reference_unreadable"
	OutcomeReasonCodexRollbackBeyondHistory         OutcomeReason = 127 // diagnostic "codex_rollback_beyond_history"
	OutcomeReasonCodexTurnAborted                   OutcomeReason = 128 // diagnostic "codex_turn_aborted"
	OutcomeReasonContentCaptureIncomplete           OutcomeReason = 129 // diagnostic "content_capture_incomplete"
	OutcomeReasonContentRecoveryRefused             OutcomeReason = 130 // diagnostic "content_recovery_refused"
	OutcomeReasonContentRecoveryUnavailable         OutcomeReason = 131 // diagnostic "content_recovery_unavailable"
	OutcomeReasonCopyFailed                         OutcomeReason = 132 // diagnostic "copy_failed"
	OutcomeReasonDeriveIdentityError                OutcomeReason = 133 // diagnostic "derive_identity_error"
	OutcomeReasonDryRunUnindexed                    OutcomeReason = 134 // diagnostic "dry_run_unindexed"
	OutcomeReasonDuplicateSessionId                 OutcomeReason = 135 // diagnostic "duplicate_session_id"
	OutcomeReasonEventDataParseError                OutcomeReason = 136 // diagnostic "event_data_parse_error"
	OutcomeReasonGitFailure                         OutcomeReason = 137 // diagnostic "git_failure"
	OutcomeReasonGitTimeout                         OutcomeReason = 138 // diagnostic "git_timeout"
	OutcomeReasonInactiveDanglingParent             OutcomeReason = 139 // diagnostic "inactive_dangling_parent"
	OutcomeReasonIncompleteTail                     OutcomeReason = 140 // diagnostic "incomplete_tail"
	OutcomeReasonIndexCoverageUnavailable           OutcomeReason = 141 // diagnostic "index_coverage_unavailable"
	OutcomeReasonIndexEmptyUnverified               OutcomeReason = 142 // diagnostic "index_empty_unverified"
	OutcomeReasonIndexRefused                       OutcomeReason = 143 // diagnostic "index_refused"
	OutcomeReasonInvalidModel                       OutcomeReason = 144 // diagnostic "invalid_model"
	OutcomeReasonInvalidParentCycle                 OutcomeReason = 145 // diagnostic "invalid_parent_cycle"
	OutcomeReasonInvalidParentSessionId             OutcomeReason = 146 // diagnostic "invalid_parent_session_id"
	OutcomeReasonMetadataRefused                    OutcomeReason = 147 // diagnostic "metadata_refused"
	OutcomeReasonMetricsIncomplete                  OutcomeReason = 148 // diagnostic "metrics_incomplete"
	OutcomeReasonMissingModel                       OutcomeReason = 149 // diagnostic "missing_model"
	OutcomeReasonMissingProject                     OutcomeReason = 150 // diagnostic "missing_project"
	OutcomeReasonMissingSessionMeta                 OutcomeReason = 151 // diagnostic "missing_session_meta"
	OutcomeReasonMissingSidecar                     OutcomeReason = 152 // diagnostic "missing_sidecar"
	OutcomeReasonMissingUserEmail                   OutcomeReason = 153 // diagnostic "missing_user_email"
	OutcomeReasonNativeCursorUnavailable            OutcomeReason = 154 // diagnostic "native_cursor_unavailable"
	OutcomeReasonNativeDiscoveryUnavailable         OutcomeReason = 155 // diagnostic "native_discovery_unavailable"
	OutcomeReasonNativeRefreshUnavailable           OutcomeReason = 156 // diagnostic "native_refresh_unavailable"
	OutcomeReasonOpencodeMissingParent              OutcomeReason = 157 // diagnostic "opencode_missing_parent"
	OutcomeReasonOpencodeOrphanPartDropped          OutcomeReason = 158 // diagnostic "opencode_orphan_part_dropped"
	OutcomeReasonOpencodeUnknownPartType            OutcomeReason = 159 // diagnostic "opencode_unknown_part_type"
	OutcomeReasonOversizeWriteAdvisory              OutcomeReason = 160 // diagnostic "oversize_write_advisory"
	OutcomeReasonPairRepairUnavailable              OutcomeReason = 161 // diagnostic "pair_repair_unavailable"
	OutcomeReasonParentSessionMismatch              OutcomeReason = 162 // diagnostic "parent_session_mismatch"
	OutcomeReasonParseError                         OutcomeReason = 163 // diagnostic "parse_error"
	OutcomeReasonReadError                          OutcomeReason = 164 // diagnostic "read_error"
	OutcomeReasonRecordTooLarge                     OutcomeReason = 165 // diagnostic "record_too_large"
	OutcomeReasonSidecarParseError                  OutcomeReason = 166 // diagnostic "sidecar_parse_error"
	OutcomeReasonSidecarSessionMismatch             OutcomeReason = 167 // diagnostic "sidecar_session_mismatch"
	OutcomeReasonStaleIndexMissingInput             OutcomeReason = 168 // diagnostic "stale_index_missing_input"
	OutcomeReasonStaleIndexOutsideSelection         OutcomeReason = 169 // diagnostic "stale_index_outside_selection"
	OutcomeReasonStatsOverflow                      OutcomeReason = 170 // diagnostic "stats-overflow"
	OutcomeReasonTranscriptReadFailed               OutcomeReason = 171 // diagnostic "transcript_read_failed"
	OutcomeReasonTranscriptReadTimeout              OutcomeReason = 172 // diagnostic "transcript_read_timeout"
	OutcomeReasonUnknownDataRetained                OutcomeReason = 173 // diagnostic "unknown_data_retained"
	OutcomeReasonRefreshNew                         OutcomeReason = 300 // refresh "new"
	OutcomeReasonRefreshSourceChanged               OutcomeReason = 301 // refresh "source_changed"
	OutcomeReasonRefreshPairMissing                 OutcomeReason = 302 // refresh "pair_missing"
	OutcomeReasonRefreshMetadataSchema              OutcomeReason = 303 // refresh "metadata_schema"
	OutcomeReasonRefreshForced                      OutcomeReason = 304 // refresh "forced"
	OutcomeReasonRefreshAdapterBehind               OutcomeReason = 305 // refresh "adapter_behind"
	OutcomeReasonRefreshAdapterBehindNativeOnly     OutcomeReason = 306 // refresh "adapter_behind_native_only"
	OutcomeReasonRefreshAdapterStampAbsent          OutcomeReason = 307 // refresh "adapter_stamp_absent"
	OutcomeReasonRefreshIndexerBehind               OutcomeReason = 308 // refresh "indexer_behind"
	OutcomeReasonRefreshRefusalReopened             OutcomeReason = 309 // refresh "refusal_reopened"
	OutcomeReasonRefreshPublicationUnbound          OutcomeReason = 310 // refresh "publication_unbound"
)

// outcomeReasonDefs is the one definition of every reason: id, kind, code and
// retirement. Ids are never reused; a code that goes out of use sets retired
// and keeps its row.
var outcomeReasonDefs = [...]outcomeReasonDef{
	{OutcomeReasonError, OutcomeKindWorkerError, "error", false},
	{OutcomeReasonAdapterCaptureFailed, OutcomeKindWorkerError, "adapter_capture_failed", false},
	{OutcomeReasonAdapterRefreshUnavailable, OutcomeKindDiagnostic, "adapter_refresh_unavailable", false},
	{OutcomeReasonArtifactCleanup, OutcomeKindDiagnostic, "artifact_cleanup", false},
	{OutcomeReasonArtifactMirrorPending, OutcomeKindDiagnostic, "artifact_mirror_pending", false},
	{OutcomeReasonArtifactRelocation, OutcomeKindDiagnostic, "artifact_relocation", false},
	{OutcomeReasonCodexCheckpointMetadataMisaligned, OutcomeKindDiagnostic, "codex_checkpoint_metadata_misaligned", false},
	{OutcomeReasonCodexCopiedPrefixIncomplete, OutcomeKindDiagnostic, "codex_copied_prefix_incomplete", false},
	{OutcomeReasonCodexCopyBoundaryMalformed, OutcomeKindDiagnostic, "codex_copy_boundary_malformed", false},
	{OutcomeReasonCodexHistoryEnvelopeMalformed, OutcomeKindDiagnostic, "codex_history_envelope_malformed", false},
	{OutcomeReasonCodexHistoryModeUnsupported, OutcomeKindDiagnostic, "codex_history_mode_unsupported", false},
	{OutcomeReasonCodexItemBodyMalformed, OutcomeKindDiagnostic, "codex_item_body_malformed", false},
	{OutcomeReasonCodexItemVariantUnknown, OutcomeKindDiagnostic, "codex_item_variant_unknown", false},
	{OutcomeReasonCodexLifecycleUnbalanced, OutcomeKindDiagnostic, "codex_lifecycle_unbalanced", false},
	{OutcomeReasonCodexNativePointerIdentityMismatch, OutcomeKindDiagnostic, "codex_native_pointer_identity_mismatch", false},
	{OutcomeReasonCodexNativePointerUnreadable, OutcomeKindDiagnostic, "codex_native_pointer_unreadable", false},
	{OutcomeReasonCodexOrdinalRegression, OutcomeKindDiagnostic, "codex_ordinal_regression", false},
	{OutcomeReasonCodexPaginatedRollbackAnomaly, OutcomeKindDiagnostic, "codex_paginated_rollback_anomaly", false},
	{OutcomeReasonCodexRecordMalformed, OutcomeKindDiagnostic, "codex_record_malformed", false},
	{OutcomeReasonCodexReferenceAmbiguous, OutcomeKindDiagnostic, "codex_reference_ambiguous", false},
	{OutcomeReasonCodexReferenceBoundsInvalid, OutcomeKindDiagnostic, "codex_reference_bounds_invalid", false},
	{OutcomeReasonCodexReferenceByteBoundaryInvalid, OutcomeKindDiagnostic, "codex_reference_byte_boundary_invalid", false},
	{OutcomeReasonCodexReferenceCoverageIncomplete, OutcomeKindDiagnostic, "codex_reference_coverage_incomplete", false},
	{OutcomeReasonCodexReferenceCycle, OutcomeKindDiagnostic, "codex_reference_cycle", false},
	{OutcomeReasonCodexReferenceDepthExceeded, OutcomeKindDiagnostic, "codex_reference_depth_exceeded", false},
	{OutcomeReasonCodexReferenceModeUnsupported, OutcomeKindDiagnostic, "codex_reference_mode_unsupported", false},
	{OutcomeReasonCodexReferenceSegmentsBounded, OutcomeKindDiagnostic, "codex_reference_segments_bounded", false},
	{OutcomeReasonCodexReferenceUnavailable, OutcomeKindDiagnostic, "codex_reference_unavailable", false},
	{OutcomeReasonCodexReferenceUnreadable, OutcomeKindDiagnostic, "codex_reference_unreadable", false},
	{OutcomeReasonCodexRollbackBeyondHistory, OutcomeKindDiagnostic, "codex_rollback_beyond_history", false},
	{OutcomeReasonCodexTurnAborted, OutcomeKindDiagnostic, "codex_turn_aborted", false},
	{OutcomeReasonContentCaptureIncomplete, OutcomeKindDiagnostic, "content_capture_incomplete", false},
	{OutcomeReasonContentRecoveryRefused, OutcomeKindDiagnostic, "content_recovery_refused", false},
	{OutcomeReasonContentRecoveryUnavailable, OutcomeKindDiagnostic, "content_recovery_unavailable", false},
	{OutcomeReasonCopyFailed, OutcomeKindDiagnostic, "copy_failed", false},
	{OutcomeReasonDeriveIdentityError, OutcomeKindDiagnostic, "derive_identity_error", false},
	{OutcomeReasonDryRunUnindexed, OutcomeKindDiagnostic, "dry_run_unindexed", false},
	{OutcomeReasonDuplicateSessionId, OutcomeKindDiagnostic, "duplicate_session_id", false},
	{OutcomeReasonEventDataParseError, OutcomeKindDiagnostic, "event_data_parse_error", false},
	{OutcomeReasonGitFailure, OutcomeKindDiagnostic, "git_failure", false},
	{OutcomeReasonGitTimeout, OutcomeKindDiagnostic, "git_timeout", false},
	{OutcomeReasonInactiveDanglingParent, OutcomeKindDiagnostic, "inactive_dangling_parent", false},
	{OutcomeReasonIncompleteTail, OutcomeKindDiagnostic, "incomplete_tail", false},
	{OutcomeReasonIndexCoverageUnavailable, OutcomeKindDiagnostic, "index_coverage_unavailable", false},
	{OutcomeReasonIndexEmptyUnverified, OutcomeKindDiagnostic, "index_empty_unverified", false},
	{OutcomeReasonIndexRefused, OutcomeKindDiagnostic, "index_refused", false},
	{OutcomeReasonInvalidModel, OutcomeKindDiagnostic, "invalid_model", false},
	{OutcomeReasonInvalidParentCycle, OutcomeKindDiagnostic, "invalid_parent_cycle", false},
	{OutcomeReasonInvalidParentSessionId, OutcomeKindDiagnostic, "invalid_parent_session_id", false},
	{OutcomeReasonMetadataRefused, OutcomeKindDiagnostic, "metadata_refused", false},
	{OutcomeReasonMetricsIncomplete, OutcomeKindDiagnostic, "metrics_incomplete", false},
	{OutcomeReasonMissingModel, OutcomeKindDiagnostic, "missing_model", false},
	{OutcomeReasonMissingProject, OutcomeKindDiagnostic, "missing_project", false},
	{OutcomeReasonMissingSessionMeta, OutcomeKindDiagnostic, "missing_session_meta", false},
	{OutcomeReasonMissingSidecar, OutcomeKindDiagnostic, "missing_sidecar", false},
	{OutcomeReasonMissingUserEmail, OutcomeKindDiagnostic, "missing_user_email", false},
	{OutcomeReasonNativeCursorUnavailable, OutcomeKindDiagnostic, "native_cursor_unavailable", false},
	{OutcomeReasonNativeDiscoveryUnavailable, OutcomeKindDiagnostic, "native_discovery_unavailable", false},
	{OutcomeReasonNativeRefreshUnavailable, OutcomeKindDiagnostic, "native_refresh_unavailable", false},
	{OutcomeReasonOpencodeMissingParent, OutcomeKindDiagnostic, "opencode_missing_parent", false},
	{OutcomeReasonOpencodeOrphanPartDropped, OutcomeKindDiagnostic, "opencode_orphan_part_dropped", false},
	{OutcomeReasonOpencodeUnknownPartType, OutcomeKindDiagnostic, "opencode_unknown_part_type", false},
	{OutcomeReasonOversizeWriteAdvisory, OutcomeKindDiagnostic, "oversize_write_advisory", false},
	{OutcomeReasonPairRepairUnavailable, OutcomeKindDiagnostic, "pair_repair_unavailable", false},
	{OutcomeReasonParentSessionMismatch, OutcomeKindDiagnostic, "parent_session_mismatch", false},
	{OutcomeReasonParseError, OutcomeKindDiagnostic, "parse_error", false},
	{OutcomeReasonReadError, OutcomeKindDiagnostic, "read_error", false},
	{OutcomeReasonRecordTooLarge, OutcomeKindDiagnostic, "record_too_large", false},
	{OutcomeReasonSidecarParseError, OutcomeKindDiagnostic, "sidecar_parse_error", false},
	{OutcomeReasonSidecarSessionMismatch, OutcomeKindDiagnostic, "sidecar_session_mismatch", false},
	{OutcomeReasonStaleIndexMissingInput, OutcomeKindDiagnostic, "stale_index_missing_input", false},
	{OutcomeReasonStaleIndexOutsideSelection, OutcomeKindDiagnostic, "stale_index_outside_selection", false},
	{OutcomeReasonStatsOverflow, OutcomeKindDiagnostic, "stats-overflow", false},
	{OutcomeReasonTranscriptReadFailed, OutcomeKindDiagnostic, "transcript_read_failed", false},
	{OutcomeReasonTranscriptReadTimeout, OutcomeKindDiagnostic, "transcript_read_timeout", false},
	{OutcomeReasonUnknownDataRetained, OutcomeKindDiagnostic, "unknown_data_retained", false},
	{OutcomeReasonRefreshNew, OutcomeKindRefresh, "new", false},
	{OutcomeReasonRefreshSourceChanged, OutcomeKindRefresh, "source_changed", false},
	{OutcomeReasonRefreshPairMissing, OutcomeKindRefresh, "pair_missing", false},
	{OutcomeReasonRefreshMetadataSchema, OutcomeKindRefresh, "metadata_schema", false},
	{OutcomeReasonRefreshForced, OutcomeKindRefresh, "forced", false},
	{OutcomeReasonRefreshAdapterBehind, OutcomeKindRefresh, "adapter_behind", false},
	{OutcomeReasonRefreshAdapterBehindNativeOnly, OutcomeKindRefresh, "adapter_behind_native_only", false},
	{OutcomeReasonRefreshAdapterStampAbsent, OutcomeKindRefresh, "adapter_stamp_absent", false},
	{OutcomeReasonRefreshIndexerBehind, OutcomeKindRefresh, "indexer_behind", false},
	{OutcomeReasonRefreshRefusalReopened, OutcomeKindRefresh, "refusal_reopened", false},
	{OutcomeReasonRefreshPublicationUnbound, OutcomeKindRefresh, "publication_unbound", false},
}

// AllOutcomeReasons lists every reason this build defines, retired ones
// included, in id order.
var AllOutcomeReasons = func() []OutcomeReason {
	all := make([]OutcomeReason, 0, len(outcomeReasonDefs))
	for _, def := range outcomeReasonDefs {
		all = append(all, def.reason)
	}
	return all
}()

var (
	outcomeReasonByID = func() map[OutcomeReason]outcomeReasonDef {
		byID := make(map[OutcomeReason]outcomeReasonDef, len(outcomeReasonDefs))
		for _, def := range outcomeReasonDefs {
			byID[def.reason] = def
		}
		return byID
	}()
	outcomeReasonByCode = func() map[string]OutcomeReason {
		byCode := make(map[string]OutcomeReason, len(outcomeReasonDefs))
		for _, def := range outcomeReasonDefs {
			byCode[def.code] = def.reason
		}
		return byCode
	}()
)

// Kind returns the kind the reason belongs to, or 0 for an undefined reason.
func (r OutcomeReason) Kind() OutcomeKind { return outcomeReasonByID[r].kind }

// Code returns the stored spelling of the reason, or "" for an undefined one.
func (r OutcomeReason) Code() string { return outcomeReasonByID[r].code }

// Retired reports whether the code is no longer written by this build.
func (r OutcomeReason) Retired() bool { return outcomeReasonByID[r].retired }

// Defined reports whether this build defines the reason.
func (r OutcomeReason) Defined() bool {
	_, ok := outcomeReasonByID[r]
	return ok
}

// NewOutcomeReason validates a code without trimming or echoing it: a rejected
// value may itself be a private message, transcript text, or a path.
func NewOutcomeReason(code string) (OutcomeReason, error) {
	if reason, ok := outcomeReasonByCode[code]; ok {
		return reason, nil
	}
	return 0, fmt.Errorf("ingest: validate outcome reason during run outcome persistence: this build defines no such code; no detail may be recorded with this value; use an existing ingest diagnostic ErrorType, worker outcome or refresh reason spelling")
}

// OutcomeReasonFromID is the read boundary for a stored reason id. It refuses
// an id this build does not define.
func OutcomeReasonFromID(id int64) (OutcomeReason, error) {
	reason := OutcomeReason(id)
	if !reason.Defined() {
		return 0, fmt.Errorf("ingest: read outcome reason id %d from the run outcome ledger: this build defines no such reason; the row was written by a newer build or seeded by a migration for an unlisted code; upgrade Peasant to read it", id)
	}
	return reason, nil
}
