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
     2. for each page case in testdata/shell-offline-cases.yaml (loaded strictly, every name
        required), in both themes, one at a time (the page under test is always the active tab):
        a. connected: the header manifest holds and no notice shows, past the socket grace period;
        b. stopped: the server process is killed with the page open. Within a few seconds the page
           shows fairtrade's LocalOfflineBanner directly under the fixed header — pinned there where
           the screen has room, at the top of the page and scrolling with it elsewhere
           (shell-header-manifest.mjs chromeClearance decides which from the notice-pinned query
           that app-shell-geometry.yaml records, and checks that <main> clears the header plus the
           notice, that the root's scroll padding covers what stays fixed, and that <main> carries
           no tabindex at rest): it says the peasant app isn't running on THIS computer and that
           the internet is fine, offers `peasant web start --port <this page's port>` and
           `try again`; the always-mounted live region says the manifest's `stopped` text; the
           header manifest still holds. Case-specific checks follow (below), then the frame is
           captured;
        c. still down: `try again` is pressed with the server still stopped; the live region says
           the manifest's `stillStopped` text followed by the check time (hh:mm:ss.);
        d. back: the server restarts on the same port, `try again` is pressed, the notice goes away
           with --app-notice-height cleared, and the live region says the manifest's `back` text.

   The cases (the fixture's `check` field): home and a transcript at 1440×900 (plain; the transcript
   must not scroll the document: its own stream is the scroller); home at 390×844 (plain); home at
   320×256 (retry-reach: 400% zoom on 1280×1024, the notice is taller than the screen, and the
   document must scroll `try again` into reach below the header); the transcript at 320×256 and
   /share at 320×568 (floor: scrolled to the bottom, the full-height page keeps
   min(24rem, screen height − header) fully in view below the header, so an already-loaded
   transcript stays readable); and home at 1440×700 (scrolled: scrolled down before the server
   stops, the pinned notice must appear inside the screen, under the header).

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
import YAML from 'yaml'
import { closeSync, mkdirSync, mkdtempSync, openSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { fileURLToPath } from 'node:url'
import { dirname, join, resolve } from 'node:path'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'
import { SHELL_DEFAULT_PROJECT, SHELL_DEFAULT_SESSION, SMOKE_MOCKS, SMOKE_THEMES } from './smoke-surfaces.mjs'
import { assertKnownProject } from './validate-mock-coordinates.mjs'
import { assertServedBuild, shellProvenanceMarkers } from './served-build.mjs'
import { chromeClearance, headerFailures, loadNoticePinnedQuery, loadShellHeaderManifest, shippedItems, WEB_ROOT } from './shell-header-manifest.mjs'

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
const EXPECTED_COMMAND = PORT === 8690 ? 'peasant web start' : `peasant web start --port ${PORT}`
const TRANSCRIPT_PATH = `/projects/${SHELL_DEFAULT_PROJECT}/${SHELL_DEFAULT_SESSION}/`
/** The floor a full-height page keeps below the header while the notice shows, as the geometry fixture records it. */
const BODY_FLOOR_REM = (() => {
  const geometry = YAML.parse(readFileSync(join(WEB_ROOT, 'src', 'components', 'testdata', 'app-shell-geometry.yaml'), 'utf8'))
  const match = /^(\d+(?:\.\d+)?)rem$/.exec(String(geometry?.bodyFloor ?? ''))
  if (!match) throw new Error(`app-shell-geometry.yaml bodyFloor must be a rem length, got ${JSON.stringify(geometry?.bodyFloor)}`)
  return Number(match[1])
})()
const CASES_FIXTURE = join(HERE, 'testdata', 'shell-offline-cases.yaml')
const REQUIRED_CASES = ['home', 'transcript', 'home-mobile', 'home-short', 'transcript-short', 'share-mobile', 'home-scrolled', 'analytics-keep-place']
const CHECKS = ['plain', 'retry-reach', 'floor', 'scrolled', 'keep-place']
const RECOVERIES = ['pointer', 'keyboard']

// The page cases: strict YAML, known fields only, one check each, and every required name.
const loadCases = () => {
  const fail = (what) => { throw new Error(`${CASES_FIXTURE}: ${what}`) }
  const document = YAML.parseDocument(readFileSync(CASES_FIXTURE, 'utf8'), { strict: true, uniqueKeys: true })
  if (document.errors.length) fail(`invalid YAML: ${document.errors.map((error) => error.message).join('; ')}`)
  const root = document.toJS()
  if (!root || typeof root !== 'object' || Array.isArray(root) || Object.keys(root).join() !== 'cases' || !Array.isArray(root.cases)) fail('the root must be a mapping with exactly one `cases` list')
  const allowed = ['name', 'path', 'body', 'width', 'height', 'check', 'floor', 'singleScroller', 'recover']
  const names = new Set()
  const cases = root.cases.map((row, index) => {
    if (!row || typeof row !== 'object' || Array.isArray(row)) fail(`cases[${index}] must be a mapping`)
    const unknown = Object.keys(row).filter((key) => !allowed.includes(key))
    if (unknown.length) fail(`cases[${index}] has unknown fields: ${unknown.join(', ')}`)
    for (const key of ['name', 'path', 'body', 'check']) if (typeof row[key] !== 'string' || row[key].trim() === '') fail(`cases[${index}].${key} must be a non-empty string`)
    for (const key of ['width', 'height']) if (!Number.isInteger(row[key]) || row[key] <= 0) fail(`cases[${index}].${key} must be a positive integer`)
    if (names.has(row.name)) fail(`cases[${index}] duplicates name ${row.name}`)
    names.add(row.name)
    if (!CHECKS.includes(row.check)) fail(`case ${row.name}: check must be one of ${CHECKS.join(', ')}, got ${JSON.stringify(row.check)}`)
    if ((row.check === 'floor') !== ('floor' in row)) fail(`case ${row.name}: \`floor\` is required with check: floor and allowed only there`)
    if ('floor' in row && (typeof row.floor !== 'string' || row.floor.trim() === '')) fail(`case ${row.name}: floor must be a selector`)
    if ('singleScroller' in row && typeof row.singleScroller !== 'boolean') fail(`case ${row.name}: singleScroller must be a boolean`)
    if ('recover' in row && !RECOVERIES.includes(row.recover)) fail(`case ${row.name}: recover must be one of ${RECOVERIES.join(', ')}, got ${JSON.stringify(row.recover)}`)
    return { ...row, path: row.path.replaceAll('$TRANSCRIPT', TRANSCRIPT_PATH) }
  })
  const missing = REQUIRED_CASES.filter((name) => !names.has(name))
  if (missing.length) fail(`required cases are missing: ${missing.join(', ')}`)
  if (!cases.some((row) => row.recover === 'keyboard')) fail('one case must recover through the keyboard (recover: keyboard)')
  return cases
}
const PAGES = Object.freeze(loadCases())
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
const PINNED_QUERY = loadNoticePinnedQuery()
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
  clearance: await page.evaluate(chromeClearance, PINNED_QUERY),
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
  const failures = [...await page.evaluate(headerFailures, manifest, { theme, shipped }), ...(await page.evaluate(chromeClearance, PINNED_QUERY)).failures]
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
    // The case must be one where the floor binds (the page would be shorter without it), and the
    // page must keep exactly that floor in view.
    const binds = Math.abs(rect.height - floor) <= 1
    const inView = rect.top >= header.bottom - 1 && rect.bottom <= window.innerHeight + 1
    return {
      ok: binds && inView,
      reason: `height=${Math.round(rect.height)} floor=${Math.round(floor)} top=${Math.round(rect.top)} bottom=${Math.round(rect.bottom)} header=${Math.round(header.bottom)} viewport=${window.innerHeight} scrollY=${Math.round(window.scrollY)}`,
      height: rect.height,
      floor,
      scrollY: window.scrollY,
    }
  }, selector, BODY_FLOOR_REM)
}

// Scroll a page down before the app stops; returns how far it went (it must actually scroll).
const scrollDown = (page) => page.evaluate(() => {
  const target = Math.min(400, document.documentElement.scrollHeight - window.innerHeight)
  window.scrollTo({ top: target, behavior: 'instant' })
  return window.scrollY
})

// Sample every frame whether the notice shows, its height, and the scroll position, so the notice's
// own effect is read at the frame it appears or goes — apart from the page's own connection states,
// which change in other frames (or, on the return, change the layout without moving the scroll).
const startFrameSampler = (page) => page.evaluate((notice) => {
  if (window.__offlineGateFrames) cancelAnimationFrame(window.__offlineGateFrames.handle)
  const samples = []
  const tick = () => {
    const shown = !!document.querySelector(notice)
    samples.push({ shown, height: Number.parseFloat(document.documentElement.style.getPropertyValue('--app-notice-height')) || 0, scrollY: window.scrollY })
    window.__offlineGateFrames.handle = requestAnimationFrame(tick)
  }
  window.__offlineGateFrames = { samples, handle: 0 }
  tick()
}, NOTICE)
// The frame the notice appeared (appearing) or went (!appearing): scroll and height either side.
const frameTransition = (page, appearing) => page.evaluate((appearing) => {
  const samples = window.__offlineGateFrames?.samples || []
  const at = samples.findIndex((sample, index) => index > 0 && sample.shown === appearing && samples[index - 1].shown !== appearing)
  if (at < 0) return null
  return { before: samples[at - 1], after: samples[at] }
}, appearing)

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
  const where = `${theme}/${spec.name} (${spec.width}×${spec.height})`

  // connected: the manifest holds and no notice shows, past the socket grace period
  const response = await page.goto(ORIGIN + spec.path, { waitUntil: 'networkidle0' })
  if (!response || response.status() >= 400) throw new Error(`the app answered HTTP ${response ? response.status() : 0} for ${spec.path}`)
  await page.waitForSelector(spec.body, { visible: true, timeout: 15000 })
  await assertHeaderHolds(page, theme, `${spec.path} ${where}, connected`)
  await pause(PAST_GRACE_MS)
  const connected = await noticeState(page)
  if (connected.shown || connected.noticeHeight || connected.live !== '') throw new Error(`the ${where} page shows or announces the offline notice while the app is running: ${JSON.stringify(connected)}`)
  let scrolledTo = 0
  if (spec.check === 'scrolled' || spec.check === 'keep-place') {
    scrolledTo = await scrollDown(page)
    if (scrolledTo < 100) throw new Error(`the ${where} page does not scroll (reached ${scrolledTo}px), so it cannot show a notice arriving on a scrolled page`)
  }
  if (spec.check === 'keep-place') {
    await pause(150)
    await startFrameSampler(page)
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
  if (spec.check === 'retry-reach' && state.clearance.notice + state.clearance.header <= spec.height) failures.push(`the short-screen case does not exercise a notice taller than the viewport (${state.clearance.header}+${state.clearance.notice}px in ${spec.height}px)`)
  if (spec.check === 'scrolled') {
    const top = state.clearance.noticeTop
    if (!state.clearance.pinnedViewport || !state.clearance.noticeFixed) failures.push(`the notice is not pinned on the roomy ${spec.width}×${spec.height} screen`)
    if (top === null || top < state.clearance.header - 1 || top + state.clearance.notice > spec.height + 1) failures.push(`on a page scrolled to ${Math.round(state.scrollY)}px the notice sits at ${top}px, not on screen under the ${state.clearance.header}px header`)
    if (state.scrollY < 100) failures.push(`the page is no longer scrolled (${state.scrollY}px), so the case proves nothing`)
  }
  let appeared = null
  if (spec.check === 'keep-place') {
    if (state.clearance.noticeFixed) failures.push(`the notice is pinned on the ${spec.width}×${spec.height} screen, so the case does not exercise a notice in the page flow`)
    appeared = await frameTransition(page, true)
    if (!appeared) failures.push('no frame caught the notice appearing')
    else if (Math.abs(appeared.after.scrollY - appeared.before.scrollY - appeared.after.height) > 1) {
      failures.push(`in the frame the ${appeared.after.height}px notice appeared the page scrolled ${Math.round(appeared.before.scrollY)} → ${Math.round(appeared.after.scrollY)}px, so the reader lost their place`)
    }
  }
  if (failures.length) throw new Error(`the ${where} page with the server stopped: ${failures.join('; ')}. State: ${JSON.stringify(state)}`)
  await assertHeaderHolds(page, theme, `${spec.path} ${where}, stopped`)

  let note = ''
  switch (spec.check) {
    case 'retry-reach': {
      const reach = await assertRetryReachable(page)
      if (!reach.ok) throw new Error(`on the ${where} screen \`try again\` cannot be scrolled into reach: ${reach.reason}`)
      note = `, try again reached after scrolling ${Math.round(reach.scrollY)}px`
      captured.push(await capture(page, gate, theme, spec.name, { keepScroll: true }))
      break
    }
    case 'floor': {
      const floor = await assertFloorKept(page, spec.floor)
      if (!floor.ok) throw new Error(`on the ${where} screen the full-height page ${spec.floor} does not keep its floor in view below the header: ${floor.reason}`)
      note = `, ${spec.floor} keeps ${Math.round(floor.height)}px (floor ${Math.round(floor.floor)}px) after scrolling ${Math.round(floor.scrollY)}px`
      captured.push(await capture(page, gate, theme, spec.name, { keepScroll: true }))
      break
    }
    case 'scrolled':
      note = `, pinned at ${Math.round(state.clearance.noticeTop)}px on a page scrolled to ${Math.round(state.scrollY)}px`
      captured.push(await capture(page, gate, theme, spec.name, { keepScroll: true }))
      break
    case 'keep-place':
      note = `, the page scrolled ${Math.round(appeared.before.scrollY)} → ${Math.round(appeared.after.scrollY)}px in the frame the ${appeared.after.height}px notice appeared`
      captured.push(await capture(page, gate, theme, spec.name, { keepScroll: true }))
      break
    default:
      captured.push(await capture(page, gate, theme, spec.name))
  }
  const c = state.clearance
  console.log(`OK offline ${where}: notice ${c.notice}px under the ${c.header}px header (${c.noticeFixed ? 'pinned' : 'in the page flow'}), <main> clears ${c.mainPaddingTop}px, scroll padding ${c.scrollPaddingTop}px, command "${state.command}"${note}`)

  // still down: a failed `try again` is announced, with the time of the check
  if (!await pressRetry(page)) throw new Error(`the ${where} notice has no \`try again\` to press`)
  const still = await waitFor(page, (s) => STILL_PATTERN.test(s.live) && s.retry === 'try again', 5000, `the ${where} live region announcing a failed try again`)
  if (!still.shown) throw new Error(`the ${where} notice went away after a failed try again with the app still stopped`)

  // back: restart, press try again (in the page, so a notice that already cleared is not an error;
  // or, for a keyboard case, focus it and press Enter, as a keyboard user would)
  await startServer()
  const keyboard = spec.recover === 'keyboard'
  let pressed
  let beforeRecovery = null
  if (keyboard) {
    await page.focus(RETRY)
    beforeRecovery = await page.evaluate(() => window.scrollY)
    await page.keyboard.press('Enter')
    pressed = true
  } else {
    pressed = await pressRetry(page)
  }
  await waitFor(page, (s) => !s.shown && s.noticeHeight === '' && s.clearance.failures.length === 0, RECOVER_WITHIN_MS, `the ${where} notice clearing after the restart (with <main> back under the header)`)
  const back = await waitFor(page, (s) => s.live === ANNOUNCE.back, 2000, `the ${where} live region announcing the return`)
  if (spec.check === 'keep-place') {
    const went = await frameTransition(page, false)
    if (!went) throw new Error(`on ${where} no frame caught the notice going`)
    if (went.before.scrollY <= went.before.height) throw new Error(`on ${where} the page was at ${Math.round(went.before.scrollY)}px when the notice went, too high to show whether the reader keeps their place`)
    // The page's own connection strip goes in the same commit as the notice (both follow the socket
    // coming back), and the browser may anchor-scroll for it too, so the return is read as "scrolled
    // back by at least the notice's height"; the appearance above is read exactly.
    if (went.before.scrollY - went.after.scrollY < went.before.height - 1) {
      throw new Error(`on ${where} in the frame the ${went.before.height}px notice went the page scrolled only ${Math.round(went.before.scrollY)} → ${Math.round(went.after.scrollY)}px, so the reader lost their place`)
    }
  }
  let keyNote = ''
  if (keyboard) {
    // Focus was on `try again` when the notice went: it moves to <main>, which is focusable only for
    // that move, without scrolling; the next Tab goes into the page and <main> drops its tabindex.
    const moved = await page.evaluate(() => ({ active: document.activeElement?.tagName.toLowerCase(), tabindex: document.querySelector('main')?.getAttribute('tabindex'), scrollY: window.scrollY }))
    if (moved.active !== 'main' || moved.tabindex !== '-1') throw new Error(`on ${where} the keyboard recovery left focus on ${moved.active} (main tabindex ${JSON.stringify(moved.tabindex)}), not on <main>`)
    if (Math.abs(moved.scrollY - beforeRecovery) > 1) throw new Error(`on ${where} the focus move scrolled the page ${Math.round(beforeRecovery)} → ${Math.round(moved.scrollY)}px`)
    await page.keyboard.press('Tab')
    await pause(200)
    const tabbed = await page.evaluate(() => ({ active: document.activeElement?.tagName.toLowerCase(), tabindex: document.querySelector('main')?.getAttribute('tabindex'), scrollY: window.scrollY }))
    if (tabbed.active === 'main' || tabbed.tabindex !== null) throw new Error(`on ${where} Tab after the focus move left <main> focused or focusable (focus ${tabbed.active}, tabindex ${JSON.stringify(tabbed.tabindex)})`)
    keyNote = `; keyboard recovery put focus on <main> without scrolling, and Tab moved on to ${tabbed.active} with <main> no longer focusable`
  }
  await assertHeaderHolds(page, theme, `${spec.path} ${where}, back`)
  console.log(`OK still+back ${where}: failed try again announced "${still.live}"; ${pressed ? (keyboard ? 'try again pressed with Enter; ' : 'try again pressed; ') : 'already reconnected; '}notice gone, page back under the header, live region "${back.live}"${keyNote}`)

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
    `  Why: when the peasant app on this computer stops, every page must say so under the header — this computer, not the internet — with the start command and try again, reachable on any screen and on a scrolled page, keep the reader's place and full-height pages readable, announce the stop, a failed retry and the return, clear when the app is back, and hand keyboard focus back to the page.\n` +
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
  console.log(`\nOK [shell-nav-default-gate.mjs] offline notice verified on ${ORIGIN}: shows under the header when the app stops (pinned where the screen has room, in the page flow elsewhere), names this computer, offers "${EXPECTED_COMMAND}" and try again (reachable at 320×256), keeps the transcript and /share at their floor on short screens, stays on screen on a scrolled page, keeps a scrolled reader's place where it scrolls, announces the stop, a failed try again and the return, clears when the app is back, and returns keyboard focus to the page, in ${SMOKE_THEMES.join(' + ')} across ${PAGES.map((p) => `${p.name} ${p.width}×${p.height}`).join(', ')}.`)
  console.log('Offline frames:')
  for (const file of captured) console.log(`  ${file}`)
}
