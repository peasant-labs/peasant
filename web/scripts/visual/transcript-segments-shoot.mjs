/* Mounted transcript-segment capture.

   Boots this worktree's peasant binary, which embeds this branch's web export,
   in its own server sandbox, against a store seeded in the sandbox's data
   directory with a synthetic multi-segment transcript.
   Opens the production transcript route on a middle turn so both load-more
   boundaries are on screen, in both themes. The seed holds system and tool rows,
   so a boundary's conversation turn range never equals its row indexes. Before
   each capture the page is read back: every caption names a range that ends at
   or below the header's total, against that same total. The home session list
   is captured too, showing the session's conversation turn count.
   assertServedBuild runs before any capture. Transient PNGs stay under review-capture/ and are not committed.
*/
import { spawn } from 'node:child_process'
import { mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { assertServedBuild } from './served-build.mjs'
import { ownedServerSandbox } from './lib/owned-server-sandbox.mjs'
import { startOwnedServer } from './lib/owned-server-start.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '../../..')
const WEB = resolve(HERE, '../..')
const BIN = resolve(REPO, 'peasant')
const PORT = process.env.PEASANT_TRANSCRIPT_SEGMENTS_PORT || '8873'
const ORIGIN = `http://127.0.0.1:${PORT}`
const CHROME = process.env.CHROME_PATH || '/home/minttea/.nix-profile/bin/google-chrome'
const OUT = process.env.PEASANT_TRANSCRIPT_SEGMENTS_CAPTURE_DIR || resolve(REPO, 'review-capture/transcript-segments')
const MARKER = 'data-segment-mode'
const HOST_MARKER = 'opened the nearest user turn'
// Strings only the conversation turn position work serves. The first is this
// checkout's directory parser; the second is the boundary copy of the pinned
// design-system release, so a build on the earlier pin fails here.
const POSITION_MARKERS = ['carries a conversation turn position this page cannot use', 'no conversation turns']
// The viewer fills the viewport. Capturing beyond the viewport lays the page out
// again at another size for the shot, which starts the outline rail's width
// transition, so the image shows a rail the page never rests at.
const IN_VIEWPORT = { captureBeyondViewport: false }
const RANGE = /^turns? ([\d,]+)(?:–([\d,]+))? of ([\d,]+)$/
const integer = (text) => Number(String(text).replaceAll(',', ''))

const pause = (ms) => new Promise((done) => setTimeout(done, ms))
const fail = (step, reason) => {
  throw new Error(`transcript segment capture failed during ${step}: ${reason}. The shot is not evidence. Rebuild this worktree and rerun.`)
}

function run(command, args, options = {}) {
  return new Promise((resolveRun, reject) => {
    const child = spawn(command, args, { cwd: REPO, stdio: ['ignore', 'pipe', 'pipe'], ...options })
    let stdout = ''
    let stderr = ''
    child.stdout.on('data', (chunk) => { stdout += chunk })
    child.stderr.on('data', (chunk) => { stderr += chunk })
    child.on('error', reject)
    child.on('close', (code) => {
      if (code !== 0) reject(fail(command, `exit ${code}: ${stderr || stdout}`))
      else resolveRun({ stdout, stderr, child })
    })
  })
}

async function directory(sessionId) {
  const response = await fetch(`${ORIGIN}/api/v1/sessions/${sessionId}/transcript/segments`)
  const body = await response.json()
  if (!response.ok) throw fail('reading the directory', `${response.status} ${JSON.stringify(body)}`)
  return body
}

mkdirSync(OUT, { recursive: true })
const sandbox = ownedServerSandbox('transcript-segments-shoot')
const seeded = JSON.parse((await run('go', ['run', './web/scripts/visual/seed-transcript-store.go', sandbox.dataDir])).stdout)
const server = await startOwnedServer({ port: PORT, portVariable: 'PEASANT_TRANSCRIPT_SEGMENTS_PORT', withinMs: 30_000, start: () => spawn(BIN, ['web', 'start', '--foreground', '--no-browser', '--port', PORT, ...sandbox.args], { cwd: REPO, env: sandbox.env, stdio: ['ignore', 'pipe', 'pipe'] }) })
  .catch((error) => { throw fail('starting peasant', error.message) })
let serverLog = ''
server.stdout.on('data', (chunk) => { serverLog += chunk })
server.stderr.on('data', (chunk) => { serverLog += chunk })
try {
  const provenance = await assertServedBuild({ origin: ORIGIN, markers: [MARKER, 'data-segment-boundaries', 'data-mounted-turn-bound'], bin: BIN, webRoot: WEB })
  const hostChunk = (await import('node:fs')).readdirSync(resolve(WEB, 'out/_next/static/chunks/app/projects/[[...segments]]')).find((name) => name.startsWith('page-'))
  if (!hostChunk) throw fail('verifying build provenance', 'the projects page chunk is missing from web/out')
  const hostPath = `/_next/static/chunks/app/projects/[[...segments]]/${hostChunk}`
  const hostBody = await (await fetch(`${ORIGIN}${hostPath}`)).text()
  if (!hostBody.includes(HOST_MARKER)) throw fail('verifying the served host chunk', `${hostPath} does not carry ${HOST_MARKER}`)
  const chunkRoot = resolve(WEB, 'out/_next/static/chunks')
  const chunkFiles = readdirSync(chunkRoot, { recursive: true }).filter((name) => String(name).endsWith('.js')).map(String)
  const positionChunks = {}
  for (const marker of POSITION_MARKERS) {
    const local = chunkFiles.filter((name) => readFileSync(resolve(chunkRoot, name), 'utf8').includes(marker))
    if (local.length === 0) throw fail('verifying build provenance', `no chunk of web/out carries ${JSON.stringify(marker)}; the export predates the conversation turn positions or was built on an earlier design-system pin`)
    for (const name of local) {
      const served = await fetch(`${ORIGIN}/_next/static/chunks/${name.split('/').map(encodeURIComponent).join('/')}`)
      if (!served.ok || !(await served.text()).includes(marker)) throw fail('verifying the served position chunk', `${name} is not served with ${JSON.stringify(marker)}`)
    }
    positionChunks[marker] = local
  }
  const index = await directory(seeded.sessionId)
  if (index.totalTurns !== seeded.turns || index.totalConversationTurns !== seeded.conversationTurns) {
    throw fail('checking the seeded counts', `the directory states ${index.totalTurns} turns and ${index.totalConversationTurns} conversation turns; the seed holds ${seeded.turns} and ${seeded.conversationTurns}`)
  }
  if (index.segments.some((segment) => !segment.conversationTurns)) throw fail('checking the seeded positions', 'a segment ref carries no conversationTurns')
  if (!index.segments.some((segment) => segment.conversationTurns.before !== segment.firstTurn)) {
    throw fail('checking the seeded positions', 'every position equals its row index; the seed must hold system and tool rows before a later segment')
  }
  if (index.totalSegments < 3) throw fail('checking the seeded cut', `only ${index.totalSegments} segments; both boundaries need a middle segment with unloaded extents on both sides`)
  const actionParents = seeded.actionParents
  const actionSegments = actionParents.map((turn) => index.segments.find((segment) => segment.firstTurn <= turn && turn <= segment.lastTurn))
  if (actionSegments.some((segment) => !segment) || actionSegments[0].index === actionSegments[1].index) {
    throw fail('checking the seeded action run', 'the action parents do not cross a segment boundary')
  }
  for (const [position, segment] of actionSegments.entries()) {
    const response = await fetch(`${ORIGIN}/api/v1/sessions/${seeded.sessionId}/transcript/segments/${segment.index}`)
    const body = await response.json()
    const turn = body.turns?.find((entry) => entry.index === actionParents[position])
    if (!response.ok || !turn?.toolCalls?.some((call) => call.id === `synthetic-tool-${actionParents[position] + 1}`)) {
      throw fail('reading the seeded action run', `segment ${segment.index} did not serve its expected tool child`)
    }
  }
  const middle = index.segments[Math.floor(index.segments.length / 2)]
  const url = `${ORIGIN}/projects/${seeded.projectHash}/${seeded.sessionId}?turn=${middle.firstTurn}`
  const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default
  const browser = await puppeteer.launch({
    executablePath: CHROME,
    headless: true,
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  })
  const captures = []
  try {
    for (const theme of ['dark', 'light']) {
      const page = await browser.newPage()
      await page.setViewport({ width: 1440, height: 1100, deviceScaleFactor: 1 })
      await page.evaluateOnNewDocument((value) => {
        localStorage.setItem('peasant-theme', value)
      }, theme)
      await page.goto(url, { waitUntil: 'networkidle0', timeout: 60_000 })
      await page.waitForSelector('[data-segment-mode="true"]', { timeout: 30_000 })
      const activeTheme = await page.$eval('html', (html) => html.getAttribute('data-theme'))
      if (activeTheme !== theme) throw fail(`confirming ${theme}`, `data-theme is ${activeTheme}`)
      try {
        await page.waitForFunction(() => {
          const nodes = document.querySelectorAll('[data-segment-boundary]')
          return nodes.length >= 2
            && [...nodes].some((node) => node.getAttribute('data-segment-boundary') === 'older')
            && [...nodes].some((node) => node.getAttribute('data-segment-boundary') === 'newer')
        }, { timeout: 45_000 })
      } catch (error) {
        const debug = await page.evaluate(() => ({
          mode: document.querySelector('[data-segment-mode]')?.getAttribute('data-segment-mode') ?? null,
          boundaries: document.querySelector('[data-segment-mode]')?.getAttribute('data-segment-boundaries') ?? null,
          text: document.body.innerText.slice(0, 800),
        }))
        throw fail(`waiting for both load-more boundaries in ${theme}`, `${error.message}; page=${JSON.stringify(debug)}`)
      }
      const viewer = await page.$('.txn-app')
      if (!viewer) throw fail(`capturing ${theme}`, 'the transcript viewer did not mount')
      // Read the header total and every boundary caption back from the page.
      // A range must end at or below the header total and be stated against it,
      // and the header total is the directory's.
      let positions = null
      const assertPositions = async () => {
        const read = await page.evaluate(() => ({
          header: document.querySelector('.txn-trace-count')?.textContent ?? '',
          captions: [...document.querySelectorAll('.txn-segboundary-caption')].map((node) => node.textContent ?? ''),
        }))
        const header = read.header.match(/^(?:[\d,]+ of )?([\d,]+) turns?$/)
        if (!header) throw fail(`reading ${theme} positions`, `the header ${JSON.stringify(read.header)} names no turn total`)
        const total = integer(header[1])
        if (total !== index.totalConversationTurns) throw fail(`reading ${theme} positions`, `the header total ${total} is not the directory's ${index.totalConversationTurns}`)
        if (read.captions.length < 2) throw fail(`reading ${theme} positions`, `expected both boundary captions, read ${JSON.stringify(read.captions)}`)
        const ranges = read.captions.map((caption) => {
          const range = caption.match(RANGE)
          if (!range) throw fail(`reading ${theme} positions`, `the caption ${JSON.stringify(caption)} names no conversation turn range`)
          const lower = integer(range[1])
          const upper = integer(range[2] ?? range[1])
          if (upper > total) throw fail(`reading ${theme} positions`, `${JSON.stringify(caption)} ends at ${upper}, past the header total ${total}`)
          if (integer(range[3]) !== total) throw fail(`reading ${theme} positions`, `${JSON.stringify(caption)} is stated against ${range[3]}, not the header total ${total}`)
          return { caption, lower, upper }
        })
        return { header: read.header, total, ranges }
      }
      // The outline rails collapse and reopen with a transition. A shot taken
      // part way shows a rail at a width no reader sees, so each shot waits
      // until no finite animation runs under the viewer and every rail keeps
      // the same box across two animation frames.
      const railWidths = {}
      const settle = async (step) => {
        try {
          await page.waitForFunction(() => new Promise((done) => {
            const root = document.querySelector('.txn-app')
            if (!root) return done(false)
            const boxes = () => JSON.stringify([...root.querySelectorAll('.txn-rail, .txn-rail-strip')].map((rail) => {
              const box = rail.getBoundingClientRect()
              return [box.left, box.width, box.height]
            }))
            const moving = () => root.getAnimations({ subtree: true }).some((animation) => (
              animation.playState === 'running' && Number.isFinite(animation.effect?.getComputedTiming().endTime)
            ))
            const first = boxes()
            requestAnimationFrame(() => requestAnimationFrame(() => done(!moving() && boxes() === first)))
          }), { timeout: 10_000, polling: 100 })
        } catch (error) {
          throw fail(`capturing ${theme} ${step}`, `the outline rails did not settle within 10s: ${error.message}`)
        }
        const outside = await viewer.evaluate((node) => {
          const box = node.getBoundingClientRect()
          return box.left < 0 || box.top < 0 || box.right > innerWidth || box.bottom > innerHeight
        })
        if (outside) throw fail(`capturing ${theme} ${step}`, 'the transcript viewer does not fit the viewport, so a shot would lay the page out again')
        railWidths[step] = await page.$$eval('.txn-app .txn-rail, .txn-app .txn-rail-strip', (rails) => rails.map((rail) => ({
          rail: rail.className,
          width: Math.round(rail.getBoundingClientRect().width),
        })))
      }
      const edge = async (name) => {
        const selector = `[data-segment-boundary="${name}"]`
        await page.$eval(selector, (boundary) => boundary.scrollIntoView({ block: 'center', inline: 'nearest' }))
        await pause(200)
        const visible = await page.$eval(selector, (boundary) => {
          const rect = boundary.getBoundingClientRect()
          const label = boundary.querySelector('.txn-segboundary-caption')
          const caption = label?.textContent ?? ''
          const button = boundary.querySelector('button')
          const unobscured = (element) => {
            if (!element) return false
            const box = element.getBoundingClientRect()
            const hit = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2)
            return !!hit && (element === hit || element.contains(hit))
          }
          return rect.top >= 0 && rect.bottom <= innerHeight && rect.width > 0 && rect.height > 0
            && /^turns? [\d,]+(?:–[\d,]+)? of [\d,]+$/.test(caption)
            && unobscured(label) && unobscured(button)
        })
        if (!visible) throw fail(`capturing ${theme} ${name}`, 'the boundary, its conversation turn range, or the load control is not visible')
        positions = await assertPositions()
        await settle(name)
        const path = resolve(OUT, `transcript-segments-${theme}-${name}.png`)
        await viewer.screenshot({ path, ...IN_VIEWPORT })
        return path
      }
      const older = await edge('older')
      const newer = await edge('newer')
      // The seed's lone tool-only replies each form an action group of their
      // own, so the run under test is found by its content, not by position.
      let actionGroup = null
      for (const group of await page.$$('.txn-modelactions')) {
        const head = await group.$('.txn-modelactions-head')
        if (head && await head.evaluate((node) => node.getAttribute('aria-expanded')) !== 'true') await head.click()
        if (await group.evaluate((node) => (node.textContent ?? '').includes('segment-action-32'))) {
          actionGroup = group
          break
        }
      }
      if (!actionGroup) throw fail(`capturing ${theme} actions`, 'the action run that crosses a segment boundary did not mount')
      const evidence = await actionGroup.evaluate((group) => ({
        text: group.textContent,
        rows: group.querySelectorAll('.txn-toolcall').length,
      }))
      if (evidence.rows !== 11 || !evidence.text.includes('segment-action-32') || !evidence.text.includes('segment-action-52')) {
        throw fail(`capturing ${theme} actions`, `the mounted run did not group both segments: ${JSON.stringify(evidence)}`)
      }
      await actionGroup.evaluate((group) => group.scrollIntoView({ block: 'center', inline: 'nearest' }))
      await pause(200)
      await settle('actions')
      const actions = resolve(OUT, `transcript-segments-${theme}-actions.png`)
      await viewer.screenshot({ path: actions, ...IN_VIEWPORT })
      const path = resolve(OUT, `transcript-segments-${theme}.png`)
      await run('magick', [older, newer, '-append', path])
      const boundaries = await page.$$eval('[data-segment-boundary]', (nodes) => nodes.map((node) => node.getAttribute('data-segment-boundary')))
      // The home session list, served by the same binary from the same store.
      // The session holds more folded turns than conversation turns, and the
      // list prints the conversation count.
      await page.goto(`${ORIGIN}/`, { waitUntil: 'networkidle0', timeout: 60_000 })
      const listTheme = await page.$eval('html', (html) => html.getAttribute('data-theme'))
      if (listTheme !== theme) throw fail(`confirming ${theme} on the session list`, `data-theme is ${listTheme}`)
      let listed
      try {
        const handle = await page.waitForFunction((want) => {
          for (const row of document.querySelectorAll('tbody tr')) {
            const cells = [...row.querySelectorAll('td')].map((cell) => cell.textContent?.trim() ?? '')
            if (cells.includes(want)) return cells
          }
          return null
        }, { timeout: 30_000 }, String(seeded.conversationTurns))
        listed = await handle.jsonValue()
      } catch (error) {
        const text = await page.evaluate(() => document.body.innerText.slice(0, 1200))
        throw fail(`reading the ${theme} session list`, `no row prints ${seeded.conversationTurns} turns: ${error.message}; page=${JSON.stringify(text)}`)
      }
      if (listed.includes(String(seeded.turns))) throw fail(`reading the ${theme} session list`, `the row also prints the folded turn count ${seeded.turns}: ${JSON.stringify(listed)}`)
      const list = resolve(OUT, `session-list-${theme}.png`)
      await page.screenshot({ path: list })
      captures.push({ theme, path, older, newer, actions, list, listed, positions, railWidths, actionSegments: actionSegments.map((segment) => segment.index), boundaries, turn: middle.firstTurn, segments: index.totalSegments })
      await page.close()
    }
  } finally {
    await browser.close()
  }
  const head = (await run('git', ['rev-parse', 'HEAD'])).stdout.trim()
  const dirty = (await run('git', ['status', '--porcelain', '--untracked-files=no'])).stdout.trim() !== ''
  const report = { origin: ORIGIN, head, dirty, provenance, positionChunks, directory: { totalTurns: index.totalTurns, totalConversationTurns: index.totalConversationTurns, segments: index.segments }, seeded: { sessionId: seeded.sessionId, projectHash: seeded.projectHash }, captures }
  writeFileSync(resolve(OUT, 'report.json'), `${JSON.stringify(report, null, 2)}\n`)
  console.log(JSON.stringify(report, null, 2))
} catch (error) {
  console.error(serverLog)
  throw error
} finally {
  server.kill('SIGTERM')
}
