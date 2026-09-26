package ingest

// testIngestArenaBytes is the bounded staging arena white-box tests use. It
// mirrors the external test helper so both packages exercise the same value;
// production keeps the environment default.
const testIngestArenaBytes = 64 << 20 // 64 MiB
