package store

// migrationV80 adds the durable refresh outcome for a missing publication capture.
// Existing reason IDs and outcome rows remain unchanged.
const migrationV80 = `
INSERT INTO ingest_outcome_reasons (id, kind_id, code, retired)
VALUES (311, 3, 'publication_capture_missing', 0);
`
