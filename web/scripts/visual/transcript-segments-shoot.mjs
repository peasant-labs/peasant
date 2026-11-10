/* Mounted transcript-segment capture.

   Boots this worktree's peasant binary, which embeds this branch's web export,
   against a throwaway store holding a synthetic multi-segment transcript.
   Opens the production transcript route on a middle turn so both load-more
   boundaries are on screen, in both themes. assertServedBuild runs before any
   capture. Transient PNGs stay under review-capture/ and are not committed.
*/
import { spawn } from 'node:child_process'
import { mkdirSync, writeFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { assertServedBuild } from './served-build.mjs'

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

async function waitForHealth(child) {
  const started = Date.now()
  while (Date.now() - started < 30_000) {
    if (child.exitCode != null) throw fail('starting peasant', `the server exited ${child.exitCode}`)
    try {
      const response = await fetch(`${ORIGIN}/api/v1/health`)
      if (response.ok) return
    } catch { /* not up yet */ }
    await pause(250)
  }
  throw fail('starting peasant', 'health did not answer within 30s')
}

async function directory(sessionId) {
  const response = await fetch(`${ORIGIN}/api/v1/sessions/${sessionId}/transcript/segments`)
  const body = await response.json()
  if (!response.ok) throw fail('reading the directory', `${response.status} ${JSON.stringify(body)}`)
  return body
}

mkdirSync(OUT, { recursive: true })
const seeded = JSON.parse((await run('go', ['run', './web/scripts/visual/seed-transcript-store.go'])).stdout)
const server = spawn(BIN, [
  'web', 'start', '--foreground', '--no-browser',
  '--port', PORT,
  '--data-dir', seeded.dataDir,
  '--config-dir', seeded.dataDir,
  '--state-dir', seeded.dataDir,
], { cwd: REPO, stdio: ['ignore', 'pipe', 'pipe'] })
let serverLog = ''
server.stdout.on('data', (chunk) => { serverLog += chunk })
server.stderr.on('data', (chunk) => { serverLog += chunk })
try {
  await waitForHealth(server)
  const provenance = await assertServedBuild({ origin: ORIGIN, markers: [MARKER, 'data-segment-boundaries', 'data-mounted-turn-bound'], bin: BIN, webRoot: WEB })
  const hostChunk = (await import('node:fs')).readdirSync(resolve(WEB, 'out/_next/static/chunks/app/projects/[[...segments]]')).find((name) => name.startsWith('page-'))
  if (!hostChunk) throw fail('verifying build provenance', 'the projects page chunk is missing from web/out')
  const hostPath = `/_next/static/chunks/app/projects/[[...segments]]/${hostChunk}`
  const hostBody = await (await fetch(`${ORIGIN}${hostPath}`)).text()
  if (!hostBody.includes(HOST_MARKER)) throw fail('verifying the served host chunk', `${hostPath} does not carry ${HOST_MARKER}`)
  const index = await directory(seeded.sessionId)
  if (index.totalSegments < 3) throw fail('checking the seeded cut', `only ${index.totalSegments} segments; both boundaries need a middle segment with unloaded extents on both sides`)
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
      const edge = async (name, top) => {
        await page.evaluate((scrollTop) => {
          const stream = document.querySelector('.txn-stream')
          if (!stream) return
          stream.scrollTop = scrollTop === 'end' ? stream.scrollHeight : 0
        }, top)
        await pause(200)
        const path = resolve(OUT, `transcript-segments-${theme}-${name}.png`)
        await viewer.screenshot({ path })
        return path
      }
      const older = await edge('older', 'start')
      const newer = await edge('newer', 'end')
      const path = resolve(OUT, `transcript-segments-${theme}.png`)
      await run('magick', [older, newer, '-append', path])
      const boundaries = await page.$$eval('[data-segment-boundary]', (nodes) => nodes.map((node) => node.getAttribute('data-segment-boundary')))
      captures.push({ theme, path, boundaries, turn: middle.firstTurn, segments: index.totalSegments })
      await page.close()
    }
  } finally {
    await browser.close()
  }
  const report = { origin: ORIGIN, provenance, seeded: { sessionId: seeded.sessionId, projectHash: seeded.projectHash }, captures }
  writeFileSync(resolve(OUT, 'report.json'), `${JSON.stringify(report, null, 2)}\n`)
  console.log(JSON.stringify(report, null, 2))
} catch (error) {
  console.error(serverLog)
  throw error
} finally {
  server.kill('SIGTERM')
}
