# Review evidence after the SQLite foundation refresh

PR #539: original images retain captured source `863e86d5b79b66754b26ebc5d6355eb9cfd585b4` and binary SHA-256 `a39f7bc5ba00e6347548763a7a5046401d8ce618c9c5af30e34e3743dbbf85d4`. No screenshot or original manifest was rewritten. Their PNG digests are recorded in `review-evidence.json`.

Refreshed source `1708ee11cf356d1ea8991828be625c545f0356ef` has the byte-identical complete tracked frontend tree. The new normal build and actual owned HTTP server are verified in `refreshed-served-provenance.json`, including actual Go build metadata and binary hash. `peasant-refreshed-explicit-asset-review.md` explains every actual changed export region; the raw per-file changes are preserved. Runtime, source and dependency proofs retain their exact source identities.

All 113 original source commits are preserved in the refreshed chain, plus three mechanical adaptations. Original captured sources are preserved on the separate `peasant-25--captured-source` ref. The merged foundation is `3217f21e86b8df2084995d02cc0cc7713dc8670c`. Feature and migration merges remain reserved for the owner.

The final Linux full integration gate passed all 68 packages and the exactly-once screen. Its pinned-Go indentation-only formatting diff is retained. Both final versioned module installation obligations passed with clean owned temporary cleanup. Required final remote checks remain pending publication.
