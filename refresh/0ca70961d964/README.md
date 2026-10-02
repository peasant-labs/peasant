# Review evidence after the SQLite foundation refresh

PR #523: original images retain captured source `5bf64a7f026aecb3ae8b434461afe4b30dbef860` and binary SHA-256 `516452fda6af2996231348ff9f8bf76d6d73be898f429531a7b7d2bde22881ba`. No screenshot or original manifest was rewritten. Their PNG digests are recorded in `review-evidence.json`.

Refreshed source `0ca70961d964302aab2b6d457a916c9e460ab9ec` has the byte-identical complete tracked frontend tree. The new normal build and actual owned HTTP server are verified in `refreshed-served-provenance.json`, including actual Go build metadata and binary hash. `peasant-refreshed-explicit-asset-review.md` explains every actual changed export region; the raw per-file changes are preserved. Runtime, source and dependency proofs retain their exact source identities.

All 113 original source commits are preserved in the refreshed chain, plus three mechanical adaptations. Original captured sources are preserved on the separate `peasant-25--captured-source` ref. The merged foundation is `3217f21e86b8df2084995d02cc0cc7713dc8670c`. Feature and migration merges remain reserved for the owner.

The final Linux full integration gate passed all 68 packages and the exactly-once screen. Its pinned-Go indentation-only formatting diff is retained. Both final versioned module installation obligations passed with clean owned temporary cleanup. Required final remote checks remain pending publication.
