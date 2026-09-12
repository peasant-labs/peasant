package store

// migrationV56 starts input proofs unknown. Existing parser versions, timestamps
// and output hashes do not establish which captured input a parser consumed.
const migrationV56 = `
ALTER TABLE sessions ADD COLUMN indexed_input_hash TEXT CHECK (
    indexed_input_hash IS NULL OR (
        length(indexed_input_hash) = 64 AND indexed_input_hash NOT GLOB '*[^0-9a-f]*'
    )
);
`
