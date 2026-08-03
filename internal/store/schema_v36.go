package store

// migrationV36 seeds ONE general-purpose free-text entry annotation type so the
// per-turn "Label" affordance in the web transcript viewer can save arbitrary
// strings (feedback G8 — "the custom labels creator is not available here").
//
//   - type: "user.custom_label" — a user-authored free-text label on a turn
//   - family: turn_metadata (a per-entry, non-quality descriptor)
//   - value domain: described / text with an EMPTY value_constraint. The
//     validator (schema.ValidateDescribedValue, from the
//     github.com/peasant-labs/schema module) treats an empty constraint
//     spec as "no constraints to enforce", so any non-empty string is accepted
//     for this type ONLY — no other type's validation is weakened.
//   - origin: user; status: active.
//
// Registers entry-level targeting (target_kind 2) so it appears in the viewer's
// per-turn label picker (entryApplicableTypes filters on the "entry" kind).
//
// Modelled on migrationV25: INSERT OR IGNORE with a randomblob-generated UUID PK
// and family/lookup resolution by name, so no Go builder or deterministic-UUID
// constant is required. V36 runs AFTER V25's blanket deprecation of active
// types, so inserting with status active here leaves it active.
const migrationV36 = `
-- G8: Seed the free-text "Custom label" entry annotation type.
INSERT OR IGNORE INTO annotation_types (
    id, type_id, version, display_name, description,
    family_id, value_domain_kind_id, datatype, value_constraint,
    scale_kind_id, lower_is_better, status_id, origin_id, created_at
) VALUES (
    lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(6))),
    'user.custom_label',
    1,
    'Custom label',
    'A user-authored free-text label on a single turn. Any short note the user types to mark a moment in the transcript.',
    (SELECT id FROM annotation_families WHERE family = 'turn_metadata'),
    (SELECT id FROM value_domain_kinds WHERE name = 'described'),
    'text',
    '',  -- empty constraint spec: any non-empty string is accepted (free text)
    (SELECT id FROM scale_kinds WHERE name = 'nominal'),
    NULL,  -- lower_is_better not applicable
    (SELECT id FROM annotation_statuses WHERE name = 'active'),
    (SELECT id FROM type_origins WHERE name = 'user'),
    strftime('%s', 'now') * 1000
);

-- G8: Allow entry-level targeting (target_kind 2) for the custom label type.
INSERT OR IGNORE INTO annotation_type_target_kinds (annotation_type_id, target_kind_id)
SELECT t.id, (SELECT id FROM target_kinds WHERE name = 'entry')
FROM annotation_types t
WHERE t.type_id = 'user.custom_label';
`
