/* Settings page evidence (/settings) on the real built binary.

   It builds a throwaway peasant world under the OS temp directory: a HOME with three git
   repositories and a Claude Code session recorded in each, a config.yaml, the auto-publish rules in
   hooks.yaml, and a stored Village sign-in whose Village is a small local double that lists two
   collectives. It starts bin/peasant on that world, ingests the sessions through the real route, and
   shoots the mounted page in both themes:

     default         the whole page as it opens (desktop, and 390px wide)
     pending         a switch whose write is held in flight
     failed          a text field the server refuses (re-import after = -5): value restored, reason shown
     tagged          the village group: a row tagged "not in peasant config"
     install         the auto-publish group offering "install in 3 repositories", before any call
     install-refusal the same action after a foreign hook appeared in one repository: two installed,
                     one blocked with its remedy

   At 390px it also opens every group and fails if the page then scrolls sideways.

   Every write and install is real. Before each theme the world's hooks are reset, so both themes
   show the same states. The temp root lives under os.tmpdir(), not under a user's home or checkout,
   so no personal path reaches a capture; it is removed at the end (KEEP=1 keeps it).

   Build provenance is checked before the browser starts: the settings copy marker must be in a
   web/out chunk no older than the page's sources, in bin/peasant (no older than that chunk), and in
   the chunk the running server serves.

   Run (after `make build` in this worktree):
     CHROME_PATH=/path/to/chrome node scripts/visual/settings-shoot.mjs [out-dir]

   Env: CHROME_PATH (required), PEASANT_BIN (default ../bin/peasant), PEASANT_SETTINGS_PORT
   (default 8795), PUPPETEER_CORE, KEEP. */
import { spawn, execFileSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs'
import http from 'node:http'
import { tmpdir } from 'node:os'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { applyDeterminism } from './determinism.mjs'
import { SMOKE_THEMES } from './smoke-surfaces.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const WEB = join(HERE, '../..')
const REPO = join(WEB, '..')
const CHROME = process.env.CHROME_PATH
const BIN = process.env.PEASANT_BIN || join(REPO, 'bin/peasant')
const PORT = process.env.PEASANT_SETTINGS_PORT || '8795'
const ORIGIN = `http://localhost:${PORT}`
const OUT = process.argv[2] || join(tmpdir(), 'peasant-settings-shots')
// Copy only the settings page carries.
const MARKER = 'these folders and repositories publish redacted on their own'
const SOURCES = [join(WEB, 'src/app/settings'), join(WEB, 'src/components/settings'), join(WEB, 'src/lib/settings'), join(WEB, 'src/lib/api/settings.ts')]
const COLLECTIVES = [
  { id: '3f9c1a2b-0000-4000-8000-000000000001', name: 'Acme Platform' },
  { id: '3f9c1a2b-0000-4000-8000-000000000002', name: 'Acme Company' },
]
const REPOS = ['api', 'web', 'worker']

const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
function fail(what, fix) {
  console.error(`ERROR [settings-shoot.mjs] ${what}\n  Fix: ${fix}`)
  process.exit(1)
}
if (!CHROME) fail('CHROME_PATH is unset.', 'set it to a Chrome or Chromium binary.')

function files(path) {
  const stat = statSync(path)
  if (!stat.isDirectory()) return [path]
  return readdirSync(path).flatMap((name) => files(join(path, name)))
}

/* ---- build provenance, before anything starts ---- */
const chunks = files(join(WEB, 'out/_next/static/chunks')).filter((path) => path.endsWith('.js'))
const markerChunk = chunks.find((path) => readFileSync(path, 'utf8').includes(MARKER))
if (!markerChunk) fail(`no web/out chunk carries the settings marker "${MARKER}".`, 'run make build in this worktree.')
const newestSource = Math.max(...SOURCES.flatMap(files).map((path) => statSync(path).mtimeMs))
if (statSync(markerChunk).mtimeMs < newestSource) fail(`${relative(REPO, markerChunk)} is older than the settings sources.`, 'run make build again.')
const binary = readFileSync(BIN)
if (!binary.includes(Buffer.from(MARKER))) fail(`${relative(REPO, BIN)} does not embed the settings marker.`, 'run make build in this worktree.')
if (statSync(BIN).mtimeMs < statSync(markerChunk).mtimeMs) fail(`${relative(REPO, BIN)} is older than web/out.`, 'run make build again.')
const binarySha = createHash('sha256').update(binary).digest('hex')
console.log(`[settings-shoot] provenance chunk=${relative(REPO, markerChunk)} binary sha256=${binarySha.slice(0, 16)}…`)

/* ---- the world ---- */
const ROOT = mkdtempSync(join(tmpdir(), 'peasant-settings-'))
const HOME = join(ROOT, 'home')
const CONFIG = join(ROOT, 'config')
const env = { ...process.env, HOME, XDG_CONFIG_HOME: CONFIG, XDG_DATA_HOME: join(ROOT, 'data'), XDG_STATE_HOME: join(ROOT, 'state'), GIT_CONFIG_NOSYSTEM: '1' }
const git = (dir, ...args) => execFileSync('git', ['-C', dir, ...args], { env, stdio: 'pipe' })
const repoPath = (name) => join(HOME, 'work/acme', name)

for (const name of REPOS) {
  const dir = repoPath(name)
  mkdirSync(dir, { recursive: true })
  git(dir, 'init', '-q', '-b', 'main')
  git(dir, 'remote', 'add', 'origin', `git@github.com:acme/${name}.git`)
  git(dir, '-c', 'user.email=alice@example.com', '-c', 'user.name=alice', 'commit', '-q', '--allow-empty', '-m', 'start')
  const id = `00000000-0000-4000-8000-00000000000${REPOS.indexOf(name) + 1}`
  const project = join(HOME, '.claude/projects', dir.replace(/[/.]/g, '-'))
  mkdirSync(project, { recursive: true })
  const base = { sessionId: id, cwd: dir, gitBranch: 'main', version: '2.0.0', userType: 'external', isSidechain: false }
  writeFileSync(join(project, `${id}.jsonl`), [
    { ...base, type: 'user', uuid: `u-${name}`, parentUuid: null, timestamp: '2026-09-20T10:00:00.000Z', message: { role: 'user', content: `Tidy the ${name} README` } },
    { ...base, type: 'assistant', uuid: `a-${name}`, parentUuid: `u-${name}`, timestamp: '2026-09-20T10:00:05.000Z', message: { id: `msg-${name}`, type: 'message', role: 'assistant', model: 'claude-sonnet-4-5', content: [{ type: 'text', text: 'Done.' }], stop_reason: 'end_turn', usage: { input_tokens: 10, output_tokens: 5 } } },
  ].map((record) => JSON.stringify(record)).join('\n') + '\n')
}

const village = http.createServer((req, res) => {
  const group = (collective) => ({
    ...collective, acceptance_mode: 'open', created_at: '2026-01-01T00:00:00Z', created_by: collective.id, data_access: 'members_only', description: null,
    display_members: true, linked_github_org: 'acme', member_count: 6, member_since: '2026-01-01T00:00:00Z', role: 'member', transcript_count: 12,
    transcript_deletion_policy: 'user_choice', updated_at: '2026-01-01T00:00:00Z',
  })
  res.setHeader('Content-Type', 'application/json')
  if (req.url === '/api/v1/groups') return res.end(JSON.stringify(COLLECTIVES.map(group)))
  if (/^\/api\/v1\/groups\/[^/]+\/repositories$/.test(req.url)) return res.end(JSON.stringify({ repositories: [] }))
  res.statusCode = 404
  res.end(JSON.stringify({ error: 'not in the double' }))
})
await new Promise((resolve) => village.listen(0, 'localhost', resolve))
const villageURL = `http://localhost:${village.address().port}`

const peasantConfig = join(CONFIG, 'peasant')
mkdirSync(peasantConfig, { recursive: true })
writeFileSync(join(peasantConfig, 'config.yaml'), `version: 1
redaction:
  level: standard
  custom_patterns:
    - id: acme-internal-host
      category: project
      pattern: '[a-z0-9-]+\\.acme\\.internal'
      replacement: <INTERNAL_HOST>
village:
  url: ${villageURL}
push:
  sharePreference: share-later
output:
  basePath: ~/.local/share/peasant/peasant-sync
`)
writeFileSync(join(peasantConfig, 'hooks.yaml'), `version: 1
autoPublish:
  - id: acme-work
    kind: folder
    match: ~/work/acme/**
    events: [pre-push]
    collectives: [${COLLECTIVES[0].id}]
  - id: personal
    kind: folder
    match: ~/personal/**
    events: []
    collectives: []
`)
const credentials = join(peasantConfig, 'credentials.json')
writeFileSync(credentials, JSON.stringify({ api_key: 'pk_evidence', key_id: 'key-evidence', user_id: 'user-evidence', username: 'alice-dev', village_url: villageURL, linked_at: '2026-09-01T00:00:00Z' }))
chmodSync(credentials, 0o600)

/** Puts every repository's pre-push slot back to empty: no peasant hook, no foreign hook. */
function resetHooks() {
  for (const name of REPOS) rmSync(join(repoPath(name), '.git/hooks/pre-push'), { force: true })
}

/* ---- the server ---- */
const server = spawn(BIN, ['web', 'start', '--port', PORT, '--foreground', '--no-browser'], { cwd: ROOT, env, stdio: ['ignore', 'ignore', 'pipe'] })
let serverLog = ''
let serverDown = false
server.stderr.on('data', (data) => { serverLog += data.toString() })
server.on('exit', () => { serverDown = true })
let browser
const teardown = async () => {
  try { if (browser) await browser.close() } catch {}
  try { if (!serverDown) server.kill('SIGTERM') } catch {}
  village.close()
  if (process.env.KEEP !== '1') rmSync(ROOT, { recursive: true, force: true })
}
const abort = async (what, fix) => {
  await teardown()
  fail(what, fix)
}

try {
  let healthy = false
  for (let attempt = 0; attempt < 60 && !healthy && !serverDown; attempt++) {
    healthy = (await fetch(`${ORIGIN}/api/v1/health`).catch(() => null))?.status === 200
    if (!healthy) await pause(250)
  }
  if (!healthy) await abort(`bin/peasant did not answer on ${ORIGIN}: ${serverLog.trim().slice(-400)}`, `free port ${PORT} or set PEASANT_SETTINGS_PORT.`)

  const servedChunk = `/${relative(join(WEB, 'out'), markerChunk).split('\\').join('/')}`
  const served = await fetch(`${ORIGIN}${servedChunk}`).catch(() => null)
  if (served?.status !== 200 || !(await served.text()).includes(MARKER)) await abort(`the running server does not serve ${servedChunk} with the marker.`, 'stop stale servers and rerun on a free port.')
  console.log(`[settings-shoot] served provenance origin=${ORIGIN} chunk=${servedChunk} marker=true`)

  await fetch(`${ORIGIN}/api/v1/sync/ingest`, { method: 'POST', headers: { Origin: ORIGIN } })
  let recorded = 0
  for (let attempt = 0; attempt < 120 && recorded < REPOS.length; attempt++) {
    await pause(250)
    const status = await (await fetch(`${ORIGIN}/api/v1/sync/ingest/status`)).json()
    if (status.status === 'running') continue
    recorded = (await (await fetch(`${ORIGIN}/api/v1/sessions`)).json()).sessions?.length ?? 0
  }
  if (recorded < REPOS.length) await abort(`ingest recorded ${recorded} of ${REPOS.length} sessions.`, 'check the server log with KEEP=1.')

  mkdirSync(OUT, { recursive: true })
  const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default
  browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: { width: 1440, height: 900, deviceScaleFactor: 1 } })
  const shots = []
  const probes = []

  /** One element, captured with the page scrolled to the top in a viewport as tall as the page, so the fixed header stays at the top and clear of it. */
  const shootRegion = async (page, selector, path) => {
    const viewport = page.viewport()
    await page.evaluate(() => window.scrollTo(0, 0))
    const height = await page.evaluate(() => document.documentElement.scrollHeight)
    await page.setViewport({ ...viewport, height })
    const box = await page.$eval(selector, (element) => {
      const rect = element.getBoundingClientRect()
      return { x: rect.left, y: rect.top, width: rect.width, height: rect.height }
    })
    const pad = 16
    await page.screenshot({ path, clip: { x: Math.max(0, box.x - pad), y: Math.max(0, box.y - pad), width: box.width + pad * 2, height: box.height + pad * 2 } })
    await page.setViewport(viewport)
    shots.push(relative(OUT, path))
  }
  const statusOf = (page, key) => page.$eval(`[data-setting-keys~="${key}"]`, (row) => row.getAttribute('data-status'))
  const waitStatus = async (page, key, want) => {
    for (let attempt = 0; attempt < 80; attempt++) {
      if (await statusOf(page, key) === want) return
      await pause(100)
    }
    await abort(`${key} never reached ${want} (last ${await statusOf(page, key)}).`, 'rerun; if it repeats, the page did not settle the write.')
  }
  const open = async (theme, width = 1440) => {
    const page = await browser.newPage()
    await page.setViewport({ width, height: 900, deviceScaleFactor: 1 })
    const errors = []
    page.on('pageerror', (error) => errors.push(error.message))
    // The browser logs every non-2xx answer; the refused write is one on purpose. Script errors still count.
    page.on('console', (message) => { if (message.type() === 'error' && !/favicon|^Failed to load resource/i.test(message.text())) errors.push(message.text()) })
    await applyDeterminism(page)
    await page.evaluateOnNewDocument((value) => { try { localStorage.setItem('peasant-theme', value) } catch {} }, theme)
    await page.goto(`${ORIGIN}/settings/`, { waitUntil: 'networkidle0' })
    await page.waitForFunction(() => document.querySelector('[data-rule-id="acme-work"]')?.textContent.includes('Acme Platform'), { timeout: 15000 })
    await page.evaluate(() => document.fonts.ready)
    const attr = await page.$eval('html', (html) => html.getAttribute('data-theme'))
    if (attr !== theme) await abort(`the page opened in ${attr}, not ${theme}.`, 'check the theme store key.')
    return { page, errors }
  }

  for (const theme of SMOKE_THEMES) {
    resetHooks()
    const dir = join(OUT, theme)
    mkdirSync(dir, { recursive: true })

    const { page, errors } = await open(theme)
    await page.screenshot({ path: join(dir, 'default.png'), fullPage: true })
    shots.push(relative(OUT, join(dir, 'default.png')))
    probes.push({ theme, ...await page.evaluate(() => {
      const style = (sel) => { const el = document.querySelector(sel); return el ? getComputedStyle(el) : null }
      return {
        rowLabelFont: style('.srow-label')?.fontFamily,
        helpFont: style('.srow-help')?.fontFamily,
        helpSize: style('.srow-help')?.fontSize,
        noteSize: style('.stg-note')?.fontSize,
        installRadius: style('.stg-install')?.borderRadius,
        groupRadius: style('.srow-group')?.borderRadius,
        countNumerals: style('.srow-summary-count')?.fontVariantNumeric,
        installCountNumerals: style('.stg-install-title .tnum')?.fontVariantNumeric,
        tagText: document.querySelector('.srow-tag')?.textContent,
      }
    }) })
    await shootRegion(page, '[data-group="village"]', join(dir, 'tagged.png'))
    await shootRegion(page, '[data-group="auto-publish"]', join(dir, 'install.png'))

    // pending: hold one switch's write in flight, then let it land and put it back
    await page.setRequestInterception(true)
    let held = null
    page.on('request', (request) => {
      if (request.method() === 'PATCH' && held === null) { held = request; return }
      void request.continue()
    })
    await page.click('[data-setting-keys~="daemon.projectMode"] [role="switch"]')
    await waitStatus(page, 'daemon.projectMode', 'pending')
    await shootRegion(page, '[data-group="projects"]', join(dir, 'pending.png'))
    await held.continue()
    await waitStatus(page, 'daemon.projectMode', 'settled')
    await page.click('[data-setting-keys~="daemon.projectMode"] [role="switch"]')
    await waitStatus(page, 'daemon.projectMode', 'settled')

    // failed: the server refuses a negative re-import time; the value comes back with the reason
    await page.evaluate(() => { document.querySelector('[data-group="advanced"]').open = true })
    const row = '[data-setting-keys~="output.stalenessThresholdSec"]'
    await page.click(`${row} .srow-edit`)
    await page.$eval(`${row} input`, (input) => input.select())
    await page.type(`${row} input`, '-5')
    await page.click(`${row} .srow-save`)
    await waitStatus(page, 'output.stalenessThresholdSec', 'failed')
    const reason = await page.$eval(`${row} [role="alert"]`, (alert) => alert.textContent)
    const restored = await page.$eval(`${row} .srow-text-value`, (value) => value.textContent)
    if (!reason.includes('stalenessThresholdSec must be >= 0') || restored !== '60') await abort(`the refused write showed "${reason}" and "${restored}".`, 'the row must restore 60 and show the server reason.')
    await shootRegion(page, '[data-group="advanced"]', join(dir, 'failed.png'))

    // install with a refusal: a foreign hook appears in one repository after the page offered the install
    const before = await page.$eval('.stg-install .btn-primary', (button) => button.textContent.trim())
    if (before !== 'install in 3 repositories') await abort(`the install action reads "${before}".`, 'reset the hooks and rerun.')
    const foreign = join(repoPath('worker'), '.git/hooks/pre-push')
    writeFileSync(foreign, '#!/bin/sh\n# the team hook\nexit 0\n')
    chmodSync(foreign, 0o755)
    await page.click('.stg-install .btn-primary')
    await page.waitForSelector('[aria-label="install results"]', { timeout: 30000 })
    await page.evaluate(() => document.querySelector('.stg-install')?.scrollIntoView())
    await shootRegion(page, '[data-group="auto-publish"]', join(dir, 'install-refusal.png'))
    const results = await page.$eval('[aria-label="install results"]', (list) => list.textContent)
    if (!results.includes('hook installed') || !results.includes('blocked') || !results.includes('Peasant did not write')) await abort(`the install results read "${results.slice(0, 200)}".`, 'two repositories must install and the foreign one must show its remedy.')
    if (errors.length > 0) await abort(`the page logged errors: ${errors.join(' | ')}`, 'fix the page errors.')
    await page.close()

    // the whole page at phone width
    resetHooks()
    const narrow = await open(theme, 390)
    await narrow.page.screenshot({ path: join(dir, 'default-390.png'), fullPage: true })
    shots.push(relative(OUT, join(dir, 'default-390.png')))
    const overflow = await narrow.page.evaluate(() => {
      const closed = document.documentElement.scrollWidth - window.innerWidth
      document.querySelectorAll('details').forEach((details) => { details.open = true })
      return Math.max(closed, document.documentElement.scrollWidth - window.innerWidth)
    })
    if (overflow > 0) await abort(`the page scrolls sideways by ${overflow}px at 390px, with its groups closed or open.`, 'fix the overflowing element.')
    await narrow.page.close()
  }

  for (const probe of probes) {
    const problems = []
    if (!/atkinson/i.test(probe.rowLabelFont) || !/mono/i.test(probe.rowLabelFont)) problems.push(`row label font ${probe.rowLabelFont}`)
    if (!/atkinson/i.test(probe.helpFont) || /mono/i.test(probe.helpFont)) problems.push(`help font ${probe.helpFont}`)
    if (parseFloat(probe.helpSize) < 16 || parseFloat(probe.noteSize) < 16) problems.push(`body below 16px (${probe.helpSize}, ${probe.noteSize})`)
    if (probe.installRadius !== '0px' || probe.groupRadius !== '0px') problems.push(`radius ${probe.installRadius} / ${probe.groupRadius}`)
    if (!probe.countNumerals.includes('tabular-nums') || !probe.installCountNumerals.includes('tabular-nums')) problems.push('counts without tabular numerals')
    if (probe.tagText !== 'not in peasant config') problems.push(`tag reads ${probe.tagText}`)
    if (problems.length > 0) await abort(`computed styles in ${probe.theme}: ${problems.join('; ')}`, 'fix the page styles.')
  }
  writeFileSync(join(OUT, 'evidence.json'), JSON.stringify({ binarySha256: binarySha, chunk: relative(REPO, markerChunk), marker: MARKER, shots, probes }, null, 2))
  console.log(`[settings-shoot] OK ${shots.length} captures in ${OUT}; computed styles hold in ${probes.map((probe) => probe.theme).join(' and ')}.`)
} finally {
  await teardown()
}
