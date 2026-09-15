/* Real-binary mounted evidence for child context-source navigation.
 *
 * Boots the exact worktree's `bin/peasant` (assets embedded from this branch's
 * `web/out`), then drives the REAL production transcript route through the one
 * Fairtrade adapter and composite. Only two things are supplied by the
 * harness: the relationship evidence and authorized read navigation for one
 * session (the shipped mock data source carries no provenance graph at all),
 * and the current-target session that link resolves to. Everything a reader
 * touches stays real — the route, the shell, the context controls, the target
 * navigation, and the Back restoration.
 *
 * The WebSocket is wrapped (not replaced): every frame from the real server is
 * forwarded unchanged except the `session_detail` frame for the two fixture
 * sessions, so quality/trends/annotations/dashboard remain the real backend's.
 *
 * Usage: CHROME_PATH=/path/to/chrome node context-navigation-shoot.mjs
 * Env:   CAPTURES (output root), PEASANT_CONTEXT_NAV_PORT (default 8793)
 */
import { spawn } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '../../..')
const WEB = join(REPO, 'web')
const BIN = join(REPO, 'bin/peasant')
const OUT = process.env.CAPTURES || '/tmp/opencode/0lo7vk-captures'
const PORT = process.env.PEASANT_CONTEXT_NAV_PORT || '8793'
const ORIGIN = `http://localhost:${PORT}`
const CHROME = process.env.CHROME_PATH
const THEMES = ['dark', 'light']

/** Feature bytes only this slice introduces, located in the served chunk. */
const FEATURE_BYTES = ['peasant:transcript-read-state:']
/** The stored current-target session the context link must open. */
const CONTEXT_TARGET_ID = '1f0c1a5e-9d5b-4a6f-8f6f-2f0a44b1c001'
const STARTER_TARGET_ID = '2a1d2b6f-0e6c-4b70-9a70-3f1b55c2d002'
const CONTEXT_TARGET_TEXT = 'current context source turn'
const STARTER_TARGET_TEXT = 'current starter source turn'
const EARLIER_TEXT = 'migrated earlier work'
const SEARCH_TEXT = 'current'

const pause = (ms) => new Promise((res) => setTimeout(res, ms))
const fail = (message) => { throw new Error(`Context-navigation visual harness failed: ${message}`) }

function filesBelow(directory) {
  if (!existsSync(directory)) return []
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name)
    return entry.isDirectory() ? filesBelow(path) : [path]
  })
}

function assertProvenance() {
  const chunks = join(WEB, 'out/_next/static/chunks')
  if (!existsSync(BIN) || !existsSync(chunks)) fail(`missing ${relative(REPO, BIN)} or exported chunks; run make build in this worktree first`)
  const javascript = filesBelow(chunks).filter((path) => path.endsWith('.js')).map((path) => ({ path, content: readFileSync(path, 'utf8') }))
  const binary = readFileSync(BIN)
  const missingBinaryBytes = FEATURE_BYTES.filter((signature) => !binary.includes(Buffer.from(signature)))
  if (missingBinaryBytes.length) fail(`the embedded binary contains no ${missingBinaryBytes.join(', ')} feature bytes; rebuild this exact worktree so bin/peasant matches web/out`)
  const match = javascript.find(({ content }) => FEATURE_BYTES.every((signature) => content.includes(signature)))
  if (!match) fail(`the built export contains no chunk with the feature bytes ${FEATURE_BYTES.join(', ')}; rebuild this exact worktree`)
  return match
}

/** The current target's own durable content, served for the followed link. */
function targetPayload(id, content) {
  return {
    id,
    harness: 'codex',
    project: 'peasant-labs/engine',
    startTime: '2026-08-28T10:00:00.000Z',
    endTime: '2026-08-28T10:02:00.000Z',
    durationMins: 2,
    totalTokens: 42,
    tokensIn: 30,
    tokensOut: 12,
    turnCount: 2,
    toolCallCount: 0,
    turns: [
      { index: 0, role: 'user', entryType: 'text', depth: 0, content, timestamp: '2026-08-28T10:00:00.000Z' },
      { index: 1, role: 'assistant', entryType: 'text', depth: 0, content: 'acknowledged', timestamp: '2026-08-28T10:01:00.000Z' },
    ],
  }
}

/** The relationship evidence and authorized read navigation for the child. */
function childExtras() {
  return {
    relationships: [
      {
        kind: 'context_from',
        targetState: 'target_known',
        targetLocalId: CONTEXT_TARGET_ID,
        evidence: 'native_typed',
        anchor: { kind: 'general_source_session' },
      },
      { kind: 'started_by', targetState: 'target_known', targetLocalId: STARTER_TARGET_ID, evidence: 'native_typed' },
    ],
    relationshipNavigation: [
      { kind: 'context_from', status: 'resolved', localId: CONTEXT_TARGET_ID },
      { kind: 'started_by', status: 'resolved', localId: STARTER_TARGET_ID },
    ],
    earlierHistory: [
      {
        state: 'uncertain_migrated',
        turns: [
          {
            index: 0,
            role: 'user',
            entryType: 'text',
            depth: 0,
            content: `${EARLIER_TEXT}: the retained earlier work is long enough that the transcript needs a real reading position.`,
            timestamp: '2026-08-28T08:00:00.000Z',
          },
          {
            index: 1,
            role: 'assistant',
            entryType: 'text',
            depth: 0,
            content: `acknowledged ${EARLIER_TEXT}. this turn repeats the retained evidence so the stream has height to scroll. `.repeat(12),
            timestamp: '2026-08-28T08:01:00.000Z',
          },
          {
            index: 2,
            role: 'assistant',
            entryType: 'text',
            depth: 0,
            content: `the earlier work stays separate from the current turns and contributes no own count. `.repeat(12),
            timestamp: '2026-08-28T08:02:00.000Z',
          },
        ],
      },
    ],
  }
}

/** Fetch the live mock catalog: the fixture project's hash and a session in it. */
async function resolveCoordinates() {
  const summary = await fetch(`${ORIGIN}/api/v1/projects/summary`).then((res) => res.json())
  const project = (summary?.projects ?? []).find((entry) => entry.project === 'fortuna')
  if (!project?.projectHash) fail(`the mock catalog serves no fortuna project: ${JSON.stringify(summary).slice(0, 400)}`)
  const payload = await fetch(`${ORIGIN}/api/v1/sessions`).then((res) => res.json())
  const sessions = (payload?.sessions ?? []).filter((entry) => entry?.projectHash === project.projectHash)
  if (sessions.length === 0) fail(`the mock catalog serves no session in ${project.projectHash}: ${JSON.stringify(payload).slice(0, 400)}`)
  return { projectHash: project.projectHash, childId: sessions[0].id, sessionCount: sessions.length }
}

/**
 * Wrap the page's WebSocket so the two fixture sessions' `session_detail`
 * frames carry the harness-supplied durable evidence and target content.
 * Everything else — every other channel and every other frame — passes through
 * from the real server untouched.
 */
async function installSessionDetailRewrite(page, payloads) {
  await page.evaluateOnNewDocument((fixturePayloads) => {
    const Real = window.WebSocket
    const rewrite = (raw) => {
      let frame
      try { frame = JSON.parse(raw) } catch { return raw }
      const id = typeof frame?.id === 'string' ? frame.id : frame?.data?.id
      const replacement = id ? fixturePayloads[id] : undefined
      if (!replacement) return raw
      if (frame.type === 'session_detail') {
        frame.data = { ...frame.data, ...replacement.extras }
        return JSON.stringify(frame)
      }
      return JSON.stringify({ type: 'session_detail', id, data: replacement.detail })
    }
    class ContextNavigationWebSocket extends Real {
      set onmessage(handler) {
        super.onmessage = handler ? (event) => handler(new MessageEvent('message', { data: rewrite(String(event.data)) })) : null
      }
      get onmessage() { return super.onmessage }
    }
    window.WebSocket = ContextNavigationWebSocket
  }, payloads)
}

async function capture(page, gate, file, selector, label) {
  mkdirSync(dirname(file), { recursive: true })
  const element = await page.waitForSelector(selector, { visible: true, timeout: 20000 }).catch(() => null)
  if (!element) fail(`${label}: selector ${selector} never mounted`)
  const box = await element.boundingBox()
  if (!box || box.width < 4 || box.height < 4) fail(`${label}: blank or zero-sized ${selector}`)
  await element.screenshot({ path: file, captureBeyondViewport: false })
  await gate.assert(label, file, { sel: selector, where: 'context-navigation-shoot.mjs' })
}

async function runTheme(page, theme, gate, coordinates, payloads, evidence) {
  await page.setViewport({ width: 1440, height: 1100, deviceScaleFactor: 1 })
  await page.evaluateOnNewDocument((value) => {
    localStorage.setItem('peasant-theme', value)
    document.documentElement.setAttribute('data-theme', value)
    document.documentElement.setAttribute('data-tb-theme', value)
  }, theme)
  await installSessionDetailRewrite(page, payloads)

  const childUrl = `${ORIGIN}/projects/${coordinates.projectHash}/${encodeURIComponent(coordinates.childId)}/`
  const child = await page.goto(childUrl, { waitUntil: 'domcontentloaded' })
  if (child?.status() !== 200) fail(`${theme}: child route HTTP ${child?.status()}`)
  await page.waitForSelector('.txn-context-link', { visible: true, timeout: 20000 }).catch(() => fail(`${theme}: the context link never mounted on the real route`))

  const childProbe = await page.evaluate(() => {
    const rows = [...document.querySelectorAll('.txn-context-source')]
    const link = document.querySelector('.txn-context-link')
    const chrome = link ? getComputedStyle(link) : null
    return {
      theme: document.documentElement.getAttribute('data-theme'),
      font: getComputedStyle(document.body).fontFamily,
      labels: rows.map((row) => row.querySelector('.txn-context-row span:not(.txn-context-status)')?.textContent ?? null),
      controls: [...document.querySelectorAll('.txn-context-link')].map((node) => node.textContent),
      statuses: rows.map((row) => row.querySelector('.txn-context-status')?.textContent ?? null),
      notes: rows.map((row) => row.querySelector('.txn-context-note')?.textContent ?? null),
      radius: chrome?.borderRadius,
      height: link?.getBoundingClientRect().height ?? 0,
      family: chrome?.fontFamily,
      size: chrome?.fontSize,
      fontLoaded: typeof document.fonts?.check === 'function' ? document.fonts.check('14px atkinsonHyperlegibleMono') : false,
      fontLinks: [...document.querySelectorAll('head link')].map((node) => node.getAttribute('href') || '').filter((href) => href.length > 0),
      tabularNums: getComputedStyle(document.querySelector('.txn-header .tnum') ?? document.body).fontVariantNumeric,
      earlierToggle: !!document.querySelector('.txn-earlier-toggle[aria-expanded="false"]'),
    }
  })
  if (childProbe.theme !== theme || !/atkinson/i.test(childProbe.font)) fail(`${theme}: theme/font probe ${JSON.stringify(childProbe)}`)
  if (childProbe.labels[0] !== 'context inherited from' || childProbe.labels[1] !== 'started by') fail(`${theme}: context labels ${JSON.stringify(childProbe.labels)}`)
  if (childProbe.controls.length !== 2 || childProbe.controls.some((text) => text !== 'open current session')) fail(`${theme}: context controls ${JSON.stringify(childProbe.controls)}`)
  if (childProbe.notes[0]?.includes('branch point')) fail(`${theme}: the host promised a branch point it cannot open: ${JSON.stringify(childProbe.notes)}`)
  if (childProbe.notes[1] !== null) fail(`${theme}: the starter row carries a context note: ${JSON.stringify(childProbe.notes)}`)
  if (childProbe.radius !== '0px' || childProbe.height < 24) fail(`${theme}: context control geometry ${JSON.stringify(childProbe)}`)
  if (!/atkinsonhyperlegible ?mono/i.test(childProbe.family) || childProbe.size !== '14px') fail(`${theme}: context control chrome typography ${JSON.stringify(childProbe)}`)
  if (!childProbe.fontLoaded || childProbe.fontLinks.length === 0) fail(`${theme}: the layout head does not load the hyperlegible families ${JSON.stringify({ fontLoaded: childProbe.fontLoaded, fontLinks: childProbe.fontLinks })}`)
  if (!childProbe.tabularNums.includes('tabular-nums')) fail(`${theme}: counts are not tabular: ${childProbe.tabularNums}`)
  if (!childProbe.earlierToggle) fail(`${theme}: the collapsed earlier-history disclosure never mounted`)
  evidence.checks.push({ theme, childProbe })
  await capture(page, gate, join(OUT, theme, 'context-child.png'), '.txn-app', `${theme}/context-child`)

  // A reading position to restore: expanded earlier history, a search query,
  // a scrolled stream, and a selected turn.
  await page.click('.txn-earlier-toggle')
  await page.waitForFunction(() => document.querySelector('.txn-earlier-toggle')?.getAttribute('aria-expanded') === 'true', { timeout: 10000 })
  await page.keyboard.down('Control')
  await page.keyboard.press('f')
  await page.keyboard.up('Control')
  await page.waitForSelector('.txn-search-input', { visible: true, timeout: 10000 }).catch(() => fail(`${theme}: the search input never opened`))
  await page.type('.txn-search-input', SEARCH_TEXT)
  const savedScroll = await page.evaluate(() => {
    const stream = document.querySelector('.txn-stream')
    const max = stream.scrollHeight - stream.clientHeight
    stream.scrollTop = Math.floor(max / 2)
    stream.dispatchEvent(new Event('scroll', { bubbles: true }))
    return { scrollTop: stream.scrollTop, max }
  })
  if (!(savedScroll.max > 80) || !(savedScroll.scrollTop > 0)) fail(`${theme}: the transcript stream does not scroll (${JSON.stringify(savedScroll)})`)
  await page.waitForFunction(() => document.querySelector('.txn-turnwrap.txn-active, .txn-turnwrap .txn-turn.txn-active') != null, { timeout: 10000 })
    .catch(() => fail(`${theme}: no turn became active after scrolling`))
  const savedTurn = await page.evaluate(() => document.querySelector('.txn-turnwrap:has(.txn-turn.txn-active)')?.getAttribute('data-turn') ?? null)

  // Follow the real context control. The host pushes the current-target route.
  await page.evaluate(() => { document.querySelectorAll('.txn-context-link')[0].click() })
  await page.waitForFunction((id) => window.location.pathname.includes(id), { timeout: 15000 }, CONTEXT_TARGET_ID)
    .catch(() => fail(`${theme}: following the context link never opened ${CONTEXT_TARGET_ID}`))
  await page.waitForFunction((text) => document.body.textContent.includes(text), { timeout: 15000 }, CONTEXT_TARGET_TEXT)
    .catch(() => fail(`${theme}: the current target's own content never rendered`))
  const targetProbe = await page.evaluate((id) => ({
    url: window.location.href,
    hasChildContext: !!document.querySelector('.txn-context-link'),
    theme: document.documentElement.getAttribute('data-theme'),
  }), CONTEXT_TARGET_ID)
  if (!targetProbe.url.includes(CONTEXT_TARGET_ID)) fail(`${theme}: the target route is not the resolved source: ${targetProbe.url}`)
  if (targetProbe.theme !== theme) fail(`${theme}: the target route rendered theme ${targetProbe.theme}`)
  if (targetProbe.hasChildContext) fail(`${theme}: the child's context controls leaked onto the target session`)
  evidence.checks.push({ theme, targetProbe })
  await capture(page, gate, join(OUT, theme, 'context-target.png'), '.txn-app', `${theme}/context-target`)

  // Browser Back re-mounts the child route; the host restores where the reader was.
  await page.goBack({ waitUntil: 'domcontentloaded' })
  await page.waitForFunction((id) => window.location.pathname.includes(id), { timeout: 15000 }, encodeURIComponent(coordinates.childId))
    .catch(() => fail(`${theme}: Back never returned to the child route`))
  await page.waitForSelector('.txn-context-link', { visible: true, timeout: 20000 }).catch(() => fail(`${theme}: the child context link did not re-mount after Back`))
  await page.waitForFunction(() => document.querySelector('.txn-earlier-toggle')?.getAttribute('aria-expanded') === 'true', { timeout: 15000 })
    .catch(() => fail(`${theme}: Back did not restore the expanded earlier-history disclosure`))
  const restored = await page.evaluate(() => {
    const stream = document.querySelector('.txn-stream')
    const active = document.querySelector('.txn-turnwrap:has(.txn-turn.txn-active)')
    return {
      scrollTop: stream?.scrollTop ?? 0,
      turn: active?.getAttribute('data-turn') ?? null,
      contextControls: document.querySelectorAll('.txn-context-link').length,
    }
  })
  if (restored.scrollTop !== savedScroll.scrollTop) fail(`${theme}: Back restored scrollTop ${restored.scrollTop}, expected ${savedScroll.scrollTop}`)
  if (restored.turn !== savedTurn) fail(`${theme}: Back restored selected turn ${restored.turn}, expected ${savedTurn}`)
  if (restored.contextControls !== 2) fail(`${theme}: Back lost the context controls (${restored.contextControls})`)
  await page.keyboard.down('Control')
  await page.keyboard.press('f')
  await page.keyboard.up('Control')
  const restoredSearch = await page.waitForSelector('.txn-search-input', { visible: true, timeout: 10000 })
    .then((input) => input.evaluate((node) => node.value))
    .catch(() => fail(`${theme}: the search input did not reopen after Back`))
  if (restoredSearch !== SEARCH_TEXT) fail(`${theme}: Back restored search ${JSON.stringify(restoredSearch)}, expected ${JSON.stringify(SEARCH_TEXT)}`)
  evidence.checks.push({ theme, savedScroll, savedTurn, restored, restoredSearch })
  await capture(page, gate, join(OUT, theme, 'context-back-restored.png'), '.txn-app', `${theme}/context-back-restored`)
}

if (!CHROME) fail('CHROME_PATH is unset; set it to google-chrome or chromium')
const chunk = assertProvenance()
mkdirSync(OUT, { recursive: true })
const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default
const server = spawn(BIN, ['web', 'start', '--port', PORT, '--foreground', '--no-browser', '--mock-data-store=web,sessions,qualitySessions,annotations'], { cwd: REPO, stdio: ['ignore', 'ignore', 'pipe'] })
let serverError = ''
server.stderr.on('data', (data) => { serverError += data.toString() })
const evidence = { chunk: relative(REPO, chunk.path), featureBytes: FEATURE_BYTES, checks: [], captures: [] }
const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: null })
try {
  let healthy = false
  for (let i = 0; i < 60 && !healthy; i++) { healthy = (await fetch(`${ORIGIN}/api/v1/health`).catch(() => null))?.status === 200; if (!healthy) await pause(250) }
  if (!healthy) fail(`the real binary did not become healthy on ${ORIGIN}: ${serverError.trim()}`)

  // Served-build provenance: the exact artifact under test must carry this change.
  const servedPath = `/_next/static/chunks/${relative(join(WEB, 'out/_next/static/chunks'), chunk.path).split('\\').join('/')}`
  const served = await fetch(`${ORIGIN}${servedPath}`)
  const servedBody = await served.text()
  if (served.status !== 200 || !FEATURE_BYTES.every((signature) => servedBody.includes(signature))) {
    fail(`served provenance: ${servedPath} returned HTTP ${served.status} without ${FEATURE_BYTES.join(', ')}; stop stale servers, rebuild this exact worktree, and rerun`)
  }
  console.log(`provenance chunk=${evidence.chunk} served=true bytes=${FEATURE_BYTES.join(',')}`)

  const coordinates = await resolveCoordinates()
  console.log(`coordinates projectHash=${coordinates.projectHash} child=${coordinates.childId} sessions=${coordinates.sessionCount}`)
  const payloads = {
    [coordinates.childId]: { extras: childExtras() },
    [CONTEXT_TARGET_ID]: { detail: targetPayload(CONTEXT_TARGET_ID, CONTEXT_TARGET_TEXT) },
    [STARTER_TARGET_ID]: { detail: targetPayload(STARTER_TARGET_ID, STARTER_TARGET_TEXT) },
  }

  const gate = new SurfaceGate(await browser.newPage())
  for (const theme of THEMES) {
    const page = await browser.newPage()
    try {
      await applyDeterminism(page)
      await runTheme(page, theme, gate, coordinates, payloads, evidence)
      console.log(`OK ${theme}`)
    } finally {
      await page.close()
    }
  }
  for (const theme of THEMES) {
    for (const surface of ['context-child', 'context-target', 'context-back-restored']) {
      evidence.captures.push(join(OUT, theme, `${surface}.png`))
    }
  }
  writeFileSync(join(OUT, 'evidence.json'), JSON.stringify(evidence, null, 2))
  console.log(`captures=${OUT}/{dark,light}/{context-child,context-target,context-back-restored}.png`)
} finally {
  await browser.close()
  server.kill('SIGTERM')
}
