package store

// migrationV77 stores the import profile and gives run outcomes stable ids.
//
// It adds three nullable timing columns to index_log (NULL means not measured:
// every earlier row, and any attempt that did not parse); creates
// ingest_run_stages, one row per stage per run, a child of ingest_log with the
// same cascade as ingest_run_outcomes; and replaces the kind and reason text of
// ingest_run_outcomes with a reason id.
//
// The two lookup tables define the outcome kinds and reasons. Reason ids below
// 1000000 are fixed by the ingest.OutcomeReason constants and are equal in
// every store; a test asserts exact membership between the constants and the
// seeded rows. A code is unique across every kind. Ids are never reused; a
// code that goes out of use sets retired and keeps its row. A later reason is
// one constant and one migration that inserts its row.
//
// SQLite cannot drop the kind CHECK, so ingest_run_outcomes is rebuilt. Every
// row keeps its id, run_id, session_id and created_at. The reason id is looked
// up by code, with one exception: a worker_error row spelled
// adapter_refresh_unavailable takes the id of adapter_capture_failed, because
// that spelling stays with the diagnostic of the same name. The lookup goes by
// code only, so a row whose stored kind disagrees with its code's seeded kind
// takes the seeded kind. No shipped build wrote such a row: worker_error rows
// carried only error or adapter_refresh_unavailable (mapped above), and
// diagnostic rows carried diagnostic codes, each seeded as a diagnostic. A
// stored code no
// build lists is seeded once as a retired row with a store-local id at or
// above 1000000, under the kind of its lowest row, so the copy cannot fail.
// Every shipped build validated a code against the closed Go set before
// writing it, so that seed should find nothing. The copy takes the rows of the
// newest 100 runs, which is all the retention rule keeps.
//
// stage carries no SQL list: the stage set grows and SQLite cannot alter a
// CHECK. ingest.NewStage validates the name at the write boundary.
const migrationV77 = `
ALTER TABLE index_log ADD COLUMN parse_ms INTEGER CHECK (parse_ms IS NULL OR parse_ms >= 0);
ALTER TABLE index_log ADD COLUMN write_ms INTEGER CHECK (write_ms IS NULL OR write_ms >= 0);
ALTER TABLE index_log ADD COLUMN bytes    INTEGER CHECK (bytes    IS NULL OR bytes    >= 0);

CREATE TABLE ingest_run_stages (
    run_id       INTEGER NOT NULL REFERENCES ingest_log(id) ON DELETE CASCADE,
    stage        TEXT    NOT NULL CHECK (length(stage) > 0),
    duration_ms  INTEGER NOT NULL CHECK (duration_ms  >= 0),
    done         INTEGER NOT NULL DEFAULT 0 CHECK (done         >= 0),
    total        INTEGER NOT NULL DEFAULT 0 CHECK (total        >= 0),
    bytes        INTEGER NOT NULL DEFAULT 0 CHECK (bytes        >= 0),
    lane_wait_ms INTEGER NOT NULL DEFAULT 0 CHECK (lane_wait_ms >= 0),
    PRIMARY KEY (run_id, stage)
) STRICT, WITHOUT ROWID;

CREATE TABLE ingest_outcome_kinds (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;
INSERT INTO ingest_outcome_kinds (id, name) VALUES (1, 'worker_error'), (2, 'diagnostic'), (3, 'refresh');

CREATE TABLE ingest_outcome_reasons (
    id      INTEGER PRIMARY KEY,
    kind_id INTEGER NOT NULL REFERENCES ingest_outcome_kinds(id),
    code    TEXT    NOT NULL UNIQUE,
    retired INTEGER NOT NULL DEFAULT 0 CHECK (retired IN (0,1))
) STRICT;
INSERT INTO ingest_outcome_reasons (id, kind_id, code, retired) VALUES
    (1, 1, 'error', 0),
    (2, 1, 'adapter_capture_failed', 0),
    (100, 2, 'adapter_refresh_unavailable', 0),
    (101, 2, 'artifact_cleanup', 0),
    (102, 2, 'artifact_mirror_pending', 0),
    (103, 2, 'artifact_relocation', 0),
    (104, 2, 'codex_checkpoint_metadata_misaligned', 0),
    (105, 2, 'codex_copied_prefix_incomplete', 0),
    (106, 2, 'codex_copy_boundary_malformed', 0),
    (107, 2, 'codex_history_envelope_malformed', 0),
    (108, 2, 'codex_history_mode_unsupported', 0),
    (109, 2, 'codex_item_body_malformed', 0),
    (110, 2, 'codex_item_variant_unknown', 0),
    (111, 2, 'codex_lifecycle_unbalanced', 0),
    (112, 2, 'codex_native_pointer_identity_mismatch', 0),
    (113, 2, 'codex_native_pointer_unreadable', 0),
    (114, 2, 'codex_ordinal_regression', 0),
    (115, 2, 'codex_paginated_rollback_anomaly', 0),
    (116, 2, 'codex_record_malformed', 0),
    (117, 2, 'codex_reference_ambiguous', 0),
    (118, 2, 'codex_reference_bounds_invalid', 0),
    (119, 2, 'codex_reference_byte_boundary_invalid', 0),
    (120, 2, 'codex_reference_coverage_incomplete', 0),
    (121, 2, 'codex_reference_cycle', 0),
    (122, 2, 'codex_reference_depth_exceeded', 0),
    (123, 2, 'codex_reference_mode_unsupported', 0),
    (124, 2, 'codex_reference_segments_bounded', 0),
    (125, 2, 'codex_reference_unavailable', 0),
    (126, 2, 'codex_reference_unreadable', 0),
    (127, 2, 'codex_rollback_beyond_history', 0),
    (128, 2, 'codex_turn_aborted', 0),
    (129, 2, 'content_capture_incomplete', 0),
    (130, 2, 'content_recovery_refused', 0),
    (131, 2, 'content_recovery_unavailable', 0),
    (132, 2, 'copy_failed', 0),
    (133, 2, 'derive_identity_error', 0),
    (134, 2, 'dry_run_unindexed', 0),
    (135, 2, 'duplicate_session_id', 0),
    (136, 2, 'event_data_parse_error', 0),
    (137, 2, 'git_failure', 0),
    (138, 2, 'git_timeout', 0),
    (139, 2, 'inactive_dangling_parent', 0),
    (140, 2, 'incomplete_tail', 0),
    (141, 2, 'index_coverage_unavailable', 0),
    (142, 2, 'index_empty_unverified', 0),
    (143, 2, 'index_refused', 0),
    (144, 2, 'invalid_model', 0),
    (145, 2, 'invalid_parent_cycle', 0),
    (146, 2, 'invalid_parent_session_id', 0),
    (147, 2, 'metadata_refused', 0),
    (148, 2, 'metrics_incomplete', 0),
    (149, 2, 'missing_model', 0),
    (150, 2, 'missing_project', 0),
    (151, 2, 'missing_session_meta', 0),
    (152, 2, 'missing_sidecar', 0),
    (153, 2, 'missing_user_email', 0),
    (154, 2, 'native_cursor_unavailable', 0),
    (155, 2, 'native_discovery_unavailable', 0),
    (156, 2, 'native_refresh_unavailable', 0),
    (157, 2, 'opencode_missing_parent', 0),
    (158, 2, 'opencode_orphan_part_dropped', 0),
    (159, 2, 'opencode_unknown_part_type', 0),
    (160, 2, 'oversize_write_advisory', 0),
    (161, 2, 'pair_repair_unavailable', 0),
    (162, 2, 'parent_session_mismatch', 0),
    (163, 2, 'parse_error', 0),
    (164, 2, 'read_error', 0),
    (165, 2, 'record_too_large', 0),
    (166, 2, 'sidecar_parse_error', 0),
    (167, 2, 'sidecar_session_mismatch', 0),
    (168, 2, 'stale_index_missing_input', 0),
    (169, 2, 'stale_index_outside_selection', 0),
    (170, 2, 'stats-overflow', 0),
    (171, 2, 'transcript_read_failed', 0),
    (172, 2, 'transcript_read_timeout', 0),
    (173, 2, 'unknown_data_retained', 0),
    (300, 3, 'new', 0),
    (301, 3, 'source_changed', 0),
    (302, 3, 'pair_missing', 0),
    (303, 3, 'metadata_schema', 0),
    (304, 3, 'forced', 0),
    (305, 3, 'adapter_behind', 0),
    (306, 3, 'adapter_behind_native_only', 0),
    (307, 3, 'adapter_stamp_absent', 0),
    (308, 3, 'indexer_behind', 0),
    (309, 3, 'refusal_reopened', 0),
    (310, 3, 'publication_unbound', 0);

INSERT INTO ingest_outcome_reasons (id, kind_id, code, retired)
SELECT 999999 + ROW_NUMBER() OVER (ORDER BY unlisted.reason_code),
       (SELECT k.id FROM ingest_outcome_kinds k WHERE k.name = (
            SELECT lowest.kind FROM ingest_run_outcomes lowest
            WHERE lowest.reason_code = unlisted.reason_code ORDER BY lowest.id LIMIT 1)),
       unlisted.reason_code,
       1
FROM (SELECT DISTINCT reason_code FROM ingest_run_outcomes) unlisted
WHERE unlisted.reason_code NOT IN (SELECT code FROM ingest_outcome_reasons);

CREATE TABLE ingest_run_outcomes_v2 (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     INTEGER NOT NULL REFERENCES ingest_log(id) ON DELETE CASCADE,
    session_id TEXT,
    reason_id  INTEGER NOT NULL REFERENCES ingest_outcome_reasons(id),
    created_at INTEGER NOT NULL
) STRICT;

INSERT INTO ingest_run_outcomes_v2 (id, run_id, session_id, reason_id, created_at)
SELECT o.id, o.run_id, o.session_id,
       (SELECT r.id FROM ingest_outcome_reasons r WHERE r.code = CASE
            WHEN o.kind = 'worker_error' AND o.reason_code = 'adapter_refresh_unavailable' THEN 'adapter_capture_failed'
            ELSE o.reason_code END),
       o.created_at
FROM ingest_run_outcomes o
WHERE o.run_id >= COALESCE((SELECT id FROM ingest_log ORDER BY id DESC LIMIT 1 OFFSET 99), 0)
ORDER BY o.id;

DELETE FROM sqlite_sequence WHERE name = 'ingest_run_outcomes_v2';
INSERT INTO sqlite_sequence (name, seq)
SELECT 'ingest_run_outcomes_v2', seq FROM sqlite_sequence WHERE name = 'ingest_run_outcomes';

DROP TABLE ingest_run_outcomes;
ALTER TABLE ingest_run_outcomes_v2 RENAME TO ingest_run_outcomes;
CREATE INDEX idx_ingest_run_outcomes_run ON ingest_run_outcomes(run_id, id);
`
