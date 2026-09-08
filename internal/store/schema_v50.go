package store

// migrationV50 records the exact raw source view accepted by ingestion. NULL
// identifies legacy rows that need one ordinary refresh to establish evidence.
const migrationV50 = `ALTER TABLE sessions ADD COLUMN source_fingerprint BLOB`
