/* Real-binary mounted evidence for the grouped local session list on the Home
 * surface. The browser intercepts only the already-defined private grouped
 * list, member, project-summary and mock-config responses; the route, shell,
 * picker, grouped list, fairtrade helper primitives and member paging code stay
 * real. The group is expanded through its real disclosure, so the member
 * request, the per-group paging footer and the fail-closed scope notice are all
 * captured from the shipped build.
 *
 * Usage: CHROME_PATH=/path/to/chrome node grouped-list-visual.mjs
 */
import { spawn } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, readdirSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '../../..')
const WEB = join(REPO, 'web')
const BIN = join(REPO, 'bin/peasant')
const OUT = process.env.CAPTURES || join(HERE, 'review-capture', 'grouped-local')
const PORT = process.env.PEASANT_GROUPED_PORT || '8723'
const ORIGIN = `http://localhost:${PORT}`
const CHROME = process.env.CHROME_PATH
const THEMES = ['dark', 'light']
// Feature bytes only this slice introduces. The app markers live in the app
// chunk; the fairtrade primitive class lives in the design-system chunk, so
// each signature is located in whichever served chunk carries it.
const APP_FEATURE_BYTES = ['data-grouped-sessions-section', 'data-helper-member-paging']
const FT_FEATURE_BYTES = ['helper-group-trigger']
const FEATURE_BYTES = [...APP_FEATURE_BYTES, ...FT_FEATURE_BYTES]
const PROJECT_HASH = 'a'.repeat(64)
const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
const fail = (message) => { throw new Error(`Grouped local list visual harness failed: ${message}`) }

function session(id, startTime, turnCount = 2) {
  return {
    id,
    harness: 'codex',
    startTime,
    durationMins: 12,
    turnCount,
    totalTokens: 1234,
    toolCallCount: 3,
    inputSubmissionCount: 1,
    project: 'peasant-labs/engine',
    projectHash: PROJECT_HASH,
  }
}

function group(groupId, helperThreadCount, memberScope) {
  return { groupId, purpose: 'helper_review', helperThreadCount, memberScope }
}

const GROUPED_PAYLOAD = {
  items: [
    { kind: 'transcript', transcript: { session: session('agent-a1', '2026-06-03T09:00:00Z') }, helperGroups: [group('hg_p1', 2, 'scope-p1')] },
    { kind: 'transcript', transcript: { session: session('agent-a2', '2026-06-02T09:00:00Z') }, helperGroups: [group('hg_p2', 1, 'scope-p2')] },
    { kind: 'transcript', transcript: { session: session('agent-a3', '2026-06-01T09:00:00Z') }, helperGroups: [] },
  ],
  page: 1,
  limit: 20,
  totalItems: 3,
  ordinarySessionTotal: 3,
  helperThreadTotal: 3,
}

const MEMBERS_P1 = {
  members: [
    { kind: 'transcript', transcript: { session: session('agent-b1', '2026-05-30T09:00:00Z') } },
    { kind: 'transcript', transcript: { session: session('agent-b2', '2026-05-29T09:00:00Z') } },
  ],
  page: 1,
  limit: 20,
  total: 2,
}

const PROJECT_SUMMARY = {
  projects: [{ project: 'peasant-labs/engine', projectHash: PROJECT_HASH, sessions: 3 }],
}

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
  const locate = (signatures, label) => {
    const match = javascript.find(({ content }) => signatures.every((signature) => content.includes(signature)))
    if (!match) fail(`the built export contains no chunk with the ${label} feature bytes ${signatures.join(', ')}; rebuild this exact worktree`)
    return match.path
  }
  return [locate(APP_FEATURE_BYTES, 'grouped-list app'), locate(FT_FEATURE_BYTES, 'fairtrade helper-group')]
}

function response(body, status = 200) {
  return { status, contentType: 'application/json', body: JSON.stringify(body) }
}

function installMocks(page, diagnostics) {
  page.on('request', (request) => {
    const url = new URL(request.url())
    if (url.origin !== ORIGIN) return void request.continue().catch((e) => diagnostics.push(e.message))
    if (url.pathname === '/api/v1/config/mock') return void request.respond(response({ enabled: false })).catch((e) => diagnostics.push(e.message))
    if (url.pathname === '/api/v1/projects/summary') return void request.respond(response(PROJECT_SUMMARY)).catch((e) => diagnostics.push(e.message))
    if (url.pathname === '/api/v1/sessions') {
      if (url.searchParams.get('view') === 'grouped') return void request.respond(response(GROUPED_PAYLOAD)).catch((e) => diagnostics.push(e.message))
      return void request.respond(response({ sessions: [] })).catch((e) => diagnostics.push(e.message))
    }
    if (url.pathname === '/api/v1/session-groups/hg_p1/members') return void request.respond(response(MEMBERS_P1)).catch((e) => diagnostics.push(e.message))
    if (url.pathname === '/api/v1/session-groups/hg_p2/members') {
      return void request.respond(response({ error: 'scope expired', code: 'group_scope_expired' }, 409)).catch((e) => diagnostics.push(e.message))
    }
    return void request.continue().catch((e) => diagnostics.push(e.message))
  })
}

async function capture(page, gate, file, selector, label) {
  mkdirSync(dirname(file), { recursive: true })
  const element = await page.waitForSelector(selector, { visible: true, timeout: 15000 }).catch(() => null)
  if (!element) fail(`${label}: selector ${selector} never mounted`)
  const box = await element.boundingBox()
  if (!box || box.width < 4 || box.height < 4) fail(`${label}: blank or zero-sized ${selector}`)
  await element.screenshot({ path: file, captureBeyondViewport: true })
  await gate.assert(label, file, { sel: selector, where: 'grouped-list-visual.mjs' })
}

async function runTheme(page, theme, gate) {
  const diagnostics = []
  page.removeAllListeners('request')
  await page.setViewport({ width: 1440, height: 1100, deviceScaleFactor: 1 })
  await page.evaluateOnNewDocument((value) => {
    localStorage.setItem('peasant-theme', value)
    document.documentElement.setAttribute('data-theme', value)
    document.documentElement.setAttribute('data-tb-theme', value)
  }, theme)
  await page.setRequestInterception(true)
  installMocks(page, diagnostics)
  const responsePage = await page.goto(`${ORIGIN}/`, { waitUntil: 'domcontentloaded' })
  if (responsePage?.status() !== 200) fail(`${theme}: HTTP ${responsePage?.status()}`)

  await page.waitForSelector('[data-grouped-sessions-section]', { visible: true, timeout: 15000 }).catch(() => fail(`${theme}: the grouped session section never mounted`))
  const base = await page.evaluate(() => {
    const body = document.body.getBoundingClientRect()
    const section = document.querySelector('[data-grouped-sessions-section]')
    const trigger = section?.querySelector('.helper-group-trigger')
    const triggerStyle = trigger ? getComputedStyle(trigger) : null
    return {
      theme: document.documentElement.getAttribute('data-theme'),
      body: { width: body.width, height: body.height },
      font: getComputedStyle(document.body).fontFamily,
      sectionPresent: !!section,
      countText: section?.querySelector('[aria-live="polite"]')?.textContent?.trim() || '',
      triggerLabel: trigger?.querySelector('.helper-group-count')?.textContent?.trim() || '',
      triggerRadius: triggerStyle?.borderRadius,
      tabularNums: triggerStyle?.fontVariantNumeric,
      owners: [...(section?.querySelectorAll('.helper-thread-row') ?? [])].map((row) => row.getAttribute('data-thread-id')),
      ownersBefore: (section?.querySelectorAll('.helper-group-members') ?? []).length,
    }
  })
  if (base.theme !== theme || base.body.width < 100 || base.body.height < 100 || !/Atkinson/i.test(base.font)) fail(`${theme}: chrome/theme/font probe ${JSON.stringify(base)}`)
  if (!base.sectionPresent || base.triggerLabel !== '2 helper threads' || base.ownersBefore !== 0) fail(`${theme}: collapsed grouped list probe ${JSON.stringify(base)}`)
  if (!base.owners.includes('agent-a1') || !base.owners.includes('agent-a3')) fail(`${theme}: ordinary rows missing ${JSON.stringify(base.owners)}`)
  if (!base.countText.includes('3 sessions') || !base.countText.includes('3 helper threads')) fail(`${theme}: route counts not rendered from the same selected set: ${JSON.stringify(base.countText)}`)
  if (base.triggerRadius !== '0px') fail(`${theme}: helper group trigger radius ${base.triggerRadius} is not 0`)

  await capture(page, gate, join(OUT, theme, 'grouped.png'), '[data-grouped-sessions-section]', `${theme}/grouped`)

  // Expand the first owner's helper group through its real disclosure.
  await page.evaluate(() => {
    const section = document.querySelector('[data-grouped-sessions-section]')
    const triggers = [...(section?.querySelectorAll('.helper-group-trigger') ?? [])]
    triggers[0]?.click()
  })
  await page.waitForSelector('[data-helper-member-paging]', { visible: true, timeout: 10000 }).catch(() => fail(`${theme}: member paging footer never mounted`))
  const expanded = await page.evaluate(() => {
    const section = document.querySelector('[data-grouped-sessions-section]')
    const group = section?.querySelector('[data-group-id="hg_p1"]')
    const link = group?.querySelector('a.helper-thread-open')
    const footer = group?.querySelector('[data-helper-member-paging]')
    return {
      members: [...(group?.querySelectorAll('.helper-thread-row') ?? [])].map((row) => row.getAttribute('data-thread-id')),
      href: link?.getAttribute('href') ?? '',
      footer: footer?.textContent?.trim() || '',
      secondGroupMembers: section?.querySelectorAll('[data-group-id="hg_p2"] .helper-group-members').length ?? 0,
    }
  })
  if (!expanded.members.includes('agent-b1') || !expanded.members.includes('agent-b2')) fail(`${theme}: expanded members missing ${JSON.stringify(expanded.members)}`)
  if (expanded.href !== `/projects/${PROJECT_HASH}/agent-b1`) fail(`${theme}: member link ${expanded.href} is not the authorized route`)
  if (!expanded.footer.includes('page 1 of 1')) fail(`${theme}: paging footer ${JSON.stringify(expanded.footer)}`)
  if (expanded.secondGroupMembers !== 0) fail(`${theme}: expanding one group disclosed another group (${expanded.secondGroupMembers})`)
  await capture(page, gate, join(OUT, theme, 'grouped-expanded.png'), '[data-grouped-sessions-section]', `${theme}/grouped-expanded`)

  // The second owner's group has no live scope: it must fail closed.
  await page.evaluate(() => {
    const section = document.querySelector('[data-grouped-sessions-section]')
    const group = section?.querySelector('[data-group-id="hg_p2"] .helper-group-trigger')
    group?.click()
  })
  await page.waitForSelector('[data-group-id="hg_p2"] .helper-group-notice', { visible: true, timeout: 10000 }).catch(() => fail(`${theme}: scope-expired notice never mounted`))
  const expired = await page.evaluate(() => {
    const group = document.querySelector('[data-group-id="hg_p2"]')
    return {
      notice: group?.querySelector('.helper-group-notice')?.textContent?.trim() || '',
      members: group?.querySelectorAll('.helper-group-members').length ?? 0,
      refresh: group?.querySelector('.helper-group-action')?.textContent?.trim() || '',
    }
  })
  if (!expired.notice.includes('saved helper query expired') || expired.members !== 0 || !expired.refresh.includes('refresh list')) {
    fail(`${theme}: fail-closed scope notice ${JSON.stringify(expired)}`)
  }
  await capture(page, gate, join(OUT, theme, 'grouped-scope-expired.png'), '[data-grouped-sessions-section]', `${theme}/grouped-scope-expired`)

  // The retained flat flow: its own disclosure opens the unchanged cross-project
  // table with the filter box and the top-level pager, beside the grouped list.
  await page.evaluate(() => {
    document.querySelector('[data-flat-sessions-disclosure] button')?.click()
  })
  await page.waitForSelector('[data-tour="all-sessions"]', { visible: true, timeout: 10000 }).catch(() => fail(`${theme}: the retained flat session table never mounted`))
  const flat = await page.evaluate(() => {
    const section = document.querySelector('[data-tour="all-sessions"]')
    const input = section?.querySelector('input[type="search"]')
    const count = section?.querySelector('[aria-live="polite"]')?.textContent?.trim() || ''
    const pager = section?.querySelector('.font-mono.text-xs.text-ink-4')?.textContent?.trim() || ''
    const grouped = document.querySelector('[data-grouped-sessions-section]')
    return {
      hasFilter: !!input,
      filterLabel: input?.getAttribute('aria-label') || '',
      count,
      pager,
      // The grouped list must remain mounted beside it.
      groupedStillMounted: !!grouped,
    }
  })
  if (!flat.hasFilter || flat.filterLabel !== 'search sessions' || !/\d+ sessions/.test(flat.count)) {
    fail(`${theme}: retained flat filter probe ${JSON.stringify(flat)}`)
  }
  if (!flat.groupedStillMounted) fail(`${theme}: opening the flat table unmounted the grouped list`)
  await capture(page, gate, join(OUT, theme, 'home-flat.png'), '[data-flat-sessions-disclosure]', `${theme}/home-flat`)

  // The project route mounts the SAME grouped list scoped to one project: the
  // request must carry the project filter, and the response rows must render.
  const sessionsUrls = []
  page.on('request', (request) => {
    const url = new URL(request.url())
    if (url.pathname === '/api/v1/sessions') sessionsUrls.push(url)
  })
  const projectPage = await page.goto(`${ORIGIN}/sessions/${PROJECT_HASH}`, { waitUntil: 'domcontentloaded' })
  if (projectPage?.status() !== 200) fail(`${theme}: project route HTTP ${projectPage?.status()}`)
  await page.waitForSelector('[data-grouped-sessions-section]', { visible: true, timeout: 15000 }).catch(() => fail(`${theme}: the project-scoped grouped section never mounted`))
  if (!sessionsUrls.some((url) => url.searchParams.get('view') === 'grouped' && url.searchParams.get('project') === PROJECT_HASH)) {
    fail(`${theme}: the project route never requested the project-scoped grouped view ${JSON.stringify(sessionsUrls.map((url) => url.search))}`)
  }
  const projectMounted = await page.evaluate(() => {
    const section = document.querySelector('[data-grouped-sessions-section]')
    const owners = [...(section?.querySelectorAll('.helper-thread-row') ?? [])].map((row) => row.getAttribute('data-thread-id'))
    return { owners, heading: document.querySelector('h1')?.textContent?.trim() || '' }
  })
  if (!projectMounted.owners.includes('agent-a1') || !projectMounted.owners.includes('agent-a3')) {
    fail(`${theme}: project-scoped grouped rows missing ${JSON.stringify(projectMounted)}`)
  }
  await capture(page, gate, join(OUT, theme, 'project-grouped.png'), '[data-grouped-sessions-section]', `${theme}/project-grouped`)

  if (diagnostics.length) fail(`${theme}: browser diagnostics ${JSON.stringify(diagnostics.slice(0, 3))}`)
}

if (!CHROME) fail('CHROME_PATH is unset; set it to google-chrome or chromium')
const chunks = assertProvenance()
mkdirSync(OUT, { recursive: true })
const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default
const server = spawn(BIN, ['web', 'start', '--port', PORT, '--foreground', '--no-browser', '--mock-data-store=web,sessions,search'], { cwd: REPO, stdio: ['ignore', 'ignore', 'pipe'] })
let serverError = ''
server.stderr.on('data', (data) => { serverError += data.toString() })
const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: null })
try {
  let healthy = false
  for (let i = 0; i < 40 && !healthy; i++) { healthy = (await fetch(`${ORIGIN}/api/v1/health`).catch(() => null))?.status === 200; if (!healthy) await pause(250) }
  if (!healthy) fail(`real binary did not become healthy on ${ORIGIN}: ${serverError.trim()}`)

  const servedPaths = chunks.map((chunk) => `/_next/static/chunks/${relative(join(WEB, 'out/_next/static/chunks'), chunk).split('\\').join('/')}`)
  for (const [index, servedPath] of servedPaths.entries()) {
    const served = await fetch(`${ORIGIN}${servedPath}`)
    const body = await served.text()
    const expected = index === 0 ? APP_FEATURE_BYTES : FT_FEATURE_BYTES
    if (served.status !== 200 || !expected.every((signature) => body.includes(signature))) {
      fail(`served provenance: ${servedPath} returned HTTP ${served.status} without the grouped-list feature bytes; stop stale servers, rebuild this exact worktree, and rerun`)
    }
  }
  console.log(`provenance chunks=${chunks.map((chunk) => relative(REPO, chunk)).join(',')} served=true bytes=${FEATURE_BYTES.join(',')}`)

  const gate = new SurfaceGate(await browser.newPage())
  for (const theme of THEMES) {
    const page = await browser.newPage()
    try {
      await applyDeterminism(page)
      await runTheme(page, theme, gate)
      console.log(`OK ${theme}`)
    } finally {
      await page.close()
    }
  }
  console.log(`captures=${OUT}/{dark,light}/{grouped,grouped-expanded,grouped-scope-expired}.png`)
} finally {
  await browser.close()
  server.kill('SIGTERM')
}
