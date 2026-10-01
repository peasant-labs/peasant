/* MOUNTED PUBLISH BAR AND POPUP STATES — real-binary evidence capture.
 *
 * Boots this worktree's `bin/peasant` (assets embedded from this branch's
 * `web/out`) with the mock data store, opens a stored session's transcript
 * through the real route (project resolution -> SessionDetailV2 -> the
 * WebSocket session_detail -> fairtrade's TranscriptViewer and publish parts),
 * and shoots every case of src/app/share/testdata/publish-popup-states.yaml in
 * both themes: the bar in each status and the popup in each state.
 *
 * The publishing routes (sign-in, publications, collectives, the local scan and
 * the push) are answered from the case by publish-popup-states.mjs, the same
 * module the mounted test uses, through request interception: the mock store
 * holds no village account, and a real publish must never leave a capture run.
 * Everything else is the real server. Before a capture is kept, the page must
 * show exactly what the case expects, so an image can only be the state its
 * name says.
 *
 * Build provenance is asserted BEFORE any capture: a string only this change
 * introduces must be in the embedded binary and in the chunk the server
 * actually serves.
 *
 * Run:  CHROME_PATH=<chrome> node web/scripts/visual/publish-states-shoot.mjs
 * Env:  PEASANT_PUBLISH_STATES_PORT         server port (default 8847)
 *       PEASANT_PUBLISH_STATES_CAPTURE_DIR  output root (default /tmp/peasant-publish-states)
 *       PEASANT_PUBLISH_STATES_ONLY         comma-separated case names to shoot
 *       PUPPETEER_CORE                      explicit puppeteer-core module path
 */
import { execFileSync, spawn } from 'node:child_process'
import { createHash } from 'node:crypto'
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join, relative, resolve } from 'node:path'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'
import { SMOKE_MOCKS, SMOKE_THEMES } from './smoke-surfaces.mjs'
import { createPublishWorld, expectedPushBody, loadPublishStates } from '../../src/app/share/testdata/publish-popup-states.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '../../..')
const WEB = join(REPO, 'web')
const BIN = join(REPO, 'bin/peasant')
const OUT = process.env.PEASANT_PUBLISH_STATES_CAPTURE_DIR || '/tmp/peasant-publish-states'
const PORT = process.env.PEASANT_PUBLISH_STATES_PORT || '8847'
const ORIGIN = `http://localhost:${PORT}`
const CHROME = process.env.CHROME_PATH
const ONLY = (process.env.PEASANT_PUBLISH_STATES_ONLY || '').split(',').map((name) => name.trim()).filter(Boolean)

/** The stored mock session the states are shown on, and its project label. */
const PROJECT = 'fortuna'
const SESSION = 'sess-c3d4e5f6-a7b8-9012-cdef-123456789012'
/** Bytes only this change introduces, located in the binary and the served chunk. */
const FEATURE_BYTES = ['reading your collectives on village', 'village no longer holds this transcript: run peasant village push --force for this session']
const hash = (bytes) => createHash('sha256').update(bytes).digest('hex')
const DESKTOP = { width: 1440, height: 1080, deviceScaleFactor: 1 }
const MOBILE = { width: 390, height: 844, deviceScaleFactor: 2, isMobile: true, hasTouch: true }
/** The cases also shot at phone width: the bar, a first publish, and an update. */

const THEME_ATTRIBUTES = ['data-theme', 'data-tb-theme']
const PUBLISHING_ROUTES = /\/api\/v1\/(sync\/auth|sync\/login|publications|village\/collectives|sync\/redactions|sync\/push)(\/|\?|$)/
const pause = (ms) => new Promise((done) => setTimeout(done, ms))

if (!CHROME) {
  console.error('ERROR [publish-states-shoot] CHROME_PATH is unset — set it to a Chrome or Chrome for Testing binary.')
  process.exit(1)
}

const fail = (step, reason) => new Error(
  `Publish state capture failed because ${reason} during ${step} in publish-states-shoot.mjs; the state is not proven on this build; inspect the served transcript route and src/app/share/testdata/publish-popup-states.yaml, fix the production path, rebuild, and rerun.`,
)

function filesBelow(directory) {
  if (!existsSync(directory)) return []
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name)
    return entry.isDirectory() ? filesBelow(path) : [path]
  })
}

/** Prove the binary embeds this change's export, and find the chunk carrying it. */
function assertBuildProvenance() {
  const chunks = join(WEB, 'out/_next/static/chunks')
  if (!existsSync(BIN) || !existsSync(chunks)) throw fail('verifying build provenance', `missing ${relative(REPO, BIN)} or exported chunks; run make build in this worktree first`)
  const binary = readFileSync(BIN)
  const missing = FEATURE_BYTES.filter((signature) => !binary.includes(Buffer.from(signature)))
  if (missing.length) throw fail('verifying build provenance', `the embedded binary carries no ${missing.join(', ')}; rebuild this exact worktree so bin/peasant matches web/out`)
  const chunk = filesBelow(chunks).filter((path) => path.endsWith('.js')).find((path) => FEATURE_BYTES.every((signature) => readFileSync(path, 'utf8').includes(signature)))
  if (!chunk) throw fail('verifying build provenance', `no exported chunk carries ${FEATURE_BYTES.join(', ')}; rebuild this exact worktree`)
  return chunk
}

async function assertServedChunk(chunkPath) {
  const servedPath = `/_next/static/chunks/${relative(join(WEB, 'out/_next/static/chunks'), chunkPath).split(/[\\/]/).map(encodeURIComponent).join('/')}`
  const response = await fetch(`${ORIGIN}${servedPath}`).catch(() => null)
  if (!response || response.status !== 200) throw fail('verifying the served artifact', `GET ${servedPath} returned ${response?.status ?? 0}`)
  const body = await response.text()
  if (!FEATURE_BYTES.every((signature) => body.includes(signature))) throw fail('verifying the served artifact', `the served chunk ${servedPath} does not carry ${FEATURE_BYTES.join(', ')}`)
  const sha256 = hash(Buffer.from(body))
  if (sha256 !== hash(readFileSync(chunkPath))) throw fail('verifying the served artifact', `the served chunk differs from this worktree's export`)
  return { path: servedPath, sha256 }
}

/**
 * The stored session's turns, as the page receives them over the WebSocket
 * session_detail channel, for "N new turns". The mock store serves the detail
 * only there, so a warm-up visit reads it off the socket's frames.
 */
async function sessionTurns(browser) {
  const page = await browser.newPage()
  try {
    const client = await page.createCDPSession()
    await client.send('Network.enable')
    let turns = null
    client.on('Network.webSocketFrameReceived', ({ response }) => {
      try {
        const message = JSON.parse(response.payloadData)
        if (message.type === 'session_detail' && message.data?.id === SESSION && Array.isArray(message.data.turns)) turns = message.data.turns
      } catch { /* a frame that is not a session detail */ }
    })
    await page.goto(`${ORIGIN}/projects/${PROJECT}/${SESSION}/`, { waitUntil: 'domcontentloaded' })
    for (let attempt = 0; attempt < 80 && !turns; attempt += 1) await pause(250)
    if (!turns?.length) throw fail('reading the mock session', `no session_detail frame for ${SESSION} arrived with turns`)
    return turns
  } finally {
    await page.close()
  }
}

/** Answer the publishing routes from the case; everything else reaches the real server. */
async function serveWorld(page, world) {
  await page.setRequestInterception(true)
  page.on('request', (request) => {
    if (request.isInterceptResolutionHandled()) return
    const url = request.url()
    if (!url.startsWith(ORIGIN) || !PUBLISHING_ROUTES.test(new URL(url).pathname + new URL(url).search)) {
      request.continue()
      return
    }
    let body
    try { body = request.postData() ? JSON.parse(request.postData()) : undefined } catch { body = undefined }
    const answer = world.respond({ method: request.method(), url, body })
    if (answer === null) { request.continue(); return }
    if ('pending' in answer) return // never answers, as the case says
    request.respond('json' in answer
      ? { status: answer.status, contentType: 'application/json', body: JSON.stringify(answer.json) }
      : { status: answer.status, contentType: 'text/plain', body: answer.text })
  })
}

async function runStep(page, step) {
  const click = async (selector, what) => {
    const handle = await page.waitForSelector(selector, { visible: true, timeout: 20000 }).catch(() => null)
    if (!handle) throw fail(`step ${JSON.stringify(step)}`, `${what} (${selector}) never appeared`)
    await handle.click()
  }
  if (step === 'arrive') return
  if (step === 'open') {
    await click('.pub-bar .pub-bar-action', 'the bar action')
    await page.waitForSelector('[role="dialog"]', { visible: true, timeout: 20000 })
    return
  }
  if (step === 'connect') return click('[role="dialog"] .si-split-primary', 'continue with GitHub')
  if (step === 'publish') return click('[role="dialog"] .pub-primary:not([disabled]):not([aria-disabled="true"])', 'an enabled publish button')
  if (step === 'retry') return click('[role="dialog"] .pub-foot .btn-primary:not([disabled])', 'retry')
  if (step === 'rescan') return click('[role="dialog"] .pub-rescan', 're-scan')
  const [verb, ...rest] = step.split(' ')
  return click(`[role="dialog"] button[aria-label="${verb} ${rest.join(' ')}"]`, `the ${verb} button`)
}

/** Wait until the page shows what the case expects, or fail naming the difference. */
async function assertExpected(page, entry) {
  const expected = entry.expect
  const check = () => page.evaluate((expect) => {
    const label = document.querySelector('.pub-bar .pub-state')
    const problems = []
    if ('alert' in expect.bar) {
      if (!document.querySelector('[role="alert"]')?.textContent.includes(expect.bar.alert)) problems.push('missing publication alert')
      const retry = [...document.querySelectorAll('[role="group"][aria-label="publish"] button')].some((button) => button.textContent.trim() === 'retry')
      if (retry !== expect.bar.retry) problems.push('wrong publication recovery action')
    } else {
    if (label?.getAttribute('data-state') !== expect.bar.state) problems.push(`bar state ${label?.getAttribute('data-state')}`)
    if (label?.textContent !== expect.bar.text) problems.push(`bar text ${JSON.stringify(label?.textContent)}`)
    const action = document.querySelector('.pub-bar .pub-bar-action')
    if (action?.textContent?.trim() !== expect.bar.action) problems.push(`bar action ${JSON.stringify(action?.textContent)}`)
    }
    const dialog = document.querySelector('[role="dialog"]')
    if (expect.popup === null) {
      if (dialog) problems.push('a popup is open')
      return problems
    }
    if (!dialog) return [...problems, 'no popup']
    const heading = document.getElementById(dialog.getAttribute('aria-labelledby') ?? '')
    const title = document.querySelector('.txn-title')?.textContent ?? ''
    if (heading?.textContent !== expect.popup.heading.replace('{title}', title)) problems.push(`heading ${JSON.stringify(heading?.textContent)}`)
    if (expect.popup.link && ![...dialog.querySelectorAll('a')].some((link) => link.textContent.trim() === expect.popup.link.text && link.href === expect.popup.link.href)) problems.push('wrong recovery link')
    for (const text of expect.popup.texts) if (!dialog.textContent.includes(text)) problems.push(`missing ${JSON.stringify(text)}`)
    const primaries = [...dialog.querySelectorAll('.pub-foot .btn-primary')]
    const primary = primaries[primaries.length - 1] ?? null
    if (expect.popup.primary === null) {
      if (primary) problems.push(`unexpected primary ${JSON.stringify(primary.textContent)}`)
    } else {
      if (primary?.textContent?.trim() !== expect.popup.primary.label) problems.push(`primary ${JSON.stringify(primary?.textContent)}`)
      if (primary && (primary.disabled || primary.getAttribute('aria-disabled') === 'true') === expect.popup.primary.enabled) problems.push(`primary enabled=${!primary.disabled}`)
    }
    return problems
  }, expected)
  let problems = []
  for (let attempt = 0; attempt < 60; attempt += 1) {
    problems = await check()
    if (!problems.length) return
    await pause(250)
  }
  throw fail(`asserting ${entry.name}`, `the page does not show the case: ${problems.join('; ')}`)
}

/** Computed-style probes: the fonts, the theme, radius 0 on the parts this change mounts. */
async function probe(page, theme, entry) {
  const found = await page.evaluate((names) => {
    const style = (selector, property) => {
      const node = document.querySelector(selector)
      return node ? getComputedStyle(node)[property] : null
    }
    return {
      themes: Object.fromEntries(names.map((name) => [name, document.documentElement.getAttribute(name)])),
      body: getComputedStyle(document.body).fontFamily,
      barLabel: style('.pub-bar .pub-state', 'fontFamily'),
      barLabelSize: style('.pub-bar .pub-state', 'fontSize'),
      actionRadius: style('.pub-bar .pub-bar-action', 'borderTopLeftRadius'),
      dialogRadius: style('[role="dialog"]', 'borderTopLeftRadius'),
      dialogBody: style('[role="dialog"] .pub-line', 'fontSize'),
      atkinson: document.fonts.check('16px "Atkinson Hyperlegible"'),
    }
  }, THEME_ATTRIBUTES)
  if (!THEME_ATTRIBUTES.every((name) => found.themes[name] === theme)) throw fail(`probing ${entry.name}`, `theme attributes ${JSON.stringify(found.themes)} are not ${theme}`)
  // next/font self-hosts the faces under their own family names (atkinsonHyperlegible, …Mono).
  if (!found.atkinson || !/atkinson/i.test(found.body) || /mono/i.test(found.body.split(',')[0])) throw fail(`probing ${entry.name}`, `the body font is ${JSON.stringify(found.body)}`)
  if (!('alert' in entry.expect.bar) && !/atkinson.*mono/i.test((found.barLabel ?? '').split(',')[0])) throw fail(`probing ${entry.name}`, `the bar status font is ${JSON.stringify(found.barLabel)}, not the mono chrome`)
  if (!('alert' in entry.expect.bar) && found.actionRadius !== '0px') throw fail(`probing ${entry.name}`, `the bar action has radius ${found.actionRadius}`)
  if (found.dialogRadius !== null && found.dialogRadius !== '0px') throw fail(`probing ${entry.name}`, `the popup has radius ${found.dialogRadius}`)
  if (found.dialogBody !== null && parseFloat(found.dialogBody) < 16) throw fail(`probing ${entry.name}`, `the popup body text is ${found.dialogBody}, under the 16px floor`)
  return found
}

async function shoot(browser, gate, fixture, entry, theme, viewport, turns, evidence) {
  const page = await browser.newPage()
  try {
    await applyDeterminism(page)
    await page.setViewport(viewport)
    await page.evaluateOnNewDocument((value) => { try { localStorage.setItem('peasant-theme', value) } catch { /* storage disabled */ } }, theme)
    const world = createPublishWorld(fixture, entry, { sessionId: SESSION, turns })
    const requests = []
    page.on('request', (request) => { if (PUBLISHING_ROUTES.test(request.url())) requests.push(`${request.method()} ${request.url().replace(ORIGIN, '')}`) })
    const errors = []
    page.on('console', (message) => { if (message.type() === 'error') errors.push(message.text()) })
    await serveWorld(page, world)
    const response = await page.goto(`${ORIGIN}/projects/${PROJECT}/${SESSION}/${entry.steps.includes('arrive') ? '?publish=open' : ''}`, { waitUntil: 'domcontentloaded' })
    if (response?.status() !== 200) throw fail(`opening ${entry.name}`, `HTTP status ${response?.status() ?? 0}`)
    await page.waitForSelector('.txn-app', { visible: true, timeout: 30000 }).catch(() => { throw fail(`opening ${entry.name}`, 'the transcript never mounted') })
    await page.waitForSelector('[role="group"][aria-label="publish"]', { visible: true, timeout: 20000 }).catch(() => { throw fail(`opening ${entry.name}`, 'the publish bar never mounted') })
    await page.evaluate(async () => { await document.fonts.ready })
    for (const step of entry.steps) await runStep(page, step)
    try {
      await assertExpected(page, entry)
      const expected = expectedPushBody(fixture, entry, SESSION)
      const actual = world.pushRequests.at(-1) ?? null
      if (JSON.stringify(expected) !== JSON.stringify(actual)) throw fail(`checking ${entry.name}`, 'the mounted push request differs from the fixture')
    } catch (error) {
      const failed = join(OUT, 'failed', `${theme}-${viewport.isMobile ? 'mobile' : 'desktop'}-${entry.name}.png`)
      mkdirSync(dirname(failed), { recursive: true })
      await page.screenshot({ path: failed }).catch(() => {})
      error.message += `\n  requests: ${requests.join(' | ')}\n  console errors: ${errors.join(' | ') || 'none'}\n  page: ${failed}`
      throw error
    }
    await pause(300)
    const probes = await probe(page, theme, entry)
    const size = viewport.isMobile ? 'mobile' : 'desktop'
    const directory = join(OUT, theme, size)
    mkdirSync(directory, { recursive: true })
    const file = join(directory, `${entry.name}.png`)
    await page.screenshot({ path: file })
    await gate.assert(`${theme}/${size}/${entry.name}`, file, { where: 'publish-states-shoot.mjs' })
    evidence.captures.push({ case: entry.name, theme, size, file, url: page.url(), probes, pushes: world.pushRequests.length, scans: world.scanRequests })
    console.log(`  ${theme} ${size} ${entry.name} -> ${file}`)
  } finally {
    await page.close()
  }
}

if (!existsSync(BIN)) { console.error(`ERROR [publish-states-shoot] ${BIN} not found — run \`make build\` first.`); process.exit(1) }
const fixture = loadPublishStates(readFileSync(join(WEB, 'src/app/share/testdata/publish-popup-states.yaml'), 'utf8'))
const cases = ONLY.length ? fixture.cases.filter((entry) => ONLY.includes(entry.name)) : fixture.cases
if (ONLY.length && cases.length !== ONLY.length) { console.error(`ERROR [publish-states-shoot] unknown case in PEASANT_PUBLISH_STATES_ONLY=${ONLY.join(',')}`); process.exit(1) }

const chunkPath = assertBuildProvenance()
console.log(`[publish-states-shoot] provenance OK: ${relative(REPO, chunkPath)} carries ${FEATURE_BYTES.join(', ')}`)

console.log(`[publish-states-shoot] starting ${relative(REPO, BIN)} on :${PORT} (mock store: ${SMOKE_MOCKS}) …`)
const server = spawn(BIN, ['web', 'start', '--port', PORT, '--foreground', '--no-browser', `--mock-data-store=${SMOKE_MOCKS}`], { cwd: REPO, stdio: ['ignore', 'ignore', 'pipe'] })
let serverError = ''
server.stderr.on('data', (data) => { serverError += data.toString() })
let serverDown = false
server.on('exit', () => { serverDown = true })

const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default
let browser
const teardown = async () => {
  try { if (browser) await browser.close() } catch { /* already closing */ }
  try { if (!serverDown) server.kill('SIGTERM') } catch { /* already gone */ }
}

let healthy = false
for (let i = 0; i < 60 && !healthy && !serverDown; i++) { healthy = (await fetch(`${ORIGIN}/api/v1/health`).catch(() => null))?.status === 200; if (!healthy) await pause(250) }
// A healthy answer from a server this run did not start (another process on
// the port) would put a stale build under the captures.
await pause(500)
if (serverDown) healthy = false
if (!healthy) { console.error(`ERROR [publish-states-shoot] the real binary did not become healthy on ${ORIGIN}: ${serverError.trim()}`); await teardown(); process.exit(2) }

const evidence = { sourceHead: execFileSync('git', ['rev-parse', 'HEAD'], { cwd: REPO, encoding: 'utf8' }).trim(), binarySha256: hash(readFileSync(BIN)), packagePins: JSON.parse(readFileSync(join(WEB, 'package.json'), 'utf8')).dependencies, fixture: 'web/src/app/share/testdata/publish-popup-states.yaml', chunk: relative(REPO, chunkPath), featureBytes: FEATURE_BYTES, servedChunk: null, captures: [] }
try {
  evidence.servedChunk = await assertServedChunk(chunkPath)
  browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new' })
  const turns = await sessionTurns(browser)
  evidence.turns = turns.length
  const gate = new SurfaceGate(await browser.newPage())
  for (const theme of SMOKE_THEMES) {
    for (const entry of cases) {
      await shoot(browser, gate, fixture, entry, theme, DESKTOP, turns, evidence)
      if (fixture.mobileCases.includes(entry.name)) await shoot(browser, gate, fixture, entry, theme, MOBILE, turns, evidence)
    }
  }
  mkdirSync(OUT, { recursive: true })
  writeFileSync(join(OUT, 'evidence.json'), JSON.stringify(evidence, null, 2))
  console.log(`\nOK [publish-states-shoot] ${evidence.captures.length} mounted captures; evidence in ${join(OUT, 'evidence.json')}`)
} catch (error) {
  console.error(`FAIL [publish-states-shoot] ${error.stack || error.message}`)
  await teardown()
  process.exit(1)
} finally {
  await teardown()
}
