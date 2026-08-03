package store

// migrationV25 adds the research annotation infrastructure:
//
//   - New class: "research" (for research-specific annotation types)
//   - New family: "episode_friction" (friction episode classification)
//   - New type: "research.friction_episode" (16 enumerated subtypes)
//   - Deprecates all existing annotation types (status → deprecated)
//     so that automated annotators only use the research type.
//
// Taxonomy source: the coding-guideline classification documented below.
const migrationV25 = `
-- Deprecate all existing annotation types so they don't appear as active choices.
UPDATE annotation_types SET status_id = 3
WHERE status_id = 2;

-- Research class
INSERT OR IGNORE INTO annotation_classes (id, class, display_name, description)
VALUES (
    lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(6))),
    'research',
    'Research',
    'Research-specific annotation types for transcript analysis studies'
);

-- Episode friction family (under research class)
INSERT OR IGNORE INTO annotation_families (id, family, class_id, display_name, description)
VALUES (
    lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(6))),
    'episode_friction',
    (SELECT id FROM annotation_classes WHERE class = 'research'),
    'Episode Friction',
    'Classification of friction episodes in developer-agent interaction transcripts'
);

-- Friction episode type: 16 enumerated subtypes across 5 categories
INSERT OR IGNORE INTO annotation_types (
    id, type_id, version, display_name, description,
    family_id, value_domain_kind_id, datatype, value_constraint,
    scale_kind_id, lower_is_better, status_id, origin_id, created_at
) VALUES (
    lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(2))) || '-' || lower(hex(randomblob(6))),
    'research.friction_episode',
    1,
    'Friction Episode',
    'Classification of friction episodes in developer-agent transcripts. Categories: BAD_HANDOFF (communication gaps), GOAL_MISALIGNMENT (wrong goal), PROCESS_ERROR (wrong method), TECHNICAL_FAILURE (bugs), INFORMATION_EXCHANGE (non-friction).',
    (SELECT id FROM annotation_families WHERE family = 'episode_friction'),
    1,  -- enumerated
    'text',
    '["handoff_missing_next_steps","handoff_inadequate_next_steps","handoff_unexplained_output","handoff_excessive_burden","misalignment_wrong_trajectory","misalignment_scope_divergence","misalignment_missed_expectation","process_skipped_step","process_insufficient_quality","process_wrong_approach","process_wasteful_exploration","failure_agent_introduced","failure_latent_surfaced","exchange_requirement_evolution","exchange_clarification","exchange_provenance"]',
    1,     -- nominal
    NULL,  -- lower_is_better not applicable for nominal
    2,     -- active
    2,     -- user origin
    strftime('%s', 'now') * 1000
);

`
