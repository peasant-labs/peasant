# Transcript follow-up visual evidence

PNGs are transient under `/tmp/opencode/evidence-peasant/` and are not checked in. The production captures below use the mounted app; the image captures are dev-fixture evidence only.

## Build and production server

On rebased source `a663c1ee7` (parent `7f3bebaa0`), `nix develop -c make build` passed, including provider build provenance. Binary SHA-256: `d0cb363b7d4595e6c986935047b30aa066756653c6194ec23b1c3aff1c34bb81`.

The isolated server was started exactly as follows, without touching the user's data directory:

```sh
./peasant web start --port 8692 --foreground --no-browser \
  --mock-data-store=web,sessions,qualitySessions,annotations,review \
  --data-dir /tmp/opencode/ev/data --config-dir /tmp/opencode/ev/config --state-dir /tmp/opencode/ev/state
```

Before capture, the served `/_next/static/chunks/5320.73a269c11615db44.js` was grepped for `every session` and compared byte-for-byte with `web/out/_next/static/chunks/5320.73a269c11615db44.js`. The served `/_next/static/chunks/app/projects/[[...segments]]/page-0a4a9dcc70a79fcd.js` was grepped for `transcript update unavailable` and compared byte-for-byte with the matching file under `web/out`. The build's own provenance check also verified the binary and export.

## Session-list count

- Route: `/` → browse by **every session** (`/?browse=flat`) → search `Render deferred Strike`.
- Light: `/tmp/opencode/evidence-peasant/list-light.png`.
- Dark: `/tmp/opencode/evidence-peasant/list-dark.png`.
- Both screenshots were inspected: the complete table header and row are visible, including the `turns` value **2**. The browser reported the requested `html[data-theme]` and no horizontal table overflow. The real `/api/v1/sessions` mock response lists 17 sessions and gives the Strike row `turnCount: 2`; `/api/v1/sessions/20260729T010203.123456789Z-AAAAAAAAAAAAAAAAAAAAAAAAAA` returns its two conversation turns. The fixture has no system records, so this capture shows the displayed value but does not independently discriminate conversation turns from raw entries.
- The default Home `sessions` view reads `/api/v1/sync/sessions?view=home`, which returned zero rows from the isolated store; the retained `every session` view instead uses the actual mock sessions list. This corrects the earlier inference that the mock list itself was empty.

## Detail header and control facet gaps

No production detail-header or control-row/facet screenshot is claimed. The list's real link for the Strike mock session leads to `/projects/eb162ff109780837cd029d2aa990cb3b3f81ad566e678429099debed9ec0514b/20260729T010203.123456789Z-AAAAAAAAAAAAAAAAAAAAAAAAAA`. Its flat `/api/v1/sessions/{id}` read returns 200 with two turns, but `/api/v1/sessions/{id}/transcript/segments` returns 200 with `totalTurns: 0`, `totalSegments: 0`, and an empty `segments` array. The mounted route consequently shows `transcript not indexed yet`, not the transcript viewer or its header. The valid `sess_contextchild` mock similarly returns five turns on the flat read and zero segments. `boot-peasant.mjs` with `PEASANT_SESSION=sess_contextchild` failed because `.txn-app` never mounted; it also reported 404 responses for the session's supplemental endpoints. No route responses or production code were changed to manufacture a viewer.

All five mock IDs accepted by the flat detail endpoint were inspected and carry no `controlKind` or `partType` values; the other twelve list IDs returned HTTP 400 from that endpoint. The mock-generated turns and the dedicated mock fixtures also contain no system/control records. The control facet therefore remains an explicit evidence gap rather than a fabricated capture.

## Inline image: dev fixture only

- Route: `http://localhost:3191/dev/visual-harness/?fb=off` (Next development server, not the production binary).
- Dark: `/tmp/opencode/evidence-peasant/image-dark.png`.
- Light: `/tmp/opencode/evidence-peasant/image-light.png`.
- These preserved captures were produced before the rebase, on the previous base using Fairtrade 0.0.64; they are not evidence of the current Fairtrade 0.0.66 build. Both were inspected. The inline PNG and text before and after it are visible without clipping; the shell's offline banner is visible because that dev server did not serve the Peasant API. The HTML and fixture-host theme attributes matched each requested theme; the image reported natural width 320.
- The then-served `/_next/static/chunks/app/dev/visual-harness/page.js` contained `Please inspect this example image`. The served `/dev/transcript-example.png` SHA-256 was `b79bc1732d91a8cd7505578c250e29e2d830f098185781bd3b2e35200faaa643`, matching the committed asset. The fixture passes ordered wire content parts through the packaged adapter and viewer; it does not prove the production WebSocket, producer, or attachment endpoint.
- The existing `peasant-shoot.mjs` timed out waiting for `networkidle0`; a temporary local copy using `domcontentloaded` then rejected the theme state and was removed. The images were captured with Playwright after checking theme and image load directly, not with the existing capture script.

## Channel-scoped error

No channel-scoped error banner was captured. `web/src/components/session-detail/v2/SessionDetailV2.faultPlacement.test.tsx`, test `keeps the loaded transcript when its channel refuses a refresh, with the note above it`, mounts the real host and WebSocket provider with a mock WebSocket (the Fairtrade viewer is stubbed), sends a channel error after detail, and verifies the retained transcript, alert, and retry. The focused file previously passed all 7 tests. This is placement evidence, not a mounted-browser screenshot.
