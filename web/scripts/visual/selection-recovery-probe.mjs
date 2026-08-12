/* Real-build computed-style probe for the shared selection-recovery panel.
   It reuses the visual harness browser, deterministic settings, themes, and mock-store definition.
   No screenshots are written.

   env: CHROME_PATH (required), PEASANT_RECOVERY_PORT (default 8707),
        PUPPETEER_CORE (optional explicit puppeteer-core module path). */
import { spawn } from 'node:child_process'
import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import YAML from 'yaml'
import { applyDeterminism } from './determinism.mjs'
import { SMOKE_MOCKS, SMOKE_THEMES } from './smoke-surfaces.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '../../..')
const WEB = join(REPO, 'web')
const BIN = join(REPO, 'bin/peasant')
const SOURCE = join(WEB, 'src/components/picker/SelectionRecoveryPanel.tsx')
const FIXTURE = join(WEB, 'src/components/picker/testdata/project_viewer_states.yaml')
const CHUNKS = join(WEB, 'out/_next/static/chunks')
const PORT = process.env.PEASANT_RECOVERY_PORT || '8707'
const ORIGIN = `http://localhost:${PORT}`
const CHROME = process.env.CHROME_PATH
const MARKER = 'Your saved selection hides all projects.'
const PANEL = '[role="status"][aria-label="project selection recovery"]'
const ROUTES = [{ id: 'home', path: '/' }, { id: 'map', path: '/map/' }]
const pause = (ms) => new Promise((done) => setTimeout(done, ms))

function requireProbe(condition, what, where, evidence, fix) {
  if (condition) return
  throw new Error(
    `Selection-recovery visual probe failed.\n` +
    `  What: ${what}\n` +
    `  Why: ${evidence}\n` +
    `  Where: ${where}\n` +
    `  When: probing the mounted all-hidden state on the real production build\n` +
    `  Means: the result cannot prove the recovery panel that users receive\n` +
    `  Fix: ${fix}`,
  )
}

function filesBelow(directory) {
  if (!existsSync(directory)) return []
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name)
    return entry.isDirectory() ? filesBelow(path) : [path]
  })
}

requireProbe(!!CHROME, 'CHROME_PATH is unset', 'probe startup', 'Puppeteer has no browser executable', 'set CHROME_PATH to google-chrome or chromium and rerun the probe')
requireProbe(existsSync(BIN) && existsSync(CHUNKS), 'production artifacts are absent', `${relative(REPO, BIN)} and ${relative(REPO, CHUNKS)}`, 'make build has not produced both artifacts', 'run make build in this worktree')

const markerBytes = Buffer.from(MARKER)
const markerChunk = filesBelow(CHUNKS).find((path) => path.endsWith('.js') && readFileSync(path).includes(markerBytes))
const sourceMtime = statSync(SOURCE).mtimeMs
requireProbe(
  !!markerChunk && readFileSync(BIN).includes(markerBytes) && statSync(markerChunk).mtimeMs >= sourceMtime && statSync(BIN).mtimeMs >= sourceMtime,
  'the exported chunk or embedded binary is stale',
  'build-provenance gate before browser launch',
  `chunk=${markerChunk ? relative(REPO, markerChunk) : 'missing'}, binaryMarker=${readFileSync(BIN).includes(markerBytes)}`,
  'run make build in this exact worktree and rerun pnpm probe:selection-recovery',
)
console.log(`[recovery-probe] provenance source=${relative(REPO, SOURCE)} chunk=${relative(REPO, markerChunk)} binaryMarker=true`)

const fixtureDocument = YAML.parseDocument(readFileSync(FIXTURE, 'utf8'), { strict: true, uniqueKeys: true })
requireProbe(fixtureDocument.errors.length === 0, 'the strict fixture cannot be parsed', relative(REPO, FIXTURE), fixtureDocument.errors.map((error) => error.message).join('; '), 'repair the fixture and rerun its focused tests')
const fixtureCase = fixtureDocument.toJS()?.cases?.find((candidate) => candidate?.name === 'all hidden by saved selection')
const selection = fixtureCase?.summary?.selection
requireProbe(
  Array.isArray(fixtureCase?.summary?.projects) && fixtureCase.summary.projects.length === 0 && selection?.active === true && Number.isInteger(selection.hiddenProjects) && Number.isInteger(selection.hiddenSessions) && selection.hiddenProjects + selection.hiddenSessions > 0 && Array.isArray(fixtureCase?.forbiddenIdentities),
  'the all-hidden fixture no longer mounts recovery',
  relative(REPO, FIXTURE),
  JSON.stringify(fixtureCase),
  'restore zero projects, active selection, and positive aggregate hidden counts',
)
const projectWord = selection.hiddenProjects === 1 ? 'project' : 'projects'
const sessionWord = selection.hiddenSessions === 1 ? 'session' : 'sessions'
const acceptedLines = [
  MARKER,
  `Peasant hides ${selection.hiddenProjects.toLocaleString()} ${projectWord} and ${selection.hiddenSessions.toLocaleString()} ${sessionWord}.`,
  'The data stays ingested and indexed.',
  'The web viewer does not list it.',
  'It is not available for a future push.',
  'Peasant did not delete data.',
  'To change the selection, run peasant kickstart.',
]
const expectedCounts = [`${selection.hiddenProjects.toLocaleString()} ${projectWord}`, `${selection.hiddenSessions.toLocaleString()} ${sessionWord}`]

const server = spawn(BIN, ['web', 'start', '--port', PORT, '--foreground', '--no-browser', `--mock-data-store=${SMOKE_MOCKS}`], { cwd: REPO, stdio: ['ignore', 'ignore', 'pipe'] })
let browser
let serverDown = false
let serverError = ''
server.stderr.on('data', (data) => { serverError += data.toString() })
server.on('exit', () => { serverDown = true })
const teardown = async () => {
  try { if (browser) await browser.close() } catch {}
  try { if (!serverDown) server.kill('SIGTERM') } catch {}
}

try {
  let healthy = false
  for (let attempt = 0; attempt < 40 && !healthy; attempt++) {
    healthy = (await fetch(`${ORIGIN}/api/v1/health`).catch(() => null))?.status === 200
    if (!healthy) await pause(250)
  }
  await pause(100)
  requireProbe(healthy && !serverDown, 'the real server did not become healthy', ORIGIN, serverError.trim() || `port ${PORT} did not answer from the spawned binary`, `free port ${PORT} or set PEASANT_RECOVERY_PORT to a free port`)
  const servedChunkPath = `/${relative(join(WEB, 'out'), markerChunk).split('\\').join('/')}`
  const servedChunk = await fetch(`${ORIGIN}${servedChunkPath}`).catch(() => null)
  const servedChunkBody = servedChunk?.status === 200 ? await servedChunk.text() : ''
  requireProbe(servedChunk?.status === 200 && servedChunkBody.includes(MARKER), 'the running server does not serve the verified recovery chunk', servedChunkPath, `http=${servedChunk?.status || 0}, marker=${servedChunkBody.includes(MARKER)}`, 'stop stale servers, rebuild this worktree, and rerun the probe on a free port')
  console.log(`[recovery-probe] served provenance origin=${ORIGIN} chunk=${servedChunkPath} marker=true`)

  const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default
  browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: { width: 1280, height: 900, deviceScaleFactor: 1 } })
  const reports = []

  for (const theme of SMOKE_THEMES) {
    const page = await browser.newPage()
    const diagnostics = []
    page.on('console', (message) => {
      const location = typeof message.location === 'function' ? message.location()?.url || '' : ''
      if (message.type() === 'error' && !/favicon/i.test(`${message.text()} ${location}`)) diagnostics.push(message.text())
    })
    page.on('pageerror', (error) => diagnostics.push(`pageerr: ${error.message}`))
    await applyDeterminism(page)
    await page.evaluateOnNewDocument((value) => { try { localStorage.setItem('peasant-theme', value) } catch {} }, theme)
    await page.setRequestInterception(true)
    let summaryRequests = 0
    page.on('request', (request) => {
      const url = new URL(request.url())
      if (url.origin === ORIGIN && url.pathname === '/api/v1/projects/summary') {
        summaryRequests++
        void request.respond({ status: 200, contentType: 'application/json', body: JSON.stringify(fixtureCase.summary) }).catch((error) => diagnostics.push(error.message))
      } else {
        void request.continue().catch((error) => diagnostics.push(error.message))
      }
    })

    try {
      for (const route of ROUTES) {
        summaryRequests = 0
        const response = await page.goto(`${ORIGIN}${route.path}`, { waitUntil: 'domcontentloaded' })
        const mounted = await page.waitForSelector(PANEL, { visible: true, timeout: 15000 }).catch(() => null)
        requireProbe(response?.status() === 200 && !!mounted && summaryRequests > 0 && diagnostics.length === 0, `${theme}/${route.id} did not mount cleanly`, `${ORIGIN}${route.path}`, `http=${response?.status()}, panel=${!!mounted}, requests=${summaryRequests}, diagnostics=${JSON.stringify(diagnostics)}`, 'fix the real route or summary request, rebuild, and rerun')
        await page.evaluate(async () => { try { await Promise.all([document.fonts.load('400 16px "Atkinson Hyperlegible"'), document.fonts.load('400 16px "Atkinson Hyperlegible Mono"')]) } catch {} ; await document.fonts.ready })

        const report = await page.evaluate(({ panelSelector, lines, counts, identities, activeTheme }) => {
          const panel = document.querySelector(panelSelector)
          const normalize = (value) => (value || '').replace(/\s+/g, ' ').trim()
          const visible = (element) => {
            if (!element) return false
            const style = getComputedStyle(element)
            const box = element.getBoundingClientRect()
            return box.width > 0 && box.height > 0 && style.display !== 'none' && style.visibility !== 'hidden' && Number(style.opacity) > 0
          }
          const descendants = panel ? [...panel.querySelectorAll('*')] : []
          const exact = (text) => descendants.find((element) => normalize(element.textContent) === text && visible(element))
          const heading = descendants.find((element) => /^H[1-6]$/.test(element.tagName) && normalize(element.textContent) === lines[0])
          const body = exact('The data stays ingested and indexed.')
          const count = exact(counts[0])
          const copy = panel?.querySelector('button[aria-label="copy command to clipboard"]') || null
          if (copy) copy.focus({ focusVisible: true })
          let surface = heading?.parentElement || null
          while (surface && surface !== panel) {
            const style = getComputedStyle(surface)
            if (parseFloat(style.borderTopWidth) >= 1 && style.backgroundColor !== 'rgba(0, 0, 0, 0)') break
            surface = surface.parentElement
          }
          const tokenProbe = document.createElement('span')
          tokenProbe.style.backgroundColor = 'var(--surface)'
          document.body.append(tokenProbe)
          const surfaceToken = getComputedStyle(tokenProbe).backgroundColor
          tokenProbe.remove()
          const panelText = normalize(panel?.textContent)
          const root = document.documentElement
          const bodyStyle = body ? getComputedStyle(body) : null
          const countStyle = count ? getComputedStyle(count) : null
          const headingStyle = heading ? getComputedStyle(heading) : null
          const surfaceStyle = surface ? getComputedStyle(surface) : null
          const copyStyle = copy ? getComputedStyle(copy) : null
          const copyBox = copy?.getBoundingClientRect() || null
          return {
            statusCount: document.querySelectorAll(panelSelector).length,
            visible: visible(panel),
            missing: lines.filter((line) => !exact(line)),
            missingCounts: counts.filter((value) => !exact(value)),
            leaks: identities.filter((identity) => panelText.includes(identity)),
            wrongGuidance: panelText.includes('peasant ingest') || panelText.includes('A saved project selection is limiting'),
            theme: [root.getAttribute('data-theme'), root.getAttribute('data-tb-theme')],
            themeMatches: root.getAttribute('data-theme') === activeTheme && root.getAttribute('data-tb-theme') === activeTheme,
            fontsLoaded: document.fonts.check('400 16px "Atkinson Hyperlegible"') && document.fonts.check('400 16px "Atkinson Hyperlegible Mono"'),
            bodyFont: bodyStyle?.fontFamily || '',
            bodyColor: bodyStyle?.color || '',
            bodyLeading: bodyStyle ? parseFloat(bodyStyle.lineHeight) / parseFloat(bodyStyle.fontSize) : 0,
            headingFont: headingStyle?.fontFamily || '',
            countFont: countStyle?.fontFamily || '',
            countVariant: countStyle?.fontVariantNumeric || '',
            background: surfaceStyle?.backgroundColor || '',
            surfaceToken,
            border: [surfaceStyle?.borderTopStyle || '', surfaceStyle ? parseFloat(surfaceStyle.borderTopWidth) : 0],
            copy: [visible(copy), copyBox?.width || 0, copyBox?.height || 0, copyStyle?.fontFamily || '', copyStyle?.outlineStyle || '', copyStyle ? parseFloat(copyStyle.outlineWidth) : 0],
          }
        }, { panelSelector: PANEL, lines: acceptedLines, counts: expectedCounts, identities: fixtureCase.forbiddenIdentities, activeTheme: theme })

        const fontKey = (value) => value.replace(/[^a-z]/gi, '').toLowerCase()
        const bodyFont = fontKey(report.bodyFont)
        const headingFont = fontKey(report.headingFont)
        const countFont = fontKey(report.countFont)
        const copyFont = fontKey(report.copy[3])
        const valid = report.statusCount === 1 && report.visible && report.missing.length === 0 && report.missingCounts.length === 0 && report.leaks.length === 0 && !report.wrongGuidance && report.themeMatches && report.fontsLoaded && bodyFont.includes('atkinsonhyperlegible') && !bodyFont.includes('mono') && headingFont.includes('atkinsonhyperlegiblemono') && countFont.includes('atkinsonhyperlegiblemono') && /tabular-nums/i.test(report.countVariant) && report.bodyLeading >= 1.5 && report.background === report.surfaceToken && report.border[0] === 'solid' && report.border[1] >= 1 && report.copy[0] && report.copy[1] >= 24 && report.copy[2] >= 24 && copyFont.includes('atkinsonhyperlegiblemono') && report.copy[4] !== 'none' && report.copy[5] >= 2
        requireProbe(valid, `${theme}/${route.id} computed styles diverged`, PANEL, JSON.stringify(report), 'restore canonical Fairtrade panel styling or mounted visibility, rebuild, and rerun')
        const styleKey = JSON.stringify({ background: report.background, color: report.bodyColor, bodyFont: report.bodyFont, headingFont: report.headingFont, countFont: report.countFont, countVariant: report.countVariant, border: report.border, copyFont: report.copy[3] })
        reports.push({ theme, route: route.id, background: report.background, color: report.bodyColor, styleKey })
        console.log(`[recovery-probe] ${theme}/${route.id} mounted=true fonts=true tabular=true focus=true background=${report.background} → OK`)
      }
    } finally {
      await page.close()
    }
  }

  for (const theme of SMOKE_THEMES) {
    const themed = reports.filter((report) => report.theme === theme)
    requireProbe(themed.length === ROUTES.length && themed.every((report) => report.styleKey === themed[0].styleKey), `${theme} Home/Map styles differ`, 'shared-component parity check', JSON.stringify(themed), 'remove consumer-specific overrides and rerun')
  }
  const dark = reports.find((report) => report.theme === 'dark')
  const light = reports.find((report) => report.theme === 'light')
  requireProbe(dark && light && dark.background !== light.background && dark.color !== light.color, 'dark and light styles are not distinct', 'cross-theme comparison', JSON.stringify({ dark, light }), 'restore Fairtrade theme tokens and rerun')
  console.log(`[recovery-probe] OK ${reports.length} real-build checks; Home/Map match; themes differ; screenshots=none`)
} catch (error) {
  console.error(error?.stack || error?.message || String(error))
  process.exitCode = 1
} finally {
  await teardown()
}
