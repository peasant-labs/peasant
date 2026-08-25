package store

// migrationV45 persists who drove a session, once the classifier has decided
// it, so every later run and every downstream surface reads a stored answer
// instead of re-deriving one.
//
// Two tables change, in one migration, because they are one change and not
// two: the sessions column is useless without the cache column that feeds it,
// so a store carrying only one of them is a state no build ever produces.
// Multi-statement migrations are already well precedented here (schema_v33.go,
// schema_v42.go each carry several statements in one script).
//
//   - sessions.session_origin is the closed three-value menu
//     (sessionorigin.All), defaulting to 'unknown' so an un-migrated row reads
//     as the fail-safe value rather than an empty one.
//   - sessions.origin_version is a monotonic watermark, not a menu, so it
//     carries no CHECK: bumping the rule version reclassifies every row that
//     has not yet been judged by the current rule, without a second migration.
//   - claude_transcript_evidence.origin is the same three-value menu PLUS the
//     empty string. The empty string is not a fourth origin: it is the marker
//     for "this cache row was written before the origin field existed", and it
//     never reaches sessions, the wire, or any user-facing surface. The cache
//     row scan resolves that marker to "record incomplete" before it is ever
//     treated as an Origin, and never calls Origin.Validate on the raw stored
//     value, so a corrupt token still fails closed through sessionorigin.Parse.
const migrationV45 = `
ALTER TABLE sessions
  ADD COLUMN session_origin TEXT NOT NULL DEFAULT 'unknown'
  CHECK (session_origin IN ('user', 'agent', 'unknown'));

ALTER TABLE sessions
  ADD COLUMN origin_version INTEGER NOT NULL DEFAULT 0;

ALTER TABLE claude_transcript_evidence
  ADD COLUMN origin TEXT NOT NULL DEFAULT ''
  CHECK (origin IN ('', 'user', 'agent', 'unknown'));
`
