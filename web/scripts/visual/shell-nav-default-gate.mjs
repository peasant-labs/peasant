/* Local shell offline gate (the server-stopped arm), on a default server this script boots itself.

   Companion to shell-nav-gate.mjs, which holds the connected header to testdata/shell-header.yaml
   against a server the caller runs. This gate needs to STOP the server, so it spawns its own
   default-mode (no --experimental) `bin/peasant web start` with the mock store and drives the real
   production path of "the peasant app on this computer stopped":

     1. provenance (shell-header-manifest.mjs assertServedBuild): bin/peasant is not older than
        web/out, the served page references exactly the chunks web/out/index.html does, and those
        chunks carry markers only this shell introduces — so a stale server or another checkout
        fails before any capture;
     2. for each page — home `/` and a transcript at desktop, home at 390px, and home on a short,
        zoomed screen (320×256) — in both themes, one at a time (the page under test is always the
        active tab):
        a. connected: the header manifest holds and no notice shows, past the socket grace period;
        b. stopped: the server process is killed with the page open. Within a few seconds the page
           shows fairtrade's LocalOfflineBanner under the fixed header, at the top of the page and
           NOT fixed itself (it scrolls with the page): it says the peasant app isn't running on
           THIS computer and that the internet is fine, offers `peasant web start --port <this
           page's port>` and `try again`; the always-mounted live region says the app stopped; the
           header manifest still holds; <main> clears the header plus the notice; and the transcript
           page owns no second (document) scroll. On the short screen the document scrolls far
           enough that `try again` comes into view and is reachable by a pointer (not under the
           header). The frame is captured (the short screen scrolled to `try again`);
        c. back: the server restarts on the same port, `try again` is pressed, the notice goes away
           with --app-notice-height cleared, and the live region says the app is running again.

   The tour: the gate can only see a tour overlay (`[role=dialog][aria-label^="Product tour"]`),
   and the tour never starts on its own, so this check cannot tell a mounted tour provider from an
   unmounted one. The unmount itself is guarded by LocalOfflineNotice.test.tsx.

   The spawned server is always killed on exit, failure, or signal; the temp config dir it made is
   removed; server.log is rewritten per run.

   Run:
     CHROME_PATH=$(command -v google-chrome) node scripts/visual/shell-nav-default-gate.mjs

   Env:
     PEASANT_BIN               binary to boot (default <repo>/bin/peasant; build it with make build)
     PEASANT_OFFLINE_PORT      port to boot it on (default 8698; must be free)
     PEASANT_OFFLINE_CONFIG_DIR config dir for the booted server (default a fresh temp dir, removed
                               afterwards; a supplied dir is kept)
     SHELL_OFFLINE_CAPTURE_DIR output root for the offline frames (default <base>/shell-offline)
     CHROME_PATH               Chrome/Chromium binary (required)
     PUPPETEER_CORE            explicit puppeteer-core module path (optional)
 */
import { spawn } from 'node:child_process'
import { closeSync, mkdirSync, mkdtempSync, openSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { fileURLToPath } from 'node:url'
import { dirname, join, resolve } from 'node:path'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'
import { SHELL_DEFAULT_PROJECT, SHELL_DEFAULT_SESSION, SMOKE_MOCKS, SMOKE_THEMES } from './smoke-surfaces.mjs'
import { assertKnownProject } from './validate-mock-coordinates.mjs'
import { assertServedBuild, chromeClearance, headerFailures, loadShellHeaderManifest, shippedItems, WEB_ROOT } from './shell-header-manifest.mjs'

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
const EXPECTED_COMMAND = PORT === 8690 ? 'peasant web start' : `peasant web start --port ${PORT}`
// What the host's always-mounted live region says (LocalOfflineNotice's OFFLINE_ANNOUNCEMENTS).
const ANNOUNCE_STOPPED = 'peasant stopped on this computer.'
const ANNOUNCE_BACK = 'peasant is running again.'
const PAGES = Object.freeze([
  { id: 'home', path: '/', body: 'main', width: 1440, height: 900 },
  { id: 'transcript', path: `/projects/${SHELL_DEFAULT_PROJECT}/${SHELL_DEFAULT_SESSION}/`, body: '.txn-app', width: 1440, height: 900, singleScroller: true },
  { id: 'home-mobile', path: '/', body: 'main', width: 390, height: 844 },
  // 400% zoom on a 1280×1024 screen: the notice alone is taller than the viewport, so it must scroll.
  { id: 'home-short', path: '/', body: 'main', width: 320, height: 256, short: true },
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
const ownConfigDir = !process.env.PEASANT_OFFLINE_CONFIG_DIR
const configDir = process.env.PEASANT_OFFLINE_CONFIG_DIR || mkdtempSync(join(tmpdir(), 'peasant-offline-gate-'))
mkdirSync(OUT, { recursive: true })
const serverLog = join(OUT, 'server.log')
closeSync(openSync(serverLog, 'w')) // one run per log: earlier runs never mix into the one a failure points to
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
  closeSync(log)
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

const removeOwnConfigDir = () => {
  if (ownConfigDir) {
    try { rmSync(configDir, { recursive: true, force: true }) } catch {}
  }
}

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    try { server?.kill('SIGKILL') } catch {}
    removeOwnConfigDir()
    process.exit(130)
  })
}
process.on('exit', () => {
  try { if (server && server.exitCode === null) server.kill('SIGKILL') } catch {}
})

// ── page probes ──────────────────────────────────────────────────────────────────────────────────
const noticeState = (page) => page.evaluate((selector) => {
  const notice = document.querySelector(selector)
  const wrapper = notice?.parentElement || null
  const header = document.querySelector('header')
  const status = notice?.querySelector('[role="status"]')
  const retry = notice ? [...notice.querySelectorAll('button')].find((b) => /try again/.test(b.textContent || '')) : null
  const live = document.querySelector('p[role="status"].sr-only')
  const box = wrapper?.getBoundingClientRect()
  let fixedAncestor = ''
  for (let el = wrapper; el && el !== document.documentElement; el = el.parentElement) {
    if (getComputedStyle(el).position === 'fixed') { fixedAncestor = el.tagName.toLowerCase(); break }
  }
  const headerHeight = header ? header.getBoundingClientRect().height : 0
  return {
    shown: !!notice,
    outsideHeader: !!notice && !!header && !header.contains(notice),
    fixedAncestor,
    documentTop: box ? box.top + window.scrollY : null,
    status: (status?.textContent || '').replace(/\s+/g, ' ').trim(),
    command: (notice?.querySelector('.cx-cmd-text')?.textContent || '').trim(),
    retry: !!retry,
    live: (live?.textContent || '').trim(),
    noticeHeight: document.documentElement.style.getPropertyValue('--app-notice-height'),
    noticeBox: box ? box.height : 0,
    headerHeight,
    mainPaddingTop: Number.parseFloat(getComputedStyle(document.querySelector('main')).paddingTop),
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

// <main> clears the header plus the notice (or the header alone) once both have settled.
const clears = (s) => Math.abs(s.mainPaddingTop - (s.headerHeight + (s.shown ? s.noticeBox : 0))) <= 1

const assertHeaderHolds = async (page, theme, where) => {
  const failures = [...await page.evaluate(headerFailures, manifest, { theme, shipped }), ...(await page.evaluate(chromeClearance)).failures]
  if (failures.length) throw new Error(`the ${theme} header at ${where} breaks the shell manifest: ${JSON.stringify(failures)}`)
}

// On the short screen: scroll `try again` into view and prove a pointer at its centre reaches it
// (the fixed header must not cover it). The scroll is instant, so a smooth-scrolling root cannot
// leave the measurement mid-animation.
const assertRetryReachable = async (page) => {
  const found = await page.evaluate((selector) => {
    const retry = [...(document.querySelector(selector)?.querySelectorAll('button') || [])].find((b) => /try again/.test(b.textContent || ''))
    retry?.scrollIntoView({ block: 'center', behavior: 'instant' })
    return !!retry
  }, NOTICE)
  if (!found) return { ok: false, reason: 'no try again button', scrollY: 0 }
  await pause(150)
  return measureRetry(page)
}

const measureRetry = (page) => page.evaluate((selector) => {
  const retry = [...(document.querySelector(selector)?.querySelectorAll('button') || [])].find((b) => /try again/.test(b.textContent || ''))
  if (!retry) return { ok: false, reason: 'no try again button', scrollY: window.scrollY }
  const rect = retry.getBoundingClientRect()
  const header = document.querySelector('header')?.getBoundingClientRect()
  const x = rect.left + rect.width / 2
  const y = rect.top + rect.height / 2
  const hit = document.elementFromPoint(x, y)
  const inView = rect.top >= 0 && rect.bottom <= window.innerHeight
  const belowHeader = !header || rect.top >= header.bottom - 0.5
  const reached = !!hit && (hit === retry || retry.contains(hit))
  return {
    ok: inView && belowHeader && reached,
    reason: `inView=${inView} belowHeader=${belowHeader} reached=${reached} hit=${hit ? hit.tagName.toLowerCase() : 'none'} rect=${Math.round(rect.top)}..${Math.round(rect.bottom)} scrollY=${Math.round(window.scrollY)} viewport=${window.innerHeight}`,
    scrollY: window.scrollY,
  }
}, NOTICE)

const capture = async (page, gate, theme, id, { keepScroll = false } = {}) => {
  const outDir = join(OUT, theme)
  mkdirSync(outDir, { recursive: true })
  const file = join(outDir, `${id}.png`)
  await page.evaluate(() => document.fonts.ready)
  if (!keepScroll) await page.evaluate(() => window.scrollTo(0, 0))
  await pause(100)
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
  await assertHeaderHolds(page, theme, `${spec.path} (${spec.width}×${spec.height}, connected)`)
  await pause(PAST_GRACE_MS)
  const connected = await noticeState(page)
  if (connected.shown || connected.noticeHeight) throw new Error(`the ${where} page shows the offline notice while the app is running: ${JSON.stringify(connected)}`)

  // stopped: the notice under the header, in the page flow, saying the right thing
  await stopServer()
  await waitFor(page, (s) => s.shown && s.noticeHeight !== '' && clears(s), OFFLINE_WITHIN_MS, `the ${where} offline notice (with <main> clearing the header plus the notice)`)
  // The live region is written by a passive effect after the notice paints; give it that render.
  const state = await waitFor(page, (s) => s.live !== '', 2000, `the ${where} live region announcing the stop`)
  const failures = []
  if (!state.outsideHeader) failures.push('the notice is inside the header')
  if (state.fixedAncestor) failures.push(`the notice rides in a fixed ${state.fixedAncestor}; it must scroll with the page`)
  if (state.documentTop === null || Math.abs(state.documentTop - state.headerHeight) > 1) failures.push(`the notice starts ${state.documentTop}px down the page, not at the ${state.headerHeight}px header bottom`)
  if (!state.status.includes("peasant isn't running on this computer")) failures.push(`the message does not name this computer: ${JSON.stringify(state.status)}`)
  if (!state.status.includes('your internet is fine')) failures.push(`the message could read as an internet outage: ${JSON.stringify(state.status)}`)
  if (state.command !== EXPECTED_COMMAND) failures.push(`the start command reads ${JSON.stringify(state.command)}, expected ${JSON.stringify(EXPECTED_COMMAND)}`)
  if (!state.retry) failures.push('no `try again` button')
  if (state.live !== ANNOUNCE_STOPPED) failures.push(`the live region says ${JSON.stringify(state.live)}, expected ${JSON.stringify(ANNOUNCE_STOPPED)}`)
  if (state.tour) failures.push('a tour overlay is showing')
  if (!/^\d+(\.\d+)?px$/.test(state.noticeHeight)) failures.push(`--app-notice-height is ${JSON.stringify(state.noticeHeight)}`)
  if (spec.singleScroller && state.documentOverflow > 0) failures.push(`the document scrolls ${state.documentOverflow}px under the transcript's own scroller`)
  if (spec.short && state.noticeBox + state.headerHeight <= spec.height) failures.push(`the short-screen case does not exercise a notice taller than the viewport (${state.headerHeight}+${state.noticeBox}px in ${spec.height}px)`)
  if (failures.length) throw new Error(`the ${where} page with the server stopped: ${failures.join('; ')}. State: ${JSON.stringify(state)}`)
  await assertHeaderHolds(page, theme, `${spec.path} (${spec.width}×${spec.height}, stopped)`)
  const clearance = await page.evaluate(chromeClearance)
  let scrolledNote = ''
  if (spec.short) {
    const reach = await assertRetryReachable(page)
    if (!reach.ok) throw new Error(`on the ${where} screen \`try again\` cannot be scrolled into reach: ${reach.reason}`)
    scrolledNote = `, try again reached after scrolling ${Math.round(reach.scrollY)}px`
    captured.push(await capture(page, gate, theme, spec.id, { keepScroll: true }))
    await page.evaluate(() => window.scrollTo(0, 0))
  } else {
    captured.push(await capture(page, gate, theme, spec.id))
  }
  console.log(`OK offline ${where}: notice ${clearance.notice}px under the ${clearance.header}px header in the page flow, <main> clears ${clearance.mainPaddingTop}px, command "${state.command}"${scrolledNote}`)

  // back: restart, press try again (in the page, so a notice that already cleared is not an error)
  await startServer()
  const pressed = await page.evaluate((selector) => {
    const button = [...(document.querySelector(selector)?.querySelectorAll('button') || [])].find((b) => /try again/.test(b.textContent || ''))
    button?.click()
    return !!button
  }, NOTICE)
  await waitFor(page, (s) => !s.shown && s.noticeHeight === '' && clears(s), RECOVER_WITHIN_MS, `the ${where} notice clearing after the restart (with <main> back under the header)`)
  const back = await waitFor(page, (s) => s.live !== ANNOUNCE_STOPPED, 2000, `the ${where} live region announcing the return`)
  if (back.live !== ANNOUNCE_BACK) throw new Error(`the ${where} live region says ${JSON.stringify(back.live)} after the app came back, expected ${JSON.stringify(ANNOUNCE_BACK)}`)
  await assertHeaderHolds(page, theme, `${spec.path} (${spec.width}×${spec.height}, back)`)
  console.log(`OK back ${where}: ${pressed ? 'try again pressed; ' : 'already reconnected; '}notice gone, page back under the header, live region "${back.live}"`)

  if (diagnostics.length) throw new Error(`page errors appeared on ${where}: ${JSON.stringify(diagnostics.slice(0, 4))}`)
  await page.close()
}

let browser = null
const captured = []
try {
  await startServer()
  const provenance = await assertServedBuild({ origin: ORIGIN, bin: BIN })
  console.log(`OK provenance: ${BIN} serves this checkout's web/out (${provenance.chunks.length} chunks) carrying ${Object.entries(provenance.markerChunks).map(([marker, chunks]) => `${marker} in ${chunks.join(' ')}`).join('; ')}`)
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
    `  Why: when the peasant app on this computer stops, every page must say so under the header — this computer, not the internet — with the start command and try again, reachable on any screen, and clear it when the app is back.\n` +
    `  Where: shell-nav-default-gate.mjs driving ${BIN} on ${ORIGIN} (server log: ${serverLog}).\n` +
    `  Means: a user whose local app stopped may see no notice, a notice that reads as an internet outage, a notice covering the page or out of reach, or one that never clears.\n` +
    `  Fix: make build so bin/peasant embeds this checkout's web/out, free port ${PORT}, then fix the reported notice, header or page geometry.`,
  )
  process.exitCode = 1
} finally {
  try { await browser?.close() } catch {}
  try { await stopServer() } catch {}
  removeOwnConfigDir()
}

if (!process.exitCode) {
  console.log(`\nOK [shell-nav-default-gate.mjs] offline notice verified on ${ORIGIN}: shows under the header in the page flow when the app stops, names this computer, offers "${EXPECTED_COMMAND}" and try again (reachable on a 320×256 screen), announces the stop and the return, clears when the app is back, in ${SMOKE_THEMES.join(' + ')} at desktop, 390px and 320×256.`)
  console.log('Offline frames:')
  for (const file of captured) console.log(`  ${file}`)
}
