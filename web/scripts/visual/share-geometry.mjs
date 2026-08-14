import { spawn } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, readdirSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import YAML from 'yaml'
import { applyDeterminism } from './determinism.mjs'
import { SurfaceGate } from './surface-gate.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '../../..')
const WEB = join(REPO, 'web')
const BIN = join(REPO, 'bin/peasant')
const OUT = process.env.CAPTURES || join(HERE, 'review-capture', 'share-geometry')
const PORT = process.env.PEASANT_SHARE_GEOMETRY_PORT || '8721'
const ORIGIN = `http://localhost:${PORT}`
const CHROME = process.env.CHROME_PATH
const FIXTURE = YAML.parse(readFileSync(join(HERE, 'testdata/search-share.yaml'), 'utf8'))
const THEMES = ['dark', 'light']
const VIEWPORTS = [
  { id: 'desktop', width: 1440, height: 1000 },
  { id: 'narrow-390', width: 390, height: 844 },
  { id: 'narrow-320', width: 320, height: 844 },
]
const pause = (ms) => new Promise((resolvePause) => setTimeout(resolvePause, ms))
const fail = (message) => { throw new Error(`Share geometry gate failed: ${message}`) }

function filesBelow(directory) {
  if (!existsSync(directory)) return []
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name)
    return entry.isDirectory() ? filesBelow(path) : [path]
  })
}

function assertProvenance() {
  const chunks = join(WEB, 'out/_next/static/chunks')
  if (!existsSync(BIN) || !existsSync(chunks)) fail('the built binary or exported chunks are missing; run make build in this worktree before this gate')
  const chunk = filesBelow(chunks).find((path) => path.endsWith('.js') && readFileSync(path, 'utf8').includes('share-footer-actions'))
  if (!chunk || !readFileSync(BIN).includes(Buffer.from('share-footer-actions'))) fail('the built artifacts do not contain the Share footer contract marker; rebuild this exact worktree and stop stale servers')
  return chunk
}

async function waitFor(page, selector, label) {
  const element = await page.waitForSelector(selector, { visible: true, timeout: 15000 }).catch(() => null)
  if (!element) fail(`${label}: required probe target ${selector} did not mount or was blank`)
  const box = await element.boundingBox()
  if (!box || box.width < 1 || box.height < 1) fail(`${label}: required probe target ${selector} has no painted geometry`)
  return element
}

async function probeChoose(page, label) {
  await waitFor(page, '.share-wizard', label)
  await waitFor(page, '.swz-body', label)
  await waitFor(page, '.swz-foot', label)
  await waitFor(page, '[aria-label="choose sessions to contribute"]', label)
  await page.evaluate(() => { const body = document.querySelector('.swz-body'); body.scrollTop = 0 })
  const top = await page.evaluate(() => {
    const rect = (element) => { const box = element.getBoundingClientRect(); return { top: box.top, bottom: box.bottom, left: box.left, right: box.right, width: box.width, height: box.height } }
    const body = document.querySelector('.swz-body')
    const foot = document.querySelector('.swz-foot')
    const chooser = document.querySelector('[aria-label="choose sessions to contribute"]')
    const rows = [...chooser.querySelectorAll('[aria-label^="select session "]')].map((input) => input.closest('.px-4') || input.parentElement)
    const toolbar = chooser.firstElementChild
    const scrolling = [...document.querySelectorAll('.share-page *')].filter((element) => {
      const style = getComputedStyle(element)
      return ['auto', 'scroll'].includes(style.overflowY)
    })
    return {
      pageWidth: { scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth },
      body: rect(body),
      foot: rect(foot),
      footPosition: getComputedStyle(foot).position,
      toolbar: rect(toolbar),
      firstRow: rect(rows[0]),
      lastRow: rect(rows.at(-1)),
      rowCount: rows.length,
      scrollOwners: scrolling.map((element) => element.className),
      bodyOverflowY: getComputedStyle(body).overflowY,
    }
  })
  if (!top.rowCount || !top.firstRow.width || !top.lastRow.width) fail(`${label}: first/last session rows are missing or blank: ${JSON.stringify(top)}`)
  if (top.pageWidth.scroll !== top.pageWidth.client) fail(`${label}: page-level horizontal overflow at Choose: ${JSON.stringify(top.pageWidth)}`)
  if (top.footPosition !== 'static' || top.body.bottom > top.foot.top + 1) fail(`${label}: footer is not normal-flow below the scroll body: ${JSON.stringify(top)}`)
  if (top.toolbar.bottom > top.firstRow.top + 1) fail(`${label}: Choose toolbar overlaps the first session row: ${JSON.stringify(top)}`)
  if (top.scrollOwners.length !== 1 || !String(top.scrollOwners[0]).includes('swz-body') || top.bodyOverflowY !== 'auto') fail(`${label}: .swz-body is not the sole vertical scroll owner: ${JSON.stringify(top.scrollOwners)}`)

  await page.evaluate(() => { const body = document.querySelector('.swz-body'); body.scrollTop = body.scrollHeight })
  await pause(50)
  const bottom = await page.evaluate(() => {
    const body = document.querySelector('.swz-body').getBoundingClientRect()
    const foot = document.querySelector('.swz-foot').getBoundingClientRect()
    const rows = [...document.querySelectorAll('[aria-label="choose sessions to contribute"] [aria-label^="select session "]')]
    const last = (rows.at(-1).closest('.px-4') || rows.at(-1).parentElement).getBoundingClientRect()
    return { bodyBottom: body.bottom, footTop: foot.top, lastTop: last.top, lastBottom: last.bottom }
  })
  if (bottom.lastTop < 0 || bottom.lastBottom > bottom.bodyBottom + 1 || bottom.lastBottom > bottom.footTop + 1) fail(`${label}: last Choose row is not reachable above the footer: ${JSON.stringify(bottom)}`)
}

function jsonResponse(body) {
  return { status: 200, contentType: 'application/json', body: JSON.stringify(body) }
}

async function installMocks(page) {
  await page.setRequestInterception(true)
  page.on('request', (request) => {
    const url = new URL(request.url())
    if (url.origin !== ORIGIN) return void request.continue()
    if (url.pathname === '/api/v1/config/mock') return void request.respond(jsonResponse({ enabled: false }))
    if (url.pathname === '/api/v1/sessions') return void request.respond(jsonResponse({ sessions: FIXTURE.sessions }))
    if (url.pathname === '/api/v1/web/discovery') return void request.respond(jsonResponse({ items: FIXTURE.discovery }))
    if (url.pathname === '/api/v1/annotations') return void request.respond(jsonResponse({ annotations: [] }))
    if (url.pathname === '/api/v1/sync/redactions') return void request.respond(jsonResponse({ categories: [] }))
    return void request.continue()
  })
}

async function reachRedaction(page, label) {
  await page.$eval('[aria-label="choose sessions to contribute"] button', (button) => button.click())
  await page.$eval('.swz-foot button:last-child', (button) => button.click())
  await page.waitForFunction(() => [...document.querySelectorAll('.swz-foot button')].some((button) => ['Skip', 'Continue'].includes(button.textContent.trim()) && !button.disabled), { timeout: 15000 }).catch(() => fail(`${label}: real Labels footer action did not become ready`))
  await page.$eval('.swz-foot button:last-child', (button) => button.click())
  await page.waitForFunction(() => {
    const labels = [...document.querySelectorAll('.swz-foot button')].map((button) => button.textContent.trim())
    return labels.includes('Re-scan') && labels.includes('Continue')
  }, { timeout: 15000 }).catch(() => fail(`${label}: real Redaction footer actions did not register`))
  const probe = await page.evaluate(() => {
    const foot = document.querySelector('.swz-foot')
    const body = document.querySelector('.swz-body')
    const boxes = [...foot.querySelectorAll('button')].map((button) => { const box = button.getBoundingClientRect(); return { label: button.textContent.trim(), left: box.left, right: box.right, top: box.top, bottom: box.bottom, width: box.width, height: box.height } })
    const footBox = foot.getBoundingClientRect()
    const bodyBox = body.getBoundingClientRect()
    return {
      pageWidth: { scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth },
      foot: { left: footBox.left, right: footBox.right, top: footBox.top, bottom: footBox.bottom, width: footBox.width, height: footBox.height },
      bodyBottom: bodyBox.bottom,
      position: getComputedStyle(foot).position,
      buttons: boxes,
    }
  })
  if (probe.pageWidth.scroll !== probe.pageWidth.client) fail(`${label}: page-level horizontal overflow at Redact: ${JSON.stringify(probe)}`)
  if (probe.position !== 'static' || probe.bodyBottom > probe.foot.top + 1) fail(`${label}: Redact footer is not normal-flow below the body: ${JSON.stringify(probe)}`)
  for (const button of probe.buttons) if (!button.width || !button.height || button.left < -0.5 || button.right > probe.pageWidth.client + 0.5) fail(`${label}: footer button is blank or outside the viewport: ${JSON.stringify(probe)}`)
  return probe
}

if (!CHROME) fail('CHROME_PATH is unset; set it to a Chrome or Chromium executable')
const chunk = assertProvenance()
mkdirSync(OUT, { recursive: true })
const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default
const server = spawn(BIN, ['web', 'start', '--port', PORT, '--foreground', '--no-browser', '--mock-data-store=web,sessions,annotations'], { cwd: REPO, stdio: ['ignore', 'ignore', 'pipe'] })
let serverError = ''
server.stderr.on('data', (data) => { serverError += data.toString() })
const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: null })
try {
  let healthy = false
  for (let attempt = 0; attempt < 40 && !healthy; attempt++) { healthy = (await fetch(`${ORIGIN}/api/v1/health`).catch(() => null))?.status === 200; if (!healthy) await pause(250) }
  if (!healthy) fail(`real built binary did not become healthy at ${ORIGIN}: ${serverError.trim()}`)
  const served = await fetch(`${ORIGIN}/_next/static/chunks/${relative(join(WEB, 'out/_next/static/chunks'), chunk).split('\\').join('/')}`)
  if (served.status !== 200 || !(await served.text()).includes('share-footer-actions')) fail('served build provenance does not match the exact worktree artifacts; stop stale servers and rebuild')
  console.log(`provenance chunk=${relative(REPO, chunk)} binaryMarker=share-footer-actions served=true`)
  const gatePage = await browser.newPage()
  const gate = new SurfaceGate(gatePage)
  for (const theme of THEMES) for (const viewport of VIEWPORTS) {
    const label = `${theme}/${viewport.id}`
    const page = await browser.newPage()
    try {
      await applyDeterminism(page)
      await page.setViewport({ width: viewport.width, height: viewport.height, deviceScaleFactor: 1 })
      await page.evaluateOnNewDocument((value) => localStorage.setItem('peasant-theme', value), theme)
      await installMocks(page)
      const response = await page.goto(`${ORIGIN}/share/`, { waitUntil: 'networkidle0' })
      if (response?.status() !== 200) fail(`${label}: /share returned HTTP ${response?.status()}`)
      if (theme === 'light') await page.evaluate(() => { document.documentElement.setAttribute('data-theme', 'light'); document.documentElement.setAttribute('data-tb-theme', 'light') })
      await probeChoose(page, label)
      const redaction = await reachRedaction(page, label)
      const directory = join(OUT, theme)
      mkdirSync(directory, { recursive: true })
      const file = join(directory, `${viewport.id}.png`)
      await page.screenshot({ path: file })
      await gate.assert(`share-geometry-${label}`, file, { sel: '.share-wizard', where: 'share-geometry.mjs' })
      console.log(`OK ${label}: page=${redaction.pageWidth.client}px footer=${Math.round(redaction.foot.width)}x${Math.round(redaction.foot.height)} buttons=${redaction.buttons.map((button) => button.label).join(',')}`)
    } finally { await page.close() }
  }
  await gatePage.close()
  console.log(`captures=${OUT}/{dark,light}/{desktop,narrow-390,narrow-320}.png`)
} finally {
  await browser.close()
  server.kill('SIGTERM')
}
