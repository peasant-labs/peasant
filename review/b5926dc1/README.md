# Stored-library review after startup-boundary correction

Source commit: `b5926dc1780687ba53718627bc560ccd471f40e3` (Peasant #759).
Published Go/npm schema 0.35.0, Fairtrade 0.0.47.

All twenty PNGs were freshly captured from the exact clean-head built binary.
The three existing root/publication/Home-paging runners passed their build and
served-byte provenance, themes/sizes and actual navigation/state assertions.
Each runner used temporary HOME/all XDG roots and a dedicated sandbox port.
Only synthetic data appears; no live store or user server was used.

Every PNG is SHA-256 identical to its preceding `e9cb8035` counterpart whose manual
inspection was either direct or already preserved by exact image identity. The
generated `image-identity.json` records both source revisions and hashes for all
twenty surfaces. This is fresh capture plus byte-identity preservation, not old
images renamed or a claim that PNG generation alone is visual inspection.

The complete frontend tree is unchanged. The new disk-loader/startup behavior is
proved separately through the real Cobra command, on-disk configuration, SQLite
store and registered HTTP routes; these static screenshots do not establish it.
The earlier rare pager ordering is likewise proved by its mounted DOM-arrival
regression and old-updater mutation, not by pixels.

Retain this screenshot branch and the older proof folders while PR links need them.
