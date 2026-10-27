# Stored-library browsing and paging review

Source commit: `e9cb80353982d53e409a85500e535454f862f3db` (Peasant #759).
Published dependencies: Go/npm schema 0.35.0, Fairtrade 0.0.47.

All twenty full-shell PNGs are fresh captures from this exact clean-head built
binary, under temporary HOME/all XDG roots and dedicated sandbox ports. The three
existing mounted runners passed build/served-byte provenance and their production
route/state/interaction assertions. Only synthetic fixtures appear; no live store
or user server was used.

- The twelve project-picker and four publication PNGs are all SHA-256 identical
  to the preceding personally inspected `85f4f64a` images. `image-identity.json`
  records both source revisions and hashes per file. Fresh capture plus exact
  identity preserves that manual visual review, not a renamed-old-artifact claim.
- The four new `home-pages/home-{dark,light}-{desktop,phone}.png` images were each
  personally opened and inspected. They show the full mounted Home and expanded
  subagents after successful next-page, failed-refresh/retry and page-one return
  interactions. Both themes retain legible provider/teammate metadata and responsive
  desktop sidebar/phone top bar with no observed new layout regression.

The deterministic stale-effect race itself is established by the mounted Vitest
DOM-arrival regression and old-updater mutation, not by a static screenshot. The
built-app Home journey additionally verifies ordinary paging/refresh navigation.
Browser mocks prove presentation; real-store HTTP tests prove metadata membership.

Prior-head folders are retained unchanged. Keep this screenshot branch while the
PR's embedded images are needed.
