package store

// migrationV61 adds the reverse logical-target indexes the independent-child
// cache reconciliation seeks by target.
//
// The reconciliation names the parents a harvest made available and must read
// only the stored children whose durable logical-parent evidence names one of
// them. Without a target-side index the query can only enumerate the harness
// population and evaluate each stored snapshot, so its cost scales with the
// whole library rather than with the named targets.
//
// The publication metadata snapshot is JSON, so the legacy V1 evidence is an
// expression index over the extracted parentUuid. Its partial predicate keeps
// rows without a retained parent out of the index, and the query repeats the
// same IS NOT NULL test so SQLite can select it. The active V2 generation
// evidence is a relational table; the composite index leads with the relation
// kind so the query's kind equality drives an equality seek, then the target so
// the named-target list seeks the parent, then the child session and target
// state so the evidence side is covered. It is deliberately not partial: a
// partial predicate on the target-state set would be silently retired if that
// closed set ever widened, whereas the leading kind equality is stable. Both
// indexes are additive: no stored row or representation changes.
const migrationV61 = `
CREATE INDEX idx_session_publication_parent_uuid
  ON session_publication_metadata(json_extract(metadata_json, '$.parentUuid'))
  WHERE json_extract(metadata_json, '$.parentUuid') IS NOT NULL;

CREATE INDEX idx_relationship_evidence_started_by_target
  ON session_relationship_evidence(kind, target_local_id, session_id, target_state);
`
