package store

// migrationV38 adds the per-transcript license_id column to the
// pulled_transcripts table — the PULL-side mirror of V37's sessions.license_id
// (push side). It records the license the village served with the transcript
// (the village's authoritative licenses.id menu, seeded in village migration
// 026) so a consumer of a pulled FOREIGN transcript can see what license it
// carries — the one place a license legally matters to a reader.
//
// CURRENT value only, refreshed on every re-pull. The upsert OVERWRITES
// (never COALESCEs): the row is a derived index that mirrors server truth,
// and the village's sanctioned ops path can legitimately clear a granted
// license — license-change history lives server-side in the village's
// governance audit. NULL = the village sent no license (unset/legacy ⇒
// default copyright, all rights reserved).
const migrationV38 = `
ALTER TABLE pulled_transcripts ADD COLUMN license_id TEXT CHECK (license_id IN ('CC0-1.0', 'CC-BY-4.0', 'CC-BY-SA-4.0'));
`
