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
// expression index over the extracted parentUuid. The partial predicate keeps
// rows without a retained parent out of the index, and the query repeats the
// same IS NOT NULL test so SQLite can select it. The active V2 generation
// evidence is a relational table; the composite index leads with the relation
// kind and the target so a target seek resolves a relationship directly. Both
// indexes are additive: no stored row or representation changes.
const migrationV61 = `
CREATE INDEX idx_session_publication_parent_uuid
  ON session_publication_metadata(json_extract(metadata_json, '$.parentUuid'))
  WHERE json_extract(metadata_json, '$.parentUuid') IS NOT NULL;

CREATE INDEX idx_relationship_evidence_started_by_target
  ON session_relationship_evidence(kind, target_local_id)
  WHERE target_local_id IS NOT NULL;
`
