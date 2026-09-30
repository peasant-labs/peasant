/* Local shell header gate (the connected arm).

   Drives a running peasant app through the local shell in both themes and holds the mounted header
   to testdata/shell-header.yaml, the required-name manifest the component tests read too
   (shell-header-manifest.mjs):

     1. home `/`: every `show` item is present (settings only once its page ships, so the header
        never carries a dead link), every `hide` item is gone (connection pill, share button, section
        nav), and nothing outside the page body (<main>) links to a route-only section; the header is ONE row of
        fairtrade's --nav-h with every item visible, inside the row and reachable by a pointer; the
        theme button carries the label for the mode it switches to; <main> clears the fixed header;
     2. the command palette, opened the way the header's search button opens it, offers none of the
        forbidden commands (per-project changes/map jumps) and every required one, and no "go to"
        command for a route-only section;
     3. every route-only section still resolves by URL: /analytics, and /review and /map under a mock
        project, each mounting its body non-blank under the SAME quiet header;
     4. the responsive widths in src/test/testdata/shell_responsive.yaml: the same header and
        geometry checks at every width, down to the 320px reflow width;
     5. keyboard after a click: <main> carries no tabindex at rest (chromeClearance, on every page
        above), a click on plain text mid-page on /analytics then Tab moves to the next control
        without jumping the page back up, and a click inside /share's session list (.swz-body) then
        PageDown scrolls that list — a click must never make <main> the focus.

   chromeClearance takes the notice-pinned media query from src/components/testdata/
   app-shell-geometry.yaml (the one declaration globals.css makes) and also checks the root's
   scroll-padding-top against the fixed header.

   Before any capture it proves the server serves THIS checkout's build (served-build.mjs: the served
   page references exactly web/out's chunks, and they carry the shell's markers; with PEASANT_BIN
   set it also checks that binary is not older than web/out), so a stale server or another worktree
   cannot produce mislabelled evidence. It writes a full-frame capture of home and of each
   route per theme (review evidence, never committed) and fails closed when anything is missing,
   blank, overflowing, or linked.

   The app's mode does not matter: a default server and an `--experimental` one must both pass (the
   code-map capability must not bring a route-only section back). The gate logs the served mode
   (from GET /api/v1/config/capabilities); set SHELL_EXPECT_MODE to require one. Start a server
   first, for example:
     ./bin/peasant web start --port 8690 --foreground --no-browser \
       --mock-data-store=web,dashboard,sessions,trends,map,review,qualitySessions,annotations

   Run:
     CHROME_PATH=$(command -v google-chrome) node scripts/visual/shell-nav-gate.mjs

   Env:
     PEASANT_REAL_ORIGIN  origin of the running app (default http://localhost:8690)
     SHELL_CAPTURE_DIR    output root for the shell captures (default <base>/shell)
     SHELL_PROJECT        mock ProjectHash the project-scoped route-only pages open under (default
                          SHELL_DEFAULT_PROJECT in smoke-surfaces.mjs; a hash, not a label, so the
                          exact-path check is not tripped by the label-to-hash canonicalization)
     SHELL_RESPONSIVE_ONLY set to 1 to run only the responsive widths
     SHELL_EXPECT_MODE    default | experimental: fail unless the server advertises exactly that
                          mode (experimental = the code_map_navigation_v1 capability is advertised)
     PEASANT_BIN          the binary serving PEASANT_REAL_ORIGIN (optional): the provenance check
                          then also proves it is not older than web/out
     CHROME_PATH          Chrome/Chromium binary (required)
     PUPPETEER_CORE       explicit puppeteer-core module path (optional)
 */
import { mkdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { parse } from 'yaml'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'
import { SHELL_DEFAULT_PROJECT, SMOKE_THEMES } from './smoke-surfaces.mjs'
import { assertKnownProject } from './validate-mock-coordinates.mjs'
import { assertServedBuild, shellProvenanceMarkers } from './served-build.mjs'
import {
  chromeClearance,
  headerFailures,
  headerGeometryFailures,
  loadNoticePinnedQuery,
  loadShellHeaderManifest,
  paletteFailures,
  shippedItems,
} from './shell-header-manifest.mjs'

const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default

const HERE = dirname(fileURLToPath(import.meta.url))
const BASE = process.argv[2] || HERE
const APP_OUT = process.env.SHELL_CAPTURE_DIR || join(BASE, 'shell')
const CHROME = process.env.CHROME_PATH
const ORIGIN = (process.env.PEASANT_REAL_ORIGIN || 'http://localhost:8690').replace(/\/$/, '')
const SHELL_PROJECT = process.env.SHELL_PROJECT || SHELL_DEFAULT_PROJECT
const RESPONSIVE_ONLY = process.env.SHELL_RESPONSIVE_ONLY === '1'
const EXPECT_MODE = process.env.SHELL_EXPECT_MODE || ''
if (EXPECT_MODE && EXPECT_MODE !== 'default' && EXPECT_MODE !== 'experimental') {
  console.error(`ERROR [shell-nav-gate.mjs] SHELL_EXPECT_MODE must be default or experimental, got ${JSON.stringify(EXPECT_MODE)}.`)
  process.exit(1)
}
const VIEWPORT = { width: 1440, height: 900, deviceScaleFactor: 1 }
const MOBILE_VIEWPORT = { width: 390, height: 844, deviceScaleFactor: 1 }
const RESPONSIVE_HEIGHT = 900
const MIN_BODY_WIDTH = 240
const MIN_BODY_HEIGHT = 120
const THEME_ATTRIBUTES = ['data-theme', 'data-tb-theme']
const FONTS = [
  '400 16px "Atkinson Hyperlegible"', '700 16px "Atkinson Hyperlegible"',
  '400 16px "Atkinson Hyperlegible Mono"', '600 16px "Atkinson Hyperlegible Mono"',
]
const RESPONSIVE_FIXTURE_PATH = join(HERE, '..', '..', 'src', 'test', 'testdata', 'shell_responsive.yaml')
const REQUIRED_WIDTHS = ['desktop', 'tablet', 'compact_tablet', 'mobile_wide', 'mobile_narrow', 'reflow']

const gateError = ({ what, why, where, means, fix }) => new Error([
  `what: ${what}`, `why: ${why}`, `where: ${where}`, `means: ${means}`, `fix: ${fix}`,
].join('\n  '))

// The responsive widths: strict YAML, a unique name and width per row, and every required name.
const loadResponsiveCases = () => {
  const fixtureError = (what, fix) => gateError({
    what,
    why: 'the shell gate must drive a complete, known set of widths',
    where: RESPONSIVE_FIXTURE_PATH,
    means: 'responsive header coverage cannot run, so the gate stops before launching a browser',
    fix,
  })
  let fixture
  try {
    fixture = parse(readFileSync(RESPONSIVE_FIXTURE_PATH, 'utf8'), { strict: true, uniqueKeys: true })
  } catch (cause) {
    throw fixtureError(`the fixture could not be read or parsed: ${cause instanceof Error ? cause.message : String(cause)}`, 'restore a valid fixture')
  }
  if (!fixture || typeof fixture !== 'object' || Array.isArray(fixture) || Object.keys(fixture).join() !== 'cases' || !Array.isArray(fixture.cases)) {
    throw fixtureError('the root must be a mapping with exactly one `cases` list', 'define `cases:` as a list of { name, width } rows')
  }
  const names = new Set()
  const widths = new Set()
  for (const [index, row] of fixture.cases.entries()) {
    if (!row || typeof row !== 'object' || Array.isArray(row) || Object.keys(row).sort().join() !== 'name,width') {
      throw fixtureError(`cases[${index}] must hold exactly name and width, got ${JSON.stringify(row)}`, 'give every row a name and a width')
    }
    if (typeof row.name !== 'string' || row.name.trim() === '' || names.has(row.name)) throw fixtureError(`cases[${index}].name must be unique and non-empty`, 'give every row a stable unique name')
    if (!Number.isInteger(row.width) || row.width <= 0 || widths.has(row.width)) throw fixtureError(`case ${row.name} width must be a unique positive integer`, 'use one CSS viewport width per row')
    names.add(row.name)
    widths.add(row.width)
  }
  const missing = REQUIRED_WIDTHS.filter((name) => !names.has(name))
  if (missing.length) throw fixtureError(`required widths are missing: ${missing.join(', ')}`, 'restore the deleted rows')
  return fixture.cases
}

const manifest = loadShellHeaderManifest()
const shipped = shippedItems(manifest)
const PINNED_QUERY = loadNoticePinnedQuery()
const RESPONSIVE_CASES = loadResponsiveCases()

if (!CHROME) {
  console.error('ERROR [shell-nav-gate.mjs] CHROME_PATH is unset. Set it to your Chrome/Chromium binary before running the shell header gate.')
  process.exit(1)
}

const pause = (ms) => new Promise((r) => setTimeout(r, ms))
const consoleLocationUrl = (m) => {
  const loc = typeof m.location === 'function' ? m.location() : null
  return loc?.url || ''
}
// The mock data provider cannot serve the grouped sessions route the home page asks for (it answers
// 500 on develop too); that one request is a mock-store limitation, not a shell failure.
const isKnownBenignConsoleMessage = (text, locationUrl) =>
  /favicon/i.test(`${text} ${locationUrl}`) ||
  (/\/api\/v1\/sessions\?view=grouped/.test(locationUrl) && /status of 500/.test(text))
const formatConsoleDiagnostic = (m) => {
  const loc = typeof m.location === 'function' ? m.location() : null
  const where = loc?.url ? ` at ${loc.url}:${loc.lineNumber ?? 0}:${loc.columnNumber ?? 0}` : ''
  return `console ${m.type()}${where}: ${m.text()}`
}
const watchDiagnostics = (page) => {
  const diagnostics = []
  page.on('console', (m) => {
    if (m.type() === 'error' && !isKnownBenignConsoleMessage(m.text(), consoleLocationUrl(m))) diagnostics.push(formatConsoleDiagnostic(m))
  })
  page.on('pageerror', (e) => diagnostics.push('pageerr: ' + (e.stack || e.message)))
  return diagnostics
}

const waitForFonts = (page) => page.evaluate(async (faces) => {
  try { await Promise.all(faces.map((f) => document.fonts.load(f))) } catch {}
  await document.fonts.ready
}, FONTS)

const goto = async (page, path) => {
  const response = await page.goto(ORIGIN + path, { waitUntil: 'domcontentloaded' }).catch((e) => {
    throw new Error(`could not load ${ORIGIN + path}: ${e.message}`)
  })
  if (!response || response.status() >= 400) throw new Error(`the app answered HTTP ${response ? response.status() : 0} for ${ORIGIN + path}`)
  await page.waitForSelector('header', { visible: true, timeout: 15000 })
}

// Polls until the header, its geometry and the chrome clearance hold on the current page, then
// returns the last state. Throws with every failure when they never do.
const assertHeader = async (page, theme, where, timeoutMs = 10000) => {
  const start = Date.now()
  let last = null
  while (Date.now() - start < timeoutMs) {
    // The manifest's checks are self-contained, so puppeteer runs the very functions the
    // component tests run.
    const manifestFailures = await page.evaluate(headerFailures, manifest, { theme, shipped })
    const geometry = await page.evaluate(headerGeometryFailures, manifest, { shipped })
    const clearance = await page.evaluate(chromeClearance, PINNED_QUERY)
    const themeFailures = await page.evaluate((attrs, t) => attrs
      .filter((attr) => document.documentElement.getAttribute(attr) !== t)
      .map((attr) => `${attr}=${document.documentElement.getAttribute(attr)}, expected ${t}`), THEME_ATTRIBUTES, theme)
    last = { failures: [...manifestFailures, ...geometry, ...clearance.failures, ...themeFailures], clearance }
    if (last.failures.length === 0) return last
    await pause(150)
  }
  throw new Error(`the ${theme} header at ${where} does not hold the shell manifest: ${JSON.stringify(last)}`)
}

const assertBodyReady = async (page, selector, where, timeoutMs = 15000) => {
  const start = Date.now()
  let last = null
  while (Date.now() - start < timeoutMs) {
    last = await page.evaluate((sel, minWidth, minHeight) => {
      const el = document.querySelector(sel)
      if (!el) return { ok: false, reason: `${sel} did not mount` }
      const rect = el.getBoundingClientRect()
      const style = getComputedStyle(el)
      const text = (el.textContent || '').replace(/\s+/g, ' ').trim()
      const signals = el.querySelectorAll('a, button, canvas, svg, img, [role], [aria-label]').length
      const ok = style.display !== 'none' && style.visibility !== 'hidden' && rect.width >= minWidth && rect.height >= minHeight &&
        rect.bottom > 96 && rect.top < window.innerHeight - 80 && (text.length >= 20 || signals >= 3)
      return { ok, reason: ok ? '' : `size=${Math.round(rect.width)}x${Math.round(rect.height)} top=${Math.round(rect.top)} text=${text.length} signals=${signals}` }
    }, selector, MIN_BODY_WIDTH, MIN_BODY_HEIGHT)
    if (last.ok) return
    await pause(200)
  }
  throw new Error(`the ${where} body ${selector} is missing or blank: ${last?.reason}`)
}

const capture = async (page, gate, theme, id) => {
  const outDir = join(APP_OUT, theme)
  mkdirSync(outDir, { recursive: true })
  const file = join(outDir, `${id}.png`)
  await waitForFonts(page)
  await page.evaluate(() => window.scrollTo(0, 0))
  await pause(100)
  await page.screenshot({ path: file, captureBeyondViewport: false })
  const measured = await gate.assert(id, file, { sel: 'body', where: 'shell-nav-gate.mjs' })
  console.log(`OK peasant-app ${theme}/${id} nonbg=${(measured.nonbgRatio * 100).toFixed(1)}% colors=${measured.distinctColors}`)
  return file
}

// Opens the palette the way the header's search button does, waits until its commands have
// rendered, and holds them to the manifest's palette rules.
const assertPalette = async (page, theme) => {
  await page.evaluate(() => window.dispatchEvent(new Event('peasant:open-command-palette')))
  await page.waitForSelector('[role="dialog"][aria-label="Command palette"] [data-command-id]', { visible: true, timeout: 10000 }).catch(() => {
    throw new Error('the command palette did not open with any command on peasant:open-command-palette')
  })
  // First-open requests settle before the commands are read (a late per-project list would show here).
  await pause(1500)
  const state = {
    failures: await page.evaluate(paletteFailures, manifest),
    ids: await page.evaluate(() => [...document.querySelectorAll('[role="dialog"][aria-label="Command palette"] [data-command-id]')].map((el) => el.getAttribute('data-command-id'))),
  }
  if (state.failures.length) throw new Error(`the ${theme} command palette breaks the shell manifest: ${JSON.stringify(state)}`)
  await page.keyboard.press('Escape')
  await page.waitForFunction(() => !document.querySelector('[role="dialog"][aria-label="Command palette"]'), { timeout: 5000 })
  console.log(`OK peasant-app ${theme} palette offers [${state.ids.join(', ')}]`)
}

const driveTheme = async (browser, theme) => {
  const page = await browser.newPage()
  await page.setViewport(VIEWPORT)
  await applyDeterminism(page)
  await page.evaluateOnNewDocument((t) => { try { localStorage.setItem('peasant-theme', t) } catch {} }, theme)
  const gate = new SurfaceGate(page)
  const diagnostics = watchDiagnostics(page)
  const files = []

  await goto(page, '/')
  await assertBodyReady(page, 'main', 'home')
  const home = await assertHeader(page, theme, '/')
  console.log(`OK peasant-app ${theme} home header chrome=${home.clearance.chrome}px main-offset=${home.clearance.mainPaddingTop}px`)
  files.push(await capture(page, gate, theme, 'shell-home'))
  await assertPalette(page, theme)

  // The narrowest width, in this theme too, as review evidence (the responsive arm asserts every width).
  await page.setViewport(MOBILE_VIEWPORT)
  await goto(page, '/')
  await assertHeader(page, theme, '/ at 390px')
  files.push(await capture(page, gate, theme, 'shell-home-mobile'))
  await page.setViewport(VIEWPORT)

  for (const [path, route] of Object.entries(manifest.routes)) {
    const url = route.project ? `${path}/${SHELL_PROJECT}/` : `${path}/`
    await goto(page, url)
    // The page may canonicalize its own trailing slash (a client-side replace); it must not leave the route.
    const landed = await page.evaluate(() => window.location.pathname)
    const strip = (path) => path.replace(/\/+$/, '')
    if (strip(landed) !== strip(url)) throw new Error(`the route-only page ${url} redirected to ${landed}; it must resolve in place`)
    await assertBodyReady(page, route.mount, url)
    await assertHeader(page, theme, url)
    files.push(await capture(page, gate, theme, `shell-route-${path.slice(1)}`))
  }

  if (diagnostics.length) throw new Error(`client diagnostics appeared while driving the ${theme} shell: ${JSON.stringify(diagnostics.slice(0, 4))}`)
  await page.close()
  return files
}

const assertResponsive = async (browser) => {
  const page = await browser.newPage()
  await applyDeterminism(page)
  const diagnostics = watchDiagnostics(page)
  for (const { name, width } of RESPONSIVE_CASES) {
    await page.setViewport({ width, height: RESPONSIVE_HEIGHT, deviceScaleFactor: 1 })
    await goto(page, '/')
    const theme = await page.evaluate(() => document.documentElement.getAttribute('data-theme'))
    const state = await assertHeader(page, theme, `/ at ${width}px`)
    console.log(`OK peasant-app responsive case=${name} width=${width} header=${state.clearance.header}px one row, every item reachable`)
  }
  if (diagnostics.length) throw new Error(`client diagnostics appeared across the responsive widths: ${JSON.stringify(diagnostics.slice(0, 4))}`)
  await page.close()
}

// A point inside `scope` (a selector, or the viewport) on plain text: an element that holds text
// of its own (not a wrapper around the page) and is not a control, not focusable, and not in the
// header. Clicking it must leave focus where a click on plain text leaves it, so the next Tab or
// keyboard scroll starts from there.
const plainPoint = (page, scope) => page.evaluate((sel) => {
  const box = sel ? document.querySelector(sel)?.getBoundingClientRect() : { left: 0, top: 0, right: window.innerWidth, bottom: window.innerHeight }
  if (!box) return null
  const header = document.querySelector('header')?.getBoundingClientRect()
  const top = Math.max(box.top, header ? header.bottom : 0) + 8
  for (let y = top + (box.bottom - top) / 2; y > top; y -= 24) {
    for (let x = box.left + 16; x < box.right - 16; x += 40) {
      const hit = document.elementFromPoint(x, y)
      if (!hit || hit === document.body || hit.closest('header')) continue
      // Not a control, and not focusable itself (a focusable ancestor such as <main> is exactly what
      // this check must catch, so ancestors' tabindex does not disqualify the point).
      if (hit.hasAttribute('tabindex') || hit.closest('a, button, input, select, textarea, summary, [contenteditable], [role="button"], [role="checkbox"], [role="option"]')) continue
      if (sel && !hit.closest(sel)) continue
      if (!sel && !hit.closest('main')) continue
      const ownText = [...hit.childNodes].some((node) => node.nodeType === Node.TEXT_NODE && node.textContent.trim().length > 2)
      if (!ownText) continue
      return { x, y, tag: hit.tagName.toLowerCase(), text: hit.textContent.replace(/\s+/g, ' ').trim().slice(0, 30) }
    }
  }
  return null
}, scope)

// <main> must never become the focus by a click (review: a resting tabindex on <main> sent Tab back
// to the top of the page and keyboard scrolling to the document instead of the pane clicked). On a
// page that scrolls, a click on plain text mid-page then Tab keeps the reader where they are; in an
// inner scroller, a click on plain content then PageDown scrolls that pane.
const assertKeyboardFromClick = async (browser) => {
  const page = await browser.newPage()
  await applyDeterminism(page)
  const diagnostics = watchDiagnostics(page)

  await page.setViewport({ width: 1440, height: 800, deviceScaleFactor: 1 })
  await goto(page, '/analytics/')
  await assertBodyReady(page, manifest.routes['/analytics'].mount, '/analytics/')
  const scrolled = await page.evaluate(() => { window.scrollTo({ top: 500, behavior: 'instant' }); return window.scrollY })
  if (scrolled < 200) throw new Error(`/analytics/ at 1440×800 does not scroll (reached ${scrolled}px), so the Tab-after-click check cannot run`)
  const text = await plainPoint(page, null)
  if (!text) throw new Error('no plain-content point to click mid-page on /analytics/')
  await page.mouse.click(text.x, text.y)
  await pause(150)
  const before = await page.evaluate(() => ({ scrollY: window.scrollY, active: document.activeElement?.tagName.toLowerCase(), mainTabindex: document.querySelector('main')?.getAttribute('tabindex') }))
  await page.keyboard.press('Tab')
  await pause(250)
  const after = await page.evaluate(() => ({ scrollY: window.scrollY, active: document.activeElement?.tagName.toLowerCase(), label: (document.activeElement?.getAttribute('aria-label') || document.activeElement?.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 40) }))
  const tabFailures = []
  if (before.active === 'main' || after.active === 'main') tabFailures.push(`a click on plain text made <main> the focus (after click: ${before.active}, after Tab: ${after.active})`)
  if (before.mainTabindex !== null) tabFailures.push(`<main> carries tabindex="${before.mainTabindex}" after a click`)
  if (after.active === 'body') tabFailures.push('Tab after the click focused nothing')
  if (after.scrollY < before.scrollY - 150) tabFailures.push(`Tab after a click at ${Math.round(before.scrollY)}px jumped the page to ${Math.round(after.scrollY)}px`)
  if (tabFailures.length) throw new Error(`keyboard focus after a click on /analytics/: ${tabFailures.join('; ')} (clicked ${text.tag} "${text.text}" at ${text.x},${text.y})`)
  console.log(`OK peasant-app keyboard: click on ${text.tag} "${text.text}" at ${Math.round(before.scrollY)}px then Tab focused ${after.active} "${after.label}" at ${Math.round(after.scrollY)}px (no jump, <main> not focused)`)

  await page.setViewport({ width: 1440, height: 600, deviceScaleFactor: 1 })
  await goto(page, '/share/')
  const PANE = '.swz-body'
  await page.waitForSelector(PANE, { visible: true, timeout: 15000 })
  const overflow = await page.evaluate((sel) => { const el = document.querySelector(sel); return el.scrollHeight - el.clientHeight }, PANE)
  if (overflow < 100) throw new Error(`${PANE} on /share/ at 1440×600 does not overflow (${overflow}px), so the PageDown-after-click check cannot run`)
  const inPane = await plainPoint(page, PANE)
  if (!inPane) throw new Error(`no plain-content point to click inside ${PANE} on /share/`)
  await page.mouse.click(inPane.x, inPane.y)
  await pause(150)
  const paneBefore = await page.evaluate((sel) => ({ scrollTop: document.querySelector(sel).scrollTop, active: document.activeElement?.tagName.toLowerCase() }), PANE)
  await page.keyboard.press('PageDown')
  await pause(400)
  const paneAfter = await page.evaluate((sel) => ({ scrollTop: document.querySelector(sel).scrollTop, pageY: window.scrollY, active: document.activeElement?.tagName.toLowerCase() }), PANE)
  if (paneBefore.active === 'main' || paneAfter.scrollTop <= paneBefore.scrollTop) {
    throw new Error(`PageDown after a click inside ${PANE} on /share/ did not scroll it: scrollTop ${paneBefore.scrollTop} → ${paneAfter.scrollTop}, page ${paneAfter.pageY}px, focus ${paneBefore.active} (clicked ${inPane.tag} at ${inPane.x},${inPane.y})`)
  }
  console.log(`OK peasant-app keyboard: click inside ${PANE} then PageDown scrolled it ${Math.round(paneBefore.scrollTop)} → ${Math.round(paneAfter.scrollTop)}px (focus ${paneBefore.active}, not <main>)`)

  if (diagnostics.length) throw new Error(`client diagnostics appeared during the keyboard checks: ${JSON.stringify(diagnostics.slice(0, 4))}`)
  await page.close()
}

// The served mode: experimental advertises the code-map navigation capability.
const servedMode = async () => {
  const response = await fetch(`${ORIGIN}/api/v1/config/capabilities`)
  if (!response.ok) throw new Error(`GET ${ORIGIN}/api/v1/config/capabilities answered HTTP ${response.status}`)
  const body = await response.json()
  const tokens = Array.isArray(body?.uiCapabilities) ? body.uiCapabilities : []
  return { mode: tokens.includes('code_map_navigation_v1') ? 'experimental' : 'default', tokens }
}

// Fail-fast checks BEFORE Puppeteer boots: the mock coordinates (validate-mock-coordinates.mjs),
// the served build, and the served mode.
let served = null
try {
  await assertKnownProject(ORIGIN, SHELL_PROJECT, { where: 'shell-nav-gate.mjs' })
  const provenance = await assertServedBuild({ origin: ORIGIN, markers: shellProvenanceMarkers(manifest), bin: process.env.PEASANT_BIN || undefined })
  console.log(`OK provenance: ${ORIGIN} serves this checkout's web/out (${provenance.chunks.length} chunks${provenance.binChecked ? `, ${process.env.PEASANT_BIN} not older than it` : ''}) carrying ${Object.entries(provenance.markerChunks).map(([marker, chunks]) => `${marker} in ${chunks.join(' ')}`).join('; ')}`)
  served = await servedMode()
  const { mode, tokens } = served
  if (EXPECT_MODE && mode !== EXPECT_MODE) throw new Error(`the server at ${ORIGIN} runs in ${mode} mode (capabilities ${JSON.stringify(tokens)}), but SHELL_EXPECT_MODE=${EXPECT_MODE}`)
  console.log(`OK mode: ${ORIGIN} runs in ${mode} mode (capabilities ${JSON.stringify(tokens)})`)
} catch (e) {
  console.error(`ERROR [shell-nav-gate.mjs] ${e.message}`)
  process.exit(2)
}

const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: VIEWPORT })
const captured = []
try {
  if (!RESPONSIVE_ONLY) {
    for (const theme of SMOKE_THEMES) captured.push(...await driveTheme(browser, theme))
    await assertKeyboardFromClick(browser)
  }
  await assertResponsive(browser)
} catch (e) {
  console.error(
    `ERROR [shell-nav-gate.mjs] local shell header gate failed.\n` +
    `  What failed: ${e.message}\n` +
    `  Why: the local header must match testdata/shell-header.yaml on every page and width, the palette must offer no route-only jump, every route-only section must still resolve, and a click must never steer Tab or keyboard scrolling to <main>.\n` +
    `  Where: shell-nav-gate.mjs while driving ${ORIGIN}.\n` +
    `  Means: users may see a removed control, a dead or hidden-section link, a clipped header, or a route that stopped resolving.\n` +
    `  Fix: serve a fresh build at PEASANT_REAL_ORIGIN (make build, then bin/peasant web start with the mock store), then fix the reported header, palette or route.`,
  )
  try { await browser.close() } catch {}
  process.exit(1)
}
await browser.close()
console.log(`\nOK [shell-nav-gate.mjs] local shell header verified at ${ORIGIN} (${served.mode} mode, provenance checked): manifest holds on home and ${Object.keys(manifest.routes).join(', ')} in ${SMOKE_THEMES.join(' + ')}, palette clean, one-row header at ${RESPONSIVE_CASES.map(({ width }) => width).join(', ')}px.`)
if (captured.length) {
  console.log('Shell frames:')
  for (const file of captured) console.log(`  ${file}`)
}
