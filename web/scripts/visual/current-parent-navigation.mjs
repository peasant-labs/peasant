/* MOUNTED CURRENT-PARENT NAVIGATION — focused real-binary evidence capture.
 *
 * Unlike the backend-free fixture route, this drives the user's canonical path:
 * the real `bin/peasant` (static export served from //go:embed web/out) with the
 * mock store, the real WebSocket `session_detail` payload, and the real
 * SessionDetailV2 + Fairtrade adapter/viewer. It follows the child's stored
 * current-parent link to the exact parent session, then goes Back and asserts
 * the child's route query, earlier-history disclosure, and inner scroll offset
 * are restored — capturing the mounted body in BOTH themes.
 *
 * Build provenance is asserted BEFORE any capture: the served JS chunk must
 * carry the host's per-session reading-state storage key, so a stale export or
 * the wrong worktree cannot silently invalidate the evidence.
 *
 * Run:  CHROME_PATH=$(command -v google-chrome) node web/scripts/visual/current-parent-navigation.mjs
 * Env:  PEASANT_NAV_SKIP_BUILD=1   reuse the existing bin/peasant
 *       PEASANT_NAV_PORT            server port (default 8694)
 *       PEASANT_NAV_CAPTURE_DIR     output root (default /tmp/opencode/current-parent-navigation-captures)
 *       PUPPETEER_CORE              explicit puppeteer-core module path
 */
import { execSync, spawn } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, readdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join, resolve } from 'node:path'
import { measurePng, MIN_NONBG_RATIO } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'
import { loadContextNavigationFixture } from './current-parent-navigation-fixture.mjs'
import { SMOKE_MOCKS, SMOKE_THEMES } from './smoke-surfaces.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '../../..')
const BIN = join(REPO, 'bin/peasant')
const OUT_ROOT = process.env.PEASANT_NAV_CAPTURE_DIR || '/tmp/opencode/current-parent-navigation-captures'
const PORT = process.env.PEASANT_NAV_PORT || '8694'
const ORIGIN = `http://localhost:${PORT}`
const CHROME = process.env.CHROME_PATH || 'google-chrome'
const MOCKS = SMOKE_MOCKS
const FIXTURE = loadContextNavigationFixture()
const MARKER = 'peasant:transcript-reading:'
const FONT = '16px "Atkinson Hyperlegible"'
const FONTS = [
  '400 16px "Atkinson Hyperlegible"', '700 16px "Atkinson Hyperlegible"',
  '400 16px "Atkinson Hyperlegible Mono"', '600 16px "Atkinson Hyperlegible Mono"',
]
const THEME_ATTRIBUTES = ['data-theme', 'data-tb-theme']
const CHILD_URL = `${ORIGIN}/projects/${encodeURIComponent(FIXTURE.projectHash)}/${FIXTURE.childSessionId}/?origin=Map`
const pause = (ms) => new Promise((r) => setTimeout(r, ms))

if (!existsSync(CHROME) && !process.env.CHROME_PATH) {
  console.error('ERROR [current-parent-navigation] CHROME_PATH is unset and google-chrome is not on PATH.')
  process.exit(1)
}

const fail = (step, reason) => new Error(
  `Mounted current-parent navigation capture failed because ${reason} during ${step} in current-parent-navigation.mjs; the mounted link/Back exit is not proven on this build; inspect the served route and internal/mock/testdata/context_navigation.yaml, fix the production path, rebuild, and rerun.`,
)

/** Prove the served artifact carries this change and not a stale export. */
function assertBuildProvenance() {
  const chunksDir = join(REPO, 'web/out/_next/static/chunks')
  if (!existsSync(chunksDir)) throw fail('verifying build provenance', `the built export has no ${chunksDir}; run make build first`)
  const files = readdirSync(chunksDir, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith('.js'))
    .map((entry) => join(entry.parentPath, entry.name))
  const carrying = files.filter((file) => readFileSync(file, 'utf8').includes(MARKER))
  if (carrying.length === 0) throw fail('verifying build provenance', `no served chunk carries the marker ${JSON.stringify(MARKER)}; the export is stale or built from another source`)
  return carrying[0]
}

async function assertServedChunkCarriesMarker(chunkPath) {
  const relative = chunkPath.slice(join(REPO, 'web/out').length).replaceAll('\\', '/')
  const response = await fetch(`${ORIGIN}${relative}`).catch(() => null)
  if (!response || response.status !== 200) throw fail('verifying served artifact', `GET ${relative} returned ${response?.status ?? 0}`)
  const body = await response.text()
  if (!body.includes(MARKER)) throw fail('verifying served artifact', `the served chunk ${relative} does not carry ${JSON.stringify(MARKER)}; the server is not serving the built export under test`)
}

async function waitForSelector(page, selector, step) {
  const element = await page.waitForSelector(selector, { timeout: 20000 }).catch(() => null)
  if (!element) throw fail(step, `selector ${JSON.stringify(selector)} never mounted; current URL is ${page.url()}`)
  return element
}

async function waitForPath(page, expected, step) {
  const reached = await page.waitForFunction((value) => window.location.pathname.includes(value), { timeout: 20000 }, expected).catch(() => null)
  if (!reached) throw fail(step, `navigation never reached a path containing ${JSON.stringify(expected)}; current URL is ${page.url()}`)
}

async function capture(page, theme, id, selector) {
  const element = await waitForSelector(page, selector, `capturing ${id}`)
  const themeAttributes = await page.evaluate((attrs) => Object.fromEntries(attrs.map((attr) => [attr, document.documentElement.getAttribute(attr)])), THEME_ATTRIBUTES)
  if (!THEME_ATTRIBUTES.every((attribute) => themeAttributes[attribute] === theme)) throw fail(`capturing ${id}`, `theme attributes were ${JSON.stringify(themeAttributes)} instead of ${JSON.stringify(theme)}`)
  await page.evaluate(async (faces) => { try { await Promise.all(faces.map((face) => document.fonts.load(face))) } catch {} ; await document.fonts.ready }, FONTS)
  if (!await page.evaluate((font) => document.fonts.check(font), FONT)) throw fail(`capturing ${id}`, 'Atkinson Hyperlegible was not loaded')
  const directory = join(OUT_ROOT, theme)
  mkdirSync(directory, { recursive: true })
  const file = join(directory, `${id}.png`)
  await element.screenshot({ path: file, captureBeyondViewport: true })
  const measurement = await measurePng(page, `data:image/png;base64,${readFileSync(file).toString('base64')}`)
  if (measurement.nonbgRatio < MIN_NONBG_RATIO) throw fail(`capturing ${id}`, `the screenshot was blank at ${(measurement.nonbgRatio * 100).toFixed(2)}% non-background pixels`)
  console.log(`  [${theme}/${id}] theme=true atkinson=true nonbg=${(measurement.nonbgRatio * 100).toFixed(1)}% → ${file}`)
  return file
}

async function runTheme(browser, theme, chunkPath) {
  const page = await browser.newPage()
  const captures = []
  try {
    await applyDeterminism(page)
    await page.evaluateOnNewDocument((value) => { try { localStorage.setItem('peasant-theme', value) } catch {} }, theme)
    await assertServedChunkCarriesMarker(chunkPath)

    const response = await page.goto(CHILD_URL, { waitUntil: 'domcontentloaded' })
    if (response?.status() !== 200) throw fail('opening the child transcript', `HTTP status was ${response?.status() ?? 0}`)

    await waitForSelector(page, '.txn-app', 'mounting the child transcript')
    await waitForSelector(page, '.txn-context', 'rendering the session context section')
    const starterRow = await page.evaluate(({ label }) => {
      const row = [...document.querySelectorAll('.txn-context-source')].find((candidate) => candidate.textContent?.includes(label))
      return !!row?.querySelector('button.txn-context-link')
    }, { label: FIXTURE.starterLabel })
    if (!starterRow) throw fail('rendering the current-parent link', `no active ${JSON.stringify(FIXTURE.starterLabel)} link was rendered`)

    // Open the retained earlier history and scroll the inner stream so Back has
    // disclosure AND scroll offset to restore. The disclosure is host-controlled
    // state, so wait for the re-render instead of reading it synchronously.
    const toggled = await page.evaluate(() => {
      const toggle = document.querySelector('.txn-earlier-toggle')
      if (!toggle) return false
      toggle.click()
      return true
    })
    if (!toggled) throw fail('opening the retained earlier history', 'no earlier-history disclosure was rendered')
    const expanded = await page.waitForFunction(() => document.querySelector('.txn-earlier-toggle')?.getAttribute('aria-expanded') === 'true', { timeout: 5000 }).catch(() => null)
    if (!expanded) throw fail('opening the retained earlier history', 'the disclosure did not expand')
    // The links and the expanded earlier history sit at the top of the inner
    // stream, so capture them there before scrolling down. Expand the collapsed
    // session metrics so the one-input / five-record separation is visible.
    await page.evaluate(() => { const scroller = document.querySelector('.txn-stream'); if (scroller) scroller.scrollTop = 0 })
    const metricsButton = await page.evaluate(() => {
      const button = [...document.querySelectorAll('button')].find((candidate) => candidate.textContent?.trim() === 'details')
      if (!button) return false
      button.click()
      return true
    })
    if (!metricsButton) throw fail('expanding the session metrics', 'no details control was rendered')
    const metricsVisible = await page.waitForFunction(() => {
      const text = document.querySelector('.txn-app')?.textContent ?? ''
      return text.includes('1 input submissions') && text.includes('5 turns')
    }, { timeout: 5000 }).catch(() => null)
    if (!metricsVisible) throw fail('expanding the session metrics', 'the mounted hero did not show the one-input / five-record counts')
    await pause(200)
    captures.push(await capture(page, theme, 'child-context-links', '.txn-app'))

    const before = await page.evaluate(() => {
      const scroller = document.querySelector('.txn-stream')
      if (!scroller) return null
      const maxScroll = scroller.scrollHeight - scroller.clientHeight
      const target = Math.min(600, maxScroll)
      scroller.scrollTop = target
      return { maxScroll, target, scrollTop: scroller.scrollTop }
    })
    if (!before) throw fail('scrolling the child transcript', 'the inner stream was not rendered')
    if (before.maxScroll < 200) throw fail('scrolling the child transcript', `the inner stream only overflows by ${before.maxScroll}px; the fixture cannot prove scroll restoration`)
    await pause(200)
    captures.push(await capture(page, theme, 'child-scrolled-before-follow', '.txn-app'))

    const clicked = await page.evaluate(({ label }) => {
      const row = [...document.querySelectorAll('.txn-context-source')].find((candidate) => candidate.textContent?.includes(label))
      const link = row?.querySelector('button.txn-context-link')
      if (!link) return false
      link.click()
      return true
    }, { label: FIXTURE.starterLabel })
    if (!clicked) throw fail('following the current-parent link', 'the actual link disappeared before click')
    await waitForPath(page, FIXTURE.parentId, 'following the current-parent link')
    await waitForSelector(page, '.txn-app', 'mounting the exact parent session')
    const parentMounted = await page.waitForFunction(({ parentId }) => window.location.pathname.includes(parentId) && !!document.querySelector('.txn-app'), { timeout: 20000 }, { parentId: FIXTURE.parentId }).catch(() => null)
    if (!parentMounted) throw fail('mounting the exact parent session', `the viewer did not settle on ${FIXTURE.parentId}`)
    captures.push(await capture(page, theme, 'parent-session', '.txn-app'))

    await page.goBack({ waitUntil: 'domcontentloaded' }).catch(() => null)
    await waitForPath(page, FIXTURE.childSessionId, 'returning to the child transcript with Back')
    const restored = await page.evaluate(({ childId, target, earlierSummary }) => {
      const scroller = document.querySelector('.txn-stream')
      const toggle = document.querySelector('.txn-earlier-toggle')
      return {
        pathname: window.location.pathname,
        search: window.location.search,
        child: window.location.pathname.includes(childId),
        scrollTop: scroller ? scroller.scrollTop : -1,
        expanded: toggle ? toggle.getAttribute('aria-expanded') : null,
        earlier: [...document.querySelectorAll('.txn-earlier')].some((section) => section.textContent?.includes(earlierSummary)),
        target,
      }
    }, { childId: FIXTURE.childSessionId, target: before.target, earlierSummary: FIXTURE.earlierSummary })
    if (!restored.child) throw fail('restoring the child on Back', `pathname was ${restored.pathname}`)
    if (!restored.search.includes('origin=Map')) throw fail('restoring the child route query on Back', `search was ${JSON.stringify(restored.search)}`)
    if (restored.expanded !== 'true') throw fail('restoring the child disclosure on Back', `earlier-history aria-expanded was ${JSON.stringify(restored.expanded)}`)
    if (Math.abs(restored.scrollTop - before.target) > 2) throw fail('restoring the child scroll offset on Back', `scrollTop was ${restored.scrollTop}, expected ${before.target}`)
    captures.push(await capture(page, theme, 'child-restored-after-back', '.txn-app'))

    // Return to the top so the restored disclosure is visible in the evidence.
    // The replay settles within ~1s of Back; wait it out, then scroll to the top.
    await pause(1400)
    await page.evaluate(() => { const scroller = document.querySelector('.txn-stream'); if (scroller) scroller.scrollTop = 0 })
    await pause(250)
    captures.push(await capture(page, theme, 'child-restored-disclosure', '.txn-app'))

    await waitForSelector(page, '.txn-app', 'settling the restored child view')
    console.log(`  [${theme}] click→${FIXTURE.parentId} · Back→${restored.pathname}${restored.search} · scroll=${restored.scrollTop}/${before.target} · disclosure=${restored.expanded} → OK`)
    return captures
  } finally {
    await page.close()
  }
}

if (!process.env.PEASANT_NAV_SKIP_BUILD) {
  console.log('[current-parent-navigation] make build (user canonical path) …')
  execSync('make build', { cwd: REPO, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })
} else {
  console.log('[current-parent-navigation] PEASANT_NAV_SKIP_BUILD=1 — reusing existing bin/peasant')
}
if (!existsSync(BIN)) { console.error(`ERROR [current-parent-navigation] ${BIN} not found — run \`make build\` first.`); process.exit(1) }

const chunkPath = assertBuildProvenance()
console.log(`[current-parent-navigation] build provenance: ${chunkPath.slice(REPO.length + 1)} carries ${JSON.stringify(MARKER)}`)

console.log(`[current-parent-navigation] starting ${BIN} on :${PORT} (mock store: ${MOCKS}) …`)
const server = spawn(BIN, ['web', 'start', '--port', PORT, '--foreground', '--no-browser', `--mock-data-store=${MOCKS}`], { cwd: REPO, stdio: ['ignore', 'ignore', 'inherit'] })
let serverDown = false
server.on('exit', () => { serverDown = true })
const teardown = async () => { try { if (browser) await browser.close() } catch {} ; try { if (!serverDown) server.kill('SIGTERM') } catch {} }

let browser
const httpStatus = async (path) => { const res = await fetch(ORIGIN + path).catch(() => null); return res ? res.status : 0 }
let healthy = false
for (let i = 0; i < 40 && !healthy; i++) { if (await httpStatus('/api/v1/health') === 200) healthy = true; else await pause(500) }
if (!healthy) { console.error('ERROR [current-parent-navigation] server never became healthy.'); await teardown(); process.exit(2) }

const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default
const captures = []
try {
  browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: { width: 1460, height: 900, deviceScaleFactor: 1 } })
  for (const theme of SMOKE_THEMES) captures.push(...await runTheme(browser, theme, chunkPath))
} catch (error) {
  console.error(`FAIL [current-parent-navigation] ${error.stack || error.message}`)
  await teardown()
  process.exit(1)
} finally {
  await teardown()
}

console.log(`\nOK [current-parent-navigation] ${captures.length} mounted captures across ${SMOKE_THEMES.length} themes:`)
for (const file of captures) console.log(`  ${file}`)
