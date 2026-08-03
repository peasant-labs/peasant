package store

// migrationV27 updates the friction episode taxonomy from v1 (16 subtypes) to v2 (3 types).
//
// Changes:
//   - Updates value_constraint on research.friction_episode to 3 values:
//     bad_handoff, bad_output, bad_process
//   - Bumps version from 1 → 2
//   - Updates description to reflect the simplified taxonomy
//
// Existing v1 annotations are preserved as-is; their values remain valid
// historical data but will not match the new constraint.
const migrationV27 = `
UPDATE annotation_types
SET
    version = 2,
    description = 'Classification of friction episodes in developer-agent transcripts. Categories: BAD_HANDOFF (communication/takeover gaps), BAD_OUTPUT (broken/wrong artifacts), BAD_PROCESS (wrong method/workflow).',
    value_constraint = '["bad_handoff","bad_output","bad_process"]'
WHERE type_id = 'research.friction_episode';
`
