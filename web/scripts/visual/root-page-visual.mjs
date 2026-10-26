/* Real-binary mounted regression evidence for the root page: the shell, search
 * box, and the session list with its publish-state filters.
 *
 * It boots peasant twice, each on its own throwaway XDG directories so no
 * real session store is read:
 *   - with the mock store (`web,sessions,qualitySessions,search`), which serves
 *     retained browse, project summaries, transcript details and search;
 *   - with no mock store, for the empty install.
 * The mock store holds no Home pages or publication receipts, so the
 * browser fulfils the home/project-options sync views and publications from
 * the mock sessions and the pattern in testdata/root-page.yaml. The project
 * summary is intercepted only for the selection notice and recovery states.
 * Global statistics are deliberately absent. The complementary bounded-page
 * journey protects first25 and loaded-window refresh request counts.
 * The page, the shell, the fairtrade components and every request path stay
 * real.
 *
 * States, each in both themes at desktop and phone width: the list under each
 * filter (all, not published, published, auto), the collapsed and expanded
 * child family, a search, the selection
 * notice, the selection recovery panel, and the empty install. Every filter's
 * membership is checked against the rows it lists, walking every page of the
 * table.
 *
 * Usage: CHROME_PATH=/path/to/chrome node scripts/visual/root-page-visual.mjs
 *        PLAYWRIGHT_MODULE may name an installed Playwright ESM entry when
 *        the runner is supplied by the workspace rather than web dependencies.
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
import { loadContextNavigationFixture } from './context-navigation-fixture.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '../../..')
const WEB = join(REPO, 'web')
const BIN = join(REPO, 'peasant')
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
  { id: 'auto', label: 'auto-publish on' },
]
// Bytes only this change introduces: the list marker and the strip's accessible
// name live in the app chunk, the strip and publish-state classes in fairtrade's.
const FEATURE_GROUPS = [
  { label: 'root page', signatures: ['data-root-session-list', 'data-home-bounded'] },
  { label: 'recursive session family', signatures: ['data-session-group-child', 'session-group-rows:', 'var(--sp-5) + var(--sp-4)'] },
  { label: 'parent-attached session disclosure', signatures: ['data-session-family'] },
  { label: 'automatic publishing reminder', signatures: ['dismiss auto-publish tip', 'your sessions can publish on their own'] },
  { label: 'fairtrade publish state', signatures: ['pub-state-text'] },
]
const FIXTURE = YAML.parse(readFileSync(join(HERE, 'testdata/root-page.yaml'), 'utf8'))
const CONTEXT = loadContextNavigationFixture()
const pause = (ms) => new Promise((done) => setTimeout(done, ms))
const fail = (message) => { throw new Error(`Root page visual harness failed: ${message}`) }
if ([PORT, PORT + 1].includes(8690)) fail('the regression runner must use sandbox ports, never the user server')

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
  if (![CONTEXT.parentId, CONTEXT.childSessionId, CONTEXT.unresolvedChildId].every((id) => sessions.some((session) => session.id === id))
    || sessions.some((session) => session.id === CONTEXT.absentTargetId)) fail('root-page-visual.mjs publishingAnswers: context_navigation.yaml stored family is missing or its absent target is served; home omission evidence would be vacuous; restore the canonical corpus and rerun visual:root-page')
  const rows = sessions.map((session, index) => ({ session, entry: {
    ...FIXTURE.pattern[index % FIXTURE.pattern.length],
    ...([CONTEXT.childSessionId, CONTEXT.unresolvedChildId].includes(session.id) ? { outsideSelection: false } : {}),
  } }))
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
    ...(session.id === CONTEXT.childSessionId ? { parentSessionId: CONTEXT.parentId } : {}),
    ...(session.id === CONTEXT.unresolvedChildId ? { parentSessionId: CONTEXT.absentTargetId } : {}),
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
  for (const id of [CONTEXT.childSessionId, CONTEXT.unresolvedChildId]) {
    const row = sync.find((row) => row.id === id)
    const parent = id === CONTEXT.childSessionId ? CONTEXT.parentId : CONTEXT.absentTargetId
    if (row?.parentSessionId !== parent || row.parentUnresolved || publications.get(id)?.outsideSelection !== false) fail(`root-page-visual.mjs publishingAnswers: context_navigation.yaml child ${id} lost its nameable parent or selected publication; omission cannot prove childhood; restore parentSessionId=${parent} and the publication row, then rerun visual:root-page`)
  }
  const expected = sync.filter((row) => !row.parentSessionId && !row.parentUnresolved)
    .map((row) => publications.get(row.id)).filter((entry) => entry && !entry.outsideSelection)
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

async function installRoutes(page, { answers, summary, diagnostics }) {
  await page.route('**/*', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const respond = (value) => route.fulfill(value).catch((e) => diagnostics.push(e.message))
    if (url.origin !== MOCK_ORIGIN && url.origin !== EMPTY_ORIGIN) return route.continue()
    // Hide the development mock badge so the capture shows the production chrome.
    if (url.pathname === '/api/v1/config/mock') return respond(json({ enabled: false }))
    if (url.pathname === '/api/v1/sync/sessions' && url.searchParams.get('view') === 'project-options') {
      const options = new Map((answers?.sync ?? []).filter(row => !row.parentSessionId && !row.parentUnresolved && !answers.publications.get(row.id)?.outsideSelection).map(row => [row.projectHash, { projectHash: row.projectHash, projectName: row.projectName }]))
      return respond(json({ projects: [...options.values()].sort((a, b) => a.projectName.localeCompare(b.projectName) || a.projectHash.localeCompare(b.projectHash)) }))
    }
    if (url.pathname === '/api/v1/sync/sessions' && url.searchParams.get('view') === 'home') {
      const parent = url.searchParams.get('parentSessionId')
      const publication = url.searchParams.get('publication') ?? 'all'
      const start = url.searchParams.get('pageToken') === 'more-sessions' ? 25 : 0
      const source = !answers || (summary?.projects?.length === 0 && summary.selection?.active) ? [] : answers.sync.filter(row => {
        const receipt = answers.publications.get(row.id)
        return receipt && !receipt.outsideSelection && !row.parentUnresolved
          && (parent ? row.parentSessionId === parent : !row.parentSessionId)
          && (parent || publication === 'all' || publication === 'auto' && receipt.autoPublish
            || publication === 'published' && receipt.state === 'published' || publication === 'not-published' && receipt.state === 'unpublished')
      })
      return respond(json({ items: source.slice(start, start + 25).map(sync => {
        const { audience, ...publication } = answers.publications.get(sync.id)
        const session = answers.sessions.find(session => session.id === sync.id)
        return { sync, publication, title: session?.title ?? '', preview: sync.firstPrompt ?? session?.preview ?? '', childCount: answers.sync.filter(child => child.parentSessionId === sync.id && !answers.publications.get(child.id)?.outsideSelection).length }
      }), next: start + 25 < source.length ? 'more-sessions' : '' }))
    }
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
    return route.continue().catch((e) => diagnostics.push(e.message))
  })
}

async function openPage(browser, origin, theme, viewport, routes) {
  const page = await browser.newPage()
  // Reuse the existing deterministic fixture; only its two browser calls
  // need translation to Playwright's media/init-script API.
  await applyDeterminism({
    emulateMediaFeatures: () => page.emulateMedia({ reducedMotion: 'reduce' }),
    evaluateOnNewDocument: (fn, ...args) => page.addInitScript(`(${fn.toString()})(...${JSON.stringify(args)})`),
  }, { epochMs: Date.now() })
  await page.setViewportSize({ width: viewport.width, height: viewport.height })
  await page.addInitScript((value) => {
    localStorage.setItem('peasant-theme', value)
    document.documentElement.setAttribute('data-theme', value)
  }, theme)
  await installRoutes(page, routes)
  const response = await page.goto(`${origin}/`, { waitUntil: 'domcontentloaded' })
  if (response?.status() !== 200) fail(`${theme}/${viewport.id}: HTTP ${response?.status()} for ${origin}/`)
  return page
}

async function shoot(page, gate, file, label, { clipHeight, contentSelector, equivalentSurfaces = [] } = {}) {
  mkdirSync(dirname(file), { recursive: true })
  // A disclosure click scrolls its control into view. Full-page captures must
  // start at the top so the real fixed product header paints above the body.
  await page.evaluate(() => window.scrollTo(0, 0))
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
    const viewport = page.viewportSize()
    if (full.bytes < 16 * 1024 || full.w !== viewport.width || full.h < viewport.height) fail(`${label}: incomplete full-frame capture`)
    // The shell's chrome: the sidebar column on a wide screen, its top bar across a phone.
    const chrome = await page.evaluate(() => {
      const visible = (element) => !!element && getComputedStyle(element).display !== 'none' && element.getBoundingClientRect().height > 0
      const bar = document.querySelector('.asb-bar')
      const panel = document.querySelector('.asb-panel')
      const shown = visible(bar) ? bar : visible(panel) ? panel : null
      const box = shown?.getBoundingClientRect()
      return box ? { which: shown === bar ? 'bar' : 'panel', x: box.x, width: box.width, height: box.height, innerWidth: window.innerWidth } : null
    })
    if (!chrome || chrome.height <= 0 || chrome.x < 0 || (chrome.which === 'bar' && Math.abs(chrome.width - chrome.innerWidth) > 1)) fail(`${label}: shell sidebar missing or incomplete: ${JSON.stringify(chrome)} viewport=${JSON.stringify(viewport)}`)
    const panel = await page.$(contentSelector)
    const panelBox = panel ? await panel.boundingBox() : null
     if (!panelBox || panelBox.width <= 0 || panelBox.height <= 0 || panelBox.x < 0 || panelBox.y < 0 || panelBox.x + panelBox.width > full.w + 1 || panelBox.y + panelBox.height > full.h + 1) fail(`${label}: mounted recovery panel is not contained in the full shell/body capture (${contentSelector})`)
    const contentFile = file.replace(/\.png$/, '.content.png')
    await panel.screenshot({ path: contentFile })
    await gate.assert(`${label}/content`, contentFile, { sel: contentSelector, where: 'root-page-visual.mjs' })
  } else {
    await gate.assert(label, file, { sel: 'full page', where: 'root-page-visual.mjs', equivalentSurfaces })
  }
}

/** Computed-style claims the design system makes about this page. */
async function probeStyles(page, where) {
  const probe = await page.evaluate(() => {
    const h1 = document.querySelector('main h1, h1')
    const option = document.querySelector('.choice-menu-trigger')
    const table = document.querySelector('[data-root-session-list] table')
    const title = document.querySelector('[data-root-session-list] [data-session-id] a')
    const root = document.documentElement
    return {
      theme: root.getAttribute('data-theme'),
      bodyFont: getComputedStyle(document.body).fontFamily,
      h1Font: h1 ? getComputedStyle(h1).fontFamily : '',
      h1Size: h1 ? getComputedStyle(h1).fontSize : '',
      h1Text: h1?.textContent?.trim() ?? '',
      optionRadius: option ? getComputedStyle(option).borderRadius : '',
      tablePresent: !!table,
      titleSize: title ? getComputedStyle(title).fontSize : '',
      pageOverflow: root.scrollWidth - root.clientWidth,
      pathname: location.pathname,
      shell: !!document.querySelector('.asb a.asb-brand[href="/"]')
        && !!document.querySelector('.asb a[href="/settings"], .asb a[href="/settings/"]')
        && !!document.querySelector('.asb button[title^="search"]')
        && !!document.querySelector('.asb button[title^="switch to "]')
        && document.querySelector('.asb-panel [aria-current="page"]')?.getAttribute('href') === '/',
    }
  })
  if (!/atkinson\s*hyperlegible/i.test(probe.bodyFont) || /mono/i.test(probe.bodyFont.split(',')[0])) fail(`${where}: body font is ${probe.bodyFont}`)
  // The title is fairtrade's hero size on a wide screen and its xl size on a phone.
  const titleSize = (await page.evaluate(() => window.innerWidth)) < 768 ? '28px' : '40px'
  if (!/atkinson\s*hyperlegible\s*mono/i.test(probe.h1Font) || probe.h1Size !== titleSize || probe.h1Text !== 'sessions') fail(`${where}: title probe ${JSON.stringify(probe)}`)
  if (probe.optionRadius !== '0px') fail(`${where}: filters trigger radius is ${probe.optionRadius}`)
  if (!probe.tablePresent || probe.titleSize !== '16px') fail(`${where}: session table probe ${JSON.stringify(probe)}`)
  if (probe.pageOverflow > 0) fail(`${where}: the page scrolls sideways by ${probe.pageOverflow}px`)
  if (!probe.shell || probe.pathname !== '/') fail(`${where}: the home route or persistent product shell is missing ${JSON.stringify(probe)}`)
  return probe
}

/** Picks a publish-state filter from the filters menu, the way a person does. */
async function pressFilter(page, label) {
  const trigger = page.getByRole('button', { name: /^filters/ })
  if (await trigger.getAttribute('aria-expanded') !== 'true') await trigger.click()
  const option = page.getByRole('menuitemradio', { name: label, exact: true })
  if (!await option.count()) fail(`the ${label} filter option never mounted`)
  await option.click()
  await page.keyboard.press('Escape')
  await pause(150)
  return label
}

/** Switches Home to one of its views from the browse-by menu. */
async function browseBy(page, view) {
  await page.getByRole('button', { name: /^browse by:/ }).click()
  await page.getByRole('menuitemradio', { name: view, exact: true }).click()
}

/** Walks the pager from the first page to the last and returns how many rows it listed. */
async function listedRows(page) {
  const list = page.locator('[data-root-session-list]')
  const idle = () => page.waitForFunction(() => !document.querySelector('[data-root-session-list]')?.hasAttribute('aria-busy'))
  const first = list.getByRole('button', { name: 'page 1', exact: true })
  if (await first.count() && await first.getAttribute('aria-current') !== 'page') {
    await first.click()
    await pause(150)
  }
  let listed = 0
  for (let i = 0; i < 100; i++) {
    await idle()
    await pause(100)
    listed += await list.locator('tbody [data-session-id]').count()
    const next = list.getByRole('button', { name: 'next page', exact: true })
    if (!await next.count() || await next.isDisabled()) break
    await next.click()
  }
  return listed
}

async function resetList(page) {
  await page.reload({ waitUntil: 'domcontentloaded' })
  await page.waitForSelector('[data-root-session-list] tbody tr', { state: 'visible', timeout: 20000 }).catch(() => fail('the session list never mounted after a reload'))
}

async function assertMissingParentOmitted(page, where) {
  const leaked = await page.evaluate((id) => {
    const list = document.querySelector('[data-root-session-list]')
    return {
      pathname: location.pathname,
      list: !!list,
      rows: list?.querySelectorAll(`[data-session-id="${id}"]`).length,
      children: list?.querySelectorAll(`[data-session-group-child="${id}"]`).length,
      links: [...(list?.querySelectorAll('a[href]') ?? [])].filter((link) => new URL(link.href).pathname.replace(/\/$/, '').endsWith(`/${id}`)).length,
    }
  }, CONTEXT.unresolvedChildId)
  if (leaked.pathname !== '/' || !leaked.list || leaked.rows !== 0 || leaked.children !== 0 || leaked.links !== 0) fail(`root-page-visual.mjs ${where}: context_navigation.yaml missing-parent child leaked into home or the home list is absent ${JSON.stringify(leaked)}; mounted omission evidence is invalid; preserve its named parent marker and fix the rendered home boundary, then rerun visual:root-page`)
}

/** Layout cannot be observed by the mounted jsdom fixture; use the real family. */
async function probeGroupAttachment(page, where) {
  const probe = await page.evaluate((id) => {
    const parent = document.querySelector(`[data-session-id="${id}"]`)
    const group = document.querySelector(`[data-session-group="${id}"]`)
    const family = parent?.closest('[data-session-family]')
    const disclosure = group?.querySelector('.sgd')
    const control = disclosure?.querySelector('.sgd-trigger')
    const icon = control?.querySelector('svg')
    const cell = parent?.closest('td')
    if (!parent || !group || !family || !disclosure || !control || !icon || !cell) return null
    const px = (value) => Number.parseFloat(value)
    const style = getComputedStyle(disclosure)
    const buttonStyle = getComputedStyle(control)
    const familyStyle = getComputedStyle(family)
    const parentBox = parent.getBoundingClientRect()
    const buttonBox = control.getBoundingClientRect()
    const iconBox = icon.getBoundingClientRect()
    // These are the published disclosure's resolved tokens, not new spacing targets.
    const indent = px(style.getPropertyValue('--sp-5'))
    const padY = px(style.getPropertyValue('--sp-3'))
    const padX = px(style.getPropertyValue('--sp-4'))
    const target = px(style.getPropertyValue('--target-comfortable'))
    const boundary = [parent, family, group, disclosure, control]
    const rules = boundary.filter((node) => {
      const computed = getComputedStyle(node)
      const border = node === parent ? computed.borderBottomWidth : node === family ? '0px' : computed.borderTopWidth
      if (px(border) > 0 || computed.boxShadow !== 'none') return true
      return ['::before', '::after'].some((pseudo) => {
        const generated = getComputedStyle(node, pseudo)
        return generated.content !== 'none' && generated.content !== 'normal' && generated.display !== 'none'
      })
    }).map((node) => node.className)
    return {
      sameCell: group.closest('td') === cell && cell.parentElement.firstElementChild === cell,
      order: parent.nextElementSibling === group && group.parentElement === family && !parent.contains(group),
      rootLeak: !!group.querySelector('[data-session-id]'),
      // Every list row is either one root row (exactly one session id) or one
      // child row (no session id, but a child marker); anything else is stray.
      extraRows: [...document.querySelectorAll('[data-root-session-list] tbody tr')].some((row) => {
        const sessions = row.querySelectorAll('[data-session-id]').length
        return sessions > 1 || (sessions === 0 && !row.querySelector('[data-session-group-child]'))
      }),
      bare: disclosure.classList.contains('sgd-bare'), indented: disclosure.classList.contains('sgd-indent'),
      bareBorder: px(style.borderTopWidth) + px(style.borderRightWidth) + px(style.borderBottomWidth) + px(style.borderLeftWidth),
      indent, padY, padX, target, rules,
      paddingLeft: px(style.paddingLeft), paddingTop: px(buttonStyle.paddingTop), paddingBottom: px(buttonStyle.paddingBottom),
      paddingX: px(buttonStyle.paddingLeft), minHeight: px(buttonStyle.minHeight), height: buttonBox.height,
      controlIndent: buttonBox.left - parentBox.left, iconIndent: iconBox.left - parentBox.left,
      gap: buttonBox.top - parentBox.bottom, contentGap: iconBox.top - parentBox.bottom,
      canonicalContentGap: padY + Math.max(0, (buttonBox.height - 2 * padY - iconBox.height) / 2),
      // A canonical tight stack has no layout gap; the control owns all spacing.
      layoutGap: familyStyle.rowGap === 'normal' ? 0 : px(familyStyle.rowGap),
      boundarySpace: boundary.reduce((sum, node) => {
        const computed = getComputedStyle(node)
        return sum + px(computed.marginTop) + px(computed.marginBottom)
          + (node === group || node === disclosure || node === family ? px(computed.paddingTop) + px(computed.paddingBottom) : 0)
      }, 0),
      rowSeparator: px(getComputedStyle(cell).borderBottomWidth),
      epsilon: 1 / devicePixelRatio,
    }
  }, CONTEXT.parentId)
  const near = (actual, expected) => Number.isFinite(actual) && Number.isFinite(expected) && Math.abs(actual - expected) <= probe.epsilon
  if (!probe || !probe.sameCell || !probe.order || probe.rootLeak || probe.extraRows || !probe.bare || !probe.indented
    || probe.bareBorder !== 0 || !(probe.indent > 0 && probe.padY > 0 && probe.padX > 0 && probe.target > 0)
    || !near(probe.paddingLeft, probe.indent) || !near(probe.controlIndent, probe.indent)
    || !near(probe.iconIndent, probe.indent + probe.padX)
    || !near(probe.paddingTop, probe.padY) || !near(probe.paddingBottom, probe.padY) || !near(probe.paddingX, probe.padX)
    || !near(probe.minHeight, probe.target) || probe.height < probe.target
    || probe.layoutGap !== 0 || probe.boundarySpace !== 0 || !near(probe.gap, probe.layoutGap)
    || !near(probe.contentGap, probe.layoutGap + probe.canonicalContentGap) || probe.rules.length || !(probe.rowSeparator > 0)) {
    fail(`root-page-visual.mjs ${where}: parent-attached disclosure geometry/boundary ${JSON.stringify(probe)}; the group is detached, unindented, separated by a rule, or its canonical hit target changed; reuse the same primary cell and published bare/indent spacing, rebuild this worktree, then rerun visual:root-page`)
  }
  console.log(`group attachment ${where} ${JSON.stringify(probe)}`)
}

/** Compare each expanded child row's glyph to its owning disclosure, never an ancestor. */
async function probeChildAlignment(page, id, where) {
  const probe = await page.evaluate((id) => {
    const control = document.querySelector(`[data-session-group="${id}"] > .sgd > button`)
    const chevron = control?.querySelector(':scope > svg')
    const parentRow = document.querySelector(`[data-session-id="${id}"], [data-session-group-child="${id}"]`)?.closest('tr')
    if (!control || !chevron || !parentRow || control.getAttribute('aria-expanded') !== 'true') return null
    const chevronBox = chevron.getBoundingClientRect()
    const table = parentRow.closest('table')
    const targets = (control.getAttribute('aria-controls') ?? '').split(/\s+/).filter(Boolean)
    const ids = targets.flatMap(target => {
      const node = document.getElementById(target)
      return node?.hasAttribute('data-session-group-child') ? [target] : (node?.getAttribute('aria-owns') ?? '').split(/\s+/).filter(Boolean)
    })
    // Each declared IDREF is a rendered child row, consecutive after the parent.
    let previous = parentRow
    const children = ids.map((childId) => {
      const node = document.getElementById(childId)
      const row = node?.closest('tr')
      const glyph = node?.querySelector('svg[data-brand]')
      const box = glyph?.getBoundingClientRect()
      const entry = {
        id: childId,
        child: !!node?.hasAttribute('data-session-group-child'),
        topLevel: !!node?.hasAttribute('data-session-id'),
        sameTable: !!row && row.closest('table') === table,
        adjacency: !!row && row.previousElementSibling === previous,
        left: box?.left,
        width: box?.width,
        height: box?.height,
        delta: box ? box.left - chevronBox.left : null,
      }
      previous = row
      return entry
    })
    // Allow at most one physical pixel of browser layout rounding.
    return { chevron: chevronBox.left, width: chevronBox.width, height: chevronBox.height, children, epsilon: 1 / devicePixelRatio }
  }, id)
  if (!probe || !Number.isFinite(probe.chevron) || !(probe.width > 0 && probe.height > 0)
    || !probe.children.length || !probe.children.every((child) => child.child && !child.topLevel && child.sameTable && child.adjacency
      && Number.isFinite(child.left) && child.width > 0 && child.height > 0 && Number.isFinite(child.delta) && Math.abs(child.delta) <= probe.epsilon)) {
    fail(`root-page-visual.mjs ${where}/${id}: immediate child rows/BrandMark to owning chevron alignment ${JSON.stringify(probe)}; the child rows are missing, detached from the table, out of order, or misaligned; reuse the child list's token padding, rebuild this worktree, then rerun visual:root-page`)
  }
  console.log(`child alignment ${where}/${id} ${JSON.stringify(probe)}`)
}

async function runNestedAlignment(browser, gate, theme, viewport, answers) {
  const family = YAML.parse(readFileSync(join(WEB, 'src/app/testdata/root-publish-filters.yaml'), 'utf8')).cases.find((entry) => entry.name === 'recursive-family-and-refresh')
  if (!family?.sessions?.length) fail('recursive-family-and-refresh fixture is missing; restore the named nested family before visual:root-page')
  const nested = { ...answers,
    sync: family.sessions.map((row) => ({
      ...answers.sync[0],
      id: row.id,
      parentSessionId: row.parentSessionId,
      syncStatus: row.syncStatus,
      firstPrompt: row.preview ?? row.id,
      turnCount: row.turnCount ?? answers.sync[0].turnCount,
      durationMs: row.durationMs ?? answers.sync[0].durationMs,
      startTime: row.startTime ?? answers.sync[0].startTime,
    })),
    publications: new Map(family.sessions.map((row) => [row.id, { ...answers.publications.values().next().value, sessionId: row.id, state: row.state, outsideSelection: false }])),
  }
  const page = await openPage(browser, MOCK_ORIGIN, theme, viewport, { answers: nested, diagnostics: [] })
  try {
    for (const step of family.steps.filter((step) => step.expand)) {
      const control = page.locator(`[data-session-group="${step.expand}"] > .sgd > button`)
      if (await control.getAttribute('aria-expanded') !== 'false') fail(`${theme}/${viewport.id}/${step.expand}: nested disclosure did not start independently collapsed`)
      await control.click()
      await page.locator(`[data-session-group-child="${step.children.at(-1)}"]`).waitFor({ state: 'visible' })
      await probeChildAlignment(page, step.expand, `${theme}/${viewport.id}/nested`)
    }
    await shoot(page, gate, join(OUT, theme, viewport.id, 'nested-group.png'), `${theme}/${viewport.id}/nested-group`)
  } finally { await page.close() }
}

async function runPopulated(browser, gate, theme, viewport, answers) {
  const where = `${theme}/${viewport.id}`
  const diagnostics = []
  const page = await openPage(browser, MOCK_ORIGIN, theme, viewport, { answers, diagnostics })
  try {
    await page.waitForSelector('[data-root-session-list] tbody tr', { state: 'visible', timeout: 20000 }).catch(() => fail(`${where}: the session list never mounted`))
    if (process.env.PEASANT_HOME_VISUAL_MUTATION === 'font') await page.evaluate(() => { document.body.style.fontFamily = 'Arial' })
    if (process.env.PEASANT_HOME_VISUAL_MUTATION === 'navigation') await page.locator('.asb a.asb-brand').evaluate(node => node.remove())
    const tip = await page.evaluate(() => {
      const aside = document.querySelector('aside[aria-label="auto-publish tip"]')
      const text = aside?.querySelector('p')
      if (!aside || !text) return null
      const box = aside.getBoundingClientRect()
      const range = document.createRange()
      range.selectNodeContents(text)
      const lines = [...range.getClientRects()]
      return { copy: text.textContent, docs: aside.querySelector('a[data-docs-topic="autoPublish"][target="_blank"]')?.getAttribute('href') ?? null, dismiss: !!aside.querySelector('button[aria-label="dismiss auto-publish tip"]'), font: getComputedStyle(text).fontFamily, size: getComputedStyle(text).fontSize, left: box.left, right: box.right, width: innerWidth, textRight: Math.max(...lines.map((line) => line.right)), textLeft: Math.min(...lines.map((line) => line.left)) }
    })
    if (!tip || !tip.copy.includes('your sessions can publish on their own') || !tip.docs || tip.copy.includes('/peasant auto') || !tip.dismiss) fail(`${where}: the actual automatic publishing tip, its docs link or its exit is missing`)
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
      await pressFilter(page, filter.label)
      await shoot(page, gate, join(OUT, theme, viewport.id, `filter-${filter.id}.png`), `${where}/filter-${filter.id}`)
      const rows = await listedRows(page)
      if (rows !== expectedCounts[filter.id]) fail(`${where}: the ${filter.label} filter lists ${rows} rows, expected fixture membership ${expectedCounts[filter.id]}`)
      await assertMissingParentOmitted(page, `${where}/filter-${filter.id}/complete`)
      await resetList(page)
      await assertMissingParentOmitted(page, `${where}/filter-${filter.id}/reload`)
    }

    const group = page.locator(`[data-session-group="${CONTEXT.parentId}"]`)
    const control = group.getByRole('button', { name: /subagents/ })
    if (await control.getAttribute('aria-expanded') !== 'false'
      || await page.locator('[data-session-group-child]').count() !== 0) fail(`${where}: the family did not start collapsed`)
    await assertMissingParentOmitted(page, `${where}/collapsed-group`)
    await probeGroupAttachment(page, `${where}/${FIXTURE.family.states[0]}`)
    await shoot(page, gate, join(OUT, theme, viewport.id, FIXTURE.family.states[0] + '.png'), `${where}/${FIXTURE.family.states[0]}`, {
      equivalentSurfaces: [`${where}/${FIXTURE.family.equivalentCollapsedSurface}`],
    })
    await control.click()
    const child = page.locator(`[data-session-group-child="${CONTEXT.childSessionId}"]`)
    await child.waitFor({ state: 'visible' })
    const familyProbe = await child.evaluate((node) => ({
      topLevel: node.hasAttribute('data-session-id'),
      href: node.querySelector('a')?.getAttribute('href'),
      title: node.querySelector('a')?.textContent?.trim(),
      fontSize: getComputedStyle(node.querySelector('a')).fontSize,
      // The state has its own column; only the phone summary repeats it in the title cell.
      publishChrome: [...node.querySelectorAll('.pub-state')].some((state) => !state.closest('.home-session-phone')),
    }))
    const childSync = answers.sync.find((row) => row.id === CONTEXT.childSessionId)
    if (familyProbe.topLevel || familyProbe.publishChrome || !familyProbe.title || familyProbe.fontSize !== '16px'
      || familyProbe.href?.replace(/\/$/, '') !== `/projects/${childSync.projectHash}/${childSync.id}`) fail(`${where}: expanded family probe ${JSON.stringify(familyProbe)}`)
    const childCells = await child.evaluate((node) => {
      const cells = [...node.closest('tr').querySelectorAll('td')]
      return {
        count: cells.length,
        project: cells[1]?.textContent ?? '',
        turns: cells[2]?.textContent?.trim() ?? '',
        duration: cells[3]?.textContent?.trim() ?? '',
        when: cells[4]?.textContent?.trim() ?? '',
        state: cells[5]?.querySelector('.pub-state-text')?.textContent?.trim() ?? '',
      }
    })
    // The child's own row must show the child's own served values, not its
    // parent's: turns, duration and when are recomputed from `childSync` the
    // way the row's own formatters render them.
    const childTurns = childSync.turnCount.toLocaleString('en-US')
    const childMinutes = Math.max(0, Math.round(childSync.durationMs / 60_000))
    const childDuration = childMinutes < 60 ? `${childMinutes}m` : `${Math.floor(childMinutes / 60)}h ${String(childMinutes % 60).padStart(2, '0')}m`
    // The page renders `when` from its own clock, which applyDeterminism freezes
    // per document at open time. Read that same instant here; node's live clock
    // is tens of seconds later, so a capture spanning a day boundary would fail
    // spuriously with a message that mimics the regression this checks for.
    const pageNow = await page.evaluate(() => Date.now())
    const childElapsed = Math.floor(Math.max(0, pageNow - Date.parse(childSync.startTime)) / 60_000)
    const childWhen = childElapsed < 1 ? 'just now' : childElapsed < 60 ? `${childElapsed}m ago` : childElapsed < 1440 ? `${Math.floor(childElapsed / 60)}h ago` : `${Math.floor(childElapsed / 1440)}d ago`
    if (childCells.count !== 6 || !childCells.project.includes(childSync.projectName)
      || childCells.turns !== childTurns || childCells.duration !== childDuration
      || childCells.when !== childWhen || !childCells.state) {
      fail(`${where}: the expanded child row does not carry its own column values; expected turns ${childTurns}, duration ${childDuration}, when ${childWhen}, a project containing ${childSync.projectName} and a publish state, got ${JSON.stringify(childCells)}`)
    }
    if (await listedRows(page) !== expectedCounts.all) fail(`${where}: expanding a family changed the top-level count`)
    await assertMissingParentOmitted(page, `${where}/expanded-group`)
    await probeGroupAttachment(page, `${where}/${FIXTURE.family.states[1]}`)
    await probeChildAlignment(page, CONTEXT.parentId, `${where}/${FIXTURE.family.states[1]}`)
    await shoot(page, gate, join(OUT, theme, viewport.id, FIXTURE.family.states[1] + '.png'), `${where}/${FIXTURE.family.states[1]}`)
    // Follow the actual child's own production exit, not a dormant renderer.
    await child.getByRole('link').click()
    await page.waitForURL(`**/projects/${childSync.projectHash}/${childSync.id}/`)
    await page.locator('.txn-app').waitFor({ state: 'visible', timeout: 20000 })
    // Hidden on home is not an access-control boundary: use the real detail route.
    const hiddenPath = `/projects/${CONTEXT.projectHash}/${CONTEXT.unresolvedChildId}/`
    const response = await page.goto(`${MOCK_ORIGIN}${hiddenPath}`, { waitUntil: 'domcontentloaded' })
    if (response?.status() !== 200) fail(`root-page-visual.mjs ${where}/missing-parent-detail: ${hiddenPath} returned HTTP ${response?.status()}; hidden child readability is unproved; restore the canonical detail route and rerun visual:root-page`)
    await page.locator('.txn-app').waitFor({ state: 'visible', timeout: 20000 })
    await page.waitForFunction((text) => {
      const app = document.querySelector('.txn-app')
      const normalize = (value) => value.replace(/\s+/g, ' ').trim()
      return app?.querySelectorAll('.txn-turn').length > 0 && normalize(app.textContent ?? '').includes(normalize(text))
    }, CONTEXT.unresolvedOpening, { timeout: 20000 }).catch(() => fail(`root-page-visual.mjs ${where}/missing-parent-detail: context_navigation.yaml opening content never rendered in .txn-app; direct-link proof is invalid; repair the real detail read and rerun visual:root-page`))
    await page.locator('.txn-context-source').waitFor({ state: 'visible', timeout: 20000 })
    const references = await page.evaluate(() => [...document.querySelectorAll('.txn-context-source')].map((row) => ({
      label: row.querySelector('.txn-context-row span:not(.txn-context-status)')?.textContent ?? null,
      link: row.querySelector('a')?.getAttribute('href') ?? null,
      status: row.querySelector('.txn-context-status')?.textContent ?? null,
    })))
    if (references.length !== 1 || references[0].label !== CONTEXT.starterLabel || references[0].link !== null || references[0].status !== 'source unavailable') fail(`root-page-visual.mjs ${where}/missing-parent-detail: context_navigation.yaml unavailable started-by reference is ${JSON.stringify(references)}; honest parent navigation is unproved; restore the unavailable reference without a link and rerun visual:root-page`)
    await page.goto(`${MOCK_ORIGIN}/`, { waitUntil: 'domcontentloaded' })
    await resetList(page)
    await assertMissingParentOmitted(page, `${where}/missing-parent-detail/return-home`)

    // A search replaces the list with the grouped search results. (By role: a hidden copy of the
    // page can sit in the streamed HTML until the browser swaps it in.)
    await page.getByRole('searchbox', { name: 'search your sessions' }).fill(FIXTURE.searchQuery)
    await page.waitForSelector('[data-grouped-sessions-section]', { state: 'visible', timeout: 15000 }).catch(() => fail(`${where}: search results never mounted`))
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
    await page.waitForSelector('[data-root-session-list] tbody tr', { state: 'visible', timeout: 20000 }).catch(() => fail(`${where}: the list never mounted under a selection notice`))
    await browseBy(page, 'project')
    await page.getByRole('status').filter({ hasText: 'hidden by a saved selection' }).waitFor({ state: 'visible' })
    const notice = await page.evaluate(() => [...document.querySelectorAll('[role="status"]')].map((node) => node.textContent.trim()).find((text) => text.includes('hidden by a saved selection')) ?? '')
    const { hiddenProjects, hiddenSessions } = FIXTURE.selectionNotice
    if (notice !== `${hiddenProjects} projects and ${hiddenSessions} sessions hidden by a saved selection`) fail(`${where}: selection notice reads ${JSON.stringify(notice)}`)
    await shoot(page, gate, join(OUT, theme, viewport.id, 'selection-notice.png'), `${where}/selection-notice`)
    // The session list is the other view of the same menu.
    await browseBy(page, 'sessions')
    await page.waitForSelector('[data-root-session-list] tbody tr', { state: 'visible', timeout: 20000 }).catch(() => fail(`${where}: the list never came back from the project view`))
    await listedRows(page)
    await assertMissingParentOmitted(page, `${where}/selection-notice`)
  } finally {
    await page.close()
  }

  page = await openPage(browser, MOCK_ORIGIN, theme, viewport, { answers, diagnostics, summary: { projects: [], selection: FIXTURE.selectionRecovery } })
  try {
    await browseBy(page, 'project')
    await page.waitForSelector('[role="status"][aria-label="project selection recovery"]', { state: 'visible', timeout: 20000 }).catch(() => fail(`${where}: the recovery panel never mounted`))
    const leaked = await page.locator('[data-root-session-list] [data-session-id], [data-browse-view="projects"] a:not([data-docs-topic])').count()
    if (leaked) fail(`${where}: selection recovery leaked session/project identities`)
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
    await browseBy(page, 'project')
    await page.waitForFunction(() => document.body.textContent.includes('no ai work recorded yet'), undefined, { timeout: 20000 }).catch(() => fail(`${where}: the empty install never showed its teaching state`))
    // The empty state is short: keep the full shell capture and hold the
    // pixel gate to the mounted state, not the blank page below it.
    await shoot(page, gate, join(OUT, theme, viewport.id, 'empty.png'), `${where}/empty`, { contentSelector: '.home-page' })
  } finally {
    await page.close()
  }
  if (diagnostics.length) fail(`${where}: browser diagnostics ${JSON.stringify(diagnostics.slice(0, 3))}`)
}

if (!CHROME) fail('CHROME_PATH is unset; set it to google-chrome or chromium')
const chunks = assertProvenance()
mkdirSync(OUT, { recursive: true })
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright')
const servers = []
let browser
try {
  browser = await chromium.launch({ executablePath: CHROME, headless: true })
  const mockServer = boot(PORT, 'web,sessions,qualitySessions,search')
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

  const gate = new SurfaceGate(await browser.newPage())
  for (const theme of THEMES) {
    for (const viewport of VIEWPORTS) {
      const { expectedCounts } = await runPopulated(browser, gate, theme, viewport, answers)
      await runNestedAlignment(browser, gate, theme, viewport, answers)
      await runSelection(browser, gate, theme, viewport, answers, summaryBase)
      await runEmpty(browser, gate, theme, viewport)
      console.log(`OK ${theme}/${viewport.id} counts=${JSON.stringify(expectedCounts)}`)
    }
  }
  console.log(`captures=${OUT}/{dark,light}/{desktop,phone}/{filter-all,filter-not-published,filter-published,filter-auto,collapsed-group,expanded-group,nested-group,search,selection-notice,selection-recovery,empty}.png`)
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
