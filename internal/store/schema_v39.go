package store

// migrationV39 seeds TWO entry-level annotation types backing the web
// transcript viewer's per-turn labeling modal (the "txn-label-popover
// was changed for no reason"; the restored modal needs REAL persisted data, not
// local-only form state):
//
//   - type: "quality.turn_outcome" — the turn's outcome verdict.
//     value domain: enumerated ["good","neutral","bad"].
//     family: turn_quality (a human quality judgment, mirrors quality.session_outcome
//     at the session grain); scale: ordinal (good > neutral > bad has a direction,
//     mirroring quality.session_outcome's own ordinal assignment).
//   - type: "quality.turn_flag" — an optional friction tag on the turn.
//     value domain: enumerated ["none","error","retry_loop","revert","highlight"].
//     family: turn_quality; scale: nominal (unordered categories, mirrors
//     quality.user_frustration/frustration_signal's nominal assignment).
//
// Both are origin "user" (human-selected via the popover, not an automated
// classifier — mirrors user.custom_label's origin from V36), status "active",
// and registered for entry-level targeting (target_kind "entry") so
// entryApplicableTypes offers them on a turn.
//
// Modelled on migrationV36 (schema_v36.go): INSERT OR IGNORE with a
// randomblob-generated UUID PK and family/lookup resolution by name, so no Go
// builder or deterministic-UUID constant is required.
const migrationV39 = `
-- Seed the "turn outcome" entry annotation type (good/neutral/bad).
INSERT OR IGNORE INTO annotation_types (
    id, type_id, version, display_name, description,
    family_id, value_domain_kind_id, datatype, value_constraint,
    scale_kind_id, lower_is_better, status_id, origin_id, created_at
) VALUES (
    lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(6))),
    'quality.turn_outcome',
    1,
    'Turn outcome',
    'A human verdict on a single turn: good, neutral, or bad. Set from the '
    || 'transcript viewer''s per-turn labeling modal.',
    (SELECT id FROM annotation_families WHERE family = 'turn_quality'),
    (SELECT id FROM value_domain_kinds WHERE name = 'enumerated'),
    'text',
    '["good","neutral","bad"]',
    (SELECT id FROM scale_kinds WHERE name = 'ordinal'),
    NULL,  -- lower_is_better not applicable (categorical verdict, not a score)
    (SELECT id FROM annotation_statuses WHERE name = 'active'),
    (SELECT id FROM type_origins WHERE name = 'user'),
    strftime('%s', 'now') * 1000
);

-- Seed the "turn flag" entry annotation type (an optional friction tag).
INSERT OR IGNORE INTO annotation_types (
    id, type_id, version, display_name, description,
    family_id, value_domain_kind_id, datatype, value_constraint,
    scale_kind_id, lower_is_better, status_id, origin_id, created_at
) VALUES (
    lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(6))),
    'quality.turn_flag',
    1,
    'Turn flag',
    'An optional friction tag on a single turn: none, error, retry_loop, revert, '
    || 'or highlight. Set from the transcript viewer''s per-turn labeling modal.',
    (SELECT id FROM annotation_families WHERE family = 'turn_quality'),
    (SELECT id FROM value_domain_kinds WHERE name = 'enumerated'),
    'text',
    '["none","error","retry_loop","revert","highlight"]',
    (SELECT id FROM scale_kinds WHERE name = 'nominal'),
    NULL,  -- lower_is_better not applicable
    (SELECT id FROM annotation_statuses WHERE name = 'active'),
    (SELECT id FROM type_origins WHERE name = 'user'),
    strftime('%s', 'now') * 1000
);

-- Allow entry-level targeting (target_kind 2) for both new types.
INSERT OR IGNORE INTO annotation_type_target_kinds (annotation_type_id, target_kind_id)
SELECT t.id, (SELECT id FROM target_kinds WHERE name = 'entry')
FROM annotation_types t
WHERE t.type_id IN ('quality.turn_outcome', 'quality.turn_flag');
`
