/* Local shell offline gate (the server-stopped arm), on a default server this script boots itself.

   Companion to shell-nav-gate.mjs, which holds the connected header to testdata/shell-header.yaml
   against a server the caller runs. This gate needs to STOP the server, so it spawns its own
   default-mode (no --experimental) `bin/peasant web start` with the mock store and drives the real
   production path of "the peasant app on this computer stopped":

     1. provenance: bin/peasant is not older than web/out, the served page references exactly the
        chunks web/out/index.html does, and those chunks carry markers only the offline notice
        introduces — so a stale server or another checkout fails before any capture;
     2. for each page — home `/` and a transcript at desktop, home at 390px — in both themes, one at
        a time (the page under test is always the active tab):
        a. connected: the header manifest holds and no notice shows, past the socket grace period;
        b. stopped: the server process is killed with the page open. Within a few seconds the page
           shows fairtrade's LocalOfflineBanner inside the fixed top chrome, directly under the
           header: it says the peasant app isn't running on THIS computer and that the internet is
           fine, offers `peasant web start --port <this page's port>` and `try again`, the header
           manifest still holds, the first-run tour is not mounted, <main> clears the grown chrome,
           and the transcript page owns no second (document) scroll. The frame is captured;
        c. back: the server restarts on the same port, `try again` is pressed, and the notice goes
           away with --app-notice-height cleared.

   The spawned server is always killed on exit, failure, or signal.

   Run:
     CHROME_PATH=$(command -v google-chrome) node scripts/visual/shell-nav-default-gate.mjs

   Env:
     PEASANT_BIN               binary to boot (default <repo>/bin/peasant; build it with make build)
     PEASANT_OFFLINE_PORT      port to boot it on (default 8698; must be free)
     PEASANT_OFFLINE_CONFIG_DIR config dir for the booted server (default a fresh temp dir)
     SHELL_OFFLINE_CAPTURE_DIR output root for the offline frames (default <base>/shell-offline)
     CHROME_PATH               Chrome/Chromium binary (required)
     PUPPETEER_CORE            explicit puppeteer-core module path (optional)
 */
import { spawn } from 'node:child_process'
import { mkdirSync, mkdtempSync, openSync, readFileSync, statSync, readdirSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { fileURLToPath } from 'node:url'
import { dirname, join, resolve } from 'node:path'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'
import { SHELL_DEFAULT_PROJECT, SHELL_DEFAULT_SESSION, SMOKE_MOCKS, SMOKE_THEMES } from './smoke-surfaces.mjs'
import { assertKnownProject } from './validate-mock-coordinates.mjs'
import { chromeClearance, headerFailures, loadShellHeaderManifest, shippedItems, WEB_ROOT } from './shell-header-manifest.mjs'

const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default

const HERE = dirname(fileURLToPath(import.meta.url))
const BASE = process.argv[2] || HERE
const REPO = resolve(WEB_ROOT, '..')
const BIN = process.env.PEASANT_BIN || join(REPO, 'bin', 'peasant')
const PORT = Number(process.env.PEASANT_OFFLINE_PORT || 8698)
const ORIGIN = `http://localhost:${PORT}`
const OUT = process.env.SHELL_OFFLINE_CAPTURE_DIR || join(BASE, 'shell-offline')
const CHROME = process.env.CHROME_PATH
const NOTICE = 'section[aria-label="peasant is not running"]'
// Markers only the offline notice brings into the served bundle: the host's chrome-height variable
// and the fairtrade banner's accessible name.
const PROVENANCE_MARKERS = ['--app-notice-height', 'peasant is not running']
const EXPECTED_COMMAND = PORT === 8690 ? 'peasant web start' : `peasant web start --port ${PORT}`
const PAGES = Object.freeze([
  { id: 'home', path: '/', body: 'main', width: 1440, height: 900 },
  { id: 'transcript', path: `/projects/${SHELL_DEFAULT_PROJECT}/${SHELL_DEFAULT_SESSION}/`, body: '.txn-app', width: 1440, height: 900, singleScroller: true },
  { id: 'home-mobile', path: '/', body: 'main', width: 390, height: 844 },
])
// The socket grace in the page (useLocalAppHealth's SOCKET_GRACE_MS) plus margin: a connected page
// must still show no notice after this long.
const PAST_GRACE_MS = 3000
const OFFLINE_WITHIN_MS = 8000
const RECOVER_WITHIN_MS = 10000

if (!CHROME) {
  console.error('ERROR [shell-nav-default-gate.mjs] CHROME_PATH is unset. Set it to your Chrome/Chromium binary before running the offline gate.')
  process.exit(1)
}

const pause = (ms) => new Promise((r) => setTimeout(r, ms))
const manifest = loadShellHeaderManifest()
const shipped = shippedItems(manifest)

// ── the server this gate owns ────────────────────────────────────────────────────────────────────
const configDir = process.env.PEASANT_OFFLINE_CONFIG_DIR || mkdtempSync(join(tmpdir(), 'peasant-offline-gate-'))
mkdirSync(OUT, { recursive: true })
const serverLog = join(OUT, 'server.log')
let server = null

const healthAnswers = async () => {
  try {
    const response = await fetch(`${ORIGIN}/api/v1/health`, { signal: AbortSignal.timeout(1000) })
    return response.ok
  } catch {
    return false
  }
}

const startServer = async () => {
  if (await healthAnswers()) throw new Error(`something already answers on ${ORIGIN}; stop it or set PEASANT_OFFLINE_PORT to a free port`)
  const log = openSync(serverLog, 'a')
  server = spawn(BIN, ['web', 'start', '--port', String(PORT), '--foreground', '--no-browser', '--config-dir', configDir, `--mock-data-store=${SMOKE_MOCKS}`], { stdio: ['ignore', log, log] })
  const started = Date.now()
  while (Date.now() - started < 20000) {
    if (server.exitCode !== null) throw new Error(`${BIN} exited with ${server.exitCode} before answering; see ${serverLog}`)
    if (await healthAnswers()) return
    await pause(200)
  }
  throw new Error(`${BIN} did not answer ${ORIGIN}/api/v1/health within 20s; see ${serverLog}`)
}

const stopServer = async () => {
  if (!server || server.exitCode !== null) return
  const exited = new Promise((r) => server.once('exit', r))
  server.kill('SIGTERM')
  await Promise.race([exited, pause(5000)])
  if (server.exitCode === null) server.kill('SIGKILL')
  const stopped = Date.now()
  while (Date.now() - stopped < 5000 && await healthAnswers()) await pause(100)
  if (await healthAnswers()) throw new Error(`${ORIGIN} still answers after the server was stopped`)
}

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    try { server?.kill('SIGKILL') } catch {}
    process.exit(130)
  })
}
process.on('exit', () => {
  try { if (server && server.exitCode === null) server.kill('SIGKILL') } catch {}
})

// ── provenance ───────────────────────────────────────────────────────────────────────────────────
const chunksOf = (html) => [...new Set(html.match(/\/_next\/static\/chunks\/[^"']+\.js/g) || [])].sort()

const assertProvenance = async () => {
  const outIndex = join(WEB_ROOT, 'out', 'index.html')
  const binTime = statSync(BIN).mtimeMs
  const outTime = statSync(outIndex).mtimeMs
  if (binTime < outTime) throw new Error(`${BIN} is older than ${outIndex}: the binary embeds an earlier web build; run make build`)
  const localChunks = chunksOf(readFileSync(outIndex, 'utf8'))
  const served = await (await fetch(`${ORIGIN}/`)).text()
  const servedChunks = chunksOf(served)
  if (JSON.stringify(servedChunks) !== JSON.stringify(localChunks)) {
    throw new Error(`the server at ${ORIGIN} serves chunks ${JSON.stringify(servedChunks)}, not this checkout's web/out ${JSON.stringify(localChunks)}`)
  }
  const bodies = await Promise.all(servedChunks.map(async (chunk) => (await fetch(`${ORIGIN}${chunk}`)).text()))
  const missing = PROVENANCE_MARKERS.filter((marker) => !bodies.some((body) => body.includes(marker)))
  if (missing.length) throw new Error(`the served bundle lacks ${JSON.stringify(missing)}: it predates the offline notice`)
  const localHasMarkers = PROVENANCE_MARKERS.every((marker) => readdirSync(join(WEB_ROOT, 'out', '_next', 'static', 'chunks'), { recursive: true })
    .some((file) => String(file).endsWith('.js') && readFileSync(join(WEB_ROOT, 'out', '_next', 'static', 'chunks', String(file)), 'utf8').includes(marker)))
  if (!localHasMarkers) throw new Error('web/out lacks the offline notice markers; rebuild web/out')
  console.log(`OK provenance: ${BIN} embeds this checkout's web/out (${servedChunks.length} chunks served identically) carrying ${PROVENANCE_MARKERS.join(' + ')}`)
}

// ── page probes ──────────────────────────────────────────────────────────────────────────────────
const noticeState = (page) => page.evaluate((selector) => {
  const notice = document.querySelector(selector)
  const header = document.querySelector('header')
  const chrome = header?.parentElement
  const status = notice?.querySelector('[role="status"]')
  const retry = notice ? [...notice.querySelectorAll('button')].find((b) => /try again/.test(b.textContent || '')) : null
  const rect = notice?.getBoundingClientRect()
  return {
    shown: !!notice,
    insideChrome: !!notice && !!chrome && chrome.contains(notice) && chrome.firstElementChild === header,
    underHeader: !!notice && !!header && rect.top >= header.getBoundingClientRect().bottom - 0.5,
    onScreen: !!rect && rect.height > 0 && rect.top < window.innerHeight,
    status: (status?.textContent || '').replace(/\s+/g, ' ').trim(),
    command: (notice?.querySelector('.cx-cmd-text')?.textContent || '').trim(),
    retry: !!retry,
    noticeHeight: document.documentElement.style.getPropertyValue('--app-notice-height'),
    noticeBox: notice ? notice.getBoundingClientRect().height : 0,
    mainPaddingTop: Number.parseFloat(getComputedStyle(document.querySelector('main')).paddingTop),
    chromeHeight: chrome ? chrome.getBoundingClientRect().height : 0,
    visibility: document.visibilityState,
    tour: !!document.querySelector('[role="dialog"][aria-label^="Product tour"]'),
    documentOverflow: document.documentElement.scrollHeight - window.innerHeight,
  }
}, NOTICE)

const waitFor = async (page, predicate, timeoutMs, what) => {
  const start = Date.now()
  let last = null
  while (Date.now() - start < timeoutMs) {
    last = await noticeState(page)
    if (predicate(last)) return last
    await pause(200)
  }
  throw new Error(`${what} did not happen within ${timeoutMs}ms; last state ${JSON.stringify(last)}`)
}

const assertHeaderHolds = async (page, theme, where) => {
  const failures = [...await page.evaluate(headerFailures, manifest, { theme, shipped }), ...(await page.evaluate(chromeClearance)).failures]
  if (failures.length) throw new Error(`the ${theme} header at ${where} breaks the shell manifest: ${JSON.stringify(failures)}`)
}

const capture = async (page, gate, theme, id) => {
  const outDir = join(OUT, theme)
  mkdirSync(outDir, { recursive: true })
  const file = join(outDir, `${id}.png`)
  await page.evaluate(() => document.fonts.ready)
  await page.screenshot({ path: file, captureBeyondViewport: false })
  const measured = await gate.assert(`offline-${id}-${theme}`, file, { sel: NOTICE, where: 'shell-nav-default-gate.mjs' })
  console.log(`OK offline ${theme}/${id} nonbg=${(measured.nonbgRatio * 100).toFixed(1)}% colors=${measured.distinctColors}`)
  return file
}

// ── the run ──────────────────────────────────────────────────────────────────────────────────────
// One page at a time, each through its own stop/restart cycle, so the page under test is always the
// browser's active tab (background tabs may be frozen and stop answering the protocol).
const drivePage = async (theme, spec, seen) => {
  const page = await browser.newPage()
  await page.bringToFront()
  await page.setViewport({ width: spec.width, height: spec.height, deviceScaleFactor: 1 })
  await applyDeterminism(page)
  await page.evaluateOnNewDocument((t) => { try { localStorage.setItem('peasant-theme', t) } catch {} }, theme)
  const gate = new SurfaceGate(page)
  gate.seen = seen
  const diagnostics = []
  page.on('pageerror', (e) => diagnostics.push('pageerr: ' + (e.stack || e.message)))
  const where = `${theme}/${spec.id}`

  // connected: the manifest holds and no notice shows, past the socket grace period
  const response = await page.goto(ORIGIN + spec.path, { waitUntil: 'networkidle0' })
  if (!response || response.status() >= 400) throw new Error(`the app answered HTTP ${response ? response.status() : 0} for ${spec.path}`)
  await page.waitForSelector(spec.body, { visible: true, timeout: 15000 })
  await assertHeaderHolds(page, theme, `${spec.path} (${spec.width}px, connected)`)
  await pause(PAST_GRACE_MS)
  const connected = await noticeState(page)
  if (connected.shown || connected.noticeHeight) throw new Error(`the ${where} page shows the offline notice while the app is running: ${JSON.stringify(connected)}`)

  // stopped: the notice, under the header, saying the right thing, with the page clearing it
  await stopServer()
  // Shown, measured, and cleared by <main>: the notice publishes its height as it mounts and again
  // on every resize, so the chrome and the page offset settle together.
  const state = await waitFor(page, (s) => s.shown && s.noticeHeight !== '' && Math.abs(s.mainPaddingTop - s.chromeHeight) <= 1, OFFLINE_WITHIN_MS, `the ${where} offline notice (with <main> clearing the grown chrome)`)
  const failures = []
  if (!state.insideChrome) failures.push('the notice is not inside the fixed top chrome, after the header')
  if (!state.underHeader) failures.push('the notice starts above the header bottom')
  if (!state.onScreen) failures.push('the notice is off screen')
  if (!state.status.includes("peasant isn't running on this computer")) failures.push(`the message does not name this computer: ${JSON.stringify(state.status)}`)
  if (!state.status.includes('your internet is fine')) failures.push(`the message could read as an internet outage: ${JSON.stringify(state.status)}`)
  if (state.command !== EXPECTED_COMMAND) failures.push(`the start command reads ${JSON.stringify(state.command)}, expected ${JSON.stringify(EXPECTED_COMMAND)}`)
  if (!state.retry) failures.push('no `try again` button')
  if (state.tour) failures.push('the first-run tour is showing')
  if (!/^\d+(\.\d+)?px$/.test(state.noticeHeight)) failures.push(`--app-notice-height is ${JSON.stringify(state.noticeHeight)}`)
  if (spec.singleScroller && state.documentOverflow > 0) failures.push(`the document scrolls ${state.documentOverflow}px under the transcript's own scroller`)
  if (failures.length) throw new Error(`the ${where} page with the server stopped: ${failures.join('; ')}. State: ${JSON.stringify(state)}`)
  await assertHeaderHolds(page, theme, `${spec.path} (${spec.width}px, stopped)`)
  const clearance = await page.evaluate(chromeClearance)
  captured.push(await capture(page, gate, theme, spec.id))
  console.log(`OK offline ${where}: notice ${clearance.notice}px under the ${clearance.header}px header, <main> clears ${clearance.mainPaddingTop}px, command "${state.command}"`)

  // back: restart, press try again (in the page, so a notice that already cleared is not an error)
  await startServer()
  const pressed = await page.evaluate((selector) => {
    const button = [...(document.querySelector(selector)?.querySelectorAll('button') || [])].find((b) => /try again/.test(b.textContent || ''))
    button?.click()
    return !!button
  }, NOTICE)
  await waitFor(page, (s) => !s.shown && s.noticeHeight === '' && Math.abs(s.mainPaddingTop - s.chromeHeight) <= 1, RECOVER_WITHIN_MS, `the ${where} notice clearing after the restart (with <main> back under the header)`)
  await assertHeaderHolds(page, theme, `${spec.path} (${spec.width}px, back)`)
  console.log(`OK back ${where}: ${pressed ? 'try again pressed; ' : 'already reconnected; '}notice gone, chrome back to the header`)

  if (diagnostics.length) throw new Error(`page errors appeared on ${where}: ${JSON.stringify(diagnostics.slice(0, 4))}`)
  await page.close()
}

let browser = null
const captured = []
try {
  await startServer()
  await assertProvenance()
  await assertKnownProject(ORIGIN, SHELL_DEFAULT_PROJECT, { where: 'shell-nav-default-gate.mjs' })
  // A short protocol timeout turns a stuck page into a named failure instead of a 3-minute stall.
  browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', protocolTimeout: 30000 })
  for (const theme of SMOKE_THEMES) {
    const seen = new Map()
    for (const spec of PAGES) await drivePage(theme, spec, seen)
  }
} catch (e) {
  console.error(
    `ERROR [shell-nav-default-gate.mjs] local shell offline gate failed.\n` +
    `  What failed: ${e.message}\n` +
    `  Why: when the peasant app on this computer stops, every page must say so under the header — this computer, not the internet — with the start command and try again, and clear it when the app is back.\n` +
    `  Where: shell-nav-default-gate.mjs driving ${BIN} on ${ORIGIN} (server log: ${serverLog}).\n` +
    `  Means: a user whose local app stopped may see no notice, a notice that reads as an internet outage, a notice covering the page, or one that never clears.\n` +
    `  Fix: make build so bin/peasant embeds this checkout's web/out, free port ${PORT}, then fix the reported notice, header or chrome geometry.`,
  )
  process.exitCode = 1
} finally {
  try { await browser?.close() } catch {}
  try { await stopServer() } catch {}
}

if (!process.exitCode) {
  console.log(`\nOK [shell-nav-default-gate.mjs] offline notice verified on ${ORIGIN}: shows under the header when the app stops, names this computer, offers "${EXPECTED_COMMAND}" and try again, clears when the app is back, tour unmounted, in ${SMOKE_THEMES.join(' + ')} at desktop and 390px.`)
  console.log('Offline frames:')
  for (const file of captured) console.log(`  ${file}`)
}
