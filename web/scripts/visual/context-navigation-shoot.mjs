/* Mounted current-parent links + retained earlier history, captured from the REAL production route of
   the REAL built binary (never the dev fixture route, never a storybook/story-only surface).

   The production path under test: `/projects/{project}/{session}/` -> the app shell chrome -> the
   `SessionDetailV2` adapter -> the WebSocket `session_detail` subscription -> the packed
   `@peasant-labs/fairtrade` `adaptTranscript` + `<TranscriptViewer>` composite. The backend is the
   binary's mock data store, so the durable relationships, the authorized read navigation and the
   retained earlier-history section all arrive over the wire.

   What each surface proves, per theme:
     context-links     the child renders a context-source link and a started-by link targeting two
                       DIFFERENT stored sessions; the retained earlier history starts collapsed, expands,
                       and the disclosure survives the click -> Back round trip back onto the child.
     unresolved-parent the child whose started-by target is no longer stored renders an honest
                       unavailable reference (no link) and stays readable.

   The script verifies build provenance BEFORE trusting any capture: a marker only this change
   introduces must be present both in the built artifact on disk AND in the chunk the server actually
   serves.

   env:
     PEASANT_REAL_ORIGIN  origin of the running real binary   (default http://localhost:8790)
     CONTEXT_NAV_OUT      capture directory                   (required; never a tracked path)
     CHROME_PATH          Chrome/Chromium binary              (required)
     PUPPETEER_CORE       explicit puppeteer-core module path (optional)
   usage: PEASANT_REAL_ORIGIN=http://localhost:8790 CONTEXT_NAV_OUT=/tmp/ctx-nav CHROME_PATH=$(command -v google-chrome) node scripts/visual/context-navigation-shoot.mjs
*/
import { mkdirSync, readFileSync, readdirSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'

const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default

const CHROME = process.env.CHROME_PATH
const ORIGIN = (process.env.PEASANT_REAL_ORIGIN || 'http://localhost:8790').replace(/\/$/, '')
const OUT = process.env.CONTEXT_NAV_OUT
if (!CHROME) {
  console.error('ERROR [context-navigation-shoot.mjs] CHROME_PATH is unset — set it to your Chrome/Chromium binary.')
  process.exit(1)
}
if (!OUT) {
  console.error('ERROR [context-navigation-shoot.mjs] CONTEXT_NAV_OUT is unset — point it at a review-capture directory (never a tracked path).')
  process.exit(1)
}
mkdirSync(OUT, { recursive: true })

// String literal that only this change introduces; it survives the production minifier and is absent
// from every earlier build, so finding it in the SERVED chunk proves the capture is this branch's build.
const PROVENANCE_MARKER = 'is not a section identifier'

const CHILD_PATH = '/projects/fortuna/sess_contextnavigationchild/'
const UNRESOLVED_PATH = '/projects/fortuna/sess_contextnavigationunresolved/'
const STARTED_BY_ID = 'sess_contextnavigationstartedby'

const pause = (ms) => new Promise((r) => setTimeout(r, ms))
const results = []

function chunkFiles(dir) {
  const found = []
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) found.push(...chunkFiles(path))
    else if (entry.name.endsWith('.js')) found.push(path)
  }
  return found
}

/* ── PROVENANCE: the built artifact on disk and the served chunk must both carry the marker ── */
const builtDir = resolve('out/_next/static/chunks')
let builtMatches = []
try {
  builtMatches = chunkFiles(builtDir).filter((file) => readFileSync(file, 'utf8').includes(PROVENANCE_MARKER))
} catch (error) {
  console.error(`ERROR [context-navigation-shoot.mjs] cannot read the built chunk directory ${builtDir}: ${error.message}. Build the web app first.`)
  process.exit(2)
}
if (builtMatches.length === 0) {
  console.error(`ERROR [context-navigation-shoot.mjs] the built artifact at ${builtDir} does not contain the change marker ${JSON.stringify(PROVENANCE_MARKER)}; the served build is not this branch. Rebuild before capturing.`)
  process.exit(2)
}

const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: { width: 1460, height: 1000, deviceScaleFactor: 1 } })
const page = await browser.newPage()
await applyDeterminism(page)
const errors = []
page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`))
page.on('console', (m) => { if (m.type() === 'error' && !/favicon|404|hydrat/.test(m.text())) errors.push(m.text()) })

const die = async (code, what) => {
  console.error(`\nSTRUCTURAL FAILURE [context-navigation-shoot.mjs] — ${what}\n  the captures would be invalid. Exiting ${code}.`)
  try { await browser.close() } catch { /* already closing */ }
  process.exit(code)
}

const waitFor = async (selector, timeoutMs = 15000) => {
  const start = Date.now()
  while (Date.now() - start < timeoutMs) {
    if (await page.$(selector)) return
    await pause(120)
  }
  throw new Error(`selector "${selector}" never mounted within ${timeoutMs}ms`)
}

const shell = await page.goto(`${ORIGIN}/`, { waitUntil: 'networkidle0' })
const shellHtml = await shell.text()
const servedChunks = [...shellHtml.matchAll(/\/_next\/static\/chunks\/[^"']+\.js/g)].map((m) => m[0])
const servedMatch = builtMatches
  .map((file) => `/${file.slice(file.indexOf('_next')).replace(/\\/g, '/')}`)
  .find((rel) => servedChunks.includes(rel))
if (!servedMatch) {
  await die(2, `provenance-mismatch: the built marker lives in ${builtMatches.map((f) => f.split('/').pop()).join(', ')} but the served page references none of them; the server is serving a different build than the one under test`)
}
const servedChunk = await page.evaluate(async (path) => (await fetch(path)).text(), servedMatch)
if (!servedChunk.includes(PROVENANCE_MARKER)) {
  await die(2, `provenance-mismatch: the served chunk ${servedMatch} does not contain the change marker; the server is serving a different build than the one under test`)
}
console.log(`[context-navigation-shoot] provenance OK: served chunk ${servedMatch} of ${builtMatches.length} built chunk(s) carries ${JSON.stringify(PROVENANCE_MARKER)}`)

const gate = new SurfaceGate(page)

/**
 * The app owns its theme and restores it from `peasant-theme`, so the capture
 * drives it exactly like the app does: seed the stored value before the
 * document loads, then require BOTH theme attributes the app stamps.
 */
async function useTheme(theme) {
  await page.evaluateOnNewDocument((value) => { try { localStorage.setItem('peasant-theme', value) } catch { /* storage disabled */ } }, theme)
}

async function assertTheme(theme) {
  const attrs = await page.evaluate(() => ({
    app: document.documentElement.getAttribute('data-theme'),
    package: document.documentElement.getAttribute('data-tb-theme'),
  }))
  if (attrs.app !== theme || attrs.package !== theme) {
    await die(3, `theme-didn't-flip: requested ${theme} but the document reports ${JSON.stringify(attrs)}`)
  }
}

async function assertAtkinson() {
  const family = await page.evaluate(() => getComputedStyle(document.body).fontFamily)
  if (!/Atkinson/i.test(family)) await die(4, `font-drift: body font-family ${JSON.stringify(family)} does not lead with Atkinson Hyperlegible`)
}

async function capture(name, theme, path) {
  await page.screenshot({ path })
  await gate.assert(name, path)
}

for (const theme of ['dark', 'light']) {
  await useTheme(theme)

  /* ── surface 1: context links, disclosure, click + Back ── */
  {
    const name = 'context-links'
    try {
      await page.goto(`${ORIGIN}${CHILD_PATH}`, { waitUntil: 'networkidle0' })
      await assertTheme(theme)
      await assertAtkinson()
      await waitFor('.txn-context-source .txn-context-link')
      const header = await page.evaluate(() => [...document.querySelectorAll('.txn-context-source')].map((row) => ({
        label: row.querySelector('.txn-context-row > span')?.textContent ?? '',
        link: !!row.querySelector('.txn-context-link'),
        status: row.querySelector('.txn-context-status')?.textContent ?? null,
      })))
      if (header.length !== 2 || !header.every((row) => row.link)) {
        throw new Error(`expected two linkable context rows, rendered ${JSON.stringify(header)}`)
      }
      if (header[0].label !== 'context inherited from' || header[1].label !== 'started by') {
        throw new Error(`context header labels drifted: ${JSON.stringify(header.map((row) => row.label))}`)
      }

      // Retained history starts collapsed, expands on the reader's action, and
      // the disclosure has to survive leaving the child and coming back.
      const toggle = await page.$('.txn-earlier-toggle')
      if (!toggle) throw new Error('no retained earlier-history section rendered on the child')
      const collapsed = await page.evaluate(() => document.querySelector('.txn-earlier-toggle')?.getAttribute('aria-expanded'))
      if (collapsed !== 'false') throw new Error(`retained history was disclosed without a reader action (aria-expanded=${collapsed})`)
      await toggle.click()
      await pause(400)
      const expanded = await page.evaluate(() => document.querySelector('.txn-earlier-toggle')?.getAttribute('aria-expanded'))
      if (expanded !== 'true') throw new Error(`clicking the retained-history toggle left aria-expanded=${expanded}`)

      const links = await page.$$('.txn-context-source .txn-context-link')
      await Promise.all([page.waitForNavigation({ waitUntil: 'networkidle0', timeout: 15000 }), links[1].click()])
      const arrivedId = page.url().replace(ORIGIN, '').split('?')[0].replace(/\/$/, '').split('/').pop()
      if (arrivedId !== STARTED_BY_ID) {
        throw new Error(`started-by link opened session ${JSON.stringify(arrivedId)}, expected the authorized target ${STARTED_BY_ID}`)
      }

      await page.goBack({ waitUntil: 'networkidle0' })
      await pause(500)
      // The label route the capture opened resolves to the canonical project
      // hash, so match the child identity rather than the literal pathname.
      const restored = new URL(page.url())
      const restoredSegments = restored.pathname.replace(/\/$/, '').split('/')
      if (restoredSegments.pop() !== 'sess_contextnavigationchild') {
        throw new Error(`Back returned to ${restored.pathname}, expected the child session route`)
      }
      if (restored.searchParams.get('earlier') !== 'earlier-0') {
        throw new Error(`Back returned to ${restored.pathname}${restored.search}, which lost the retained-history disclosure`)
      }
      await assertTheme(theme)
      const restoredExpanded = await page.evaluate(() => document.querySelector('.txn-earlier-toggle')?.getAttribute('aria-expanded'))
      if (restoredExpanded !== 'true') {
        throw new Error(`Back returned to the child but the retained-history disclosure collapsed (aria-expanded=${restoredExpanded})`)
      }
      const path = join(OUT, `${theme}-${name}.png`)
      await capture(name, theme, path)
      results.push({ name, theme, status: 'ok', path, info: `rows="${header.map((row) => row.label).join(' | ')}"; disclosure collapsed->expanded->restored-after-back; started-by opened ${STARTED_BY_ID}` })
    } catch (error) {
      results.push({ name, theme, status: 'gap', info: error.message })
      console.error(`GAP [${name}/${theme}] ${error.message}`)
    }
  }

  /* ── surface 2: unavailable current-parent reference ── */
  {
    const name = 'unresolved-parent'
    try {
      await page.goto(`${ORIGIN}${UNRESOLVED_PATH}`, { waitUntil: 'networkidle0' })
      await assertTheme(theme)
      await assertAtkinson()
      await waitFor('.txn-context-source')
      const row = await page.evaluate(() => {
        const first = document.querySelector('.txn-context-source')
        return first
          ? {
              label: first.querySelector('.txn-context-row > span')?.textContent ?? '',
              link: !!first.querySelector('.txn-context-link'),
              status: first.querySelector('.txn-context-status')?.textContent ?? null,
            }
          : null
      })
      if (!row) throw new Error('no context header rendered for the child with a stored-absent target')
      if (row.link || row.label !== 'started by' || row.status !== 'source unavailable') {
        throw new Error(`unavailable reference drifted: ${JSON.stringify(row)}`)
      }
      const readable = await page.evaluate(() => (document.querySelector('.txn-app')?.textContent ?? '').includes('no longer stored'))
      if (!readable) throw new Error('the child did not stay readable while its current-parent target is unavailable')

      const path = join(OUT, `${theme}-${name}.png`)
      await capture(name, theme, path)
      results.push({ name, theme, status: 'ok', path, info: `label="${row.label}" status="${row.status}" link=false; child readable` })
    } catch (error) {
      results.push({ name, theme, status: 'gap', info: error.message })
      console.error(`GAP [${name}/${theme}] ${error.message}`)
    }
  }

  if (errors.length) console.error(`[context-navigation-shoot] console errors during ${theme}: ${errors.join(' | ')}`)
}

await browser.close()

console.log('\n[context-navigation-shoot] results')
for (const row of results) {
  console.log(`  ${row.status === 'ok' ? 'OK ' : 'GAP'} ${row.theme}/${row.name}${row.path ? ` -> ${row.path}` : ''} (${row.info})`)
}
const ok = results.filter((row) => row.status === 'ok').length
console.log(`\n[context-navigation-shoot] ${ok}/${results.length} captures passed the real-data mount + interaction check`)
if (ok !== results.length) process.exit(1)
