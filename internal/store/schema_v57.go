package store

// Old results have no proof of their consumed input or semantic output.
const migrationV57 = `
ALTER TABLE session_metrics ADD COLUMN input_hash TEXT CHECK (
    input_hash IS NULL OR (length(input_hash) = 64 AND input_hash NOT GLOB '*[^0-9a-f]*')
);
ALTER TABLE session_metrics ADD COLUMN output_hash TEXT CHECK (
    output_hash IS NULL OR (length(output_hash) = 64 AND output_hash NOT GLOB '*[^0-9a-f]*')
);
ALTER TABLE annotation_run_state ADD COLUMN metrics_output_hash TEXT CHECK (
    metrics_output_hash IS NULL OR (length(metrics_output_hash) = 64 AND metrics_output_hash NOT GLOB '*[^0-9a-f]*')
);
`
