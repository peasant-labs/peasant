# Review evidence after the SQLite foundation refresh

PR #538: original images retain captured source `afc2f34d17c93225203f1f1cbbeeeae92ad52ba8` and binary SHA-256 `cbce9ad1969430fa4cd92e20ec0e7e5371de5f8f4b9928aabf52ec109105a44c`. No screenshot or original manifest was rewritten. Their PNG digests are recorded in `review-evidence.json`.

Refreshed source `cc4d238399a8ef923c7503bc395993a7e2768a3f` has the byte-identical complete tracked frontend tree. The new normal build and actual owned HTTP server are verified in `refreshed-served-provenance.json`, including actual Go build metadata and binary hash. `peasant-refreshed-explicit-asset-review.md` explains every actual changed export region; the raw per-file changes are preserved. Runtime, source and dependency proofs retain their exact source identities.

All 113 original source commits are preserved in the refreshed chain, plus three mechanical adaptations. Original captured sources are preserved on the separate `peasant-25--captured-source` ref. The merged foundation is `3217f21e86b8df2084995d02cc0cc7713dc8670c`. Feature and migration merges remain reserved for the owner.

The final Linux full integration gate passed all 68 packages and the exactly-once screen. Its pinned-Go indentation-only formatting diff is retained. Both final versioned module installation obligations passed with clean owned temporary cleanup. Required final remote checks remain pending publication.
