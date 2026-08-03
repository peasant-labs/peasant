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
prop shape (`streamPrelude`, `graphSlot` plugging transcript-browser's `@xyflow` engine, peasant's
per-turn label popover in `renderTurnActions`) — but fed a **bundled `sess_demo_0001` fixture**
(`sample-session.ts`) instead of the WebSocket `session_detail` subscription. So a plain `next dev` is
enough: no backend, no mock store, no auth. The route `404`s in a production build (`output: export`),
so it never ships as a public route.

**The app and the canonical fairtrade demo now render the literal same component** —
`<TranscriptViewer>` — not two independent implementations. (Before the transcript composition slice,
the app instead mounted transcript-browser's own `<SessionDetail>` composer, a sibling implementation
that had drifted from the demo with every design-system change — see `SessionDetailV2.tsx`'s history
note. That composer is now deprecated: still published for third-party consumers of
`@peasant-labs/transcript-browser`, but nothing first-party mounts it anymore.) Rendering the SAME
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
era when the app mounted transcript-browser's `<SessionDetail>` composer (`.tb-*`, page-scrolled) —
depicted a composer the app no longer renders and was retired rather than re-blessed.
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
| `peasant-shoot.mjs` | Captures the 10 transcript surfaces for one theme. Most surfaces are captured at the base viewport (`captureBeyondViewport:true`); the full trace canvas (whose content scrolls inside the bounded `.txn-stream`) is captured in FULL by temporarily growing the viewport to the stream's natural height (`shotTall`, ported from fairtrade's own `shootdemo.mjs`). Every capture passes the non-empty `SurfaceGate`. **Two-tier failures** (below). |
| `surface-gate.mjs` | The shared non-empty assertion (byte floor, non-background ratio, distinct-colour count, no byte-identical duplicates). Vendored from the demo side so both are held to the same bar. |
| `boot-peasant.mjs` | Host-integration check (oracle arm 2): boots the **real** `/projects/[name]/[id]` route against a running backend and asserts the composite renders through the real `SessionDetailV2` adapter + WebSocket path (exit 2 if `.txn-app` never mounts). The fixture route is backend-free, so this is the only arm that exercises the real transport. Validates its `PEASANT_PROJECT` coordinate against the live backend first (`validate-mock-coordinates.mjs`) so a stale default fails loud, not as a misleading transport-broken diagnosis. |
| `validate-mock-coordinates.mjs` | Shared fail-fast guard against a recurring bug class: a script/fixture's hardcoded mock project literal drifting from the Go mock's actual catalog. Queries the live `GET /api/v1/projects/summary` (the same endpoint the Home picker uses) before any script boots Puppeteer, and throws an actionable error naming the exact invalid coordinate + the current valid set if it doesn't match. Wired into `boot-peasant.mjs`, `full-app-smoke.mjs`, `shell-nav-gate.mjs`, and `transcript-input-gate.mjs`. |
| `shell-nav-gate.mjs` | Graph shell frame boot check: drives the fairtrade in-use graph demo and the running app through `analytics`, `code map`, and `changes` in both themes, verifies nav order, active state (a filled amber pill — bg-amber/text-on-amber/border-amber — matching the demo's `.iu-subnav-item.active`), theme attrs, route/view mount points, and non-blank body selectors — drilling `code map`/`changes` into a representative `SHELL_PROJECT`-scoped surface past the picker — then writes fairtrade-left/current-app-right full-frame shell captures for the SxS arm. |
| `stitch-sxs.mjs` | Builds the height-matched **REFERENCE \| SUBJECT** composites per surface per theme (`REF_DIR`/`APP_DIR`) into a `SURFACE_SET`-distinct `SXS_OUT_SUBDIR` (`sxs-smoke`/`sxs-shell`/`sxs`). Both panes drawn to the taller height, top-aligned; the shorter is **padded, never scaled**, with a sampled background + a dashed end-hairline. For `transcript`/`changes`/`shell`, a missing subject capture becomes a labeled placeholder so the set stays complete (gate still fails on the missing pair). For `smoke`, a missing reference OR subject capture writes **no placeholder at all** — the surface is skipped and logged as a `FAIL`, so durable smoke evidence can never contain a fake stand-in. **Default `REF_DIR=demo`** pairs the fairtrade demo reference against the app captures (the retired pre-composite-migration `tb` golden was removed — see the Oracle section above). |

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
| `REF_DIR` | `demo` | stitch (reference/left; `demo`=the fairtrade demo reference — cross-component, informational; same-component gates are `changes` and the smoke baseline) |
| `REF_LABEL` | same-component baseline caption | stitch (reference column caption) |
| `APP_DIR` | `peasant` | stitch (subject/right capture subdir) |
| `APP_LABEL` | peasant wiring caption | stitch (subject column caption) |
| `PEASANT_REAL_ORIGIN` | `http://localhost:8690` | boot (backend-served real app origin) |
| `DEMO_URL` | `http://localhost:5180` | shell nav gate fairtrade demo origin |
| `SHELL_CAPTURE_DIR` | `<base>/shell` | shell nav gate current peasant app capture destination |
| `SHELL_REFERENCE_DIR` | `<base>/shell-demo` | shell nav gate fairtrade in-use demo capture destination; generated and ignored |
| `SHELL_PROJECT` | the canonical `ProjectHash` for the mock's `fortuna` project (`SHELL_DEFAULT_PROJECT` in `smoke-surfaces.mjs`) | shell nav gate: mock project used to drill code-map/changes past the picker into a representative, project-scoped surface (must exist in the running app's mock store; a hash, not a label, so the gate's exact-URL check isn't tripped by the legitimate legacy-label-to-hash redirect) |
| `SXS_OUT_SUBDIR` | per `SURFACE_SET` (`smoke`→`sxs-smoke`, `shell`→`sxs-shell`, else `sxs`) | stitch: output subdir for composites, kept DISTINCT per arm so smoke-SxS and shell/nav-SxS evidence can never be confused for one another |
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
  REQUIRED for `/review`.
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
  — a DISTINCT directory from shell/nav SxS (§4c) and the transcript/changes `sxs/` so reviewers never
  mix up which evidence they're looking at.
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

### 4c. Graph shell frame SxS + boot gate
- **cmd:** `cd PEASANT/web && pnpm shell:gate` with §2's peasant server running (mock store MUST include the
  project `SHELL_PROJECT` resolves to — default the mock's `fortuna` project, given as its canonical
  `ProjectHash`) and FAIRTRADE `pnpm dev` serving the in-use demo at `DEMO_URL` (default
  `http://localhost:5180`).
- **what:** captures the fairtrade in-use graph demo full shell frame as the REFERENCE/left side and the current
  peasant app full shell frame as the SUBJECT/right side for `analytics`, `code map`, and `changes` in both
  themes. Here "shell" means the persistent product header, graph section nav, active state, route/view wiring,
  and visible mounted body content below the nav. The peasant side asserts exact three-link order/hrefs,
  `aria-current`, and the active section's FILLED AMBER PILL (bg-amber + on-amber text + amber border, on the
  link element itself — matching the fairtrade demo's `.iu-subnav-item.active`, not an underline marker),
  theme attrs, route mounts, and body selectors. The fairtrade side asserts the canonical graph-demo order,
  active state, theme, graph app selection, and non-blank view body before taking each reference capture.
- **representative body content, not the picker:** the bare `/map` and `/` (changes) nav hrefs land on the
  CROSS-PROJECT picker by design (the production IA). After confirming the nav itself lands there correctly,
  the gate drills into a real, project-scoped surface — `/map/{SHELL_PROJECT}/` (`.mc` canvas) and
  `/review/{SHELL_PROJECT}/` (`.gmp-changes-root`) — for the actual shell capture, so the SxS shows chrome +
  representative mounted content, never the picker/default route. The shell capture also hides any element
  flagged `data-visual-exclude` before the screenshot (a generic hook for interim/transient copy that would
  otherwise skew fairtrade-reference parity comparisons) — the production feature itself is untouched. No
  production element currently carries this flag.
- **outputs:** fairtrade reference captures under `scripts/visual/shell-demo/<theme>/shell-{analytics,map,changes}.png`,
  current peasant captures under `scripts/visual/shell/<theme>/shell-{analytics,map,changes}.png`, and SxS
  composites under `scripts/visual/sxs-shell/<theme>/shell-{analytics,map,changes}.png` — a DISTINCT
  directory from the smoke SxS (§4b) so the two evidence sets can never be confused.
- **fail-closed behavior:** the boot arm exits non-zero on broken nav, missing route/view mounts, blank body
  captures, or missing demo/app server. The SxS arm runs in `IMGDIFF_MODE=presence`: `NO-REF`, `NO-APP`, or zero paired surfaces
  fails, while size/pixel drift is preserved in the composite for human review instead of being treated as
  a pixel-parity gate between different hosts.

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
