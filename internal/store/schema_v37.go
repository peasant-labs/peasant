package store

// migrationV37 adds the per-transcript license_id column to the sessions table.
// It mirrors the village's authoritative licenses.id (the CC0-1.0 / CC-BY-4.0 /
// CC-BY-SA-4.0 menu seeded in village migration 026): the contributor's chosen
// license is recorded locally so the CLI can display it. The publish/pull
// producer wiring landed alongside this migration (the license is stamped onto
// the publish request via mapper.go and recorded atomically with its authoritative
// receipt by SavePublication). CURRENT value only — license-change history lives
// server-side on the village (transcript_governance_events). NULL = unset/legacy.
const migrationV37 = `
ALTER TABLE sessions ADD COLUMN license_id TEXT CHECK (license_id IN ('CC0-1.0', 'CC-BY-4.0', 'CC-BY-SA-4.0'));
`
