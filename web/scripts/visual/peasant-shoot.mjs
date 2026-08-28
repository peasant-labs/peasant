/* Screenshot the REAL assembled peasant transcript view (the dev visual-harness route):
     wire SessionDetailPayload -> the shared @peasant-labs/fairtrade/ui <TranscriptViewer> composite
     via the SAME adaptTranscript wire adapter peasant's SessionDetailV2 adapter uses (detected phases,
     derived pattern annotations feeding computeAnalytics, peasant's per-turn label popover in the
     renderTurnActions slot), with fairtrade's `/graph` @xyflow TrajectoryGraph plugged into the graph
     slot — the one engine that entry still owns.

   Correction: this harness used to mount a sibling package's OWN `<SessionDetail>` composer (`.tb-*`
   classes, page-scroll model) — a sibling implementation that drifted from the canonical demo with every
   design-system change. `SessionDetailV2` moved onto the shared `<TranscriptViewer>` composite directly
   (the same one the canonical demo renders), and this harness + capture script were corrected to match.

   Mirrors the surface set of the canonical fairtrade demo (fairtrade's scripts/shootdemo.mjs) so the
   shots pair 1:1 with the DEMO captures — both now render literally the same composite with `.txn-*`
   classes. The harness route bundles the SAME session the demo renders (sess_demo_0001) and mounts the
   composite with no backend/auth, so a plain `next dev` is enough.

   Scroll model: the composite owns exactly ONE bounded inner scroller — `.txn-stream` — not the page.
   The harness host gives `.txn-app` a fixed height (viewport minus the harness's own 44px chrome strip),
   matching production's `--app-header-height` contract, so the page itself never overflows. A surface
   whose content scrolls inside `.txn-stream` (the full trace canvas) is captured in FULL by temporarily
   growing the viewport to the stream's natural (scrollHeight) size — `shotTall`, ported from fairtrade's
   shootdemo.mjs — then restoring the base viewport afterwards so the scroll-reveal surfaces (scrubber,
   rails) still behave normally. Theme is toggled via the harness route's own `.theme-btn` (which sets
   `[data-theme]` on the document element, the way the real app's theme control does), not a URL param.

   Every successful capture is run through the non-empty-surface gate (SurfaceGate.assert) BEFORE it is
   accepted, so a surface that paints blank (e.g. an empty graph) fails LOUD and is recorded as a gap
   instead of silently passing a vacuous side-by-side. Each surface is wrapped so one failure records a
   gap and the run continues — maximising artifacts + an honest gap list for the manifest.

   env:
     PEASANT_URL     dev-server URL of the harness route   (default http://localhost:3000/dev/visual-harness)
     CHROME_PATH     Chrome/Chromium binary puppeteer drives    (required)
     PUPPETEER_CORE  explicit module path to puppeteer-core     (optional; only if a bare import won't resolve)
   usage: PEASANT_URL=http://localhost:3000/dev/visual-harness CHROME_PATH=/path/to/chrome node peasant-shoot.mjs <theme> <outdir>
*/
import { mkdirSync, statSync } from 'node:fs'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'
const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default

const CHROME = process.env.CHROME_PATH
const URL = process.env.PEASANT_URL || 'http://localhost:3000/dev/visual-harness'
const theme = process.argv[2] || 'dark'
const out = process.argv[3] || `/tmp/peasant-${theme}`
mkdirSync(out, { recursive: true })

const BASE_VP = { width: 1460, height: 1000, deviceScaleFactor: 1 }
const CAP = 4000

const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: { ...BASE_VP } })
const page = await browser.newPage()
await applyDeterminism(page) // reduced-motion + frozen clock/PRNG (set BEFORE goto) so each capture is deterministic for imgdiff
const errs = []
page.on('console', (m) => { if (m.type() === 'error' && !/favicon|404|hydrat/.test(m.text())) errs.push(m.text()) })
page.on('pageerror', (e) => errs.push('pageerr: ' + e.message))
await page.goto(URL, { waitUntil: 'networkidle0' })
await new Promise((r) => setTimeout(r, 900))

const pause = (ms) => new Promise((r) => setTimeout(r, ms))
const results = [] // { name, status, info }
const gate = new SurfaceGate(page) // non-empty-surface assertion, per run (tracks duplicates too)

/* TWO-TIER FAILURE CONTRACT
   - STRUCTURAL gates (the run is invalid as a whole) → HARD non-zero exit, abort the run:
       * theme-didn't-flip       (every capture would be the wrong theme)
       * composite-not-rendered / stream-not-scrolling  (the composite never mounted, or its inner
         `.txn-stream` scroller never overflowed, so there is nothing to capture)
       * a surface exceeds the >CAP raster ceiling → flagged structural, run finishes, exits non-zero.
   - PER-SURFACE gaps (one surface failed, the rest are fine) → recorded + the run CONTINUES, exit 0
       (selector never mounted, a popover didn't open, a single blank/duplicate SurfaceGate failure). */
let structuralFail = false
const die = async (code, what) => {
  console.error(`\nSTRUCTURAL FAILURE [peasant-shoot.mjs] — ${what}\n  the run is aborted; the captures would be invalid. Exiting ${code}.`)
  try { await browser.close() } catch { /* already closing */ }
  process.exit(code)
}

/* ── theme: default is dark; click .theme-btn once to reach light, then verify [data-theme] ── */
if (theme === 'light') {
  await page.evaluate(() => document.querySelector('.theme-btn')?.click())
  await pause(500)
}
const activeTheme = await page.evaluate(() => document.documentElement.getAttribute('data-theme'))
if (activeTheme !== theme) {
  await die(3, `theme-didn't-flip: requested theme="${theme}" but [data-theme]="${activeTheme}" after clicking .theme-btn`)
}

/* ── structural: the composite must have mounted and its inner `.txn-stream` scroller must overflow
   (the fixture has enough turns to actually need scrolling). The page itself must NOT overflow — the
   composite owns exactly one bounded inner scroller, not the document. ── */
{
  try { const s = Date.now(); while (Date.now() - s < 5000) { if (await page.$('.txn-app')) break; await pause(100) } } catch { /* checked below */ }
  const st = await page.evaluate(() => {
    const sc = document.querySelector('.txn-stream')
    return {
      mounted: !!document.querySelector('.txn-app'),
      streamScrollHeight: sc ? sc.scrollHeight : 0,
      streamClientHeight: sc ? sc.clientHeight : 0,
      pageScrollHeight: document.documentElement.scrollHeight,
      pageInnerHeight: window.innerHeight,
    }
  })
  if (!st.mounted) await die(4, 'composite-not-rendered: ".txn-app" never mounted on the harness route — the <TranscriptViewer> composite did not render the fixture (is the dev server up + the route reachable?)')
  if (st.streamScrollHeight <= st.streamClientHeight) await die(5, `stream-not-scrolling: .txn-stream scrollHeight (${st.streamScrollHeight}) <= clientHeight (${st.streamClientHeight}) — the transcript stream did not overflow its bounded container (fixture too short, or the "full trace" tab / list view-mode is not active by default)`)
  if (st.pageScrollHeight > st.pageInnerHeight + 4) await die(6, `second-scroll-owner: document.scrollHeight (${st.pageScrollHeight}) > innerHeight (${st.pageInnerHeight}) — the PAGE overflows, meaning something outside the composite's bounded .txn-app host is growing; the composite must own the ONLY scroller`)
}

/* ── nav helpers (the composite's tab strip + list/graph segmented toggle + tool switch) ── */
const waitFor = async (sel, timeoutMs = 3000) => {
  const start = Date.now()
  while (Date.now() - start < timeoutMs) { const el = await page.$(sel); if (el) return el; await pause(80) }
  throw new Error(`selector "${sel}" never mounted (${timeoutMs}ms)`)
}
const resetView = async () => {
  await page.setViewport({ ...BASE_VP })
  await page.evaluate(() => window.scrollTo(0, 0))
  await pause(150)
}
const txnTab = async (label) => {
  const ok = await page.evaluate((label) => {
    const b = [...document.querySelectorAll('.txn-tab')].find((x) => x.textContent.toLowerCase().includes(label))
    if (!b) return false; b.click(); return true
  }, label)
  if (!ok) throw new Error(`tab "${label}" not found`)
  await pause(450)
}
// the list/graph view-mode toggle: the composite's own scoped segmented control (.txn-viewtoggle .bs-seg-opt).
const viewMode = async (mode) => {
  const ok = await page.evaluate((mode) => {
    const btns = [...document.querySelectorAll('.txn-viewtoggle .bs-seg-opt')]
    let b = btns.find((x) => (x.textContent || '').toLowerCase().includes(mode))
    if (!b) b = btns[mode === 'graph' ? 1 : 0]
    if (!b) return false; b.click(); return true
  }, mode)
  if (!ok) throw new Error(`view-mode "${mode}" not found`)
  await pause(550)
}
// Best-effort: expanding tool calls enriches the trace shot but is not essential; a miss warns, never aborts.
const expandAllTools = async () => {
  const ok = await page.evaluate(() => {
    const sw = [...document.querySelectorAll('.txn-viewsw')].find((x) => /expand all tool calls/i.test(x.textContent))
    const btn = sw?.querySelector('button[role="switch"], button') || document.querySelector('button[aria-label="expand all tool calls"]')
    if (!btn) return false; btn.click(); return true
  })
  if (!ok) console.error('note: "expand all tool calls" switch not found — trace captured with tools collapsed')
  await pause(500)
}
// scroll the .txn-stream past the sticky threshold (56px) so the condensed scrubber header reveals.
const revealScrubber = async () => {
  const scrolled = await page.evaluate(() => {
    const sc = document.querySelector('.txn-stream')
    if (!sc) return false
    sc.scrollTop = 240
    sc.dispatchEvent(new Event('scroll', { bubbles: true })) // dispatch synchronously so React's onScroll fires
    return true
  })
  if (!scrolled) throw new Error('.txn-stream not found — cannot reveal the scrubber (is "full trace" + list view-mode active?)')
  const scrub = await waitFor('.txn-scrub', 2000).catch(() => null)
  if (!scrub) throw new Error('.txn-scrub did not mount after scrolling .txn-stream past the 56px sticky threshold — the fixture stream may be too short')
  await pause(200)
}
// reset the inner stream scroll to top and wait for the sticky header to clear.
const resetStreamScroll = async () => {
  await page.evaluate(() => {
    const sc = document.querySelector('.txn-stream')
    if (sc) { sc.scrollTop = 0; sc.dispatchEvent(new Event('scroll', { bubbles: true })) }
  })
  const cleared = await page.evaluate(() => new Promise((resolve) => {
    let n = 0
    const check = () => { if (!document.querySelector('.txn-sticky')) return resolve(true); if (++n > 40) return resolve(false); setTimeout(check, 25) }
    check()
  }))
  if (!cleared) await pause(400)
}

/* capture a surface fully in view: the composite is height-bounded (no page overflow, verified by the
   structural gate above), so a plain element screenshot with captureBeyondViewport:true rasters the
   whole `.txn-app` (or a sub-region) in one frame without growing the viewport. Asserts non-empty. */
const shotFull = async (name, sel) => {
  await waitFor(sel)
  const el = await page.$(sel)
  const box = await el.boundingBox()
  if (!box || box.width < 4 || box.height < 4) throw new Error(`"${sel}" blank/zero-size: ${JSON.stringify(box)}`)
  if (box.height > CAP) {
    structuralFail = true
    console.error(`STRUCTURAL: "${sel}" is ${Math.round(box.height)}px tall (> ${CAP}px raster ceiling) — captured, but the run will exit non-zero. Raise CAP or split the surface.`)
  }
  await el.screenshot({ path: `${out}/${name}.png`, captureBeyondViewport: true })
  await gate.assert(name, `${out}/${name}.png`, { sel, where: 'peasant-shoot.mjs' })
  const bytes = statSync(`${out}/${name}.png`).size
  results.push({ name, status: 'ok', info: `${Math.round(box.width)}x${Math.round(box.height)} ${(bytes / 1024).toFixed(1)}KB` })
  console.log('shot', name.padEnd(22), `${Math.round(box.width)}x${Math.round(box.height)}`.padEnd(11), `${(bytes / 1024).toFixed(1)}KB`)
}

/* capture a surface whose content scrolls inside `.txn-stream` IN FULL (ported from fairtrade's
   shootdemo.mjs shotTall): temporarily grow the viewport so the whole stream lays out on-screen — every
   turn card, thinking block, per-kind tool renderer, phase/task/checkpoint marker, and nested subagent
   turn — then live-capture it (captureBeyondViewport:false, since growing the viewport IS the mechanism;
   an off-screen raster would just re-clip at the original bound). Capped at CAP px; a stream taller than
   that fails loud rather than silently clipping. Restores the base viewport afterwards. */
const shotTall = async (name, sel = '.txn-app', scroller = '.txn-stream') => {
  await waitFor(sel)
  const baseVp = page.viewport()
  const extra = await page.evaluate((s) => { const el = document.querySelector(s); return el ? Math.max(0, el.scrollHeight - el.clientHeight) : 0 }, scroller)
  const tallH = Math.min(baseVp.height + extra + 24, CAP)
  await page.setViewport({ ...baseVp, height: tallH })
  await pause(350)
  const el = await page.$(sel)
  const box = await el.boundingBox()
  if (!box || box.width < 4 || box.height < 4) {
    await page.setViewport(baseVp)
    throw new Error(`"${sel}" blank/zero-size after growing the viewport to ${tallH}px: ${JSON.stringify(box)}`)
  }
  if (box.y + box.height > tallH + 0.5) {
    structuralFail = true
    console.error(`STRUCTURAL: full-stream "${sel}" extends to y=${(box.y + box.height).toFixed(0)} but the viewport is capped at ${tallH}px — the stream (${scroller}) is taller than the ${CAP}px raster ceiling; captured (clipped), but the run will exit non-zero.`)
  }
  await el.screenshot({ path: `${out}/${name}.png`, captureBeyondViewport: false })
  await gate.assert(name, `${out}/${name}.png`, { sel, where: 'peasant-shoot.mjs' })
  const bytes = statSync(`${out}/${name}.png`).size
  results.push({ name, status: 'ok', info: `${Math.round(box.width)}x${Math.round(box.height)} ${(bytes / 1024).toFixed(1)}KB (full stream)` })
  console.log('shot', name.padEnd(22), `${Math.round(box.width)}x${Math.round(box.height)}`.padEnd(11), `${(bytes / 1024).toFixed(1)}KB (full stream)`)
  await page.setViewport(baseVp)
  await pause(300)
}

/* capture an element in place (no scroll/viewport reset) — used for transient on-screen overlays (the
   revealed sticky condensed header, the label popover) where the caller has already set up the view. */
const shotFold = async (name, sel) => {
  const el = await waitFor(sel)
  const box = await el.boundingBox()
  if (!box || box.width < 4 || box.height < 4) throw new Error(`"${sel}" blank/zero-size: ${JSON.stringify(box)}`)
  await el.screenshot({ path: `${out}/${name}.png`, captureBeyondViewport: true })
  await gate.assert(name, `${out}/${name}.png`, { sel, where: 'peasant-shoot.mjs' })
  const bytes = statSync(`${out}/${name}.png`).size
  results.push({ name, status: 'ok', info: `${Math.round(box.width)}x${Math.round(box.height)} ${(bytes / 1024).toFixed(1)}KB` })
  console.log('shot', name.padEnd(22), `${Math.round(box.width)}x${Math.round(box.height)}`.padEnd(11), `${(bytes / 1024).toFixed(1)}KB`)
}

const surface = async (name, fn) => {
  try { await fn() } catch (e) {
    results.push({ name, status: 'GAP', info: e.message })
    console.error('GAP ', name.padEnd(22), e.message)
  }
}

/* ── deep walk: every tab + sub-surface, mirroring the demo's surface set ── */

// highlights tab — carries the scorecard + the highlights outline
await surface('txn-highlights', async () => { await resetView(); await txnTab('highlights'); await shotFull('txn-highlights', '.txn-app') })
await surface('txn-scorecard', async () => { await shotFull('txn-scorecard', '.txn-scorecard') })

// full trace — list canvas: subagent nesting + thinking + per-kind tool renderers + markers. Captured
// IN FULL via shotTall (grows the viewport to the stream's natural height) since the container is a
// bounded inner scroller, not a page-flow element.
await surface('txn-trace-canvas', async () => {
  await resetView()
  await txnTab('full trace'); await viewMode('list'); await expandAllTools()
  await shotTall('txn-trace-canvas')
})

// scrubber — the sticky condensed header (carrying the scrub bar + the condensed title/model) reveals
// once the inner .txn-stream scrolls past the 56px threshold. Clips the .txn-scrub element directly.
await surface('txn-scrubber', async () => {
  await resetView()
  await txnTab('full trace'); await viewMode('list'); await pause(300)
  await revealScrubber()
  await shotFold('txn-scrubber', '.txn-scrub')
  await resetStreamScroll()
})

// rails — the left outline + right filters rail alongside the trace, at scroll-top
await surface('txn-rails', async () => {
  await txnTab('full trace'); await viewMode('list'); await resetStreamScroll()
  await shotFull('txn-rails', '.txn-app')
})

// per-turn label popover overlay — peasant mounts its own TurnLabelPopover via the renderTurnActions
// slot (trigger button[aria-label="Label this turn"] -> the restored outcome+flag modal,
// fairtrade's canonical `TranscriptLabelPopover`: a `.txn-label-scrim`-wrapped `.pop-card
// txn-label-pop` portaled to document.body, with NO role="dialog" of its own — that role only
// belongs to the separate "More labels" secondary picker). Open it, then capture the WHOLE
// viewer with the popover overlay open (pairing with the demo's whole-app-with-popover shot).
await surface('txn-label-popover', async () => {
  await resetView()
  await txnTab('full trace'); await viewMode('list'); await resetStreamScroll()
  const opened = await page.evaluate(() => {
    const b = document.querySelector('button[aria-label="Label this turn"]')
    if (!b) return false; b.scrollIntoView({ block: 'center' }); b.click(); return true
  })
  if (!opened) throw new Error('no per-turn label trigger found (button[aria-label="Label this turn"]) — canLabel may be off or the renderTurnActions slot is unwired')
  await pause(350)
  const pop = await page.$('.txn-label-scrim')
  if (!pop) throw new Error('label trigger clicked but no popover (.txn-label-scrim) mounted')
  await shotFold('txn-label-popover', '.txn-app')
  await page.keyboard.press('Escape'); await pause(250)
})

// trajectory graph view-mode (fairtrade's `/graph` @xyflow engine in the composite's graph slot).
// Captures the WHOLE composite (.txn-app), not the inner .txn-graphslot sub-region: every sibling
// surface below shoots .txn-app, and fairtrade's own shootdemo.mjs does the same for its txn-graph
// shot (`shot('txn-graph')` with no selector override -> its default `.txn-app`) — so this pairs
// 1:1 with both the demo capture and this harness's own other 9 surfaces. Shooting just
// .txn-graphslot regressed to a mis-scoped sliver once the composite's grid layout narrowed that
// slot in graph mode (a separate, pre-existing layout characteristic of the composite itself, not
// something this capture script should crop around).
await surface('txn-graph', async () => {
  await txnTab('full trace'); await resetView()
  await viewMode('graph'); await pause(600)
  await shotFull('txn-graph', '.txn-app')
  await viewMode('list')
})

// remaining tabs
await surface('txn-diffs', async () => { await resetView(); await txnTab('diffs'); await shotFull('txn-diffs', '.txn-app') })
await surface('txn-files', async () => { await resetView(); await txnTab('files'); await shotFull('txn-files', '.txn-app') })
await surface('txn-annotations', async () => { await resetView(); await txnTab('annotations'); await shotFull('txn-annotations', '.txn-app') })

console.log(`\nTHEME=${theme} active=[data-theme]=${activeTheme}`)
console.log('captured:', results.filter((r) => r.status === 'ok').length, 'gaps:', results.filter((r) => r.status === 'GAP').length)
console.log('console errors:', errs.length ? errs.slice(0, 6) : 'none')
// emit a machine-readable summary for the manifest builder
console.log('RESULTS_JSON=' + JSON.stringify(results))
await browser.close()
// PER-SURFACE gaps do not fail the run (exit 0); a STRUCTURAL breach (a surface over the raster
// ceiling) exits non-zero. The other structural gates (theme-didn't-flip / composite-not-rendered /
// stream-not-scrolling / second-scroll-owner) have already aborted via die() above if they tripped.
if (structuralFail) {
  console.error('\nEXIT non-zero: a structural raster-ceiling breach occurred (see STRUCTURAL lines above).')
  process.exit(1)
}
