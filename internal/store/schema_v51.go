package store

// migrationV51 separates retained adapter output from computed analytics.
// Historical producer and input evidence stays unknown until validated files
// are reconciled; existing metrics and index producer stamps are unchanged.
const migrationV51 = `
ALTER TABLE sessions ADD COLUMN adapter_version INTEGER CHECK (adapter_version > 0);
ALTER TABLE sessions ADD COLUMN artifact_hash TEXT CHECK (artifact_hash IS NULL OR (length(artifact_hash) = 64 AND artifact_hash NOT GLOB '*[^0-9a-f]*'));
ALTER TABLE sessions ADD COLUMN metric_seed_json TEXT CHECK (metric_seed_json IS NULL OR json_valid(metric_seed_json));
`
