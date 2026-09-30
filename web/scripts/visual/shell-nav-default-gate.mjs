/* Local shell offline gate (the server-stopped arm), on a default server this script boots itself.

   Companion to shell-nav-gate.mjs, which holds the connected header to testdata/shell-header.yaml
   against a server the caller runs. This gate needs to STOP the server, so it spawns its own
   default-mode (no --experimental) `bin/peasant web start` with the mock store and drives the real
   production path of "the peasant app on this computer stopped":

     1. provenance (served-build.mjs): bin/peasant is not older than web/out, the served page
        references exactly the chunks web/out/index.html does, and those chunks carry markers only
        this shell introduces (the notice's height variable and the live region's own
        announcements from the manifest) — so a stale server or another checkout fails before any
        capture;
     2. for each page case below, in both themes, one at a time (the page under test is always the
        active tab):
        a. connected: the header manifest holds and no notice shows, past the socket grace period;
        b. stopped: the server process is killed with the page open. Within a few seconds the page
           shows fairtrade's LocalOfflineBanner directly under the fixed header — pinned there where
           the screen has room, at the top of the page and scrolling with it elsewhere
           (shell-header-manifest.mjs chromeClearance decides which, and that <main> clears the
           header plus the notice): it says the peasant app isn't running on THIS computer and that
           the internet is fine, offers `peasant web start --port <this page's port>` and
           `try again`; the always-mounted live region says the manifest's `stopped` text; the
           header manifest still holds. Case-specific checks follow (below), then the frame is
           captured;
        c. still down: `try again` is pressed with the server still stopped; the live region says
           the manifest's `stillStopped` text followed by the check time (hh:mm:ss.);
        d. back: the server restarts on the same port, `try again` is pressed, the notice goes away
           with --app-notice-height cleared, and the live region says the manifest's `back` text.

   The cases: home and a transcript at 1440×900 (the transcript must not scroll the document: its
   own stream is the scroller); home at 390×844; home at 320×256 (400% zoom on 1280×1024: the notice
   is taller than the screen, and the document must scroll `try again` into reach below the header);
   the transcript at 320×256 (scrolled to the bottom, the transcript host keeps its floor —
   min(24rem, screen height − header) — fully in view below the header, so an already-loaded
   transcript stays readable); /share at 320×568 (the share page keeps the same floor); and home at
   1440×700 scrolled down before the server stops (the pinned notice must appear inside the screen,
   under the header, not above the scroll position).

   The tour: the gate can only see a tour overlay (`[role=dialog][aria-label^="Product tour"]`),
   and the tour never starts on its own, so this check cannot tell a mounted tour provider from an
   unmounted one. The unmount itself is guarded by LocalOfflineNotice.test.tsx.

   The spawned server is killed on exit, failure, SIGINT or SIGTERM; the temp config dir it made is
   removed then too (a SIGKILL of the gate leaks both); server.log is rewritten per run.

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
import { assertServedBuild, shellProvenanceMarkers } from './served-build.mjs'
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
// fairtrade's banner button (LocalOfflineBanner renders `.cx-offline-retry` for `try again`).
const RETRY = `${NOTICE} .cx-offline-retry`
const LIVE = 'p[role="status"].sr-only'
const TRANSCRIPT_HOST = '[data-tour="transcript-view"]'
const EXPECTED_COMMAND = PORT === 8690 ? 'peasant web start' : `peasant web start --port ${PORT}`
const TRANSCRIPT_PATH = `/projects/${SHELL_DEFAULT_PROJECT}/${SHELL_DEFAULT_SESSION}/`
/** The floor a full-height page keeps below the header while the notice shows (globals.css --app-body-height). */
const BODY_FLOOR_REM = 24
const PAGES = Object.freeze([
  { id: 'home', path: '/', body: 'main', width: 1440, height: 900 },
  { id: 'transcript', path: TRANSCRIPT_PATH, body: '.txn-app', width: 1440, height: 900, singleScroller: true },
  { id: 'home-mobile', path: '/', body: 'main', width: 390, height: 844 },
  // 400% zoom on a 1280×1024 screen: the notice alone is taller than the viewport, so it must scroll.
  { id: 'home-short', path: '/', body: 'main', width: 320, height: 256, retryReach: true },
  // The same zoom on the transcript: the page scrolls the notice away and the transcript keeps its floor.
  { id: 'transcript-short', path: TRANSCRIPT_PATH, body: '.txn-app', width: 320, height: 256, floor: TRANSCRIPT_HOST },
  // A small phone on /share: the share page keeps its floor below the notice.
  { id: 'share-mobile', path: '/share/', body: '.share-page', width: 320, height: 568, floor: '.share-page' },
  // A roomy screen scrolled down before the app stops: the pinned notice must still be on screen.
  { id: 'home-scrolled', path: '/', body: 'main', width: 1440, height: 700, scrollFirst: true },
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
const ANNOUNCE = manifest.announcements
const escapeRegExp = (text) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
const STILL_PATTERN = new RegExp(`^${escapeRegExp(ANNOUNCE.stillStopped)} \\d{2}:\\d{2}:\\d{2}\\.$`)

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
// What the notice says and offers. Its position and the page's clearance come from
// chromeClearance, the same probe the header gate and the component contract share.
const noticeState = async (page) => ({
  ...await page.evaluate(({ notice, retry, live }) => {
    const section = document.querySelector(notice)
    const status = section?.querySelector('[role="status"]')
    return {
      shown: !!section,
      status: (status?.textContent || '').replace(/\s+/g, ' ').trim(),
      command: (section?.querySelector('.cx-cmd-text')?.textContent || '').trim(),
      retry: (document.querySelector(retry)?.textContent || '').replace(/\s+/g, ' ').trim(),
      live: (document.querySelector(live)?.textContent || '').trim(),
      noticeHeight: document.documentElement.style.getPropertyValue('--app-notice-height'),
      tour: !!document.querySelector('[role="dialog"][aria-label^="Product tour"]'),
      documentOverflow: document.documentElement.scrollHeight - window.innerHeight,
      scrollY: window.scrollY,
    }
  }, { notice: NOTICE, retry: RETRY, live: LIVE }),
  clearance: await page.evaluate(chromeClearance),
})

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

const pressRetry = (page) => page.evaluate((selector) => {
  const button = document.querySelector(selector)
  button?.click()
  return !!button
}, RETRY)

// Scroll `try again` into view and prove a pointer at its centre reaches it (the fixed header must
// not cover it). The scroll is instant, so a smooth-scrolling root cannot leave the measurement
// mid-animation.
const assertRetryReachable = async (page) => {
  const found = await page.evaluate((selector) => {
    const retry = document.querySelector(selector)
    retry?.scrollIntoView({ block: 'center', behavior: 'instant' })
    return !!retry
  }, RETRY)
  if (!found) return { ok: false, reason: 'no try again button', scrollY: 0 }
  await pause(150)
  return page.evaluate((selector) => {
    const retry = document.querySelector(selector)
    if (!retry) return { ok: false, reason: 'no try again button', scrollY: window.scrollY }
    const rect = retry.getBoundingClientRect()
    const header = document.querySelector('header')?.getBoundingClientRect()
    const hit = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2)
    const inView = rect.top >= 0 && rect.bottom <= window.innerHeight
    const belowHeader = !header || rect.top >= header.bottom - 0.5
    const reached = !!hit && (hit === retry || retry.contains(hit))
    return {
      ok: inView && belowHeader && reached,
      reason: `inView=${inView} belowHeader=${belowHeader} reached=${reached} hit=${hit ? hit.tagName.toLowerCase() : 'none'} rect=${Math.round(rect.top)}..${Math.round(rect.bottom)} scrollY=${Math.round(window.scrollY)} viewport=${window.innerHeight}`,
      scrollY: window.scrollY,
    }
  }, RETRY)
}

// Scroll the document to its bottom and prove the full-height page keeps its floor, fully in view
// below the header.
const assertFloorKept = async (page, selector) => {
  await page.evaluate(() => window.scrollTo({ top: document.documentElement.scrollHeight, behavior: 'instant' }))
  await pause(150)
  return page.evaluate((sel, floorRem) => {
    const el = document.querySelector(sel)
    if (!el) return { ok: false, reason: `${sel} did not mount` }
    const rem = Number.parseFloat(getComputedStyle(document.documentElement).fontSize)
    const header = document.querySelector('header').getBoundingClientRect()
    const floor = Math.min(floorRem * rem, window.innerHeight - header.height)
    const rect = el.getBoundingClientRect()
    const tallEnough = rect.height >= floor - 1
    const inView = rect.top >= header.bottom - 1 && rect.bottom <= window.innerHeight + 1
    return {
      ok: tallEnough && inView,
      reason: `height=${Math.round(rect.height)} floor=${Math.round(floor)} top=${Math.round(rect.top)} bottom=${Math.round(rect.bottom)} header=${Math.round(header.bottom)} viewport=${window.innerHeight} scrollY=${Math.round(window.scrollY)}`,
      height: rect.height,
      floor,
      scrollY: window.scrollY,
    }
  }, selector, BODY_FLOOR_REM)
}

// Scroll a roomy page down before the app stops; returns how far it went (it must actually scroll).
const scrollDown = (page) => page.evaluate(() => {
  const target = Math.min(400, document.documentElement.scrollHeight - window.innerHeight)
  window.scrollTo({ top: target, behavior: 'instant' })
  return window.scrollY
})

const capture = async (page, gate, theme, id, { keepScroll = false } = {}) => {
  const outDir = join(OUT, theme)
  mkdirSync(outDir, { recursive: true })
  const file = join(outDir, `${id}.png`)
  await page.evaluate(() => document.fonts.ready)
  if (!keepScroll) await page.evaluate(() => window.scrollTo({ top: 0, behavior: 'instant' }))
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
  const where = `${theme}/${spec.id} (${spec.width}×${spec.height})`

  // connected: the manifest holds and no notice shows, past the socket grace period
  const response = await page.goto(ORIGIN + spec.path, { waitUntil: 'networkidle0' })
  if (!response || response.status() >= 400) throw new Error(`the app answered HTTP ${response ? response.status() : 0} for ${spec.path}`)
  await page.waitForSelector(spec.body, { visible: true, timeout: 15000 })
  await assertHeaderHolds(page, theme, `${spec.path} ${where}, connected`)
  await pause(PAST_GRACE_MS)
  const connected = await noticeState(page)
  if (connected.shown || connected.noticeHeight || connected.live !== '') throw new Error(`the ${where} page shows or announces the offline notice while the app is running: ${JSON.stringify(connected)}`)
  let scrolledTo = 0
  if (spec.scrollFirst) {
    scrolledTo = await scrollDown(page)
    if (scrolledTo < 100) throw new Error(`the ${where} page does not scroll (reached ${scrolledTo}px), so it cannot show a notice arriving on a scrolled page`)
  }

  // stopped: the notice under the header, saying the right thing, the page clearing it
  await stopServer()
  await waitFor(page, (s) => s.shown && s.noticeHeight !== '' && s.clearance.failures.length === 0, OFFLINE_WITHIN_MS, `the ${where} offline notice (placed under the header, <main> clearing both)`)
  // The live region is written by a passive effect after the notice paints; give it that render.
  const state = await waitFor(page, (s) => s.live !== '', 2000, `the ${where} live region announcing the stop`)
  const failures = []
  if (!state.status.includes("peasant isn't running on this computer")) failures.push(`the message does not name this computer: ${JSON.stringify(state.status)}`)
  if (!state.status.includes('your internet is fine')) failures.push(`the message could read as an internet outage: ${JSON.stringify(state.status)}`)
  if (state.command !== EXPECTED_COMMAND) failures.push(`the start command reads ${JSON.stringify(state.command)}, expected ${JSON.stringify(EXPECTED_COMMAND)}`)
  if (state.retry !== 'try again') failures.push(`the retry button reads ${JSON.stringify(state.retry)}, expected "try again"`)
  if (state.live !== ANNOUNCE.stopped) failures.push(`the live region says ${JSON.stringify(state.live)}, expected ${JSON.stringify(ANNOUNCE.stopped)}`)
  if (state.tour) failures.push('a tour overlay is showing')
  if (!/^\d+(\.\d+)?px$/.test(state.noticeHeight)) failures.push(`--app-notice-height is ${JSON.stringify(state.noticeHeight)}`)
  if (spec.singleScroller && state.documentOverflow > 0) failures.push(`the document scrolls ${state.documentOverflow}px under the transcript's own scroller`)
  if (spec.retryReach && state.clearance.notice + state.clearance.header <= spec.height) failures.push(`the short-screen case does not exercise a notice taller than the viewport (${state.clearance.header}+${state.clearance.notice}px in ${spec.height}px)`)
  if (spec.scrollFirst) {
    const top = state.clearance.noticeTop
    if (!state.clearance.pinnedViewport || !state.clearance.noticeFixed) failures.push(`the notice is not pinned on the roomy ${spec.width}×${spec.height} screen`)
    if (top === null || top < state.clearance.header - 1 || top + state.clearance.notice > spec.height + 1) failures.push(`on a page scrolled to ${Math.round(state.scrollY)}px the notice sits at ${top}px, not on screen under the ${state.clearance.header}px header`)
    if (state.scrollY < 100) failures.push(`the page is no longer scrolled (${state.scrollY}px), so the case proves nothing`)
  }
  if (failures.length) throw new Error(`the ${where} page with the server stopped: ${failures.join('; ')}. State: ${JSON.stringify(state)}`)
  await assertHeaderHolds(page, theme, `${spec.path} ${where}, stopped`)

  let note = ''
  if (spec.retryReach) {
    const reach = await assertRetryReachable(page)
    if (!reach.ok) throw new Error(`on the ${where} screen \`try again\` cannot be scrolled into reach: ${reach.reason}`)
    note = `, try again reached after scrolling ${Math.round(reach.scrollY)}px`
    captured.push(await capture(page, gate, theme, spec.id, { keepScroll: true }))
  } else if (spec.floor) {
    const floor = await assertFloorKept(page, spec.floor)
    if (!floor.ok) throw new Error(`on the ${where} screen the full-height page ${spec.floor} does not keep its floor in view below the header: ${floor.reason}`)
    note = `, ${spec.floor} keeps ${Math.round(floor.height)}px (floor ${Math.round(floor.floor)}px) after scrolling ${Math.round(floor.scrollY)}px`
    captured.push(await capture(page, gate, theme, spec.id, { keepScroll: true }))
  } else if (spec.scrollFirst) {
    note = `, pinned at ${Math.round(state.clearance.noticeTop)}px on a page scrolled to ${Math.round(state.scrollY)}px`
    captured.push(await capture(page, gate, theme, spec.id, { keepScroll: true }))
  } else {
    captured.push(await capture(page, gate, theme, spec.id))
  }
  const c = state.clearance
  console.log(`OK offline ${where}: notice ${c.notice}px under the ${c.header}px header (${c.noticeFixed ? 'pinned' : 'in the page flow'}), <main> clears ${c.mainPaddingTop}px, command "${state.command}"${note}`)

  // still down: a failed `try again` is announced, with the time of the check
  if (!await pressRetry(page)) throw new Error(`the ${where} notice has no \`try again\` to press`)
  const still = await waitFor(page, (s) => STILL_PATTERN.test(s.live) && s.retry === 'try again', 5000, `the ${where} live region announcing a failed try again`)
  if (!still.shown) throw new Error(`the ${where} notice went away after a failed try again with the app still stopped`)

  // back: restart, press try again (in the page, so a notice that already cleared is not an error)
  await startServer()
  const pressed = await pressRetry(page)
  await waitFor(page, (s) => !s.shown && s.noticeHeight === '' && s.clearance.failures.length === 0, RECOVER_WITHIN_MS, `the ${where} notice clearing after the restart (with <main> back under the header)`)
  const back = await waitFor(page, (s) => s.live === ANNOUNCE.back, 2000, `the ${where} live region announcing the return`)
  await assertHeaderHolds(page, theme, `${spec.path} ${where}, back`)
  console.log(`OK still+back ${where}: failed try again announced "${still.live}"; ${pressed ? 'try again pressed; ' : 'already reconnected; '}notice gone, page back under the header, live region "${back.live}"`)

  if (diagnostics.length) throw new Error(`page errors appeared on ${where}: ${JSON.stringify(diagnostics.slice(0, 4))}`)
  await page.close()
}

let browser = null
const captured = []
try {
  await startServer()
  const provenance = await assertServedBuild({ origin: ORIGIN, markers: shellProvenanceMarkers(manifest), bin: BIN })
  console.log(`OK provenance: ${BIN} (not older than web/out) serves this checkout's web/out (${provenance.chunks.length} chunks) carrying ${Object.entries(provenance.markerChunks).map(([marker, chunks]) => `${marker} in ${chunks.join(' ')}`).join('; ')}`)
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
    `  Why: when the peasant app on this computer stops, every page must say so under the header — this computer, not the internet — with the start command and try again, reachable on any screen and on a scrolled page, keep full-height pages readable, announce the stop, a failed retry and the return, and clear when the app is back.\n` +
    `  Where: shell-nav-default-gate.mjs driving ${BIN} on ${ORIGIN} (server log: ${serverLog}).\n` +
    `  Means: a user whose local app stopped may see no notice, a notice that reads as an internet outage, a notice covering the page or out of reach, a crushed transcript, silence for a screen reader, or a notice that never clears.\n` +
    `  Fix: make build so bin/peasant embeds this checkout's web/out, free port ${PORT}, then fix the reported notice, header or page geometry.`,
  )
  process.exitCode = 1
} finally {
  try { await browser?.close() } catch {}
  try { await stopServer() } catch {}
  removeOwnConfigDir()
}

if (!process.exitCode) {
  console.log(`\nOK [shell-nav-default-gate.mjs] offline notice verified on ${ORIGIN}: shows under the header when the app stops (pinned where the screen has room, in the page flow elsewhere), names this computer, offers "${EXPECTED_COMMAND}" and try again (reachable at 320×256), keeps the transcript and /share readable on short screens, stays on screen on a scrolled page, announces the stop, a failed try again and the return, clears when the app is back, in ${SMOKE_THEMES.join(' + ')} across ${PAGES.map((p) => `${p.id} ${p.width}×${p.height}`).join(', ')}.`)
  console.log('Offline frames:')
  for (const file of captured) console.log(`  ${file}`)
}
