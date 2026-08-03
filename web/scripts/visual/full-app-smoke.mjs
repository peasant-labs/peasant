/* FULL-APP SMOKE — the standing, repeatable gate on the user's canonical path.
 *
 * It builds the real binary the way the user does (`make build` → pnpm primary), boots
 * `bin/peasant web start` with the full mock store, then drives EVERY first-class surface
 * through the REAL Go binary (static-export SPA served from //go:embed web/out, NOT the dev
 * harness) and ASSERTS each is:
 *   1. SERVED        — HTTP 200 on its route,
 *   2. MOUNTED       — its surface selector appears (not just the app shell),
 *   3. NON-BLANK     — the captured element clears the surface-gate ink floor,
 *   4. ATKINSON      — document.fonts.check() confirms the production font path loaded
 *                      (the Next prod CSS @import-drop regression renders the whole app mono).
 * It writes real-binary screenshots to web/scripts/visual/smoke/<theme>/<surface>.png and
 * exits non-zero if any surface fails, so broken /review package resolution or a font
 * regression fails CI loudly.
 *
 * Run:  CHROME_PATH=$(command -v google-chrome) node web/scripts/visual/full-app-smoke.mjs
 * Env:  SMOKE_SKIP_BUILD=1  reuse the existing bin/peasant (skip `make build`)
 *       PEASANT_SMOKE_PORT  server port (default 8699)
 *       SMOKE_PROJECT / SMOKE_SESSION / SMOKE_BRANCH  mock fixture coordinates
 *       PUPPETEER_CORE      explicit puppeteer-core module path
 */
import { execSync, spawn } from 'node:child_process'
import { mkdirSync, existsSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve, join } from 'node:path'
import { measurePng, MIN_NONBG_RATIO } from './surface-gate.mjs'
import { SMOKE_MOCKS, SMOKE_THEMES, makeSmokeSurfaces } from './smoke-surfaces.mjs'
import { assertKnownProject } from './validate-mock-coordinates.mjs'
import { applyDeterminism } from './determinism.mjs'
import { loadStrikeSmokeFixture } from './strike-smoke-fixture.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '../../..')            // .../peasant/<worktree>
const OUT = join(HERE, 'smoke')                   // web/scripts/visual/smoke/<theme>/<surface>.png
const STRIKE_OUT = join(OUT, 'strike')            // web/scripts/visual/smoke/strike/<theme>/<surface>.png
const PORT = process.env.PEASANT_SMOKE_PORT || '8699'
const ORIGIN = `http://localhost:${PORT}`
// The mock generator's project was renamed to 'fortuna' at some point without
// this default following; SESSION/BRANCH below already match 'fortuna's real
// mock data (verified against a live --mock-data-store=...,map,review server),
// which is how this drift was caught — every project surface 404's the
// project-resolve API and renders blank against an old sample project.
const PROJECT = process.env.SMOKE_PROJECT || 'fortuna'
const SESSION = process.env.SMOKE_SESSION || 'sess-c3d4e5f6-a7b8-9012-cdef-123456789012'
const BRANCH = process.env.SMOKE_BRANCH || 'feat/queue-retry'
const CHROME = process.env.CHROME_PATH
const MOCKS = SMOKE_MOCKS
const STRIKE = loadStrikeSmokeFixture()
const STRIKE_MARK_PATH = 'M18.5 6L11 17.5h5.2L13.5 26 21 14.5h-5.2L18.5 6z'

if (!CHROME) { console.error('ERROR [full-app-smoke] CHROME_PATH is unset.'); process.exit(1) }

const ATKINSON = '16px "Atkinson Hyperlegible"'
const FONTS = [
  '400 16px "Atkinson Hyperlegible"', '700 16px "Atkinson Hyperlegible"',
  '400 16px "Atkinson Hyperlegible Mono"', '600 16px "Atkinson Hyperlegible Mono"',
]
/* Each surface: route on the REAL binary + the selector proving the SURFACE (not just the
   shell) mounted. Shared with stitch-sxs so smoke screenshots can become review SxS. */
const SURFACES = makeSmokeSurfaces({ project: PROJECT, session: SESSION, branch: BRANCH })
const THEMES = SMOKE_THEMES
const THEME_ATTRIBUTES = ['data-theme', 'data-tb-theme']
const pause = (ms) => new Promise((r) => setTimeout(r, ms))
const HYDRATION_DIAGNOSTIC = /hydrat|Minified React error #?418|React.*#?418|react\.dev\/errors\/418/i
const consoleLocationUrl = (m) => {
  const loc = typeof m.location === 'function' ? m.location() : null
  return loc?.url || ''
}
const isHydrationConsoleMessage = (text, locationUrl) => HYDRATION_DIAGNOSTIC.test(text) || HYDRATION_DIAGNOSTIC.test(locationUrl)
const isKnownBenignConsoleMessage = (text, locationUrl) => !isHydrationConsoleMessage(text, locationUrl) && /favicon/i.test(`${text} ${locationUrl}`)
const shouldCaptureConsoleMessage = (m) => {
  const text = m.text()
  const locationUrl = consoleLocationUrl(m)
  return isHydrationConsoleMessage(text, locationUrl) || (m.type() === 'error' && !isKnownBenignConsoleMessage(text, locationUrl))
}
const formatConsoleDiagnostic = (m) => {
  const loc = typeof m.location === 'function' ? m.location() : null
  const where = loc?.url ? ` at ${loc.url}:${loc.lineNumber ?? 0}:${loc.columnNumber ?? 0}` : ''
  return `console ${m.type()}${where}: ${m.text()}`
}
const formatClientDiagnostics = (diagnostics) => {
  if (!diagnostics.length) return ''
  const shown = diagnostics.slice(0, 3)
  const more = diagnostics.length > shown.length ? ` (+${diagnostics.length - shown.length} more)` : ''
  return `client diagnostics ${JSON.stringify(shown)}${more}`
}

const strikeError = (step, reason) => new Error(
  `Strike real-binary smoke failed because ${reason} during ${step} in full-app-smoke.mjs; users could not safely discover or read the shared Strike session through the mounted app; inspect the named surface against internal/mock/testdata/strike_mounted_web.yaml, fix the production route or package integration, rebuild, and rerun this smoke.`,
)

async function waitForStrikeSelector(page, selector, step) {
  const element = await page.waitForSelector(selector, { timeout: 15000 }).catch(() => null)
  if (!element) throw strikeError(step, `selector ${JSON.stringify(selector)} never mounted`)
  return element
}

async function waitForPath(page, expected, step) {
  const reached = await page.waitForFunction(
    (value) => window.location.pathname.includes(value),
    { timeout: 15000 },
    expected,
  ).catch(() => null)
  if (!reached) throw strikeError(step, `navigation never reached a path containing ${JSON.stringify(expected)}; current URL is ${page.url()}`)
}

async function captureStrikeSurface(page, theme, id, selector) {
  const element = await waitForStrikeSelector(page, selector, `capturing ${id}`)
  const themeAttributes = await page.evaluate((attrs) => Object.fromEntries(
    attrs.map((attr) => [attr, document.documentElement.getAttribute(attr)]),
  ), THEME_ATTRIBUTES)
  if (!THEME_ATTRIBUTES.every((attribute) => themeAttributes[attribute] === theme)) {
    throw strikeError(`capturing ${id}`, `theme attributes were ${JSON.stringify(themeAttributes)} instead of ${JSON.stringify(theme)}`)
  }
  await page.evaluate(async (faces) => {
    try { await Promise.all(faces.map((face) => document.fonts.load(face))) } catch {}
    await document.fonts.ready
  }, FONTS)
  if (!await page.evaluate((font) => document.fonts.check(font), ATKINSON)) {
    throw strikeError(`capturing ${id}`, 'Atkinson Hyperlegible was not loaded')
  }
  const directory = join(STRIKE_OUT, theme)
  mkdirSync(directory, { recursive: true })
  const file = join(directory, `${id}.png`)
  await element.screenshot({ path: file, captureBeyondViewport: true })
  const buffer = (await import('node:fs')).readFileSync(file)
  const measurement = await measurePng(page, `data:image/png;base64,${buffer.toString('base64')}`)
  if (measurement.nonbgRatio < MIN_NONBG_RATIO) {
    throw strikeError(`capturing ${id}`, `the screenshot was blank at ${(measurement.nonbgRatio * 100).toFixed(2)}% non-background pixels`)
  }
  console.log(`  [${theme}/${id}] mounted=true theme=true atkinson=true nonbg=${(measurement.nonbgRatio * 100).toFixed(1)}% → OK`)
  return file
}

async function assertTranscriptStrikeIdentity(page, step) {
  const result = await page.evaluate(({ assistantContent }) => {
    const app = document.querySelector('.txn-app')
    const mark = app?.querySelector('.txn-meta .brand[data-brand="strike"]')
    const icon = mark?.closest('.pv-icon')
    const tokenProbe = document.createElement('span')
    tokenProbe.style.color = 'var(--clay)'
    document.body.append(tokenProbe)
    const clay = getComputedStyle(tokenProbe).color
    tokenProbe.remove()
    return {
      app: !!app,
      content: [...document.querySelectorAll('.txn-turnwrap')].some((turn) => turn.textContent?.includes(assistantContent)),
      mark: !!mark,
      markPath: mark?.querySelector('path')?.getAttribute('d') ?? '',
      iconColor: icon ? getComputedStyle(icon).color : '',
      clay,
    }
  }, { assistantContent: STRIKE.assistantContent })
  if (!result.app || !result.content || !result.mark || result.markPath !== STRIKE_MARK_PATH || !result.clay || result.iconColor !== result.clay) {
    throw strikeError(step, `transcript identity probe returned ${JSON.stringify(result)}`)
  }
}

async function clickExact(page, selector, text, step) {
  const clicked = await page.evaluate(({ selector, text }) => {
    const element = [...document.querySelectorAll(selector)].find((candidate) => candidate.textContent?.trim() === text)
    if (!element) return false
    element.click()
    return true
  }, { selector, text })
  if (!clicked) throw strikeError(step, `no ${selector} control had exact text ${JSON.stringify(text)}`)
}

async function runStrikeFlow(browser, theme) {
  const page = await browser.newPage()
  const diagnostics = []
  page.on('console', (message) => { if (shouldCaptureConsoleMessage(message)) diagnostics.push(formatConsoleDiagnostic(message)) })
  page.on('pageerror', (error) => diagnostics.push(`pageerr: ${error.stack || error.message}`))
  await applyDeterminism(page, { epochMs: STRIKE.epochMs + 60_000 })
  await page.evaluateOnNewDocument((value) => { try { localStorage.setItem('peasant-theme', value) } catch {} }, theme)
  const captures = []
  try {
    const mapResponse = await page.goto(`${ORIGIN}/map/${encodeURIComponent(STRIKE.projectName)}/`, { waitUntil: 'domcontentloaded' })
    if (mapResponse?.status() !== 200) throw strikeError('opening the Map project rail', `HTTP status was ${mapResponse?.status() ?? 0}`)
    await waitForStrikeSelector(page, '.gmp-root', 'opening the Map project rail')
    const mapConversationReady = await page.waitForFunction(({ sessionId, title }) => {
      const link = [...document.querySelectorAll('a')].find((candidate) => candidate.getAttribute('aria-label') === `Open the conversation ${sessionId}`)
      if (link) link.scrollIntoView({ block: 'center' })
      return !!link && link.textContent?.includes(title)
    }, { timeout: 15000 }, { sessionId: STRIKE.sessionId, title: STRIKE.mapConversationTitle }).catch(() => null)
    if (!mapConversationReady) throw strikeError('opening the Map project rail', 'the shared conversation link and title never rendered')
    captures.push(await captureStrikeSurface(page, theme, 'map-conversation', '.gmp-root'))

    const clickedMapConversation = await page.evaluate((sessionId) => {
      const link = [...document.querySelectorAll('a')].find((candidate) => candidate.getAttribute('aria-label') === `Open the conversation ${sessionId}`)
      if (!link) return false
      link.click()
      return true
    }, STRIKE.sessionId)
    if (!clickedMapConversation) throw strikeError('following the Map conversation link', 'the actual conversation link disappeared before click')
    await waitForPath(page, STRIKE.sessionId, 'following the Map conversation link')
    await waitForStrikeSelector(page, '.txn-app', 'mounting transcript detail from Map')
    await assertTranscriptStrikeIdentity(page, 'mounting transcript detail from Map')
    captures.push(await captureStrikeSurface(page, theme, 'transcript-list', '.txn-app'))

    await clickExact(page, 'button.bs-seg-opt', 'graph', 'activating transcript graph mode')
    await waitForStrikeSelector(page, '.tb-graph', 'activating transcript graph mode')
    const graphIdentity = await page.evaluate(() => {
      const harnessShell = document.querySelector('[data-harness="strike"]')
      const node = harnessShell?.querySelector('.ft-gnode')
      const legend = [...document.querySelectorAll('.ft-graph-legend-item')].find((item) => item.textContent?.trim().toLowerCase() === 'strike')
      const glyph = legend?.querySelector('.ft-graph-legend-glyph')
      const mark = document.querySelector('.txn-meta .brand[data-brand="strike"]')
      const tokenProbe = document.createElement('span')
      tokenProbe.style.background = 'var(--clay)'
      document.body.append(tokenProbe)
      const clayColor = getComputedStyle(tokenProbe).backgroundColor
      tokenProbe.remove()
      return {
        harnessShell: !!harnessShell,
        accent: node ? getComputedStyle(node).getPropertyValue('--ft-gnode-accent').trim() : '',
        clayToken: getComputedStyle(document.documentElement).getPropertyValue('--clay').trim(),
        clayColor,
        legendColor: glyph ? getComputedStyle(glyph).backgroundColor : '',
        markPath: mark?.querySelector('path')?.getAttribute('d') ?? '',
      }
    })
    if (!graphIdentity.harnessShell || !graphIdentity.clayToken || graphIdentity.accent !== graphIdentity.clayToken || !graphIdentity.clayColor || graphIdentity.legendColor !== graphIdentity.clayColor || graphIdentity.markPath !== STRIKE_MARK_PATH) {
      throw strikeError('activating transcript graph mode', `graph identity probe returned ${JSON.stringify(graphIdentity)}`)
    }
    captures.push(await captureStrikeSurface(page, theme, 'transcript-graph', '.txn-app'))

    await page.goBack({ waitUntil: 'domcontentloaded' }).catch(() => null)
    await waitForStrikeSelector(page, '.gmp-navigator', 'returning from transcript to Map')
    const selectedNode = await page.evaluate(() => {
      const row = [...document.querySelectorAll('.gmp-navigator-row')].find((candidate) => candidate.textContent?.trim().startsWith('internal'))
      if (!row) return false
      row.click()
      return true
    })
    if (!selectedNode) throw strikeError('selecting a Map code area', 'the internal navigator row was unavailable')
    await waitForStrikeSelector(page, '[aria-label="Clear node selection"]', 'selecting a Map code area')
    const commitIdentity = await page.waitForFunction(({ hash, subject, sessionId, title, markPath }) => {
      const group = [...document.querySelectorAll('ul')].find((candidate) => candidate.getAttribute('aria-label') === `Conversations behind commit ${subject}`)
      const link = [...(group?.querySelectorAll('a') ?? [])].find((candidate) => candidate.getAttribute('href')?.includes(sessionId))
      const mark = link?.querySelector('.brand[data-brand="strike"]')
      if (link) link.scrollIntoView({ block: 'center' })
      return !!link && group?.closest('li')?.textContent?.includes(hash.slice(0, 7)) && link.textContent?.includes(title) && mark?.querySelector('path')?.getAttribute('d') === markPath
    }, { timeout: 15000 }, {
      hash: STRIKE.mapCommitHash,
      subject: STRIKE.mapCommitSubject,
      sessionId: STRIKE.sessionId,
      title: STRIKE.mapConversationTitle,
      markPath: STRIKE_MARK_PATH,
    }).catch(() => null)
    if (!commitIdentity) throw strikeError('selecting a Map code area', 'the shared Strike provider/commit row never rendered')
    captures.push(await captureStrikeSurface(page, theme, 'map-commit', '.gmp-root'))

    const clickedCommit = await page.evaluate((sessionId) => {
      const link = [...document.querySelectorAll('a')].find((candidate) => candidate.getAttribute('href')?.includes(sessionId) && candidate.querySelector('.brand[data-brand="strike"]'))
      if (!link) return false
      link.click()
      return true
    }, STRIKE.sessionId)
    if (!clickedCommit) throw strikeError('following the Map commit conversation', 'the actual provider/commit link disappeared before click')
    await waitForPath(page, STRIKE.sessionId, 'following the Map commit conversation')
    await waitForStrikeSelector(page, '.txn-app', 'following the Map commit conversation')
    await assertTranscriptStrikeIdentity(page, 'following the Map commit conversation')

    const reviewResponse = await page.goto(`${ORIGIN}/review/${encodeURIComponent(STRIKE.projectName)}/`, { waitUntil: 'domcontentloaded' })
    if (reviewResponse?.status() !== 200) throw strikeError('opening Review/Changes', `HTTP status was ${reviewResponse?.status() ?? 0}`)
    await waitForStrikeSelector(page, '.gmp-changes-root', 'opening Review/Changes')
    const reviewIdentity = await page.waitForFunction(({ title, markPath }) => {
      const button = [...document.querySelectorAll('button')].find((candidate) => candidate.textContent?.includes(title) && candidate.querySelector('.brand[data-brand="strike"]'))
      const mark = button?.querySelector('.brand[data-brand="strike"]')
      if (button) button.scrollIntoView({ block: 'center' })
      return !!button && mark?.querySelector('path')?.getAttribute('d') === markPath
    }, { timeout: 15000 }, { title: STRIKE.reviewSessionTitle, markPath: STRIKE_MARK_PATH }).catch(() => null)
    if (!reviewIdentity) throw strikeError('opening Review/Changes', 'the shared Strike session action and official mark never rendered')
    captures.push(await captureStrikeSurface(page, theme, 'review-changes', '.gmp-changes-root'))

    const clickedReviewSession = await page.evaluate((title) => {
      const button = [...document.querySelectorAll('button')].find((candidate) => candidate.textContent?.includes(title) && candidate.querySelector('.brand[data-brand="strike"]'))
      if (!button) return false
      button.click()
      return true
    }, STRIKE.reviewSessionTitle)
    if (!clickedReviewSession) throw strikeError('following the Review session action', 'the actual Changes callback disappeared before click')
    await waitForPath(page, STRIKE.sessionId, 'following the Review session action')
    await waitForStrikeSelector(page, '.txn-app', 'following the Review session action')
    await assertTranscriptStrikeIdentity(page, 'following the Review session action')

    if (diagnostics.length > 0) throw strikeError('finishing the interaction flow', formatClientDiagnostics(diagnostics))
    return captures
  } finally {
    await page.close()
  }
}

// ── 1. build (the user's exact path) ──────────────────────────────────────────
if (!process.env.SMOKE_SKIP_BUILD) {
  console.log('[smoke] make build (user canonical path) …')
  execSync('make build', { cwd: REPO, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })
  console.log('[smoke] build path: pnpm (required)')
} else {
  console.log('[smoke] SMOKE_SKIP_BUILD=1 — reusing existing bin/peasant')
}
const BIN = join(REPO, 'bin/peasant')
if (!existsSync(BIN)) { console.error(`ERROR [full-app-smoke] ${BIN} not found — run \`make build\` first or unset SMOKE_SKIP_BUILD.`); process.exit(1) }

// ── 2. boot the real binary ───────────────────────────────────────────────────
console.log(`[smoke] starting ${BIN} on :${PORT} (mock store: ${MOCKS}) …`)
const server = spawn(BIN, ['web', 'start', '--port', PORT, '--foreground', '--no-browser', `--mock-data-store=${MOCKS}`], { cwd: REPO, stdio: ['ignore', 'ignore', 'inherit'] })
let serverDown = false
server.on('exit', () => { serverDown = true })

const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default
let browser
const teardown = async () => { try { if (browser) await browser.close() } catch {} ; try { if (!serverDown) server.kill('SIGTERM') } catch {} }

const httpGet = async (path) => {
  const res = await fetch(ORIGIN + path).catch(() => null)
  return res ? res.status : 0
}
// poll health
let healthy = false
for (let i = 0; i < 40 && !healthy; i++) { if (await httpGet('/api/v1/health') === 200) healthy = true; else await pause(500) }
if (!healthy) { console.error('ERROR [full-app-smoke] server never became healthy.'); await teardown(); process.exit(2) }

// Fail-fast coordinate check BEFORE driving any surface — see validate-mock-coordinates.mjs for why
// (this is the systemic guard against the recurring stale-mock-project-default class of bug).
try {
  await assertKnownProject(ORIGIN, PROJECT, { where: 'full-app-smoke.mjs' })
} catch (e) {
  console.error(e.message)
  await teardown()
  process.exit(2)
}

// ── 3. drive every surface, both themes ───────────────────────────────────────
const rows = []
const strikeCaptures = []
const strikeFailures = []
try {
  browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: { width: 1460, height: 1000, deviceScaleFactor: 1 } })
  for (const theme of THEMES) {
    mkdirSync(join(OUT, theme), { recursive: true })
    for (const s of SURFACES) {
      const row = { theme, id: s.id, served: 0, mounted: false, themeAttrs: false, atkinson: false, nonbg: 0, ok: false, note: '' }
      const page = await browser.newPage()
      const clientDiagnostics = []
      // Capture diagnostics for every first-class surface; only favicon noise is benign.
      page.on('console', (m) => { if (shouldCaptureConsoleMessage(m)) clientDiagnostics.push(formatConsoleDiagnostic(m)) })
      page.on('pageerror', (e) => clientDiagnostics.push('pageerr: ' + (e.stack || e.message)))
      try {
        // drive the theme exactly like the app: the real useTheme reads localStorage `peasant-theme`
        await page.evaluateOnNewDocument((t) => { try { localStorage.setItem('peasant-theme', t) } catch {} }, theme)
        let resp = null
        for (let attempt = 1; attempt <= 3 && !row.mounted; attempt++) {
          resp = await page.goto(ORIGIN + s.path, { waitUntil: 'domcontentloaded' }).catch(() => null)
          const el = await page.waitForSelector(s.mount, { timeout: 15000 }).catch(() => null)
          if (el) row.mounted = true
        }
        row.served = resp ? resp.status() : 0
        const themeAttrs = await page.evaluate((attrs) => {
          const root = document.documentElement
          return Object.fromEntries(attrs.map((attr) => [attr, root.getAttribute(attr)]))
        }, THEME_ATTRIBUTES)
        row.themeAttrs = THEME_ATTRIBUTES.every((attr) => themeAttrs[attr] === theme)
        await page.evaluate(async (faces) => { try { await Promise.all(faces.map((f) => document.fonts.load(f))) } catch {} ; await document.fonts.ready }, FONTS)
        await pause(800)
        row.atkinson = await page.evaluate((f) => !!(document.fonts && document.fonts.check(f)), ATKINSON)
        if (row.mounted) {
          const file = join(OUT, theme, `${s.id}.png`)
          const el = await page.$(s.cap)
          await el.screenshot({ path: file, captureBeyondViewport: true })
          const buf = (await import('node:fs')).readFileSync(file)
          const m = await measurePng(page, 'data:image/png;base64,' + buf.toString('base64'))
          row.nonbg = m.nonbgRatio
        }
        row.ok = row.served === 200 && row.mounted && row.themeAttrs && row.atkinson && row.nonbg >= MIN_NONBG_RATIO && clientDiagnostics.length === 0
        if (!row.ok) row.note = [row.served !== 200 && `HTTP ${row.served}`, !row.mounted && `no ${s.mount}`, !row.themeAttrs && `theme attrs ${JSON.stringify(themeAttrs)}`, !row.atkinson && 'NO Atkinson', row.nonbg < MIN_NONBG_RATIO && `blank ${(row.nonbg * 100).toFixed(2)}%`, formatClientDiagnostics(clientDiagnostics)].filter(Boolean).join(', ')
      } catch (e) {
        row.note = String(e).slice(0, 80)
      }
      rows.push(row)
      console.log(`  [${theme}/${s.id}] served=${row.served} mounted=${row.mounted} theme=${row.themeAttrs} atkinson=${row.atkinson} nonbg=${(row.nonbg * 100).toFixed(1)}% → ${row.ok ? 'OK' : 'FAIL (' + row.note + ')'}`)
      await page.close()
    }
  }
  for (const theme of THEMES) {
    try {
      strikeCaptures.push(...await runStrikeFlow(browser, theme))
    } catch (error) {
      strikeFailures.push({ theme, error: error.stack || error.message || String(error) })
      console.error(`  [${theme}/strike-flow] → FAIL (${error.message || error})`)
    }
  }
} finally {
  await teardown()
}

// ── 4. verdict ────────────────────────────────────────────────────────────────
console.log('\n=== FULL-APP SMOKE (real bin/peasant) ===')
console.log('theme  surface                served  mount  theme  atkinson  nonbg     verdict')
for (const r of rows) {
  console.log(`${r.theme.padEnd(6)} ${r.id.padEnd(22)} ${String(r.served).padEnd(7)} ${String(r.mounted).padEnd(6)} ${String(r.themeAttrs).padEnd(6)} ${String(r.atkinson).padEnd(9)} ${(r.nonbg * 100).toFixed(1).padStart(5)}%   ${r.ok ? 'OK' : 'FAIL: ' + r.note}`)
}
const failed = rows.filter((r) => !r.ok)
console.log(`\nscreenshots: ${OUT}/<theme>/<surface>.png`)
console.log(`Strike screenshots: ${STRIKE_OUT}/<theme>/<surface>.png`)
if (failed.length || strikeFailures.length) {
  for (const failure of strikeFailures) console.error(`Strike ${failure.theme}: ${failure.error}`)
  console.error(`\nFAIL [full-app-smoke] ${failed.length}/${rows.length} generic surface(s) and ${strikeFailures.length}/${THEMES.length} Strike flow(s) failed on the real binary.`)
  process.exit(1)
}
console.log(`\nOK [full-app-smoke] all ${rows.length} generic surface checks and ${strikeCaptures.length} interaction-driven Strike captures passed on the real binary.`)
