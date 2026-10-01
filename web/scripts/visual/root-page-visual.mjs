/* Real-binary mounted evidence for the root page: the stats strip, the search
 * box, and the session list with its publish-state filters.
 *
 * It boots bin/peasant twice, each on its own throwaway XDG directories so no
 * real session store is read:
 *   - with the mock store (`web,dashboard,sessions,trends,qualitySessions,search`), which serves the WebSocket topics
 *     (sessions, titles, dashboard, trends), the project summary and search;
 *   - with no mock store, for the empty install.
 * The mock store holds no sync list and no publication receipts, so the
 * browser fulfils GET /api/v1/sync/sessions and GET /api/v1/publications from
 * the mock sessions and the pattern in testdata/root-page.yaml. The project
 * summary is intercepted only for the selection notice and recovery states.
 * The corpus's fixed historical dates leave current trends empty. The named
 * trends fixture replaces only trends payloads on the real WebSocket, so the
 * mounted stats strip and eight weekly bars have representative evidence.
 * The page, the shell, the fairtrade components and every request path stay
 * real.
 *
 * States, each in both themes at desktop and phone width: the list under each
 * filter (all, not published, published, auto), a search, the selection
 * notice, the selection recovery panel, and the empty install. Every filter's
 * count is checked against the rows it lists, with `load more` pressed until
 * the list is complete.
 *
 * Usage: CHROME_PATH=/path/to/chrome node scripts/visual/root-page-visual.mjs
 * env:   CAPTURES (output dir, default /tmp/peasant-root-page),
 *        PEASANT_ROOT_PORT (default 8731; the empty install uses port + 1)
 */
import { spawn } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import YAML from 'yaml'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '../../..')
const WEB = join(REPO, 'web')
const BIN = join(REPO, 'bin/peasant')
const OUT = process.env.CAPTURES || join(tmpdir(), 'peasant-root-page')
const PORT = Number(process.env.PEASANT_ROOT_PORT || '8731')
const MOCK_ORIGIN = `http://localhost:${PORT}`
const EMPTY_ORIGIN = `http://localhost:${PORT + 1}`
const CHROME = process.env.CHROME_PATH
const THEMES = ['dark', 'light']
const VIEWPORTS = [
  { id: 'desktop', width: 1440, height: 1100 },
  { id: 'phone', width: 390, height: 844 },
]
const FILTERS = [
  { id: 'all', label: 'all' },
  { id: 'not-published', label: 'not published' },
  { id: 'published', label: 'published' },
  { id: 'auto', label: 'auto' },
]
// Bytes only this change introduces: the list marker and the strip's accessible
// name live in the app chunk, the strip and publish-state classes in fairtrade's.
const FEATURE_GROUPS = [
  { label: 'root page', signatures: ['data-root-session-list', 'your sessions in numbers'] },
  { label: 'automatic publishing reminder', signatures: ['dismiss auto-publish tip', 'tick the auto-publish box the next time you publish'] },
  { label: 'fairtrade stats strip and publish state', signatures: ['sst-pair', 'pub-state-text'] },
]
const FIXTURE = YAML.parse(readFileSync(join(HERE, 'testdata/root-page.yaml'), 'utf8'))
const pause = (ms) => new Promise((done) => setTimeout(done, ms))
const fail = (message) => { throw new Error(`Root page visual harness failed: ${message}`) }
if (FIXTURE.trends?.name !== 'eight-active-weeks-show-the-summary-and-weekly-bars'
  || !Array.isArray(FIXTURE.trends.weeklySessions) || FIXTURE.trends.weeklySessions.length !== 8
  || !FIXTURE.trends.weeklySessions.every((count) => Number.isSafeInteger(count) && count >= 0)) fail('the named eight-week trends fixture is missing or invalid')

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
  return FEATURE_GROUPS.map(({ label, signatures }) => {
    const missing = signatures.filter((signature) => !binary.includes(Buffer.from(signature)))
    if (missing.length) fail(`the embedded binary lacks the ${label} bytes ${missing.join(', ')}; rebuild this exact worktree`)
    const match = javascript.find(({ content }) => signatures.every((signature) => content.includes(signature)))
    if (!match) fail(`no exported chunk carries the ${label} bytes ${signatures.join(', ')}; rebuild this exact worktree`)
    return { label, signatures, path: match.path }
  })
}

function boot(port, mock) {
  const home = mkdtempSync(join(tmpdir(), 'peasant-root-page-'))
  const env = { ...process.env, HOME: home, XDG_CONFIG_HOME: join(home, 'config'), XDG_DATA_HOME: join(home, 'data'), XDG_STATE_HOME: join(home, 'state'), XDG_CACHE_HOME: join(home, 'cache') }
  const server = spawn(BIN, ['web', 'start', '--port', String(port), '--foreground', '--no-browser', `--mock-data-store=${mock}`], { cwd: REPO, env, stdio: ['ignore', 'ignore', 'pipe'] })
  let stderr = ''
  server.stderr.on('data', (data) => { stderr += data.toString() })
  return { server, home, stderr: () => stderr }
}

async function waitHealthy(origin, booted) {
  for (let i = 0; i < 60; i++) {
    if ((await fetch(`${origin}/api/v1/health`).catch(() => null))?.status === 200) return
    await pause(250)
  }
  fail(`the real binary did not become healthy on ${origin}: ${booted.stderr().trim()}`)
}

/** The sync list and publication rows for the mock sessions, from the pattern. */
function publishingAnswers(sessions) {
  const rows = sessions.map((session, index) => ({ session, entry: FIXTURE.pattern[index % FIXTURE.pattern.length] }))
  const sync = rows.map(({ session, entry }) => ({
    id: session.id,
    harness: session.harness,
    projectName: session.project ?? 'unknown project',
    projectHash: session.projectHash,
    hostSlug: 'mock-host',
    startTime: session.startTime,
    durationMs: Math.round(session.durationMins * 60_000),
    totalTokens: session.totalTokens,
    turnCount: session.turnCount,
    model: 'mock-model',
    syncStatus: entry.syncStatus,
    previouslyPushed: entry.state === 'published',
  }))
  const publications = new Map(rows.map(({ session, entry }, index) => [session.id, {
    sessionId: session.id,
    state: entry.state,
    autoPublish: entry.autoPublish,
    outsideSelection: entry.outsideSelection,
    ...(entry.state === 'published'
      ? {
          transcriptId: `00000000-0000-4000-8000-${String(index).padStart(12, '0')}`,
          transcriptUrl: `https://village.example/transcripts/${index}`,
          publishedAt: session.startTime,
        }
      : {}),
    audience: entry.audience,
  }]))
  const expected = rows.filter(({ entry }) => !entry.outsideSelection).map(({ entry }) => entry)
  return { sessions, sync, publications, expected }
}

function searchAnswer(sessions, query) {
  const needle = query.trim().toLowerCase()
  const matching = sessions.filter((session) => needle && (session.preview ?? '').toLowerCase().includes(needle))
  return {
    items: matching.map((session) => ({
      kind: 'transcript',
      transcript: {
        session,
        matches: [{ entryIndex: 0, project: session.project ?? '', projectHash: session.projectHash, role: 'user', score: 1, sessionId: session.id, snippet: session.preview }],
      },
      helperGroups: [],
    })),
    page: 1,
    limit: 20,
    totalItems: matching.length,
    ordinarySessionTotal: matching.length,
    helperThreadTotal: 0,
  }
}

function json(body, status = 200) {
  return { status, contentType: 'application/json', body: JSON.stringify(body) }
}

function installRoutes(page, { answers, summary, diagnostics }) {
  page.on('request', (request) => {
    const url = new URL(request.url())
    const respond = (value) => void request.respond(value).catch((e) => diagnostics.push(e.message))
    if (url.origin !== MOCK_ORIGIN && url.origin !== EMPTY_ORIGIN) return void request.continue().catch((e) => diagnostics.push(e.message))
    // Hide the development mock badge so the capture shows the production chrome.
    if (url.pathname === '/api/v1/config/mock') return respond(json({ enabled: false }))
    if (answers && url.pathname === '/api/v1/sync/sessions') return respond(json({ sessions: answers.sync }))
    if (answers && url.pathname === '/api/v1/publications') {
      const audience = url.searchParams.get('include') === 'audience'
      const ids = (url.searchParams.get('sessionIds') ?? '').split(',').filter(Boolean)
      const rows = ids.map((id) => answers.publications.get(id)).filter(Boolean).map(({ audience: count, ...row }) => (
        audience && row.state === 'published'
          ? { ...row, audience: Array.from({ length: count }, (_, n) => ({ collectiveId: `10000000-0000-4000-8000-${String(n).padStart(12, '0')}`, name: `collective ${n + 1}`, status: 'approved' })) }
          : row
      ))
      return respond(json({ publications: rows }))
    }
    if (summary && url.pathname === '/api/v1/projects/summary') return respond(json(summary))
    // The mock store cannot serve the grouped search route, so the capture
    // answers it from the mock sessions whose first prompt holds the query.
    if (answers && url.pathname === '/api/v1/search' && url.searchParams.get('view') === 'grouped') {
      return respond(json(searchAnswer(answers.sessions, url.searchParams.get('q') ?? '')))
    }
    return void request.continue().catch((e) => diagnostics.push(e.message))
  })
}

async function openPage(browser, origin, theme, viewport, routes) {
  const page = await browser.newPage()
  await applyDeterminism(page, { epochMs: Date.now() })
  await page.setViewport({ width: viewport.width, height: viewport.height, deviceScaleFactor: 1 })
  await page.evaluateOnNewDocument((value) => {
    localStorage.setItem('peasant-theme', value)
    document.documentElement.setAttribute('data-theme', value)
  }, theme)
  if (routes.answers) {
    const dayMs = 86_400_000
    const now = new Date()
    const monday = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()) - ((now.getUTCDay() + 6) % 7) * dayMs
    const days = FIXTURE.trends.weeklySessions.map((sessions, index, weeks) => ({ date: new Date(monday - (weeks.length - 1 - index) * 7 * dayMs).toISOString().slice(0, 10), tokens: 0, sessions }))
    await page.evaluateOnNewDocument((host, trends) => {
      const NativeWebSocket = window.WebSocket
      window.WebSocket = class extends NativeWebSocket {
        constructor(url, protocols) {
          super(url, protocols)
          if (new URL(url).host !== host) return
          this.addEventListener('message', (event) => {
            let message
            try { message = JSON.parse(event.data) } catch { return }
            if (message.type === 'trends') Object.defineProperty(event, 'data', { value: JSON.stringify({ ...message, data: trends }) })
          })
        }
      }
    }, new URL(origin).host, { days, totalTokens: 0, totalSessions: days.reduce((sum, day) => sum + day.sessions, 0) })
  }
  await page.setRequestInterception(true)
  installRoutes(page, routes)
  const response = await page.goto(`${origin}/`, { waitUntil: 'domcontentloaded' })
  if (response?.status() !== 200) fail(`${theme}/${viewport.id}: HTTP ${response?.status()} for ${origin}/`)
  return page
}

async function shoot(page, gate, file, label, { clipHeight, contentSelector } = {}) {
  mkdirSync(dirname(file), { recursive: true })
  await pause(300)
  if (clipHeight) {
    const width = await page.evaluate(() => document.documentElement.clientWidth)
    await page.screenshot({ path: file, clip: { x: 0, y: 0, width, height: clipHeight } })
  } else {
    await page.screenshot({ path: file, fullPage: true })
  }
  if (contentSelector) {
    // A short recovery panel paints only a small part of the full frame. Keep
    // the complete shell capture, and apply the unchanged pixel gate to the
    // actual mounted panel rather than the empty space below it.
    const full = await gate.measure(file)
    const viewport = page.viewport()
    if (full.bytes < 16 * 1024 || full.w !== viewport.width || full.h < viewport.height) fail(`${label}: incomplete full-frame capture`)
    const layoutWidth = await page.$eval('header', (element) => {
      const limit = Number.parseFloat(getComputedStyle(element).maxWidth)
      return Math.min(window.innerWidth, Number.isFinite(limit) ? limit : window.innerWidth)
    })
    const header = await page.$('header')
    const headerBox = header ? await header.boundingBox() : null
    if (!headerBox || headerBox.height <= 0 || Math.abs(headerBox.width - layoutWidth) > 1) fail(`${label}: shell header missing or incomplete: box=${JSON.stringify(headerBox)} layoutWidth=${layoutWidth} viewport=${JSON.stringify(viewport)}`)
    const panel = await page.$(contentSelector)
    const panelBox = panel ? await panel.boundingBox() : null
    if (!panelBox || panelBox.width <= 0 || panelBox.height <= 0 || panelBox.x < 0 || panelBox.y < 0 || panelBox.x + panelBox.width > viewport.width + 1 || panelBox.y + panelBox.height > viewport.height + 1) fail(`${label}: mounted recovery panel is not fully visible (${contentSelector})`)
    const contentFile = file.replace(/\.png$/, '.content.png')
    await panel.screenshot({ path: contentFile })
    await gate.assert(`${label}/content`, contentFile, { sel: contentSelector, where: 'root-page-visual.mjs' })
  } else {
    await gate.assert(label, file, { sel: 'full page', where: 'root-page-visual.mjs' })
  }
}

/** Computed-style claims the design system makes about this page. */
async function probeStyles(page, where) {
  const probe = await page.evaluate(() => {
    const h1 = document.querySelector('main h1, h1')
    const value = document.querySelector('.sst-value')
    const option = document.querySelector('.bs-seg-opt')
    const table = document.querySelector('[data-root-session-list] table')
    const title = document.querySelector('[data-root-session-list] [data-session-id] a')
    const root = document.documentElement
    return {
      theme: root.getAttribute('data-theme'),
      bodyFont: getComputedStyle(document.body).fontFamily,
      h1Font: h1 ? getComputedStyle(h1).fontFamily : '',
      h1Size: h1 ? getComputedStyle(h1).fontSize : '',
      h1Text: h1?.textContent?.trim() ?? '',
      stripNums: value ? getComputedStyle(value).fontVariantNumeric : '',
      optionRadius: option ? getComputedStyle(option).borderRadius : '',
      tablePresent: !!table,
      titleSize: title ? getComputedStyle(title).fontSize : '',
      pageOverflow: root.scrollWidth - root.clientWidth,
    }
  })
  if (!/atkinson\s*hyperlegible/i.test(probe.bodyFont) || /mono/i.test(probe.bodyFont.split(',')[0])) fail(`${where}: body font is ${probe.bodyFont}`)
  if (!/atkinson\s*hyperlegible\s*mono/i.test(probe.h1Font) || probe.h1Size !== '28px' || probe.h1Text !== 'your sessions') fail(`${where}: title probe ${JSON.stringify(probe)}`)
  if (!/tabular-nums/.test(probe.stripNums)) fail(`${where}: stats strip values are not tabular (${probe.stripNums})`)
  if (probe.optionRadius !== '0px') fail(`${where}: filter option radius is ${probe.optionRadius}`)
  if (!probe.tablePresent || probe.titleSize !== '16px') fail(`${where}: session table probe ${JSON.stringify(probe)}`)
  if (probe.pageOverflow > 0) fail(`${where}: the page scrolls sideways by ${probe.pageOverflow}px`)
  return probe
}

async function pressFilter(page, label) {
  const pressed = await page.evaluate((wanted) => {
    const options = [...document.querySelectorAll('[data-root-session-list] .bs-seg-opt')]
    const option = options.find((candidate) => candidate.textContent.trim().replace(/\s+[\d,]+$/, '') === wanted)
    option?.click()
    return option ? option.textContent.trim() : null
  }, label)
  if (!pressed) fail(`the ${label} filter option never mounted`)
  await pause(150)
  return Number(pressed.replace(/^.*\s([\d,]+)$/, '$1').replace(/,/g, ''))
}

/** Presses load more until the list is complete and returns the rows listed. */
async function listedRows(page) {
  for (let i = 0; i < 100; i++) {
    const more = await page.evaluate(() => {
      const button = [...document.querySelectorAll('[data-root-session-list] button')].find((candidate) => candidate.textContent.trim() === 'load more')
      button?.click()
      return !!button
    })
    if (!more) break
    await pause(100)
  }
  return page.evaluate(() => document.querySelectorAll('[data-root-session-list] tbody tr').length)
}

async function resetList(page) {
  await page.reload({ waitUntil: 'domcontentloaded' })
  await page.waitForSelector('[data-root-session-list] tbody tr', { visible: true, timeout: 20000 }).catch(() => fail('the session list never mounted after a reload'))
  await waitForStats(page, 'the reloaded list')
}

async function waitForStats(page, where) {
  const weeklyLabel = `sessions per week, last 8 weeks: ${FIXTURE.trends.weeklySessions.join(', ')}`
  await page.waitForSelector(`[role="img"][aria-label="${weeklyLabel}"]`, { visible: true, timeout: 10000 }).catch(() => fail(`${where}: the weekly session bars never mounted with the fixture values`))
  await page.waitForFunction(() => {
    const stats = document.querySelector('[aria-label="general stats"]')?.textContent ?? ''
    return stats.includes('this week') && stats.includes('longest streak') && stats.includes('median session')
  }, { timeout: 10000 }).catch(() => fail(`${where}: the complete stats strip never mounted`))
}

async function runPopulated(browser, gate, theme, viewport, answers) {
  const where = `${theme}/${viewport.id}`
  const diagnostics = []
  const page = await openPage(browser, MOCK_ORIGIN, theme, viewport, { answers, diagnostics })
  try {
    await page.waitForSelector('[data-root-session-list] tbody tr', { visible: true, timeout: 20000 }).catch(() => fail(`${where}: the session list never mounted`))
    await page.waitForSelector('.sst-pair', { visible: true, timeout: 10000 }).catch(() => fail(`${where}: the stats strip never mounted`))
    await waitForStats(page, where)
    const tip = await page.evaluate(() => {
      const aside = document.querySelector('aside[aria-label="auto-publish tip"]')
      const text = aside?.querySelector('p')
      if (!aside || !text) return null
      const box = aside.getBoundingClientRect()
      const range = document.createRange()
      range.selectNodeContents(text)
      const lines = [...range.getClientRects()]
      return { copy: text.textContent, brand: !!aside.querySelector('[data-brand="claude"]'), dismiss: !!aside.querySelector('button[aria-label="dismiss auto-publish tip"]'), font: getComputedStyle(text).fontFamily, size: getComputedStyle(text).fontSize, left: box.left, right: box.right, width: innerWidth, textRight: Math.max(...lines.map((line) => line.right)), textLeft: Math.min(...lines.map((line) => line.left)) }
    })
    if (!tip || !tip.copy.includes('tick the auto-publish box the next time you publish') || !tip.copy.includes('/peasant auto') || !tip.brand || !tip.dismiss) fail(`${where}: the actual automatic publishing tip or its exit is missing`)
    if (tip.left < 0 || tip.right > tip.width + 1 || tip.textRight > tip.right + 1 || tip.textLeft < tip.left - 1) fail(`${where}: the mounted tip overflows its phone or desktop bounds ${JSON.stringify(tip)}`)
    if (!/atkinson/i.test(tip.font) || parseFloat(tip.size) < 16) fail(`${where}: the tip body violates the canonical font or 16px floor ${JSON.stringify(tip)}`)
    const probe = await probeStyles(page, where)
    if (probe.theme !== theme) fail(`${where}: data-theme is ${probe.theme}`)

    const expectedCounts = {
      all: answers.expected.length,
      'not-published': answers.expected.filter((entry) => entry.state === 'unpublished').length,
      published: answers.expected.filter((entry) => entry.state === 'published').length,
      auto: answers.expected.filter((entry) => entry.autoPublish).length,
    }
    for (const filter of FILTERS) {
      const count = await pressFilter(page, filter.label)
      if (count !== expectedCounts[filter.id]) fail(`${where}: the ${filter.label} count is ${count}, the answers hold ${expectedCounts[filter.id]}`)
      await shoot(page, gate, join(OUT, theme, viewport.id, `filter-${filter.id}.png`), `${where}/filter-${filter.id}`)
      const rows = await listedRows(page)
      if (rows !== count) fail(`${where}: the ${filter.label} filter says ${count} and lists ${rows} rows`)
      await resetList(page)
    }

    // A search replaces the list with the grouped search results.
    const input = await page.waitForSelector('input[type="search"]', { visible: true })
    await input.type(FIXTURE.searchQuery)
    await page.waitForSelector('[data-grouped-sessions-section]', { visible: true, timeout: 15000 }).catch(() => fail(`${where}: search results never mounted`))
    await pause(300)
    await shoot(page, gate, join(OUT, theme, viewport.id, 'search.png'), `${where}/search`)
    if (diagnostics.length) fail(`${where}: browser diagnostics ${JSON.stringify(diagnostics.slice(0, 3))}`)
    return { probe, expectedCounts }
  } finally {
    await page.close()
  }
}

async function runSelection(browser, gate, theme, viewport, answers, summaryBase) {
  const where = `${theme}/${viewport.id}`
  const diagnostics = []
  let page = await openPage(browser, MOCK_ORIGIN, theme, viewport, { answers, diagnostics, summary: { ...summaryBase, selection: FIXTURE.selectionNotice } })
  try {
    await page.waitForSelector('[data-root-session-list] tbody tr', { visible: true, timeout: 20000 }).catch(() => fail(`${where}: the list never mounted under a selection notice`))
    await waitForStats(page, `${where}/selection-notice`)
    const notice = await page.evaluate(() => [...document.querySelectorAll('[role="status"]')].map((node) => node.textContent.trim()).find((text) => text.includes('hidden by a saved selection')) ?? '')
    const { hiddenProjects, hiddenSessions } = FIXTURE.selectionNotice
    if (notice !== `${hiddenProjects} projects and ${hiddenSessions} sessions hidden by a saved selection`) fail(`${where}: selection notice reads ${JSON.stringify(notice)}`)
    await shoot(page, gate, join(OUT, theme, viewport.id, 'selection-notice.png'), `${where}/selection-notice`)
  } finally {
    await page.close()
  }

  page = await openPage(browser, MOCK_ORIGIN, theme, viewport, { answers, diagnostics, summary: { projects: [], selection: FIXTURE.selectionRecovery } })
  try {
    await page.waitForSelector('[role="status"][aria-label="project selection recovery"]', { visible: true, timeout: 20000 }).catch(() => fail(`${where}: the recovery panel never mounted`))
    const leaked = await page.evaluate(() => ({ list: !!document.querySelector('[data-root-session-list]'), strip: !!document.querySelector('.sst') }))
    if (leaked.list || leaked.strip) fail(`${where}: the recovery state still shows ${JSON.stringify(leaked)}`)
    await shoot(page, gate, join(OUT, theme, viewport.id, 'selection-recovery.png'), `${where}/selection-recovery`, { contentSelector: '[role="status"][aria-label="project selection recovery"]' })
  } finally {
    await page.close()
  }
  if (diagnostics.length) fail(`${where}: browser diagnostics ${JSON.stringify(diagnostics.slice(0, 3))}`)
}

async function runEmpty(browser, gate, theme, viewport) {
  const where = `${theme}/${viewport.id}`
  const diagnostics = []
  const page = await openPage(browser, EMPTY_ORIGIN, theme, viewport, { diagnostics })
  try {
    await page.waitForFunction(() => document.body.textContent.includes('no ai work recorded yet'), { timeout: 20000 }).catch(() => fail(`${where}: the empty install never showed its teaching state`))
    // The teaching state is short: capture the shell and the state, not the
    // blank page below it.
    await shoot(page, gate, join(OUT, theme, viewport.id, 'empty.png'), `${where}/empty`, { clipHeight: viewport.id === 'phone' ? 760 : 560 })
  } finally {
    await page.close()
  }
  if (diagnostics.length) fail(`${where}: browser diagnostics ${JSON.stringify(diagnostics.slice(0, 3))}`)
}

if (!CHROME) fail('CHROME_PATH is unset; set it to google-chrome or chromium')
const chunks = assertProvenance()
mkdirSync(OUT, { recursive: true })
const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default
const servers = []
let browser
try {
  browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: null })
  const mockServer = boot(PORT, 'web,dashboard,sessions,trends,qualitySessions,search')
  servers.push(mockServer)
  const emptyServer = boot(PORT + 1, 'none')
  servers.push(emptyServer)
  await waitHealthy(MOCK_ORIGIN, mockServer)
  await waitHealthy(EMPTY_ORIGIN, emptyServer)
  for (const chunk of chunks) {
    const servedPath = `/_next/static/chunks/${relative(join(WEB, 'out/_next/static/chunks'), chunk.path).split('\\').join('/')}`
    const served = await fetch(`${MOCK_ORIGIN}${servedPath}`)
    const body = await served.text()
    if (served.status !== 200 || !chunk.signatures.every((signature) => body.includes(signature))) {
      fail(`served provenance: ${servedPath} returned HTTP ${served.status} without the ${chunk.label} bytes; stop stale servers, rebuild this exact worktree, and rerun`)
    }
  }
  console.log(`provenance chunks=${chunks.map((chunk) => relative(REPO, chunk.path)).join(',')} served=true`)

  const mockSessions = (await (await fetch(`${MOCK_ORIGIN}/api/v1/sessions`)).json()).sessions
  const summaryBase = await (await fetch(`${MOCK_ORIGIN}/api/v1/projects/summary`)).json()
  if (!Array.isArray(mockSessions) || mockSessions.length < FIXTURE.pattern.length) fail(`the mock store served ${mockSessions?.length} sessions; the pattern needs at least ${FIXTURE.pattern.length}`)
  const answers = publishingAnswers(mockSessions)
  if (FIXTURE.trends.weeklySessions.reduce((sum, count) => sum + count, 0) !== mockSessions.length) fail('the trends fixture does not total the mock session corpus')

  const gate = new SurfaceGate(await browser.newPage())
  for (const theme of THEMES) {
    for (const viewport of VIEWPORTS) {
      const { expectedCounts } = await runPopulated(browser, gate, theme, viewport, answers)
      await runSelection(browser, gate, theme, viewport, answers, summaryBase)
      await runEmpty(browser, gate, theme, viewport)
      console.log(`OK ${theme}/${viewport.id} counts=${JSON.stringify(expectedCounts)}`)
    }
  }
  console.log(`captures=${OUT}/{dark,light}/{desktop,phone}/{filter-all,filter-not-published,filter-published,filter-auto,search,selection-notice,selection-recovery,empty}.png`)
} finally {
  const cleanup = await Promise.allSettled([browser?.close(), ...servers.map(async (booted) => {
    await new Promise((resolve, reject) => {
      if (booted.server.exitCode !== null || booted.server.signalCode !== null) return resolve()
      const terminate = setTimeout(() => booted.server.kill('SIGKILL'), 2000)
      const deadline = setTimeout(() => reject(new Error('the owned capture server did not stop within five seconds')), 5000)
      booted.server.once('exit', () => { clearTimeout(terminate); clearTimeout(deadline); resolve() })
      booted.server.kill('SIGTERM')
    })
    rmSync(booted.home, { recursive: true, force: true })
  })])
  const failures = cleanup.filter((result) => result.status === 'rejected').map((result) => result.reason)
  if (failures.length) throw new AggregateError(failures, 'root-page capture cleanup failed')
}
