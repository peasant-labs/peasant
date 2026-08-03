/* Graph shell frame gate.

   This boots a browser against the fairtrade in-use demo and a running peasant app,
   drives the shared graph shell nav through the three persistent sections, and captures
   the full shell frame for the SxS arm: persistent product chrome, graph section nav,
   active state, route/view wiring, and representative mounted body content. It fails
   closed when either side is absent, reordered, incorrectly linked, missing its active
   marker, pointed at a route/view that does not mount, blank-bodied, or not themed
   correctly.

   Start a real app first, for example:
     ./bin/peasant web start --port 8690 --foreground --no-browser \
       --mock-data-store=web,dashboard,sessions,trends,map,review,qualitySessions,annotations

   Run:
     CHROME_PATH=$(command -v google-chrome) node scripts/visual/shell-nav-gate.mjs

   Env:
      PEASANT_REAL_ORIGIN  origin of the running app (default http://localhost:8690)
      DEMO_URL             origin of the fairtrade demo (default http://localhost:5180)
      SHELL_CAPTURE_DIR    output root for peasant shell captures (default <base>/shell)
      SHELL_REFERENCE_DIR  output root for fairtrade shell captures (default <base>/shell-demo)
      SHELL_PROJECT        mock project used to drill code-map/changes past the picker
                            into a representative, project-scoped surface (default the
                            canonical hash for SHELL_DEFAULT_PROJECT in smoke-surfaces.mjs —
                            must exist in the running app's mock store; use a ProjectHash,
                            not a label, so this gate's exact-URL check isn't tripped by the
                            legitimate legacy-label-to-hash redirect)
      SHELL_RESPONSIVE_ONLY set to 1 to skip demo/capture work and run only the optimized
                            peasant shell checks at desktop, 768, 540, 480, and 390px
      CHROME_PATH          Chrome/Chromium binary (required)
      PUPPETEER_CORE       explicit puppeteer-core module path (optional)
 */
import { mkdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { parse } from 'yaml'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'
import { GRAPH_SHELL_NAV_DEFS, SHELL_DEFAULT_PROJECT, SMOKE_THEMES } from './smoke-surfaces.mjs'
import { assertKnownProject } from './validate-mock-coordinates.mjs'

const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default

const HERE = dirname(fileURLToPath(import.meta.url))
const BASE = process.argv[2] || HERE
const APP_OUT = process.env.SHELL_CAPTURE_DIR || join(BASE, 'shell')
const REF_OUT = process.env.SHELL_REFERENCE_DIR || join(BASE, 'shell-demo')
const CHROME = process.env.CHROME_PATH
const ORIGIN = (process.env.PEASANT_REAL_ORIGIN || 'http://localhost:8690').replace(/\/$/, '')
const DEMO = (process.env.DEMO_URL || 'http://localhost:5180').replace(/\/$/, '')
const SHELL_PROJECT = process.env.SHELL_PROJECT || SHELL_DEFAULT_PROJECT
const EXPECTED_NAV = GRAPH_SHELL_NAV_DEFS.map(({ label, href }) => ({ label, href }))
// GRAPH_SHELL_NAV_DEFS labels are already lowercase (smoke-surfaces.mjs); `.toLowerCase()` here
// is a defensive no-op so the demo-side comparison stays correct even if a label's source casing
// ever changes.
const EXPECTED_DEMO_NAV = GRAPH_SHELL_NAV_DEFS.map(({ label }) => label.toLowerCase())
const DEMO_MOUNTS = Object.freeze({
  'shell-changes': '.gmp-changes-root',
  'shell-map': '.gmp-root',
  'shell-analytics': '.gan-root',
})
const MIN_BODY_WIDTH = 240
const MIN_BODY_HEIGHT = 120
const MIN_SHELL_CAPTURE_HEIGHT = 520
const THEME_ATTRIBUTES = ['data-theme', 'data-tb-theme']
const FONTS = [
  '400 16px "Atkinson Hyperlegible"', '700 16px "Atkinson Hyperlegible"',
  '400 16px "Atkinson Hyperlegible Mono"', '500 16px "Atkinson Hyperlegible Mono"',
  '600 16px "Atkinson Hyperlegible Mono"', '700 16px "Atkinson Hyperlegible Mono"',
]
const RESPONSIVE_HEIGHT = 900
const RESPONSIVE_ONLY = process.env.SHELL_RESPONSIVE_ONLY === '1'
const RESPONSIVE_FIXTURE_PATH = join(HERE, '..', '..', 'src', 'test', 'testdata', 'shell_responsive.yaml')

const responsiveFixtureError = (what, fix) => new Error([
  `what: ${what}`,
  'why: the production shell viewport gate must run a complete, deterministic matrix with explicit layout expectations',
  `where: ${RESPONSIVE_FIXTURE_PATH}`,
  'when: loading responsive shell cases before launching the headless browser',
  'means: responsive navigation coverage cannot run safely, so the shell gate is stopped',
  `fix: ${fix}`,
].join('\n'))

const loadResponsiveCases = () => {
  let source
  try {
    source = readFileSync(RESPONSIVE_FIXTURE_PATH, 'utf8')
  } catch (cause) {
    throw responsiveFixtureError(
      `responsive shell fixture could not be read: ${cause instanceof Error ? cause.message : String(cause)}`,
      'restore the fixture file and ensure it is readable, then rerun the shell gate',
    )
  }

  let fixture
  try {
    fixture = parse(source)
  } catch (cause) {
    throw responsiveFixtureError(
      `responsive shell fixture is not valid YAML: ${cause instanceof Error ? cause.message : String(cause)}`,
      'correct the YAML syntax while preserving the viewport case schema, then rerun the shell gate',
    )
  }

  if (!fixture || typeof fixture !== 'object' || Array.isArray(fixture)) {
    throw responsiveFixtureError(
      'responsive shell fixture root must be a mapping',
      'define expectedCaseCount and a non-empty cases sequence in the fixture',
    )
  }
  const expectedCaseCount = fixture.expectedCaseCount
  const cases = fixture.cases
  if (!Number.isInteger(expectedCaseCount) || expectedCaseCount <= 0) {
    throw responsiveFixtureError(
      `expectedCaseCount must be a positive integer, got ${JSON.stringify(expectedCaseCount)}`,
      'set expectedCaseCount to the number of required responsive cases',
    )
  }
  if (!Array.isArray(cases) || cases.length === 0 || cases.length !== expectedCaseCount) {
    throw responsiveFixtureError(
      `cases must contain exactly ${expectedCaseCount} rows, got ${Array.isArray(cases) ? cases.length : JSON.stringify(cases)}`,
      'restore every required viewport row or update expectedCaseCount alongside an intentional matrix change',
    )
  }

  const names = new Set()
  const widths = new Set()
  for (const [index, row] of cases.entries()) {
    if (!row || typeof row !== 'object' || Array.isArray(row)) {
      throw responsiveFixtureError(
        `cases[${index}] must be a mapping, got ${JSON.stringify(row)}`,
        'define name, width, expectedHeaderHeight, and commandPaletteVisible for the row',
      )
    }
    if (typeof row.name !== 'string' || row.name.trim() === '' || row.name !== row.name.trim() || names.has(row.name)) {
      throw responsiveFixtureError(
        `cases[${index}].name must be a unique non-empty string, got ${JSON.stringify(row.name)}`,
        'give every responsive case a stable unique name',
      )
    }
    if (!Number.isInteger(row.width) || row.width <= 0 || widths.has(row.width)) {
      throw responsiveFixtureError(
        `case ${JSON.stringify(row.name)} width must be a unique positive integer, got ${JSON.stringify(row.width)}`,
        'use one positive CSS viewport width per responsive case',
      )
    }
    if (!Number.isInteger(row.expectedHeaderHeight) || row.expectedHeaderHeight <= 0) {
      throw responsiveFixtureError(
        `case ${JSON.stringify(row.name)} expectedHeaderHeight must be a positive integer, got ${JSON.stringify(row.expectedHeaderHeight)}`,
        'set the expected mounted header height in CSS pixels',
      )
    }
    if (typeof row.commandPaletteVisible !== 'boolean') {
      throw responsiveFixtureError(
        `case ${JSON.stringify(row.name)} commandPaletteVisible must be boolean, got ${JSON.stringify(row.commandPaletteVisible)}`,
        'set commandPaletteVisible to true when the visible command control must be reachable, otherwise false',
      )
    }
    names.add(row.name)
    widths.add(row.width)
  }
  return cases
}

const RESPONSIVE_CASES = loadResponsiveCases()

if (!CHROME) {
  console.error('ERROR [shell-nav-gate.mjs] CHROME_PATH is unset. Set it to your Chrome/Chromium binary before running the shell navigation gate.')
  process.exit(1)
}

const pause = (ms) => new Promise((r) => setTimeout(r, ms))
const consoleLocationUrl = (m) => {
  const loc = typeof m.location === 'function' ? m.location() : null
  return loc?.url || ''
}
const isKnownBenignConsoleMessage = (text, locationUrl) => /favicon/i.test(`${text} ${locationUrl}`)
const shouldCaptureConsoleMessage = (m) => {
  const text = m.text()
  return m.type() === 'error' && !isKnownBenignConsoleMessage(text, consoleLocationUrl(m))
}
const formatConsoleDiagnostic = (m) => {
  const loc = typeof m.location === 'function' ? m.location() : null
  const where = loc?.url ? ` at ${loc.url}:${loc.lineNumber ?? 0}:${loc.columnNumber ?? 0}` : ''
  return `console ${m.type()}${where}: ${m.text()}`
}

const fail = async (browser, message, code = 1) => {
  console.error(message)
  try { await browser.close() } catch {}
  process.exit(code)
}

// Waits for the persistent shell nav to settle on `section`. By default this checks the
// nav's own production href/mount/heading (the nav-WIRING assertion). Pass `expectedPath` /
// `mountSelector` / `bodySelector` / `heading` overrides to instead assert a representative,
// project-scoped drill-down (e.g. /map/{project}, /review/{project}) reached by an in-app
// navigation past the cross-project picker, while still requiring the SAME nav active-state
// invariants (label, marker, color, aria-current) to hold there.
const waitForSection = async (page, section, theme, {
  timeoutMs = 15000,
  expectedPath = section.href,
  mountSelector = section.mount,
  bodySelector = section.body || section.mount,
  heading = section.heading,
} = {}) => {
  const start = Date.now()
  let last = null
  while (Date.now() - start < timeoutMs) {
    last = await page.evaluate(({ section, expectedNav, theme, attrs, minBodyWidth, minBodyHeight, expectedPath, mountSelector, bodySelector, heading }) => {
      const normalizePath = (path) => {
        if (!path || path === '/') return '/'
        return path.replace(/\/+$/, '') || '/'
      }
      const navSnapshot = (expected) => {
        const navRoot = document.querySelector('nav[aria-label="Main navigation"]')
        if (!navRoot) return { ok: false, reason: 'nav[aria-label="Main navigation"] did not mount', links: [] }
        const links = [...navRoot.querySelectorAll('a')].map((a) => {
          // The active section is a FILLED AMBER PILL (matches the fairtrade demo's
          // `.iu-subnav-item.active`: color on-amber, background amber, border-color amber)
          // on the link element itself — not an underline marker child.
          const classes = (a.className || '').split(/\s+/)
          const isAmberPill =
            classes.includes('bg-amber') && classes.includes('text-on-amber') && classes.includes('border-amber')
          return {
            label: (a.textContent || '').trim(),
            href: normalizePath(new URL(a.href, window.location.origin).pathname),
            current: a.getAttribute('aria-current') || '',
            isAmberPill,
          }
        })
        const expectedText = JSON.stringify(expected)
        const actualText = JSON.stringify(links.map(({ label, href }) => ({ label, href })))
        if (actualText !== expectedText) {
          return { ok: false, reason: `expected links ${expectedText}, got ${actualText}`, links }
        }
        return { ok: true, reason: '', links }
      }
      const nav = navSnapshot(expectedNav)
      const path = normalizePath(window.location.pathname)
      const active = nav.links.filter((link) => link.current === 'page')
      const activeLabels = active.map((link) => link.label)
      const activeOk = active.length === 1 && active[0].label === section.label && active[0].isAmberPill
      const inactiveMarkersOk = nav.links.every((link) => link.label === section.label || !link.isAmberPill)
      const mountOk = !!document.querySelector(mountSelector)
      const body = document.querySelector(bodySelector)
      const bodyRect = body?.getBoundingClientRect()
      const bodyText = (body?.textContent || '').replace(/\s+/g, ' ').trim()
      const bodyVisible = !!body && getComputedStyle(body).visibility !== 'hidden' && getComputedStyle(body).display !== 'none'
      const bodyInViewport = !!bodyRect && bodyRect.bottom > 96 && bodyRect.top < window.innerHeight - 80
      const bodySignals = body ? body.querySelectorAll('a, button, canvas, svg, img, [role], [aria-label]').length : 0
      const bodyOk = !!bodyRect && bodyVisible && bodyInViewport && bodyRect.width >= minBodyWidth && bodyRect.height >= minBodyHeight && (bodyText.length >= 20 || bodySignals >= 3)
      const headingOk = !heading || [...document.querySelectorAll('h1, h2')].some((h) => (h.textContent || '').trim().toLowerCase() === heading.toLowerCase())
      const themeAttrs = Object.fromEntries(attrs.map((attr) => [attr, document.documentElement.getAttribute(attr)]))
      const themeOk = attrs.every((attr) => themeAttrs[attr] === theme)
      return {
        ok: nav.ok && path === normalizePath(expectedPath) && activeOk && inactiveMarkersOk && mountOk && bodyOk && headingOk && themeOk,
        path,
        nav,
        activeLabels,
        activeOk,
        inactiveMarkersOk,
        mountOk,
        bodySelector,
        bodyOk,
        bodyInViewport,
        bodyRect: bodyRect ? { width: bodyRect.width, height: bodyRect.height, top: bodyRect.top, bottom: bodyRect.bottom } : null,
        bodyTextLength: bodyText.length,
        bodySignals,
        headingOk,
        themeAttrs,
        themeOk,
      }
    }, { section, expectedNav: EXPECTED_NAV, theme, attrs: THEME_ATTRIBUTES, minBodyWidth: MIN_BODY_WIDTH, minBodyHeight: MIN_BODY_HEIGHT, expectedPath, mountSelector, bodySelector, heading })
    if (last.ok) return last
    await pause(150)
  }
  throw new Error(
    `Timed out waiting for ${section.label} at ${expectedPath}. ` +
    `Last state: ${JSON.stringify(last)}`
  )
}

const waitForDemoSection = async (page, section, theme, timeoutMs = 15000) => {
  const start = Date.now()
  let last = null
  while (Date.now() - start < timeoutMs) {
    last = await page.evaluate(({ section, expectedNav, theme, mount, minBodyWidth, minBodyHeight }) => {
      const navRoot = document.querySelector('nav[aria-label="peasant sections"]')
      const nav = navRoot
        ? [...navRoot.querySelectorAll('button, a')].map((item) => ({
            label: (item.textContent || '').trim().toLowerCase(),
            current: item.getAttribute('aria-current') || '',
            active: item.classList.contains('active'),
          }))
        : []
      const active = nav.filter((item) => item.current === 'page' || item.active)
      const wanted = section.label.toLowerCase()
      const attr = document.documentElement.getAttribute('data-theme')
      const body = document.querySelector(mount)
      const bodyRect = body?.getBoundingClientRect()
      const bodyText = (body?.textContent || '').replace(/\s+/g, ' ').trim()
      const bodyVisible = !!body && getComputedStyle(body).visibility !== 'hidden' && getComputedStyle(body).display !== 'none'
      const bodyInViewport = !!bodyRect && bodyRect.bottom > 96 && bodyRect.top < window.innerHeight - 80
      const bodySignals = body ? body.querySelectorAll('a, button, canvas, svg, img, [role], [aria-label]').length : 0
      const bodyOk = !!bodyRect && bodyVisible && bodyInViewport && bodyRect.width >= minBodyWidth && bodyRect.height >= minBodyHeight && (bodyText.length >= 20 || bodySignals >= 3)
      return {
        ok:
          !!navRoot &&
          JSON.stringify(nav.map(({ label }) => label)) === JSON.stringify(expectedNav) &&
          active.length === 1 &&
          active[0].label === wanted &&
          active[0].current === 'page' &&
          !!document.querySelector(mount) &&
          bodyOk &&
          document.querySelector('#iu-tab-graph')?.getAttribute('aria-selected') === 'true' &&
          (theme === 'light' ? attr === 'light' : attr !== 'light'),
        nav,
        active,
        mount,
        mountOk: !!document.querySelector(mount),
        bodyOk,
        bodyInViewport,
        bodyRect: bodyRect ? { width: bodyRect.width, height: bodyRect.height, top: bodyRect.top, bottom: bodyRect.bottom } : null,
        bodyTextLength: bodyText.length,
        bodySignals,
        graphAppSelected: document.querySelector('#iu-tab-graph')?.getAttribute('aria-selected') === 'true',
        themeAttr: attr,
      }
    }, { section, expectedNav: EXPECTED_DEMO_NAV, theme, mount: DEMO_MOUNTS[section.id], minBodyWidth: MIN_BODY_WIDTH, minBodyHeight: MIN_BODY_HEIGHT })
    if (last.ok) return last
    await pause(150)
  }
  throw new Error(
    `Timed out waiting for the fairtrade graph demo ${section.label} view. ` +
    `Last state: ${JSON.stringify(last)}`
  )
}

const clickNav = async (page, section) => {
  const clicked = await page.evaluate((label) => {
    const nav = document.querySelector('nav[aria-label="Main navigation"]')
    const link = [...(nav?.querySelectorAll('a') || [])].find((a) => (a.textContent || '').trim() === label)
    if (!link) return false
    link.click()
    return true
  }, section.label)
  if (!clicked) throw new Error(`Could not click the ${section.label} link in nav[aria-label="Main navigation"].`)
}

const clickDemoNav = async (page, section) => {
  const clicked = await page.evaluate((label) => {
    const nav = document.querySelector('nav[aria-label="peasant sections"]')
    const item = [...(nav?.querySelectorAll('button, a') || [])].find((x) => (x.textContent || '').trim().toLowerCase() === label.toLowerCase())
    if (!item) return false
    item.click()
    return true
  }, section.label)
  if (!clicked) throw new Error(`Could not click the ${section.label} button in the fairtrade graph demo nav.`)
}

const waitForFonts = async (page) => {
  await page.evaluate(async (faces) => {
    try { await Promise.all(faces.map((f) => document.fonts.load(f))) } catch {}
    await document.fonts.ready
  }, FONTS)
}

const assertBodyReady = async (page, section, bodySelector, where) => {
  const body = await page.evaluate((selector, minBodyWidth, minBodyHeight) => {
    const el = document.querySelector(selector)
    if (!el) return { ok: false, reason: `${selector} did not mount`, selector }
    const rect = el.getBoundingClientRect()
    const text = (el.textContent || '').replace(/\s+/g, ' ').trim()
    const style = getComputedStyle(el)
    const signals = el.querySelectorAll('a, button, canvas, svg, img, [role], [aria-label]').length
    const visible = style.visibility !== 'hidden' && style.display !== 'none'
    const inViewport = rect.bottom > 96 && rect.top < window.innerHeight - 80
    const ok = visible && inViewport && rect.width >= minBodyWidth && rect.height >= minBodyHeight && (text.length >= 20 || signals >= 3)
    return {
      ok,
      reason: ok ? '' : `selector=${selector} visible=${visible} inViewport=${inViewport} size=${Math.round(rect.width)}x${Math.round(rect.height)} text=${text.length} signals=${signals}`,
      selector,
      width: rect.width,
      height: rect.height,
      top: rect.top,
      bottom: rect.bottom,
      textLength: text.length,
      signals,
    }
  }, bodySelector, MIN_BODY_WIDTH, MIN_BODY_HEIGHT)
  if (!body.ok) {
    throw new Error(`The ${where} ${section.label} shell body is missing or blank: ${body.reason}.`)
  }
  return body
}

const captureShellFrame = async (page, gate, theme, section, { outRoot, selector, bodySelector, where, mode, scrollSelector = null, clipToBody = false }) => {
  const outDir = join(outRoot, theme)
  mkdirSync(outDir, { recursive: true })
  const file = join(outDir, `${section.id}.png`)
  await waitForFonts(page)
  // Hide any production UI flagged `data-visual-exclude` (a generic hook for
  // interim/transient copy that would otherwise skew shell-parity evidence)
  // before measuring/capturing. This only affects THIS capture's DOM
  // snapshot — it never touches the production feature, which stays live
  // for real users. No production element currently carries this flag; it
  // stays available for a future transient-UI case.
  await page.evaluate(() => {
    document.querySelectorAll('[data-visual-exclude]').forEach((el) => { el.style.display = 'none' })
  })
  const body = await assertBodyReady(page, section, bodySelector, where)
  let box
  if (mode === 'viewport') {
    if (scrollSelector) await page.evaluate((sel) => document.querySelector(sel)?.scrollIntoView({ block: 'start' }), scrollSelector)
    else await page.evaluate(() => window.scrollTo(0, 0))
    await pause(100)
    const viewport = page.viewport() || { width: 1460, height: 1000 }
    if (clipToBody) {
      const scroll = await page.evaluate(() => ({ x: window.scrollX, y: window.scrollY }))
      const height = Math.min(viewport.height, Math.max(MIN_SHELL_CAPTURE_HEIGHT, Math.ceil(body.bottom + 48)))
      box = { width: viewport.width, height }
      await page.screenshot({ path: file, clip: { x: scroll.x, y: scroll.y, width: viewport.width, height } })
    } else {
      box = { width: viewport.width, height: viewport.height }
      await page.screenshot({ path: file, captureBeyondViewport: false })
    }
  } else {
    const frame = await page.$(selector)
    if (!frame) throw new Error(`${selector} did not mount; cannot capture ${where}.`)
    await page.evaluate((sel) => document.querySelector(sel)?.scrollIntoView({ block: 'start' }), selector)
    await pause(100)
    box = await frame.boundingBox()
    if (!box || box.width < 100 || box.height < 160) {
      throw new Error(`${selector} has an invalid shell-frame capture box for ${where}: ${JSON.stringify(box)}.`)
    }
    await frame.screenshot({ path: file, captureBeyondViewport: true })
  }
  if (!box || box.width < 100 || box.height < 24) {
    throw new Error(`${selector} has an invalid capture box for ${where}: ${JSON.stringify(box)}.`)
  }
  const measured = await gate.assert(section.id, file, { sel: selector, where: 'shell-nav-gate.mjs' })
  console.log(`OK ${where} ${theme}/${section.id} active=${section.label} frame=${Math.round(box.width)}x${Math.round(box.height)} body=${body.selector} ${Math.round(body.width)}x${Math.round(body.height)} nonbg=${(measured.nonbgRatio * 100).toFixed(1)}% colors=${measured.distinctColors}`)
}

const captureFairtradeReference = async (browser) => {
  for (const theme of SMOKE_THEMES) {
    const page = await browser.newPage()
    await applyDeterminism(page)
    const gate = new SurfaceGate(page)
    const diagnostics = []
    page.on('console', (m) => { if (shouldCaptureConsoleMessage(m)) diagnostics.push(formatConsoleDiagnostic(m)) })
    page.on('pageerror', (e) => diagnostics.push('pageerr: ' + (e.stack || e.message)))

    const demoUrl = `${DEMO}/?fb=off${theme === 'light' ? '&theme=light' : ''}`
    const response = await page.goto(demoUrl, { waitUntil: 'networkidle0' }).catch((e) => {
      throw new Error(`Could not load fairtrade demo at ${demoUrl}: ${e.message}`)
    })
    if (!response || response.status() >= 400) {
      throw new Error(`The fairtrade demo returned HTTP ${response ? response.status() : 0} for ${demoUrl}.`)
    }
    await page.evaluate(() => document.getElementById('inuse')?.scrollIntoView({ block: 'start' }))
    await pause(250)
    const graphSelected = await page.evaluate(() => {
      const tab = document.querySelector('#iu-tab-graph')
      if (!tab) return false
      tab.click()
      return true
    })
    if (!graphSelected) throw new Error('The fairtrade in-use app switcher did not expose #iu-tab-graph.')
    await pause(300)

    for (const section of GRAPH_SHELL_NAV_DEFS) {
      await clickDemoNav(page, section)
      await page.evaluate(() => document.getElementById('inuse')?.scrollIntoView({ block: 'start' }))
      await waitForDemoSection(page, section, theme)
      await page.evaluate(() => document.getElementById('inuse')?.scrollIntoView({ block: 'start' }))
      await captureShellFrame(page, gate, theme, section, {
        outRoot: REF_OUT,
        selector: 'body',
        bodySelector: DEMO_MOUNTS[section.id],
        where: 'fairtrade-demo',
        mode: 'viewport',
        scrollSelector: '#inuse',
      })
    }

    if (diagnostics.length) {
      throw new Error(`Client diagnostics appeared while driving the ${theme} fairtrade graph demo: ${JSON.stringify(diagnostics.slice(0, 4))}`)
    }
    await page.close()
  }
}

const capturePeasantApp = async (browser) => {
  for (const theme of SMOKE_THEMES) {
    const page = await browser.newPage()
    await applyDeterminism(page)
    await page.evaluateOnNewDocument((t) => { try { localStorage.setItem('peasant-theme', t) } catch {} }, theme)
    const gate = new SurfaceGate(page)
    const diagnostics = []
    page.on('console', (m) => { if (shouldCaptureConsoleMessage(m)) diagnostics.push(formatConsoleDiagnostic(m)) })
    page.on('pageerror', (e) => diagnostics.push('pageerr: ' + (e.stack || e.message)))

    const first = GRAPH_SHELL_NAV_DEFS[0]
    const response = await page.goto(ORIGIN + first.href, { waitUntil: 'domcontentloaded' }).catch((e) => {
      throw new Error(`Could not load ${ORIGIN + first.href}: ${e.message}`)
    })
    if (!response || response.status() >= 400) {
      throw new Error(`The running app returned HTTP ${response ? response.status() : 0} for ${ORIGIN + first.href}.`)
    }
    await waitForSection(page, first, theme)

    for (const section of GRAPH_SHELL_NAV_DEFS.slice(1)) {
      await clickNav(page, section)
      await waitForSection(page, section, theme)
      let bodySelector = section.body || section.mount
      if (section.representativePath) {
        // The bare nav href landed on the cross-project picker (verified above). Drill into a
        // real, project-scoped surface — same in-app route the user reaches via the picker —
        // so the shell capture shows representative MOUNTED body content, not the picker.
        const path = section.representativePath(SHELL_PROJECT)
        const response = await page.goto(ORIGIN + path, { waitUntil: 'domcontentloaded' }).catch((e) => {
          throw new Error(`Could not load representative ${section.label} surface at ${ORIGIN + path}: ${e.message}`)
        })
        if (!response || response.status() >= 400) {
          throw new Error(`The running app returned HTTP ${response ? response.status() : 0} for representative ${section.label} surface ${ORIGIN + path}.`)
        }
        await waitForSection(page, section, theme, {
          expectedPath: path,
          mountSelector: section.representativeBody,
          bodySelector: section.representativeBody,
          heading: null,
        })
        bodySelector = section.representativeBody
      }
      await captureShellFrame(page, gate, theme, section, {
        outRoot: APP_OUT,
        selector: 'body',
        bodySelector,
        where: 'peasant-app',
        mode: 'viewport',
        clipToBody: true,
      })
    }
    await clickNav(page, first)
    await waitForSection(page, first, theme)
    await captureShellFrame(page, gate, theme, first, {
      outRoot: APP_OUT,
      selector: 'body',
      bodySelector: first.body || first.mount,
      where: 'peasant-app',
      mode: 'viewport',
      clipToBody: true,
    })

    if (diagnostics.length) {
      throw new Error(`Client diagnostics appeared while driving the ${theme} peasant shell nav: ${JSON.stringify(diagnostics.slice(0, 4))}`)
    }
    await page.close()
  }
}

// The optimized export uses trailingSlash, so this runs against the exact mounted
// production path and guards the whole persistent shell at the widths where its
// desktop row previously clipped. It verifies geometry and reachability rather
// than relying on a component-only render or an app-generated screenshot.
// Read the Share button's live computed background/color/border-top-color —
// the exact properties assertResponsivePeasantShell later compares against
// the root's live --amber/--on-amber values.
const readShareComputedColors = (page) => page.evaluate(() => {
  const share = document.querySelector('header a[href="/share"]')
  if (!share) return null
  const style = getComputedStyle(share)
  return { backgroundColor: style.backgroundColor, color: style.color, borderColor: style.borderTopColor }
})

// Wait until two reads of the Share button's computed colors, more than
// settleIntervalMs apart, AGREE — i.e. wait for style computation to have
// genuinely settled, not just for an attribute to be present.
//
// Root cause: the Share button's [data-theme] flip
// (useTheme's post-mount useEffect) is applied instantly and correctly, but
// .btn's background/color/border-color transition (fairtrade src/index.css,
// now gated behind prefers-reduced-motion: no-preference) interpolates the
// PAINTED color across ~120ms before settling on the new theme's --amber —
// a live CSS animation, not a wrong rule or a stale token. A presence-only
// wait for the [data-theme] attribute is a near-no-op here (the SSR shell
// already bakes data-theme="dark" in from byte one — see layout.tsx), so it
// resolves before the transition has necessarily even started, let alone
// finished. Reduced-motion emulation (applyDeterminism, plus the explicit
// call below) removes the transition entirely at the source once the
// Fairtrade gating fix has landed; this settle-wait is kept as an
// independent, general safety net against any future ungated transition on
// this or any other probed element, motion emulation notwithstanding.
const waitForSettledShareColors = async (page, { settleIntervalMs = 160, timeoutMs = 5000 } = {}) => {
  const start = Date.now()
  let previous = await readShareComputedColors(page)
  while (Date.now() - start < timeoutMs) {
    await new Promise((resolve) => setTimeout(resolve, settleIntervalMs))
    const current = await readShareComputedColors(page)
    if (
      previous && current &&
      previous.backgroundColor === current.backgroundColor &&
      previous.color === current.color &&
      previous.borderColor === current.borderColor
    ) {
      return current
    }
    previous = current
  }
  throw new Error(
    `The Share button's computed colors never settled (kept changing between reads ${settleIntervalMs}ms apart) after ${timeoutMs}ms. Last read: ${JSON.stringify(previous)}`
  )
}

const assertResponsivePeasantShell = async (browser) => {
  const page = await browser.newPage()
  await applyDeterminism(page)

  for (const responsiveCase of RESPONSIVE_CASES) {
    const { name, width, expectedHeaderHeight, commandPaletteVisible } = responsiveCase
    await page.setViewport({ width, height: RESPONSIVE_HEIGHT, deviceScaleFactor: 1 })
    // Motion-free captures are the correct posture for a visual harness regardless
    // (applyDeterminism above already emulates this once for the page, but it is
    // re-asserted per case here so it is never contingent on emulation surviving a
    // navigation) — with the Fairtrade .btn transition gated behind
    // prefers-reduced-motion: no-preference, this alone removes the transition
    // class of race at its source for every width this loop drives.
    await page.emulateMediaFeatures([{ name: 'prefers-reduced-motion', value: 'reduce' }])
    const url = `${ORIGIN}/share/`
    const response = await page.goto(url, { waitUntil: 'domcontentloaded' }).catch((e) => {
      throw new Error(`Could not load optimized share shell at ${url} for ${width}px: ${e.message}`)
    })
    if (!response || response.status() >= 400) {
      throw new Error(`The optimized app returned HTTP ${response ? response.status() : 0} for ${url} at ${width}px.`)
    }
    await page.waitForSelector('header a[href="/share"]', { visible: true, timeout: 15000 })
    // Wait for the Share button's computed colors to genuinely settle (two
    // reads >150ms apart agree) before reading ANY computed style below — see
    // waitForSettledShareColors's doc comment for why an attribute-presence
    // wait alone is insufficient.
    await waitForSettledShareColors(page)

    const state = await page.evaluate(({ expectedNav, expectedWidth }) => {
      const visibleRect = (element) => {
        if (!element) return null
        const rect = element.getBoundingClientRect()
        const style = getComputedStyle(element)
        const centerX = Math.max(0, Math.min(window.innerWidth - 1, rect.left + rect.width / 2))
        const centerY = Math.max(0, Math.min(window.innerHeight - 1, rect.top + rect.height / 2))
        const hit = document.elementFromPoint(centerX, centerY)
        return {
          left: rect.left,
          right: rect.right,
          top: rect.top,
          bottom: rect.bottom,
          width: rect.width,
          height: rect.height,
          visible:
            style.display !== 'none' &&
            style.visibility !== 'hidden' &&
            rect.width > 0 &&
            rect.height > 0 &&
            rect.left >= 0 &&
            rect.right <= window.innerWidth,
          reachable: !!hit && (hit === element || element.contains(hit)),
        }
      }

      const header = document.querySelector('header')
      const nav = document.querySelector('nav[aria-label="Main navigation"]')
      const home = header?.querySelector('a[aria-label="Peasant home"]')
      const share = header?.querySelector('a[href="/share"]')
      const palette = header?.querySelector('button[aria-label="Open the command palette (Command or Control + K)"]')
      const theme = header?.querySelector('button[aria-label^="Switch to "]')
      const main = document.querySelector('main')
      const links = [...(nav?.querySelectorAll('a') || [])]
      const linkState = links.map((link) => ({
        label: (link.textContent || '').trim(),
        href: new URL(link.href, window.location.origin).pathname.replace(/\/+$/, '') || '/',
        rect: visibleRect(link),
      }))
      const headerRect = header?.getBoundingClientRect()
      const mainPaddingTop = main ? Number.parseFloat(getComputedStyle(main).paddingTop) : null
      const rootStyle = getComputedStyle(document.documentElement)
      const normalizeColor = (value) => {
        const probe = document.createElement('span')
        probe.style.color = value
        probe.style.display = 'none'
        document.body.append(probe)
        const normalized = getComputedStyle(probe).color
        probe.remove()
        return normalized
      }
      const amber = rootStyle.getPropertyValue('--amber').trim()
      const onAmber = rootStyle.getPropertyValue('--on-amber').trim()
      const shareStyle = share ? getComputedStyle(share) : null
      const actualNav = linkState.map(({ label, href }) => ({ label, href }))
      const expectedNavText = JSON.stringify(expectedNav)
      const actualNavText = JSON.stringify(actualNav)

      return {
        expectedWidth,
        viewportWidth: window.innerWidth,
        documentWidth: document.documentElement.scrollWidth,
        headerWidth: header?.scrollWidth || 0,
        headerClientWidth: header?.clientWidth || 0,
        headerRect: headerRect ? { left: headerRect.left, right: headerRect.right, bottom: headerRect.bottom, height: headerRect.height } : null,
        mainPaddingTop,
        navOk: actualNavText === expectedNavText,
        expectedNavText,
        actualNavText,
        links: linkState,
        home: visibleRect(home),
        palette: visibleRect(palette),
        share: visibleRect(share),
        shareCurrent: share?.getAttribute('aria-current') || '',
        shareColors: shareStyle ? {
          backgroundColor: shareStyle.backgroundColor,
          color: shareStyle.color,
          borderColor: shareStyle.borderTopColor,
        } : null,
        expectedShareColors: {
          backgroundColor: normalizeColor(amber),
          color: normalizeColor(onAmber),
          borderColor: normalizeColor(amber),
        },
        shareTokens: { amber, onAmber },
        theme: visibleRect(theme),
      }
    }, { expectedNav: EXPECTED_NAV, expectedWidth: width })

    const linksReady = state.links.length === EXPECTED_NAV.length && state.links.every((link) => link.rect?.visible && link.rect?.reachable)
    const noHeaderOverflow =
      state.viewportWidth === width &&
      state.headerRect?.left >= 0 &&
      state.headerRect?.right <= width &&
      state.headerWidth <= state.headerClientWidth
    const bodyClearsHeader = state.mainPaddingTop !== null && state.headerRect && state.mainPaddingTop >= state.headerRect.height
    const headerHeightReady = !!state.headerRect && Math.abs(state.headerRect.height - expectedHeaderHeight) < 0.5
    const paletteReady = commandPaletteVisible
      ? state.palette?.visible && state.palette?.reachable
      : !state.palette?.visible
    const controlsReady =
      state.home?.visible &&
      state.home?.reachable &&
      paletteReady &&
      state.share?.visible &&
      state.share?.reachable &&
      state.theme?.visible &&
      state.theme?.reachable
    const shareTreatmentReady =
      state.shareTokens.amber !== '' &&
      state.shareTokens.onAmber !== '' &&
      state.shareColors?.backgroundColor === state.expectedShareColors.backgroundColor &&
      state.shareColors?.color === state.expectedShareColors.color &&
      state.shareColors?.borderColor === state.expectedShareColors.borderColor
    const ready = noHeaderOverflow && headerHeightReady && bodyClearsHeader && state.navOk && linksReady && controlsReady && shareTreatmentReady && state.shareCurrent === 'page'

    if (!ready) {
      throw new Error(
        `Responsive optimized shell case ${JSON.stringify(name)} failed at ${width}px. ` +
        `Expected a ${expectedHeaderHeight}px header without overflow, body below it, three reachable graph links, reachable home/share/theme controls, command palette visible=${commandPaletteVisible}, current /share/, and computed share colors equal to --amber/--on-amber/--amber. ` +
        `State: ${JSON.stringify(state)}`
      )
    }
    console.log(`OK peasant-app responsive case=${name} width=${width} header=${Math.round(state.headerRect.height)}px share=current nav=${state.links.length}`)
  }

  await page.close()
}

// Fail-fast coordinate check BEFORE Puppeteer boots — see validate-mock-coordinates.mjs for why
// (this is the systemic guard against the recurring stale-mock-project-default class of bug).
// SHELL_PROJECT is used by both the demo and responsive-only paths, so this always runs.
try {
  await assertKnownProject(ORIGIN, SHELL_PROJECT, { where: 'shell-nav-gate.mjs' })
} catch (e) {
  console.error(e.message)
  process.exit(2)
}

const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: { width: 1460, height: 1000, deviceScaleFactor: 1 } })

try {
  if (!RESPONSIVE_ONLY) {
    await captureFairtradeReference(browser)
    await capturePeasantApp(browser)
  }
  await assertResponsivePeasantShell(browser)
} catch (e) {
  await fail(
    browser,
    `ERROR [shell-nav-gate.mjs] graph shell navigation gate failed.\n` +
    `  What failed: ${e.message}\n` +
    `  Why: shell evidence requires a current peasant app with unclipped, reachable navigation at desktop and representative narrow widths${RESPONSIVE_ONLY ? '' : ', plus a fairtrade in-use demo reference'}.\n` +
    `  Where: shell-nav-gate.mjs while driving ${RESPONSIVE_ONLY ? `the optimized peasant shell at ${ORIGIN}` : `fairtrade ${DEMO} and peasant ${ORIGIN}`}.\n` +
    `  Means: users may see broken persistent navigation, or reviewers may inspect an app-vs-app baseline instead of the design-system reference.\n` +
    `  Fix: ${RESPONSIVE_ONLY ? 'serve a fresh optimized web/out at PEASANT_REAL_ORIGIN, then fix the reported responsive geometry or control reachability' : 'verify fairtrade dev is running at DEMO_URL, peasant is running with mock data at PEASANT_REAL_ORIGIN, then fix the reported nav/view wiring or responsive geometry'}.`
  )
}

await browser.close()
console.log(`\nOK [shell-nav-gate.mjs] responsive peasant shell verified at ${RESPONSIVE_CASES.map(({ width }) => width).join(', ')}px.`)
if (!RESPONSIVE_ONLY) {
  console.log(`Graph shell frames captured for ${SMOKE_THEMES.length} themes x ${GRAPH_SHELL_NAV_DEFS.length} sections:`)
  console.log(`  fairtrade reference: ${REF_OUT}/<theme>/`)
  console.log(`  peasant subject:      ${APP_OUT}/<theme>/`)
}
