/* Real-binary mounted evidence for the Command Palette search annotations and /share hierarchy.
 * The browser intercepts only the already-defined private discovery, search, sessions, config, and
 * project-summary responses; route, shell, components, navigation, and selection code remain real.
 * Usage: node search-share-visual.mjs [--self-test]
 */
import { spawn } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, readdirSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import YAML from 'yaml'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '../../..')
const WEB = join(REPO, 'web')
const BIN = join(REPO, 'bin/peasant')
const OUT = process.env.CAPTURES || join(HERE, 'review-capture', 'search-share')
const PORT = process.env.PEASANT_SEARCH_SHARE_PORT || '8718'
const ORIGIN = `http://localhost:${PORT}`
const CHROME = process.env.CHROME_PATH
const FIXTURE = join(HERE, 'testdata/search-share.yaml')
const FEATURE_BYTES = Object.freeze({
  search: ['data-search-annotation', 'repositoryLocationId'],
  share: ['share-hierarchy-check__mixed', 'select repository location', 'select branch', 'omitted projectHash'],
  discoveryRoute: '/api/v1/web/discovery',
})
const THEMES = ['dark', 'light']
const VIEWPORTS = [{ id: 'desktop', width: 1440, height: 1000 }, { id: 'mobile', width: 390, height: 844 }]
const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
const fail = (message) => { throw new Error(`Search/share visual harness failed: ${message}`) }

function filesBelow(directory) {
  if (!existsSync(directory)) return []
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name)
    return entry.isDirectory() ? filesBelow(path) : [path]
  })
}
function readFixture() {
  const document = YAML.parseDocument(readFileSync(FIXTURE, 'utf8'), { strict: true, uniqueKeys: true })
  if (document.errors.length) fail(`fixture ${relative(REPO, FIXTURE)} is invalid: ${document.errors.map((e) => e.message).join('; ')}`)
  const fixture = document.toJS()
  const projectHashesValid = fixture.sessions.every((session) => typeof session.projectHash === 'string' && /^[a-f0-9]{64}$/.test(session.projectHash))
  if (fixture.marker !== undefined || fixture.search.results.length < 2 || fixture.sessions.length < 4 || fixture.discovery.length !== fixture.sessions.length || !projectHashesValid) {
    fail('fixture must omit feature markers, contain two search rows, four sessions with canonical projectHash values, and one discovery row per session')
  }
  return fixture
}
function assertProvenance() {
  const chunks = join(WEB, 'out/_next/static/chunks')
  if (!existsSync(BIN) || !existsSync(chunks)) fail(`missing ${relative(REPO, BIN)} or exported chunks; run make build in this worktree first`)
  const javascript = filesBelow(chunks).filter((path) => path.endsWith('.js')).map((path) => ({ path, content: readFileSync(path, 'utf8') }))
  const featureChunks = Object.fromEntries(Object.entries(FEATURE_BYTES).filter(([name]) => name !== 'discoveryRoute').map(([name, signatures]) => {
    const match = javascript.find(({ content }) => signatures.every((signature) => content.includes(signature)))
    return [name, match]
  }))
  const binary = readFileSync(BIN)
  const missingBinaryBytes = Object.values(FEATURE_BYTES).flat().filter((signature) => !binary.includes(Buffer.from(signature)))
  const missingChunks = Object.entries(featureChunks).filter(([, chunk]) => !chunk).map(([name]) => name)
  if (missingChunks.length || missingBinaryBytes.length || !binary.includes(Buffer.from(FEATURE_BYTES.discoveryRoute))) {
    fail(`stale provenance: missing shipped feature chunks=${missingChunks.join(',') || 'none'}, missing binary bytes=${missingBinaryBytes.join(',') || 'none'}, discoveryRoute=${binary.includes(Buffer.from(FEATURE_BYTES.discoveryRoute))}; rebuild this exact worktree`)
  }
  return Object.values(featureChunks).map(({ path }) => path)
}
function response(body) { return { status: 200, contentType: 'application/json', body: JSON.stringify(body) } }
function installMocks(page, fixture, diagnostics) {
  page.on('request', (request) => {
    const url = new URL(request.url())
    if (url.origin !== ORIGIN) return void request.continue().catch((e) => diagnostics.push(e.message))
    if (url.pathname === '/api/v1/config/mock') return void request.respond(response({ enabled: false })).catch((e) => diagnostics.push(e.message))
    if (url.pathname === '/api/v1/projects/summary') return void request.respond(response({ projects: [{ project: 'peasant-labs/engine', projectHash: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', sessions: 4 }] })).catch((e) => diagnostics.push(e.message))
    if (url.pathname === '/api/v1/search') return void request.respond(response({ query: fixture.search.query, results: fixture.search.results })).catch((e) => diagnostics.push(e.message))
    if (url.pathname === '/api/v1/sessions') return void request.respond(response({ sessions: fixture.sessions })).catch((e) => diagnostics.push(e.message))
    if (url.pathname === '/api/v1/web/discovery') return void request.respond(response({ items: fixture.discovery })).catch((e) => diagnostics.push(e.message))
    return void request.continue().catch((e) => diagnostics.push(e.message))
  })
}
async function wait(page, selector, label, visible = true) {
  const element = await page.waitForSelector(selector, { ...(visible ? { visible: true } : {}), timeout: 15000 }).catch(() => null)
  if (!element) fail(`${label}: selector ${selector} never mounted`)
  return element
}
async function assertCommon(page, theme, label) {
  const result = await page.evaluate((expected) => {
    const body = document.body.getBoundingClientRect()
    return { theme: document.documentElement.getAttribute('data-theme'), body: { width: body.width, height: body.height }, font: getComputedStyle(document.body).fontFamily }
  }, theme)
  if (result.theme !== theme || result.body.width < 100 || result.body.height < 100 || !/Atkinson/i.test(result.font)) fail(`${label}: chrome/theme/font probe ${JSON.stringify(result)}`)
}
async function capture(page, gate, file, selector, label) {
  mkdirSync(dirname(file), { recursive: true })
  const element = await wait(page, selector, label)
  const box = await element.boundingBox()
  if (!box || box.width < 4 || box.height < 4) fail(`${label}: blank or zero-sized ${selector}`)
  await element.screenshot({ path: file, captureBeyondViewport: true })
  await gate.assert(label, file, { sel: selector, where: 'search-share-visual.mjs' })
}
async function runSurface(page, fixture, theme, viewport, kind, gate) {
  const diagnostics = []
  page.removeAllListeners('request')
  await page.setViewport({ width: viewport.width, height: viewport.height, deviceScaleFactor: 1 })
  await page.evaluateOnNewDocument((value) => { localStorage.setItem('peasant-theme', value); document.documentElement.setAttribute('data-theme', value); document.documentElement.setAttribute('data-tb-theme', value) }, theme)
  await page.setRequestInterception(true)
  installMocks(page, fixture, diagnostics)
  const path = kind === 'share' ? '/share/' : '/'
  const responsePage = await page.goto(`${ORIGIN}${path}`, { waitUntil: 'domcontentloaded' })
  if (responsePage?.status() !== 200) fail(`${theme}/${viewport.id}/${kind}: HTTP ${responsePage?.status()}`)
  if (theme === 'light') await page.evaluate(() => { document.documentElement.setAttribute('data-theme', 'light'); document.documentElement.setAttribute('data-tb-theme', 'light') })
  await assertCommon(page, theme, `${theme}/${viewport.id}/${kind}`)
  if (kind === 'search') {
    await pause(500)
    await page.keyboard.down('Control')
    await page.keyboard.press('k')
    await page.keyboard.up('Control')
    await wait(page, '[role="dialog"][aria-label="Command palette"]', 'command palette')
    const input = await wait(page, '[role="combobox"]', 'command palette input')
    await input.type(fixture.search.query)
    await page.waitForFunction(() => {
      const options = [...document.querySelectorAll('[role="option"]')]
      return options.length >= 2 && options.every((option) => option.querySelector('[data-search-annotation="true"]')?.textContent?.trim())
    }, { timeout: 10000 })
    const activeButton = await page.waitForSelector('[role="option"][aria-selected="true"] button', { visible: true, timeout: 10000 })
    await activeButton.focus()
    const probe = await page.evaluate(() => {
      const options = [...document.querySelectorAll('[role="option"]')]
      const viewport = { width: window.innerWidth, height: window.innerHeight }
      const annotationGeometry = options.map((option) => {
        const annotation = option.querySelector('[data-search-annotation="true"]')
        const box = annotation?.getBoundingClientRect()
        const style = annotation ? getComputedStyle(annotation) : null
        const clippingAncestors = []
        for (let ancestor = annotation?.parentElement; ancestor; ancestor = ancestor.parentElement) {
          const ancestorStyle = getComputedStyle(ancestor)
          if (['hidden', 'clip', 'scroll', 'auto'].includes(ancestorStyle.overflow) || ['hidden', 'clip', 'scroll', 'auto'].includes(ancestorStyle.overflowX) || ['hidden', 'clip', 'scroll', 'auto'].includes(ancestorStyle.overflowY)) {
            const ancestorBox = ancestor.getBoundingClientRect()
            clippingAncestors.push({ tag: ancestor.tagName, className: ancestor.className, within: box && box.left >= ancestorBox.left && box.right <= ancestorBox.right && box.top >= ancestorBox.top && box.bottom <= ancestorBox.bottom, ancestor: { left: ancestorBox.left, right: ancestorBox.right, top: ancestorBox.top, bottom: ancestorBox.bottom }, box: box && { left: box.left, right: box.right, top: box.top, bottom: box.bottom } })
          }
        }
        const onscreen = box && box.left >= 0 && box.top >= 0 && box.right <= viewport.width && box.bottom <= viewport.height
        return {
          visible: !!annotation && !!box && box.width > 0 && box.height > 0,
          unclipped: !!onscreen && clippingAncestors.every((ancestor) => ancestor.within) && style?.overflow !== 'hidden' && style?.overflowX !== 'hidden' && style?.overflowY !== 'hidden',
          clippingAncestors,
          text: annotation?.textContent?.trim() || '',
        }
      })
      const active = options.filter((o) => o.getAttribute('aria-selected') === 'true').length
      const row = document.querySelector('[role="option"][aria-selected="true"] button')
      const focused = document.activeElement === row
      const focusStyle = row ? getComputedStyle(row) : null
      const descriptions = options.map((o) => {
        const button = o.querySelector('button')
        const id = button?.getAttribute('aria-describedby')
        const description = id ? document.getElementById(id) : null
        const box = description?.getBoundingClientRect()
        return { text: description?.textContent?.trim() || '', visible: !!box && box.width > 0 && box.height > 0 }
      })
      return {
        annotationGeometry,
        active,
        focused,
        focusStyle: { outlineStyle: focusStyle?.outlineStyle, outlineWidth: focusStyle?.outlineWidth, outlineColor: focusStyle?.outlineColor },
        descriptions,
        rowOverflow: row ? row.scrollWidth > row.clientWidth : false,
      }
    })
    if (!probe.annotationGeometry.every((annotation) => annotation.visible && annotation.unclipped && annotation.text) || !probe.annotationGeometry.some((annotation) => annotation.text.includes('worktrees/engine-main') && annotation.text.includes('selected')) || !probe.annotationGeometry.some((annotation) => annotation.text.includes('worktrees/engine-feature') && annotation.text.includes('unselected')) || !probe.descriptions.some((description) => description.visible && description.text.includes('feat/retry-observability') && description.text.includes('unselected')) || probe.active !== 1 || !probe.focused || probe.focusStyle.outlineStyle === 'none' || probe.focusStyle.outlineWidth === '0px' || probe.focusStyle.outlineColor === 'rgba(0, 0, 0, 0)' || probe.rowOverflow) fail(`${theme}/${viewport.id}/search: annotation/focus probe ${JSON.stringify(probe)}`)
    await capture(page, gate, join(OUT, theme, viewport.id, 'search.png'), '[role="dialog"][aria-label="Command palette"]', `${theme}/${viewport.id}/search`)
  } else {
    await wait(page, '[aria-label="choose sessions to contribute"]', 'share chooser')
    await page.click('[aria-label="select session sess-visual-share-003"]')
    await page.focus('[aria-label="select branch feat/retry-observability"]')
    const probe = await page.evaluate(() => {
      const chooser = document.querySelector('[aria-label="choose sessions to contribute"]')
      const labels = [...chooser.querySelectorAll('[aria-label^="project "]')].map((e) => e.getAttribute('aria-label'))
      const locations = [...chooser.querySelectorAll('[aria-label^="repository location "]')].map((e) => e.getAttribute('aria-label'))
      const branches = [...chooser.querySelectorAll('[aria-label^="branch "]')].map((e) => e.getAttribute('aria-label'))
      const boxes = [...chooser.querySelectorAll('input[type="checkbox"]')]
      const hierarchyBoxes = [...chooser.querySelectorAll('[aria-label^="select project "], [aria-label^="select repository location "], [aria-label^="select branch "]')]
      const focusedBox = chooser.querySelector('[aria-label="select branch feat/retry-observability"]')
      const disabled = boxes.filter((box) => box.disabled).length
      const selected = boxes.filter((box) => box.checked).length
      const toolbar = chooser.querySelector('button')
      const focusStyle = focusedBox ? getComputedStyle(focusedBox) : null
      const glyphs = [...chooser.querySelectorAll('.share-hierarchy-check__mixed')]
      const glyph = glyphs[0]
      const glyphStyle = glyph ? getComputedStyle(glyph) : null
      const rails = [...chooser.querySelectorAll('.share-hierarchy-rail')].map((rail) => {
        const style = getComputedStyle(rail, '::before')
        const box = rail.getBoundingClientRect()
        return { left: style.borderLeftStyle, bottom: style.borderBottomStyle, insetBottom: style.bottom, width: style.width, rowHeight: box.height, containerIsRow: /^H[34]$/.test(rail.tagName) }
      })
      const childRails = [...chooser.querySelectorAll('.share-hierarchy-children')].map((rail) => getComputedStyle(rail, '::before').borderLeftStyle)
      const joins = [...chooser.querySelectorAll('.share-hierarchy-child-row')].map((row) => {
        const style = getComputedStyle(row, '::before')
        return { top: style.borderTopStyle, width: style.width }
      })
      return { labels, locations, branches, checkboxes: boxes.length, hierarchyMixed: hierarchyBoxes.filter((box) => box.indeterminate && box.getAttribute('aria-checked') === 'mixed').length, disabled, selected, toolbar: toolbar?.textContent?.trim(), overflow: chooser.scrollWidth > chooser.clientWidth, focused: document.activeElement === focusedBox, focus: focusStyle?.outlineStyle, focusWidth: focusStyle?.outlineWidth, focusColor: focusStyle?.outlineColor, glyphs: glyphs.length, glyph: { opacity: glyphStyle?.opacity, width: glyphStyle?.width, height: glyphStyle?.height, background: glyphStyle?.backgroundColor }, rails, childRails, joins }
    })
    if (probe.labels.length !== 1 || probe.locations.length !== 3 || probe.branches.length !== 3 || probe.checkboxes !== 11 || probe.selected !== 1 || probe.hierarchyMixed !== 3 || probe.glyphs !== 3 || probe.toolbar !== 'select all' || probe.overflow || !probe.focused || probe.focusWidth === '0px' || probe.focusColor === 'rgba(0, 0, 0, 0)' || probe.glyph.opacity !== '1' || probe.glyph.width === '0px' || probe.glyph.height === '0px' || probe.glyph.background === 'rgba(0, 0, 0, 0)' || probe.rails.length !== 3 || probe.rails.some((rail) => !rail.containerIsRow || rail.left === 'none' || rail.bottom === 'none' || rail.insetBottom === '0px' || rail.insetBottom === `${rail.rowHeight}px` || rail.width === '0px') || probe.childRails.length !== 6 || probe.childRails.some((rail) => rail === 'none') || probe.joins.length !== 7 || probe.joins.some((join) => join.top === 'none' || join.width === '0px')) fail(`${theme}/${viewport.id}/share: hierarchy/state/focus/mixed-glyph/connected-rail probe ${JSON.stringify(probe)}`)
    await capture(page, gate, join(OUT, theme, viewport.id, 'share.png'), 'main', `${theme}/${viewport.id}/share`)
  }
  if (diagnostics.length) fail(`${theme}/${viewport.id}/${kind}: browser diagnostics ${JSON.stringify(diagnostics.slice(0, 3))}`)
}

if (process.argv.includes('--self-test')) {
  const fixture = readFixture()
  console.log(`OK search-share visual self-test: ${fixture.sessions.length} sessions, ${fixture.discovery.length} discovery rows, feature bytes=search/share/discovery-route`)
  process.exit(0)
}
if (!CHROME) fail('CHROME_PATH is unset; set it to google-chrome or chromium')
const fixture = readFixture()
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
  for (const chunk of chunks) {
    const chunkPath = `/_next/static/chunks/${relative(join(WEB, 'out/_next/static/chunks'), chunk).split('\\').join('/')}`
    const served = await fetch(`${ORIGIN}${chunkPath}`)
    const body = await served.text()
    const expected = Object.values(FEATURE_BYTES).flat().some((signature) => body.includes(signature))
    if (served.status !== 200 || !expected) fail(`served provenance: ${chunkPath} returned HTTP ${served.status} without verified feature bytes; stop stale servers, rebuild this exact worktree, and rerun the visual harness`)
  }
  console.log(`provenance chunks=${chunks.map((chunk) => relative(REPO, chunk)).join(',')} binaryBytes=search/share/discovery-route served=true`)
  const gate = new SurfaceGate(await browser.newPage())
  for (const theme of THEMES) for (const viewport of VIEWPORTS) for (const kind of ['search', 'share']) {
    const page = await browser.newPage()
    try { await applyDeterminism(page); await runSurface(page, fixture, theme, viewport, kind, gate); console.log(`OK ${theme}/${viewport.id}/${kind}`) } finally { await page.close() }
  }
  console.log(`captures=${OUT}/{dark,light}/{desktop,mobile}/{search,share}.png`)
} finally {
  await browser.close()
  server.kill('SIGTERM')
}
