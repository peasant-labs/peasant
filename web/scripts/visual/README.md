# Transcript visual-regression harness

A repeatable fidelity gate for the peasant transcript viewer. It captures every transcript surface
from the **real assembled app** — which now mounts the exact same shared composite as the canonical
fairtrade demo — and stitches each one side-by-side against that demo, so a reviewer can confirm — per
surface, in both themes — the app renders the design system faithfully with no regression. It exists to
catch host-integration regressions (theme divergence, font drift, an empty graph, a blanked surface) the
moment they appear, as a visible diff rather than a silent one.

## How the app side mounts

The harness drives a **dev-only fixture route**, `/dev/visual-harness`
(`web/src/app/dev/visual-harness/page.tsx`), which mounts the SAME shared
`@peasant-labs/fairtrade` `<TranscriptViewer>` composite the production `/projects/[name]/[id]` page
(the `SessionDetailV2` adapter) renders — through the SAME `adaptTranscript` wire adapter, with the same
prop shape (`streamPrelude`, `graphSlot` plugging fairtrade's `/graph` `@xyflow` engine, peasant's
per-turn label popover in `renderTurnActions`) — but fed a **bundled `sess_demo_0001` fixture**
(`sample-session.ts`) instead of the WebSocket `session_detail` subscription. So a plain `next dev` is
enough: no backend, no mock store, no auth. The route `404`s in a production build (`output: export`),
so it never ships as a public route.

**The app and the canonical fairtrade demo now render the literal same component** —
`<TranscriptViewer>` — not two independent implementations. (Before the transcript composition slice,
the app instead mounted `transcript-browser`'s own `<SessionDetail>` composer, an
implementation that had drifted from the demo with every design-system change — see
`SessionDetailV2.tsx`'s history note. `transcript-browser` is now deprecated and no longer a peasant
dependency; nothing first-party mounts its composer anymore.) Rendering the SAME
`sess_demo_0001` the fairtrade demo renders makes the side-by-side a true height-matched, **same-data,
same-component** comparison. The composite renders `.txn-*` surfaces (`.txn-app` root, `.txn-center`
trace column, `.txn-scorecard`, `.txn-sticky` condensed header, the `.txn-viewtoggle .bs-seg-opt`
list/graph toggle, `.txn-graphslot` graph, `.txn-rail-left`/`.txn-rail-right` rails) and owns exactly
ONE bounded inner scroller (`.txn-stream`) — it does not scroll the page.

## Oracle: what is and isn't a gate

Because the app and demo render the identical composite fed the identical data, a **surface-by-surface
parity comparison against the demo is now meaningful** — this is a genuine no-regression gate, not just a
design-language sanity check. The transcript oracle has two arms:
1. **demo parity (the transcript gate)** — the app's `<TranscriptViewer>` captures vs the fairtrade
   demo's `<TranscriptViewer>` captures, same `sess_demo_0001` data, same component. `REF_DIR=demo`
   pairs `demo | app`. A real diff here means the app's wiring (props, capabilities, callbacks) has
   drifted from what the demo exercises — not a "different component" artifact to read past.
2. **no host-integration regression** — the real `/projects/[name]/[id]` route renders through the real
   `SessionDetailV2` adapter + WebSocket path (amber breadcrumb / keybind hints / empty graph). The
   fixture route is backend-free, so `boot-peasant.mjs` covers this arm against a running backend.

**Retired golden:** the old `scripts/visual/baseline/tb/{dark,light}/` reference — captured from the
era when the app mounted `transcript-browser`'s `<SessionDetail>` composer (`.tb-*`, page-scrolled),
depicted a composer the app no longer renders, and was retired rather than re-blessed.
The transcript surface's same-component regression coverage lives in the real-binary smoke golden
(`baseline/smoke-baseline/`, §4b); `REF_DIR=demo` remains the cross-component design-language ref.

The actual **automated PASS/FAIL gates** live in the shoot (theme-flip exit 3, structural mount exit 4 /
stream-not-scrolling exit 5 / second-scroll-owner exit 6, the per-shot non-empty `SurfaceGate`) + the
boot check — independent of the demo. **"10/10 surfaces" is a capture-completeness count** (every
surface rendered non-empty), separate from the demo-parity pixel diff.

## Pieces

| Script | What it does |
|---|---|
| `probe-peasant.mjs` | DOM validator. Reports the `.txn-*` box sizes, the tab + view-toggle labels, the theme control, and the `.txn-stream` scroll metrics (its `overflowY`/scroll vs client height, plus `.txn-stream-prelude`'s computed `position` — must never be `fixed`/`sticky`) and confirms the PAGE itself does not overflow (the composite owns the only scroller). Run it first whenever the harness route or the composite changes. |
| `selection-recovery-probe.mjs` | Real-build computed-style probe for the shared selection-recovery panel. It verifies build provenance, boots `bin/peasant`, mounts the all-hidden state on Home and Map through the production summary request, and checks both themes for accepted copy, visibility, Atkinson fonts, tabular counts, canonical surface styling, and the copy control focus ring. It writes no screenshots. |
| `peasant-shoot.mjs` | Captures the 10 transcript surfaces for one theme. Most surfaces are captured at the base viewport (`captureBeyondViewport:true`); the full trace canvas (whose content scrolls inside the bounded `.txn-stream`) is captured in FULL by temporarily growing the viewport to the stream's natural height (`shotTall`, ported from fairtrade's own `shootdemo.mjs`). Every capture passes the non-empty `SurfaceGate`. **Two-tier failures** (below). |
| `surface-gate.mjs` | The shared non-empty assertion (byte floor, non-background ratio, distinct-colour count, no byte-identical duplicates). Vendored from the demo side so both are held to the same bar. |
| `boot-peasant.mjs` | Host-integration check (oracle arm 2): boots the **real** `/projects/[name]/[id]` route against a running backend and asserts the composite renders through the real `SessionDetailV2` adapter + WebSocket path (exit 2 if `.txn-app` never mounts). The fixture route is backend-free, so this is the only arm that exercises the real transport. Validates its `PEASANT_PROJECT` coordinate against the live backend first (`validate-mock-coordinates.mjs`) so a stale default fails loud, not as a misleading transport-broken diagnosis. |
| `validate-mock-coordinates.mjs` | Shared fail-fast guard against a recurring bug class: a script/fixture's hardcoded mock project literal drifting from the Go mock's actual catalog. Queries the live `GET /api/v1/projects/summary` (the same endpoint the Home picker uses) before any script boots Puppeteer, and throws an actionable error naming the exact invalid coordinate + the current valid set if it doesn't match. Wired into `boot-peasant.mjs`, `full-app-smoke.mjs`, `shell-nav-gate.mjs`, `shell-nav-default-gate.mjs`, and `transcript-input-gate.mjs`. |
| `inspect-feedback-shoot.mjs` | Captures the dev-only inspect + feedback tool (armed picker, note popup, saved + re-armed, and `?fb=off`) in both themes over a running `next dev`. It verifies build provenance first (the served JS chunk must carry the change marker), asserts the in-browser flow — arm from the control and from `c`, the pick, the `POST /api/v1/local/feedback` payload, the re-arm, and the disable switch — and fulfills the save locally so the run never writes `llm/ui-feedback.md`. The tool is dev-gated, so this arm is the only one that can see it: every other capture script holds it off with `?fb=off`. |
| `shell-nav-gate.mjs` | Local shell header gate (connected arm), against a running app in either mode: first proves the server serves this checkout's `web/out` (`served-build.mjs`; with `PEASANT_BIN` also that the binary is not older) and logs the served mode (`SHELL_EXPECT_MODE` can require one), then holds the mounted header to `testdata/shell-header.yaml` through `shell-header-manifest.mjs` (the same checks the component tests run) on home and on every route-only page (`/analytics`, and `/review` and `/map` under `SHELL_PROJECT`) in both themes — required items present (settings only once its page ships), removed items (connection pill, share button, fairtrade's section sub-nav anywhere on the page) gone, no link to a route-only section anywhere outside `<main>` — plus one-row `--nav-h` geometry with every item visible and reachable, `<main>` clearing the header with no tabindex at rest, the root's scroll padding matching the fixed header, and a palette with no per-project or route-only jump. Then a keyboard check: a click on plain text mid-page on `/analytics` then Tab keeps the page where it is, and a click inside `/share`'s session list then PageDown scrolls that list (a click must never make `<main>` the focus). Runs the same header checks at every width in `src/test/testdata/shell_responsive.yaml`, down to the 320px reflow width, and writes full-frame review captures. |
| `shell-nav-default-gate.mjs` | Local shell offline gate (server-stopped arm): boots its own default-mode `bin/peasant`, verifies build provenance (`served-build.mjs`: binary not older than `web/out`, served chunks identical to `web/out`, the shell's markers present), holds each page to the header manifest while connected, then STOPS the server with the page open and asserts fairtrade's `LocalOfflineBanner` shows directly under the fixed header — pinned where the screen has room, in the page flow elsewhere (`chromeClearance`) — this computer, not the internet; `peasant web start --port <page port>`; `try again` — with the live region announcing the stop, the header manifest intact and `<main>` clearing the header plus the notice. Case checks (from `testdata/shell-offline-cases.yaml`, strict, required names): no second scroll on the desktop transcript; `try again` scrolled into reach at 320×256; the transcript (320×256) and `/share` (320×568) keep their floor fully in view; the pinned notice on screen on a page scrolled down at 1440×700; on every case the root's scroll padding matches what stays fixed. Then a `try again` with the server still down is announced with the check time; after a restart, `try again` clears the notice and the return is announced. Captures every case in both themes. |
| `shell-header-manifest.mjs` | The one reader of `testdata/shell-header.yaml` (strict, required names pinned) and the self-contained DOM checks shared by the component tests (jsdom) and the two shell gates (`page.evaluate`): `headerFailures`, `paletteFailures`, and the browser-only `headerGeometryFailures` and `chromeClearance` (placement, `<main>` clearance and resting tabindex, root scroll padding; it takes the notice-pinned query that `loadNoticePinnedQuery` reads from `src/components/testdata/app-shell-geometry.yaml`, and returns the notice's measured placement). |
| `served-build.mjs` | Build provenance for any gate that boots or targets a served build: `assertServedBuild({ origin, markers, bin? })` proves the served page references exactly `web/out/index.html`'s chunks, those chunks carry every marker the caller names, and (given `bin`) the binary is not older than `web/out`; `shellProvenanceMarkers(manifest)` names the shell's markers (`--app-notice-height` and the manifest's `back` / `stillStopped` announcements). |
| `context-navigation-shoot.mjs` | Mounted current-parent and child-context navigation evidence on the REAL binary: boots `bin/peasant` with the mock store and opens the child transcript through the real WebSocket `session_detail` payload, so the stored `context_from` source, the stored `started_by` parent, and the retained earlier-history partition all arrive through the real store → decoration boundary → adapter → composite path. It asserts the two stable links, that the disclosure toggle writes only `earlier=` (the reader's view is held by the browser's scroll anchoring for exactly the inserted height — compared and retained, not merely recorded), following the current-parent link to the EXACT stored parent, and Back restoring the child's route, disclosure, `.txn-stream` offset, selected turn, and query — the asserted state is the one pictured, and the state is re-sampled after each key capture to prove it. Reopening the search panel is a separate, labelled action that runs the viewer's own match jump, so it gets its own sample and its own capture instead of being folded into the restoration claim. Also asserts a reload, a copied disclosed link, and the honest unavailable reference for a child whose target is no longer stored. Both themes; served-chunk + embedded-binary build provenance is asserted before any capture. Fixture: `internal/mock/testdata/context_navigation.yaml` (shared with the Go mock provider via `context-navigation-fixture.mjs`). |
| `settings-shoot.mjs` | Settings page evidence (`/settings`) on the real built binary. It builds a throwaway world under the OS temp directory (three git repositories each with a recorded Claude Code session, a `config.yaml`, auto-publish rules in `hooks.yaml`, and a stored Village sign-in whose Village is a local double listing two collectives), ingests through the real route, and shoots both themes: the page as it opens (desktop and 390px), a switch whose write is held pending, a text field the server refuses (value restored, reason shown), a tagged row, the install action offering 3 repositories before any call, and the same action after a foreign hook appeared (two installed, one blocked with its remedy). It checks build provenance first (the settings marker in a fresh `web/out` chunk, in `bin/peasant`, and in the served chunk), probes computed fonts, sizes, radius and numerals, and fails if the page scrolls sideways at 390px with its groups open. Run `pnpm shoot:settings [out-dir]` with `CHROME_PATH` after `make build`. |
| `root-page-visual.mjs` | Mounted evidence for the root page (`pnpm visual:root-page`): the stats strip, the search box and the session list with its publish-state filters. It verifies build provenance, then boots `bin/peasant` twice on throwaway XDG directories (so no real store is read): once with the mock store for the WebSocket topics, the project summary and search, and once with no mock store for the empty install. The mock store holds no sync list or publication receipts, so the browser answers `GET /api/v1/sync/sessions`, `GET /api/v1/publications` and the grouped search from the mock sessions and `testdata/root-page.yaml`. It captures every filter, a search, the selection notice, the selection recovery panel and the empty install, in both themes at desktop and phone width, checks each filter's count against the rows it lists (pressing `load more` until the list is complete), and probes fonts, the title size, tabular strip values, square filter options and sideways overflow. Captures go to `$CAPTURES` (default `/tmp/peasant-root-page`). |
| `stitch-sxs.mjs` | Builds the height-matched **REFERENCE \| SUBJECT** composites per surface per theme (`REF_DIR`/`APP_DIR`) into a `SURFACE_SET`-distinct `SXS_OUT_SUBDIR` (`sxs-smoke`/`sxs`). Both panes drawn to the taller height, top-aligned; the shorter is **padded, never scaled**, with a sampled background + a dashed end-hairline. For `transcript`/`changes`, a missing subject capture becomes a labeled placeholder so the set stays complete (gate still fails on the missing pair). For `smoke`, a missing reference OR subject capture writes **no placeholder at all** — the surface is skipped and logged as a `FAIL`, so durable smoke evidence can never contain a fake stand-in. **Default `REF_DIR=demo`** pairs the fairtrade demo reference against the app captures (the retired pre-composite-migration `tb` golden was removed — see the Oracle section above). |

## Two-tier failure contract

- **STRUCTURAL** (the whole run is invalid) → hard **non-zero exit**: the theme didn't flip
  (`[data-theme]` wrong after clicking `.theme-btn`); the composite never mounted (`.txn-app`); its
  inner `.txn-stream` scroller doesn't overflow (the fixture is too short to actually need scrolling);
  the page itself overflows (a second scroll owner — the composite must own the ONLY scroller); or a
  surface exceeds the 4000px raster ceiling.
- **PER-SURFACE gaps** (one surface failed, the rest are fine) → recorded + the run **continues**
  (exit 0): a selector never mounted, a popover didn't open, a single blank/duplicate `SurfaceGate`
  failure. Gaps are visible in the stitch as labeled placeholders, never silently dropped.

## The 10 surfaces (and the app-side selector each targets)

| Surface | App selector |
|---|---|
| `txn-highlights` | `.txn-app` (highlights tab) |
| `txn-scorecard` | `.txn-scorecard` |
| `txn-trace-canvas` | `.txn-app` (full-trace/list, expand-all; captured in full via `shotTall`) |
| `txn-scrubber` | `.txn-scrub` (revealed by scrolling `.txn-stream` past its 56px sticky threshold) |
| `txn-rails` | `.txn-app` (left outline + right filters rail, at scroll-top) |
| `txn-label-popover` | `button[aria-label="Label this turn"]` → `.pop-card[role="dialog"]` |
| `txn-graph` | `.txn-graphslot` (view-mode → graph) |
| `txn-diffs` | `.txn-app` (diffs tab) |
| `txn-files` | `.txn-app` (files tab) |
| `txn-annotations` | `.txn-app` (annotations tab) |

Captured in both `dark` and `light`.

## Adding a surface to the two-arm gate

The harness is two arms over a shared surface set: the **capture+diff arm** (`*-shoot.mjs` →
`stitch-sxs.mjs`, which now runs an **imgdiff** pixel gate, not only a human-glance composite) and the
**host-integration boot arm** (`boot-peasant.mjs`). To register a new surface end-to-end:

1. **Capture it** — add a `await surface('txn-<name>', async () => { … await shotFull('txn-<name>', '<sel>') })`
   block in `peasant-shoot.mjs` (navigate/reveal the surface, then shoot its selector). Captures run under
   `applyDeterminism` (frozen clock + seeded `Math.random` + reduced motion, from `determinism.mjs`) so the
   PNG is byte-stable for the diff.
2. **Register it in the diff arm** — append `['txn-<name>', null]` to the `SURFACES` array in
   `stitch-sxs.mjs`. The stitch pixel-diffs the raw reference vs the raw app capture per `[surface, theme]`.
3. **Register a boot arm** — append a `{ id, url, mount, capture, interact }` entry to the `SURFACES`
   registry in `boot-peasant.mjs` (the block comment there documents each field).
4. **Add a baseline PNG** — for a first-class smoke surface, bless a real-binary capture into
   `scripts/visual/baseline/smoke-baseline/{dark,light}/<surface>.png` (§4b); a missing baseline fails
   the diff closed as `NO-REF`.
5. **imgdiff thresholds** — the gate uses `IMGDIFF_TOL = 16` (per-channel, /255; absorbs AA shimmer) and
   FAILs any surface whose differing-pixel share exceeds `IMGDIFF_FAIL_PCT = 0.5` (%), or that is
   non-comparable (missing ref/app, or a size mismatch). Both consts live at the top of `stitch-sxs.mjs`.

## Prerequisites

- **Chrome/Chromium** — set `CHROME_PATH`, e.g. `export CHROME_PATH=$(command -v google-chrome)`.
- **puppeteer-core** — a `devDependency` here, so `pnpm probe:peasant` works out of the box. If a
  bare import does not resolve (monorepo hoisting), point `PUPPETEER_CORE` at an install that has it.
- **The app** — `pnpm dev` (plain `next dev` on :3000); the fixture route needs no backend.
- **A demo side** — run the shared design-system `shootdemo` (its dev server on :5180/5181) to produce
  `demo/<theme>/<surface>.png` for the stitch to pair against.

## Run it

```bash
export CHROME_PATH=$(command -v google-chrome)
CAPTURES=./review-capture                    # output root

pnpm dev   # terminal 1 — next dev on :3000

# 0. sanity-check the DOM (optional)
pnpm probe:peasant

# 0b. probe the production selection-recovery panel on Home + Map in both themes
pnpm probe:selection-recovery

# 1. app side — both themes
pnpm shoot:peasant -- dark  "$CAPTURES/peasant/dark"
pnpm shoot:peasant -- light "$CAPTURES/peasant/light"

# 2. (optional) demo side for the design-language sanity panel — shared design-system shootdemo
#    node scripts/shootdemo.mjs dark  "$CAPTURES/demo/dark" ; node scripts/shootdemo.mjs light "$CAPTURES/demo/light"

# 3. stitch the design-language sanity panel (default REF_DIR=demo — the demo TranscriptViewer reference;
#    informational, cross-component). Same-component regression gates are sxs:changes and sxs:smoke.
pnpm sxs -- "$CAPTURES"                       # writes $CAPTURES/sxs/...

# 4. host-integration arm — boot the REAL route against a backend (needs mock data; see boot-peasant.mjs)
#    PEASANT_REAL_ORIGIN=http://localhost:8690 pnpm boot:peasant
```

Output layout:

```
review-capture/                         # runtime output (ephemeral, gitignored)
├── peasant/{dark,light}/<surface>.png  # the real peasant app (this harness)
├── demo/{dark,light}/<surface>.png     # canonical fairtrade demo, same composite — the meaningful parity reference
└── sxs/{dark,light}/<surface>.png      # height-matched REFERENCE | SUBJECT composites
```

## Environment variables

| Var | Default | Used by |
|---|---|---|
| `CHROME_PATH` | — (required) | all |
| `PUPPETEER_CORE` | bare `puppeteer-core` | all (set only if the bare import won't resolve) |
| `PEASANT_URL` | `http://localhost:3000/dev/visual-harness` | probe, shoot |
| `PEASANT_RECOVERY_PORT` | `8707` | selection-recovery probe real-binary server |
| `REF_DIR` | `demo` | stitch (reference/left; `demo`=the fairtrade demo reference — cross-component, informational; same-component gates are `changes` and the smoke baseline) |
| `REF_LABEL` | same-component baseline caption | stitch (reference column caption) |
| `APP_DIR` | `peasant` | stitch (subject/right capture subdir) |
| `APP_LABEL` | peasant wiring caption | stitch (subject column caption) |
| `PEASANT_REAL_ORIGIN` | `http://localhost:8690` | boot (backend-served real app origin) |
| `DEMO_URL` | `http://localhost:5180` | demo shoot: fairtrade demo origin |
| `SHELL_CAPTURE_DIR` | `<base>/shell` | shell header gate capture destination |
| `SHELL_PROJECT` | the canonical `ProjectHash` for the mock's `fortuna` project (`SHELL_DEFAULT_PROJECT` in `smoke-surfaces.mjs`) | shell header gate: mock project the project-scoped route-only pages (`/review/<hash>/`, `/map/<hash>/`) open under (must exist in the running app's mock store; a hash, not a label, so the gate's exact-path check isn't tripped by the legitimate label-to-hash canonicalization) |
| `SHELL_RESPONSIVE_ONLY` | unset | shell header gate: `1` runs only the responsive widths |
| `SHELL_EXPECT_MODE` | unset | shell header gate: `default` or `experimental` — fail unless the server advertises exactly that mode |
| `PEASANT_BIN` | `<repo>/bin/peasant` | shell offline gate: the binary it boots |
| `PEASANT_OFFLINE_PORT` | `8698` | shell offline gate: the port it boots the binary on (must be free) |
| `PEASANT_OFFLINE_CONFIG_DIR` | a fresh temp dir, removed afterwards | shell offline gate: config dir of the booted server (a supplied dir is kept) |
| `SHELL_OFFLINE_CAPTURE_DIR` | `<base>/shell-offline` | shell offline gate capture destination (also holds `server.log`) |
| `SXS_OUT_SUBDIR` | per `SURFACE_SET` (`smoke`→`sxs-smoke`, else `sxs`) | stitch: output subdir for composites, kept DISTINCT per arm so smoke-SxS evidence can never be confused with the transcript/changes composites |
| `PEASANT_PROJECT` / `PEASANT_SESSION` | `fortuna` / a mock session | boot (real viewer route); validated against the live backend's mock catalog before use (`validate-mock-coordinates.mjs`) |

---

## Full-app gate + Changes / graph surfaces

Runbook for the build + gate operations a zero-context team needs to verify the lifted
`<Changes>`/`<ChangeDetail>` (and future graph) surfaces. The section above (the transcript
`<TranscriptViewer>` harness) is unchanged; this is the full-app + graph-surface gate. Every command
below assumes `export CHROME_PATH=$(command -v google-chrome)`.

Repositories referenced:
- **PEASANT** = this repository, with the app in `web/`.
- **FAIRTRADE** = a separate design-system checkout matching the published version in
  `web/package.json`; it supplies the canonical demo used for side-by-side captures.
- Peasant builds from published package versions and the single committed pnpm lockfile.
  Local workspace links are not part of the supported build path.

### 0. Dependency and demo provenance
Use `pnpm install --frozen-lockfile` in Peasant so app captures use the exact published package
graph. When changing Fairtrade itself, run `pnpm build:lib` and its package gates in that repository;
Peasant consumes the change only after a published version is pinned in `web/package.json` and the
lockfile is regenerated. Run the Fairtrade demo from the checkout matching that pinned version.
  (A stale `dist/lib` also yields misleading `tsc` "cannot find module @peasant-labs/fairtrade/*"
  diagnostics — re-run `build:lib`, then `tsc`.)

### 1. `make build` — the canonical build (the user's path)
- **cmd:** `cd PEASANT && make build`  → `bin/peasant` (Next static export embedded via `//go:embed web/out`).
- **cwd:** PEASANT (repo root).
- **when:** before any real-binary verification; the ONLY trusted build path.
- **expect:** a pnpm install, a clean `next build` route table (including
  `/review/[[...segments]]`), `go build`, `bin/peasant` produced.
- **failure modes:**
  - **pnpm unavailable:** `make web` fails before dependency resolution. Fix by entering
    `nix develop` or enabling the pnpm version pinned in `web/package.json`, then rebuild.
  - **embed guard:** `make web` refuses if `web/out` lacks the lifted surface (marker
    `gmp-changes-root`) → the web build is stale/stub; rebuild the web side with pnpm first.
  - **stale binary:** if an old surface appears, rebuild and confirm the running executable is the
    newly generated `bin/peasant`; the embed guard prevents a new stub export from passing.

### 2. `./bin/peasant web start` — run the real binary
- **cmd:** `cd PEASANT && ./bin/peasant web start --port 8690 --foreground --no-browser --mock-data-store=web,sessions,qualitySessions,annotations,review`
- **when:** to hit the SERVED HTTP routes (the real artifact). `review` in the mock store is
  REQUIRED for `/review`. `--experimental` only advertises the code-map capability; the local
  header and palette are the same in both modes (the code map is route-only, see `EXPERIMENTAL.md`),
  and `/map/<project>/` stays directly routable either way — `shell-nav-gate.mjs` must pass on both.
- **expect:** `curl -sf localhost:8690/api/v1/health`; serves `/review/<project>/`,
  `/projects/<project>/<session>/` (transcript), `/` (dashboard), `/map/<project>/`.

### 3. `full-app-smoke.mjs` — the durable real-binary gate for every visual change
- **cmd:** `cd PEASANT/web && node scripts/visual/full-app-smoke.mjs`
- **what:** runs `make build`, boots the REAL `bin/peasant`, drives EVERY surface over its HTTP
  routes (NOT the dev harness), asserting SERVED 200 + MOUNTED + Atkinson + NON-BLANK.
- **env:** `SMOKE_SKIP_BUILD=1` reuse the existing bin (skip `make build`); `PEASANT_SMOKE_PORT`
  (default 8699); `SMOKE_PROJECT`/`SMOKE_SESSION`/`SMOKE_BRANCH` fixture coordinates.
- **when:** before closing ANY surface + before launching a new graph surface — the collateral-regression catcher.
- **expect:** a per-row table `served=200 mounted=true atkinson=true` for all surfaces (dark+light)
  + `OK [full-app-smoke] all N surface checks passed on the real binary`. Real-binary screenshots →
  `scripts/visual/smoke/<theme>/<surface>.png` (local serve-proof; ignored by git).
- **failure modes:** `served!=200` → routing/build (see §1); `atkinson=false` → the font
  `@import`-drop regressed (see §Font guard); non-blank fail → blank/broken surface.

### 4. Fidelity composites (DEMO reference | app SUBJECT — the user-eyeball artifact)
Side-by-side of the design-system demo vs the app render over the SAME dev fixture. Distinct from
the §5 regression gate. Needs both dev servers up: FAIRTRADE `pnpm dev` (:5180) + PEASANT
`pnpm dev` (:3000, serves `/dev/changes-harness`).
- **a. app subject:** `cd PEASANT/web && pnpm shoot:changes -- <base>` → `<base>/peasant/<theme>/gmp-*.png`
- **b. demo reference:** `cd PEASANT/web && node scripts/visual/demo-shoot.mjs <base>` → `<base>/demo/<theme>/gmp-*.png`
- **c. stitch:** `cd PEASANT/web && SURFACE_SET=changes REF_DIR=demo APP_DIR=peasant node scripts/visual/stitch-sxs.mjs <base>` → `<base>/sxs/<theme>/gmp-*.png`
- **expect:** per-pane captions `fairtrade demo · <Surface>` | `peasant /review · <Surface>`, top-left
  `surface · theme` title, top-right surface-under-scrutiny label, DIMENSION-MATCHED, Atkinson.
  OPEN them — this is the eyeball; the `%`-number (cross-engine AA ~1–3%) is INFORMATIONAL.
  These composites are LOCAL REVIEW ARTIFACTS under `<base>/sxs/<theme>/gmp-{changes,change-detail}.png`
  (gitignored, not committed) — the durable, blessed reference for the SAME-ENV regression gate (§5) is
  `scripts/visual/baseline/changes/<theme>/gmp-*.png`, which IS committed.
- **failure mode — DIM:** a dimension mismatch = a sticky shell element reflowed slack into the
  surface box (trailing whitespace, NOT a content clip — verify the content-end `y` is identical in
  both). Hide the shell ONLY for surfaces whose chrome OVERLAPS content (e.g. change-detail's crumb,
  not changes) — `demo-shoot.mjs` does this per-surface (`hideShell`).

### 4b. Real-binary smoke SxS (same smoke captures → review composites)
- **cmd:** `cd PEASANT/web && pnpm sxs:smoke`
- **what:** reuses the same surface registry as `full-app-smoke.mjs` and stitches the current real-binary
  screenshots from `scripts/visual/smoke/<theme>/<surface>.png` against the COMMITTED durable baseline at
  `scripts/visual/baseline/smoke-baseline/<theme>/<surface>.png` into `scripts/visual/sxs-smoke/<theme>/`
  — a DISTINCT directory from the transcript/changes `sxs/` so reviewers never mix up which evidence
  they're looking at.
- **references are DURABLE, never placeholder:** the six smoke surfaces (`analytics`, `dashboard`, `map`,
  `review-change-detail`, `review-changes`, `transcript`) each have a COMMITTED reference baseline at
  `scripts/visual/baseline/smoke-baseline/<theme>/<surface>.png` (blessed the same way the `changes` arm's
  `baseline/changes/` is — capture once from the real binary, then commit). If a reference OR a current
  app capture is missing for a surface, the stitch does **NOT** draw a placeholder composite for it — it
  logs a `FAIL` line and skips writing that file, then the run **exits non-zero**. A reviewer can therefore
  trust that every `.png` under `sxs-smoke/` is real, fully-paired evidence — never a "not staged" stand-in.
- **re-bless after an intentional render change:** `pnpm sxs:smoke` (or rerun `full-app-smoke.mjs`) to
  produce fresh `scripts/visual/smoke/<theme>/<surface>.png`, get the change blessed by the user, then
  `cp scripts/visual/smoke/<theme>/*.png scripts/visual/baseline/smoke-baseline/<theme>/`.
- **why:** smoke and SxS share one surface manifest, so adding a first-class smoke surface wires it into
  visual-review SxS without copying another hardcoded list.

### 4c. Local shell gates (header manifest + offline notice)
- **header (connected) cmd:** `cd PEASANT/web && pnpm boot:shell` with §2's peasant server running from a
  fresh `make build` (mock store MUST include the project `SHELL_PROJECT` resolves to — default the mock's
  `fortuna` project, given as its canonical `ProjectHash`). Run it against a default server
  (`SHELL_EXPECT_MODE=default`) AND an `--experimental` one (`SHELL_EXPECT_MODE=experimental`): both must
  pass, since the code-map capability must not bring a route-only section back.
- **offline (server stopped) cmd:** `cd PEASANT/web && pnpm shell:offline` after `make build`. It boots
  its own default-mode `bin/peasant` on `PEASANT_OFFLINE_PORT` (default `8698`), so nothing else may
  hold that port; it kills that server by its own process handle on exit, failure or signal, removes the
  temp config dir it made, and rewrites `server.log` per run. `pnpm shell:gate` runs both arms.
- **provenance first:** both gates run `served-build.mjs`'s `assertServedBuild` before any capture —
  `web/out` must not be older than the newest file under `web/src`, the served page must reference
  exactly `web/out/index.html`'s chunks, and those chunks must carry
  `--app-notice-height` and the manifest's `back` and `stillStopped` announcements (the `stopped` one
  repeats fairtrade's banner headline, so it proves nothing about this build) — so a stale server or
  another worktree fails before it can produce mislabelled evidence. The offline gate also checks its
  binary is not older than `web/out`; the header gate does so only when `PEASANT_BIN` names it.
- **the manifest:** `testdata/shell-header.yaml` names what the header must carry (`brand`, `search`,
  `theme`, and `settings` once `src/app/settings/page.tsx` exists — before that a settings link would
  be dead, so it must be absent), what must not come back (`connection-pill`, `share-button`,
  `section-nav` — fairtrade's sub-nav included, wherever it mounts), the route-only sections that must
  still resolve (`/analytics`, `/review`, `/map`) and that nothing outside `<main>` may link to, the
  palette commands forbidden (`proj-changes:`, `proj-map:`) and required, and the live region's
  announcements (`stopped`, `stillStopped`, `back`). The component tests (`TopNavbar.test.tsx`, `CommandPalette.test.tsx`,
  `LayoutShell.capabilities.test.tsx`) read the same file through `shell-header-manifest.mjs`, so the unit
  render and the served build are held to one list.
- **the pin query:** "a screen with room to pin the notice" is declared once, in `globals.css`
  (`@custom-variant notice-pinned (@media …)`), and `src/components/testdata/app-shell-geometry.yaml`
  records it (`AppShellGeometry.test.ts` holds `globals.css` to that record). Both gates read the query
  from that fixture (`loadNoticePinnedQuery`) and pass it to `chromeClearance`, so no gate carries its
  own copy.
- **what the header arm asserts:** on home and on each route-only page, in both themes: the manifest;
  one row of `--nav-h` with every item visible, inside the row and reachable by a pointer; `<main>`
  clearing the header, carrying no tabindex at rest, and the root's `scroll-padding-top` equal to the
  fixed header; theme attributes; and a palette that offers no per-project or route-only jump.
  The responsive arm repeats the header checks at every width in `src/test/testdata/shell_responsive.yaml`,
  down to the 320px `reflow` row. It checks the header's own overflow, not the page body's (the home
  project picker already scrolls sideways at 390px; that body is rebuilt separately).
- **keyboard after a click (header arm):** a click must never make `<main>` the focus, which would send
  Tab back to the top of the page and keyboard scrolling to the document instead of the pane clicked.
  On `/analytics` at 1440×800, scrolled down, a click on plain text (an element with text of its own,
  not a control, with no focusable ancestor but `<main>` — a chart's own `<g tabindex>` would take the
  click and hide what `<main>` does) must leave focus on `<body>`, and Tab must move to a control
  without jumping the page up; on `/share` at 1440×600, a click on plain content inside the session list
  (`.swz-body`, which overflows in the mock store) then PageDown must scroll that list. A `tabindex` on
  `<main>` fails either half on its own.
- **what the offline arm asserts:** with each page open and the server killed: the notice directly under
  the fixed header — pinned (`position: fixed`, its top at the header's bottom) where the `notice-pinned`
  query matches, in the page flow with no fixed ancestor elsewhere — with
  "peasant isn't running on this computer", "your internet is fine", `peasant web start --port <port>`
  and `try again`; the live region saying the manifest's `stopped` text; the header manifest intact;
  `--app-notice-height` set; `<main>` clearing the header plus the notice; the root's
  `scroll-padding-top` equal to what stays fixed (the header plus the notice where it is pinned, the
  header alone elsewhere, so a focused element never scrolls under either); `<main>` with no tabindex
  at rest. The cases come from `testdata/shell-offline-cases.yaml` (loaded strictly: unknown fields fail
  and the names in the gate's `REQUIRED_CASES` must all be there; each row has one `check`: `plain`,
  `retry-reach`, `floor`, `scrolled` or `keep-place`). Per case: the 1440×900
  transcript owns the only scroller; at 320×256 (400% zoom) the notice is taller than the viewport and
  the document scrolls `try again` into view below the header, reachable by a pointer; the transcript at
  320×256 and `/share` at 320×568, scrolled to the bottom, keep exactly their floor (`min(bodyFloor,
  screen height − header)`, `bodyFloor` from `app-shell-geometry.yaml`) fully in view below the header,
  which also proves the floor binds there; home at 1440×700, scrolled down before the server stops,
  shows the pinned notice on screen under the header; `/analytics` at 1440×500 (not pinned), scrolled
  down, scrolls by exactly the notice's height in the frame it appears and back by at least that in the
  frame it goes (the page's own connection strip goes in the same frame), so the reader keeps their
  place. Then `try again` with
  the server still down makes the live region say the `stillStopped` text with the check time; after a
  restart, `try again` clears the notice, returns the page under the header, and the live region says the
  `back` text. On the `recover: keyboard` case `try again` is focused and pressed with Enter: focus moves
  to `<main>` (focusable only for that move) without scrolling, and the next Tab moves on with `<main>`
  no longer focusable.
- **the tour:** the gate can only see a tour overlay, and the tour never starts on its own, so the
  mounted check cannot tell a mounted tour provider from an unmounted one. The unmount itself is guarded
  by `LocalOfflineNotice.test.tsx` (a mounted provider would render its marker).
- **no SxS arm:** the fairtrade in-use demo's local app renders a section sub-nav, not this header, so a
  demo-vs-app composite would compare different chromes; both gates assert the mounted shell directly.
  peasant-labs/fairtrade-design-system#139 (the demo carries this header, or fairtrade exports it) is
  what brings a demo-left / app-right arm back, for the header and the offline notice.
- **outputs:** `scripts/visual/shell/<theme>/shell-{home,home-mobile,route-analytics,route-review,route-map}.png`
  and `scripts/visual/shell-offline/<theme>/{home,transcript,home-mobile,home-short,transcript-short,share-mobile,home-scrolled,analytics-keep-place,analytics-end-keeps-place}.png`
  — review artifacts, never committed.
- **mock limitation:** the mock data store cannot serve the grouped sessions route (`GET
  /api/v1/sessions?view=grouped` answers 500), so the home body shows that error panel in mock captures;
  the header gate ignores exactly that console error.
- **test promotion checklist (both gates):**
  - *Subject:* header gate — the header manifest, the palette rules, route-only resolution and one-row
    geometry on the served build; offline gate — the notice's behaviour when the real server stops and
    comes back.
  - *Necessity:* jsdom computes no layout, hit-testing or real socket close; the component tests hold the
    same manifest checks but cannot see geometry, reachability or a real process exit.
  - *Production path:* the real routes, socket and health route of a running `bin/peasant`; nothing is
    mocked but the data store. Both gates prove `web/out` is not older than `web/src` and that the served
    chunks equal it; the offline gate also proves its binary is not older than `web/out`, the header gate
    only when `PEASANT_BIN` is given.
  - *Cost:* one headless browser; the header gate drives 2 themes × (home, 390px, 3 routes) + 6 widths
    and two keyboard checks in about a minute; the offline gate runs 16 stop/restart cycles (2 themes × 8
    cases) in about five minutes.
  - *Lifetime:* the header gate spawns no server; the offline gate kills its server and removes its temp
    config dir on exit, failure, SIGINT and SIGTERM. A SIGKILL of the gate itself orphans the server and
    leaks the temp config dir; the next run then fails loudly on the busy port.
  - *Concurrency:* the header gate uses the caller's origin; the offline gate uses one fixed port
    (checked free first). Both write to a capture dir that is fixed per checkout (`<base>/shell`,
    `<base>/shell-offline`), so two concurrent runs from one checkout overwrite each other's frames and
    `server.log`; pass a different `<base>` (or the capture-dir variables) per run.
  - *CI parity:* neither runs in CI; both need `make build` and a Chrome binary, and run from a clean
    checkout.
  - *Evidence:* full-frame captures of the mounted shell in both themes: desktop, 390px, 320×256 (home and
    transcript), 320×568 (`/share`), a scrolled 1440×700 page and a scrolled 1440×500 `/analytics` and its 390×844 page end while stopped; the keyboard
    recovery (focus to `<main>`, then Tab) is asserted on the real browser.
  - *Mutation:* a re-added pill or share link, any link to a route-only section outside `<main>` (and
    fairtrade's section sub-nav anywhere), a dead settings link, a notice fixed on a small screen or
    unpinned on a roomy one, a missing or wrong `scroll-padding-top` (focused elements scrolling under
    the fixed header or pinned notice), a resting tabindex on `<main>` (a click would steer Tab and
    keyboard scrolling to it — caught by the click-then-Tab and click-then-PageDown checks themselves, not
    only the attribute check), a crushed full-height page or a floor that no longer binds, a scrolled
    reader losing their place (the stable content reference must return to its original viewport top ±1px after recovery, including at the page end), a keyboard recovery that leaves focus on `<body>` or `<main>` focusable, a
    missing live-region announcement, an unreachable `try again`, a stale `web/out`, or a deleted offline
    case each fail a named check.
  - *Exit condition:* when the visual harness is consolidated into one toolkit, or the shell header gets
    a fairtrade demo counterpart, fold these gates into it and retire the duplicated boot/probe code.

### 4d. Mounted context / current-parent navigation (real binary, both themes)
- **cmd:** `cd PEASANT/web && CHROME_PATH=$(command -v google-chrome) pnpm visual:context-navigation` (this script boots its own `bin/peasant` on `PEASANT_CONTEXT_NAV_PORT`, default `8793`; `PEASANT_CONTEXT_NAV_SKIP_BUILD=1` reuses an existing `bin/peasant`).
- **what:** drives the child transcript's real WebSocket `session_detail` payload: the stored `context_from`/`started_by` links, the retained earlier-history disclosure written to `?earlier=`, following the current-parent link to the exact stored parent, and Back restoring the child's query, disclosure, selected turn, search, and inner scroll offset. Also asserts a reload and a copied disclosed link reopen the disclosure, and that a child whose target is no longer stored renders an honest unavailable reference.
- **provenance:** before capturing, both the embedded `bin/peasant` and the served `/_next/static/chunks` artifact must carry the host reading-state marker; a stale export fails loud.
- **outputs:** `PEASANT_CONTEXT_NAV_CAPTURE_DIR` (default `/tmp/opencode/context-navigation-captures`)/`<theme>/{context-links,context-child-scrolled,parent-session,context-back-restored,context-back-search-reopened,context-reloaded,context-copied-link,unresolved-parent}.png` — review artifacts, never committed.

### 5. SAME-ENV regression gate (~0%) + boot-arm
- **regression:** `cd PEASANT/web && pnpm sxs:changes -- <base>` (= `SURFACE_SET=changes REF_DIR=changes
  APP_DIR=peasant … stitch-sxs.mjs`) — diffs the COMMITTED app baseline (`baseline/changes/<theme>/`)
  vs a fresh `shoot:changes` capture. **expect worst 0.0000%**; `>0.5%` = REAL drift. RE-BLESS only
  after the user blesses the new render: `pnpm shoot:changes -- <base>` then
  `cp <base>/peasant/<theme>/gmp-*.png baseline/changes/<theme>/`.
- **boot-arm:** `cd PEASANT/web && PEASANT_REAL_ORIGIN=http://localhost:8690 PEASANT_REVIEW_PROJECT=<a sessions project> pnpm boot:peasant`
  with §2's server running → asserts the real `/review` route mounts non-empty through the real
  adapter (`.gmp-changes-root`). **Exit 0 = ok.**

### 6. FAIRTRADE package gates (from FAIRTRADE)
- **cmd:** `cd FAIRTRADE && pnpm build:lib` — builds `/ui` `/graph` `/commons` and runs `test:gates`
  (bundle-isolation + css-token-lint teeth-tests) + `test:contract` (JSDoc contract type-tests) +
  the contrast gate. **expect** exit 0 + `all gate teeth-tests passed` + `per-surface bundle isolation OK`.
- standalone: `cd FAIRTRADE && pnpm test:gates` / `pnpm test:contract`.

### Font guard (keep the `<link>` form, never a CSS `@import`)
- **cmd:** `cd PEASANT/web && pnpm exec vitest run src/test/font-import-guard.test.ts` (also runs in the suite).
- **what:** fails if any SHIPPED source reintroduces a `fonts.css` import or a remote `@import url(https://…)`
  — the Next prod CSS bundler drops a relocated remote `@import`, rendering the WHOLE app mono. Atkinson
  must load via the root-layout `<link rel="stylesheet">` (`layout.tsx`).

### 7. Per-surface gate sequence (a new graph surface, in order)
1. Lift into FAIRTRADE `src/ui/graph|commons` + `build:lib` green.
2. App adapter + compatibility-preserving deprecation + `tsc` 0.
3. `shoot:changes` + `demo-shoot` + stitch (`REF_DIR=demo`) → fidelity composites (open them).
4. `sxs:changes` same-env ~0% + `boot:peasant` exit 0.
5. 3-axis tests (interaction / empty+error+loading / real-data-shape).
6. `full-app-smoke` all-green from the REAL binary.
7. Complete independent review and visual sign-off before updating the baseline.

### Known failure modes (quick index)
- **pnpm unavailable** (§1) — enter `nix develop` or enable the version pinned in `web/package.json`.
- **stale binary** (§1) — the embed + pnpm guards prevent it; old surfaces ⇒ built wrong.
- **font `@import` drop → whole-app mono** (§Font guard) — use `<link>`, not `@import`; the guard test catches it.
- **DIM in fidelity composites** (§4) — a sticky-shell reflow added trailing whitespace; hide the shell only where it overlaps content.
- **dist-empty stale `tsc`** (§0) — re-run FAIRTRADE `build:lib`, then `tsc`.
