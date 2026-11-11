# Transcript follow-up visual evidence

These are local review captures, not a completed production-path visual gate. The PNGs are transient under `/tmp/opencode/evidence-peasant/` and are not checked in.

## Build and production route

`nix develop -c make build` produced the Next export and the Peasant binary but exited 2 at `web/scripts/provider-build-provenance.mjs`: the installed schema 0.48.0 exposes 14 harnesses while that script expects the eight-harness 0.20.0 contract. This gate was not bypassed or changed. The browser server was the newly built binary on port 8691 with `web,sessions,qualitySessions,annotations,review` mocks and an isolated data directory.

The marker `transcript update unavailable` was found in the served `/_next/static/chunks/app/projects/[[...segments]]/page-0675ab8a8c9f48e4.js`; the fetched bytes compared equal with `web/out/_next/static/chunks/app/projects/[[...segments]]/page-0675ab8a8c9f48e4.js`. This verifies the route chunk's served bytes, not a successful build gate.

`boot-peasant.mjs` failed on `/projects/fortuna/sess-c3d4e5f6-a7b8-9012-cdef-123456789012/`: `.txn-app` never mounted and segment requests returned HTTP 400. Retrying with the mock Strike fixture `20260729T010203.123456789Z-AAAAAAAAAAAAAAAAAAAAAAAAAA` also failed to mount, with HTTP 404s. The Home route `/` showed an empty session list in the isolated mock-backed store. Therefore the session-list conversation count, transcript header/details count, and compact control rows and per-kind facet have **no accepted mounted production captures in either theme**. The generated mock turns contain user and assistant turns, not control records. No control rows or count semantics were simulated for a screenshot.

## Inline image: dev fixture only

- Route: `http://localhost:3191/dev/visual-harness/?fb=off` (Next development server, not the production binary).
- Dark: `/tmp/opencode/evidence-peasant/image-dark.png`.
- Light: `/tmp/opencode/evidence-peasant/image-light.png`.
- Both captures were inspected. The inline PNG and text before and after it are visible without clipping; the shell's offline banner is visible because this dev server does not serve the Peasant API. `html[data-theme]` and the fixture host's `data-theme` matched each requested theme; the image reported natural width 320.
- The served `/_next/static/chunks/app/dev/visual-harness/page.js` was grepped for `Please inspect this example image`. The served `/dev/transcript-example.png` SHA-256 was `b79bc1732d91a8cd7505578c250e29e2d830f098185781bd3b2e35200faaa643`, matching the fixture asset. The static image and ordered wire content parts pass through the packaged adapter and viewer, but do not prove the production WebSocket, producer, or local attachment endpoint.
- Existing `peasant-shoot.mjs` timed out waiting for `networkidle0` on the dev route. A temporary local copy using `domcontentloaded` then rejected the theme state; it was removed. The two images above were captured with Playwright after checking the theme and image load directly, not with the existing capture script. They are illustrative, not a substitute for its gate.

## Channel-scoped error

No channel-scoped error banner was captured. The mock transcript did not mount, and forcing an error frame or changing production code would not be the requested unmodified mock path. `web/src/components/session-detail/v2/SessionDetailV2.faultPlacement.test.tsx`, test `keeps the loaded transcript when its channel refuses a refresh, with the note above it`, mounts the real host and WebSocket provider with a mock WebSocket (the Fairtrade viewer is stubbed), sends a channel error after detail, and verifies the retained transcript, alert and retry. The focused file passed: 7 tests. This is placement evidence, not a mounted-browser screenshot.
