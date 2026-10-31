package ingest

import "fmt"

// AuditReason is the ingest-owned, payload-free vocabulary of existing worker
// outcomes and diagnostic ErrorType spellings. It does not change wire diagnostics.
type AuditReason string

const (
	AuditReasonError                     AuditReason = "error"
	AuditReasonAdapterRefreshUnavailable AuditReason = "adapter_refresh_unavailable"
)

// NewAuditReason validates without trimming or echoing untrusted input: rejected
// values may themselves be private messages, transcript text, or paths.
func NewAuditReason(raw string) (AuditReason, error) {
	switch raw {
	case string(AuditReasonError), string(AuditReasonAdapterRefreshUnavailable),
		"artifact_cleanup", "artifact_mirror_pending", "artifact_relocation",
		"codex_checkpoint_metadata_misaligned", "codex_copied_prefix_incomplete",
		"codex_copy_boundary_malformed", "codex_history_envelope_malformed",
		"codex_history_mode_unsupported", "codex_item_body_malformed",
		"codex_item_variant_unknown", "codex_lifecycle_unbalanced",
		"codex_native_pointer_identity_mismatch", "codex_native_pointer_unreadable",
		"codex_ordinal_regression", "codex_paginated_rollback_anomaly",
		"codex_record_malformed", "codex_reference_ambiguous",
		"codex_reference_bounds_invalid", "codex_reference_byte_boundary_invalid",
		"codex_reference_coverage_incomplete", "codex_reference_cycle",
		"codex_reference_depth_exceeded", "codex_reference_mode_unsupported",
		"codex_reference_segments_bounded", "codex_reference_unavailable",
		"codex_reference_unreadable", "codex_rollback_beyond_history", "codex_turn_aborted",
		"content_capture_incomplete", "content_recovery_refused", "content_recovery_unavailable",
		"copy_failed", "derive_identity_error", "dry_run_unindexed", "duplicate_session_id", "event_data_parse_error",
		"git_failure", "git_timeout", "index_coverage_unavailable", "index_empty_unverified",
		"index_refused", "invalid_model", "invalid_parent_cycle", "invalid_parent_session_id",
		"metadata_refused", "metrics_incomplete", "missing_model", "missing_project",
		"missing_session_meta", "missing_user_email", "native_cursor_unavailable",
		"native_discovery_unavailable", "native_refresh_unavailable", "pair_repair_unavailable",
		"parent_session_mismatch", "parse_error", "read_error", "sidecar_session_mismatch",
		"stale_index_missing_input", "stale_index_outside_selection",
		"sidecar_parse_error", "missing_sidecar", "transcript_read_failed", "transcript_read_timeout",
		"incomplete_tail", "inactive_dangling_parent", "oversize_write_advisory", "stats-overflow",
		string(OpenCodeGraphMissingParent), string(OpenCodeGraphOrphanPartDropped),
		string(OpenCodeUnknownPartType), string(ContentCaptureUnknownDataRetained), OversizedRecordDiagnosticType:
		return AuditReason(raw), nil
	default:
		return "", fmt.Errorf("ingest: validate audit reason during run outcome persistence: unknown producer code; no detail may be recorded with this value; use an existing ingest diagnostic ErrorType or worker outcome spelling")
	}
}
