/* Host-integration BOOT check (the transcript oracle's "no host-integration regression" arm).

   The capture harness (peasant-shoot.mjs) drives a backend-free dev FIXTURE route for determinism, so it
   does NOT exercise peasant's REAL data path: the `/projects/[name]/[id]` route → the SessionDetailV2
   adapter → the WebSocket `session_detail` subscription → the <SessionDetail> composer. This script is
   that missing arm: it boots each REAL viewer surface against a running backend and asserts the composite
   actually renders through the real adapter + transport — so a broken WS path / adapter wiring / host
   shell fails LOUD here even though the fixture-route captures are green.

   It needs a backend with data. The simplest is the served app + mock store on one port (include
   `review` so the lifted Changes arm reaches /api/v1/review):
     make build
     ./bin/peasant web start --port 8690 --foreground --no-browser \
         --mock-data-store=web,sessions,qualitySessions,annotations,review
   then point this at it (PEASANT_REAL_ORIGIN). Or run `next dev` with NEXT_PUBLIC_WS_URL aimed at the
   backend and PEASANT_REAL_ORIGIN=http://localhost:3000.

   Exit codes (held PER SURFACE; first failure exits immediately): 0 = EVERY boot-arm surface rendered a
   non-empty composite; 2 = a surface's real route never mounted (its mount selector never appeared — the
   real adapter/WS path is broken/unreachable); 1 = a surface mounted but failed the non-empty gate.

   env:
     PEASANT_REAL_ORIGIN  origin of the running backend-served app  (default http://localhost:8690)
     PEASANT_PROJECT      project segment of the viewer route        (default fortuna)
     PEASANT_SESSION      session id segment of the viewer route     (default a mock session)
     PEASANT_REAL_URL     full viewer URL, overrides the above
     CHROME_PATH          Chrome/Chromium binary                     (required)
     PUPPETEER_CORE       explicit puppeteer-core module path        (optional)
*/
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'
import { assertKnownProject } from './validate-mock-coordinates.mjs'
const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default

const CHROME = process.env.CHROME_PATH
const ORIGIN = (process.env.PEASANT_REAL_ORIGIN || 'http://localhost:8690').replace(/\/$/, '')
// The mock generator only ever served 'fortuna'/'docs-site' (internal/mock/provider_map.go's
// ProjectSummaries()) — an old sample project was a stale default that returned 404 from project resolution
// step, misdiagnosing as a transport/adapter regression (see validate-mock-coordinates.mjs, wired
// in below as the fail-fast guard for exactly this class of drift).
const PROJECT = process.env.PEASANT_PROJECT || 'fortuna'
const SESSION = process.env.PEASANT_SESSION || 'sess-c3d4e5f6-a7b8-9012-cdef-123456789012'
const URL = process.env.PEASANT_REAL_URL || `${ORIGIN}/projects/${encodeURIComponent(PROJECT)}/${SESSION}/`

if (!CHROME) {
  console.error('ERROR [boot-peasant.mjs] CHROME_PATH is unset — set it to your Chrome/Chromium binary.')
  process.exit(1)
}

// Fail-fast coordinate check BEFORE Puppeteer boots — see validate-mock-coordinates.mjs for why.
// (PEASANT_REAL_URL overrides bypass this check by construction — the caller supplied an explicit
// URL, not a coordinate this module knows how to validate.)
if (!process.env.PEASANT_REAL_URL) {
  try {
    await assertKnownProject(ORIGIN, PROJECT, { where: 'boot-peasant.mjs' })
  } catch (e) {
    console.error(e.message)
    process.exit(2)
  }
}

/* ── BOOT-ARM REGISTRY ──────────────────────────────────────────────────────────────────────────────
   Each entry is ONE real-route surface proven through the REAL data path (adapter + transport) — the arm
   the backend-free fixture-route captures cannot cover. Per-surface exit contract:
     2 = the surface's real route never mounted (`mount` selector never appeared) — adapter/transport broken
     1 = the surface mounted but failed the non-empty gate (rendered blank/duplicate)
     0 = every surface mounted AND passed the non-empty gate

   To add a surface, append an entry below the seed:
     {
       id:       'unique-id',                 // log label + the non-empty-gate key (must be unique)
       url:      `${ORIGIN}/...`,             // the REAL backend-served route that reaches the surface
       mount:    '.css-selector',             // data-ready signal; absence within `timeoutMs` => exit 2
       capture:  '.css-selector',             // element screenshotted + run through the non-empty gate (=> exit 1)
       interact: async (page, pause) => {},   // OPTIONAL: clicks/scrolls to reveal the surface before capture
     }
   The seed below is the current working arm; keep it and add new arms as lifted surfaces land real routes. */
const REVIEW_PROJECT = process.env.PEASANT_REVIEW_PROJECT || PROJECT
const SURFACES = [
  {
    id: 'session-detail',
    url: URL,
    // .txn-app is Fairtrade's shared TranscriptViewer composite, the same one
    // SessionDetailV2 mounts in production. It appears only after the
    // session_detail payload arrives over the real WebSocket path.
    mount: '.txn-app',
    capture: '.txn-app',
    interact: null,
  },
  {
    // the lifted Changes surface through the REAL /api/v1/review path → ReviewSurface adapter
    // → <Changes>. Needs the backend booted with `review` in --mock-data-store. PEASANT_REVIEW_PROJECT
    // selects the project segment (default = PEASANT_PROJECT).
    id: 'review-changes',
    url: `${ORIGIN}/review/${encodeURIComponent(REVIEW_PROJECT)}/`,
    mount: '.gmp-changes-root',
    capture: '.gmp-changes-root',
    interact: null,
  },
  {
    id: 'analytics',
    url: `${ORIGIN}/analytics/`,
    mount: '.gan-root',
    capture: '.gan-root',
    interact: null,
  },
]

const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: { width: 1460, height: 1000, deviceScaleFactor: 1 } })
const page = await browser.newPage()
await applyDeterminism(page) // reduced-motion + frozen clock/PRNG (set BEFORE any navigation) so the real render is deterministic
const HYDRATION_DIAGNOSTIC = /hydrat|Minified React error #?418|React.*#?418|react\.dev\/errors\/418/i
// A resource-load-failure console message's .text() usually does NOT carry the failing URL (only
// .location().url does) — Chrome's automatic favicon.ico fetch is the recurring benign case (this app
// ships no favicon), so the location URL must be checked too, or "Failed to load resource: 404" reads
// as a hydration-adjacent diagnostic every run. Matches full-app-smoke.mjs's already-correct pattern.
const consoleLocationUrl = (m) => {
  const loc = typeof m.location === 'function' ? m.location() : null
  return loc?.url || ''
}
const isKnownBenignConsoleMessage = (text, locationUrl) => !HYDRATION_DIAGNOSTIC.test(text) && /favicon/i.test(`${text} ${locationUrl}`)
const shouldCaptureConsoleMessage = (m) => {
  const text = m.text()
  const locationUrl = consoleLocationUrl(m)
  return HYDRATION_DIAGNOSTIC.test(text) || (m.type() === 'error' && !isKnownBenignConsoleMessage(text, locationUrl))
}
const formatConsoleDiagnostic = (m) => `console ${m.type()}: ${m.text()}`
const errs = []
page.on('console', (m) => { if (shouldCaptureConsoleMessage(m)) errs.push(formatConsoleDiagnostic(m)) })
page.on('pageerror', (e) => errs.push('pageerr: ' + e.message))

const pause = (ms) => new Promise((r) => setTimeout(r, ms))

/* data-ready wait: the mount selector only appears once the real adapter resolves its payload */
const waitFor = async (sel, timeoutMs = 12000) => {
  const start = Date.now()
  while (Date.now() - start < timeoutMs) { const el = await page.$(sel); if (el) return el; await pause(120) }
  return null
}

const gate = new SurfaceGate(page) // one gate across all arms → also rejects two arms capturing a byte-identical view
const failOnNewDiagnostics = async (surface, errStart) => {
  const diagnostics = errs.slice(errStart)
  if (!diagnostics.length) return
  console.error(
    `ERROR [boot-peasant.mjs] host-integration FAILED for "${surface.id}" — client diagnostics appeared while loading ${surface.url}.\n` +
    `  What failed: the real route emitted console/page errors during the boot gate.\n` +
    `  Why: hydration mismatches (including React #418) or runtime exceptions make the acceptance evidence unsafe.\n` +
    `  Where: boot-peasant.mjs console/pageerror listener for "${surface.id}".\n` +
    `  Means: the production route may visibly mismatch or crash for users even though the non-empty screenshot passed.\n` +
    `  Fix: resolve the reported hydration/runtime diagnostics; known favicon-only noise is ignored.\n` +
    `  diagnostics: ${JSON.stringify(diagnostics.slice(0, 4))}`
  )
  await browser.close()
  process.exit(1)
}

for (const s of SURFACES) {
  const errStart = errs.length
  /* navigate to the surface's real route */
  try {
    await page.goto(s.url, { waitUntil: 'networkidle0' })
  } catch (e) {
    console.error(
      `ERROR [boot-peasant.mjs] host-integration FAILED for "${s.id}" — could not load the real viewer route ${s.url}.\n` +
      `  What failed: ${e.message}\n` +
      `  Why: the backend-served app is not reachable at that origin.\n` +
      `  Fix: start a backend with data (peasant web start --mock-data-store=web,sessions,qualitySessions,annotations) and set PEASANT_REAL_ORIGIN to its origin.`
    )
    await browser.close()
    process.exit(2)
  }

  /* optional interaction to reach the surface (tab click, scroll, popover open, …) */
  if (s.interact) {
    try { await s.interact(page, pause) } catch (e) {
      console.error(`ERROR [boot-peasant.mjs] host-integration FAILED for "${s.id}" — interaction to reach the surface threw: ${e.message}`)
      await browser.close()
      process.exit(2)
    }
  }

  /* data-ready mount gate (exit 2) */
  const app = await waitFor(s.mount, 12000)
  if (!app) {
    console.error(
      `ERROR [boot-peasant.mjs] host-integration FAILED for "${s.id}" — the real viewer ("${s.mount}") never mounted at ${s.url}.\n` +
      `  What failed: the SessionDetailV2 adapter did not render a composite within 12s.\n` +
      `  Why: the real data path is broken — the session_detail WebSocket returned nothing (no backend, wrong WS url, unknown session), or the adapter/host shell errored.\n` +
      `  Where: boot-peasant.mjs real-route data-ready wait for "${s.id}".\n` +
      `  Means: the production transcript viewer would render blank — a host-integration regression the fixture-route capture cannot catch.\n` +
      `  Fix: start a backend with data (peasant web start --mock-data-store=web,sessions,qualitySessions,annotations) and point PEASANT_REAL_ORIGIN at it; confirm PEASANT_SESSION (${SESSION}) exists there.\n` +
      `  console errors: ${errs.length ? JSON.stringify(errs.slice(0, 4)) : 'none'}`
    )
    await browser.close()
    process.exit(2)
  }
  await pause(800)
  await failOnNewDiagnostics(s, errStart)

  /* assert the rendered surface is non-empty (not a blank skeleton) via the shared gate (exit 1) */
  const tmp = `/tmp/peasant-boot-${s.id}.png`.replace(/[^\w./-]/g, '_')
  const el = await page.$(s.capture)
  const box = await el.boundingBox()
  await el.screenshot({ path: tmp, captureBeyondViewport: true })
  try {
    const r = await gate.assert(s.id, tmp, { sel: s.capture, where: 'boot-peasant.mjs' })
    await failOnNewDiagnostics(s, errStart)
    console.log(`OK host-integration: "${s.id}" real route rendered ${s.capture} ${Math.round(box.width)}x${Math.round(box.height)} — nonbg=${(r.nonbgRatio * 100).toFixed(1)}% colors=${r.distinctColors}`)
  } catch (e) {
    console.error(e.message)
    await browser.close()
    process.exit(1)
  }
}

if (errs.length) {
  console.error(
    `ERROR [boot-peasant.mjs] host-integration FAILED — client diagnostics appeared after surface checks.\n` +
    `  What failed: the real routes emitted console/page errors during the boot gate.\n` +
    `  Why: diagnostics may arrive after the per-surface non-empty assertion; passing despite them would make the gate unsafe.\n` +
    `  Fix: resolve the reported hydration/runtime diagnostics; known favicon-only noise is ignored.\n` +
    `  diagnostics: ${JSON.stringify(errs.slice(0, 4))}`
  )
  await browser.close()
  process.exit(1)
}

console.log(`OK host-integration: all ${SURFACES.length} boot-arm surface(s) rendered non-empty through the real route.`)
console.log('console/page diagnostics: none')
await browser.close()
