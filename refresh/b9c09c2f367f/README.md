# Review evidence after the SQLite foundation refresh

PR #536: original images retain captured source `023225ee5cbe58ed36c1c568678991a4f00c197a` and binary SHA-256 `babb7ccd2024dffbee8e22838c33d1e407dcd1a9a9ad16e010f61fd47ae5f6c0`. No screenshot or original manifest was rewritten. Their PNG digests are recorded in `review-evidence.json`.

Refreshed source `b9c09c2f367f96334dc18e8e4f2a4e8ec5cddc58` has the byte-identical complete tracked frontend tree. The new normal build and actual owned HTTP server are verified in `refreshed-served-provenance.json`, including actual Go build metadata and binary hash. `peasant-refreshed-explicit-asset-review.md` explains every actual changed export region; the raw per-file changes are preserved. Runtime, source and dependency proofs retain their exact source identities.

All 113 original source commits are preserved in the refreshed chain, plus three mechanical adaptations. Original captured sources are preserved on the separate `peasant-25--captured-source` ref. The merged foundation is `3217f21e86b8df2084995d02cc0cc7713dc8670c`. Feature and migration merges remain reserved for the owner.

The final Linux full integration gate passed all 68 packages and the exactly-once screen. Its pinned-Go indentation-only formatting diff is retained. Both final versioned module installation obligations passed with clean owned temporary cleanup. Required final remote checks remain pending publication.
