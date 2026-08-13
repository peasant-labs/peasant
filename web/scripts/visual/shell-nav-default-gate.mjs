/* Default-mode graph shell gate (the discoverability-gate fail-closed arm).

   Companion to shell-nav-gate.mjs. That gate boots the app WITH `--experimental`
   and asserts all three graph sections (`analytics · changes · code map`) are
   discoverable. This gate boots the app WITHOUT `--experimental` and asserts the
   opposite discoverability contract on the SAME production build:

     1. the persistent top nav shows EXACTLY `analytics · changes`, in that order,
        with a mounted, non-blank body and the correct active-state amber pill;
     2. the code map is absent from BOTH the nav AND the command palette — no
        "go to code map" nav command and no per-project "· map" jump — while the
        palette is genuinely populated (per-project "· changes" jumps present), so
        the absence is a real gate, not an empty list;
     3. the `/map/<project>` route is STILL directly mounted (`.gmp-root` renders a
        non-blank body) — the gate hides discoverability, it never removes routes.

   It fails closed when the nav, palette, or a mounted body is missing, blank,
   reordered, or when a map entry point leaks into default mode.

   Start a real app first, WITHOUT --experimental (mirror shell-nav-gate.mjs minus
   the flag):
     ./bin/peasant web start --port 8690 --foreground --no-browser \
       --mock-data-store=web,dashboard,sessions,trends,map,review,qualitySessions,annotations

   Run:
     CHROME_PATH=$(command -v google-chrome) node scripts/visual/shell-nav-default-gate.mjs

   Env:
     PEASANT_REAL_ORIGIN  origin of the running default-mode app (default http://localhost:8690)
     SHELL_CAPTURE_DIR    output root for shell captures (default <base>/shell-default)
     SHELL_PROJECT        mock ProjectHash used to drill changes/map past the picker
                          into a representative, project-scoped surface (default the
                          canonical SHELL_DEFAULT_PROJECT from smoke-surfaces.mjs)
     CHROME_PATH          Chrome/Chromium binary (required)
     PUPPETEER_CORE       explicit puppeteer-core module path (optional)
 */
import { mkdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'
import { GRAPH_SHELL_NAV_DEFS, SHELL_DEFAULT_PROJECT, SMOKE_THEMES } from './smoke-surfaces.mjs'
import { assertKnownProject } from './validate-mock-coordinates.mjs'

const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default

const HERE = dirname(fileURLToPath(import.meta.url))
const BASE = process.argv[2] || HERE
const APP_OUT = process.env.SHELL_CAPTURE_DIR || join(BASE, 'shell-default')
const CHROME = process.env.CHROME_PATH
const ORIGIN = (process.env.PEASANT_REAL_ORIGIN || 'http://localhost:8690').replace(/\/$/, '')
const SHELL_PROJECT = process.env.SHELL_PROJECT || SHELL_DEFAULT_PROJECT

// Default-mode discoverability contract: exactly the first two graph sections, in
// canonical order. The map section (GRAPH_SHELL_NAV_DEFS[2]) must NOT be advertised.
const DEFAULT_SECTIONS = GRAPH_SHELL_NAV_DEFS.filter((s) => s.id !== 'shell-map')
const MAP_SECTION = GRAPH_SHELL_NAV_DEFS.find((s) => s.id === 'shell-map')
const EXPECTED_NAV = DEFAULT_SECTIONS.map(({ label, href }) => ({ label, href }))
if (EXPECTED_NAV.length !== 2 || !MAP_SECTION) {
  console.error('ERROR [shell-nav-default-gate.mjs] GRAPH_SHELL_NAV_DEFS must define analytics, changes, and a gated code map section.')
  process.exit(1)
}

const MIN_BODY_WIDTH = 240
const MIN_BODY_HEIGHT = 120
const MIN_SHELL_CAPTURE_HEIGHT = 520
const THEME_ATTRIBUTES = ['data-theme', 'data-tb-theme']
const FONTS = [
  '400 16px "Atkinson Hyperlegible"', '700 16px "Atkinson Hyperlegible"',
  '400 16px "Atkinson Hyperlegible Mono"', '500 16px "Atkinson Hyperlegible Mono"',
  '600 16px "Atkinson Hyperlegible Mono"', '700 16px "Atkinson Hyperlegible Mono"',
]

if (!CHROME) {
  console.error('ERROR [shell-nav-default-gate.mjs] CHROME_PATH is unset. Set it to your Chrome/Chromium binary before running the default-mode shell navigation gate.')
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

// Waits for the persistent shell nav to settle on `section` with EXACTLY the
// default-mode two-link nav. Reuses the same active-state invariants as
// shell-nav-gate.mjs: one filled amber pill on the active link, no stray pill on
// the others, mounted non-blank body, correct theme attributes. `expectedPath` /
// `mountSelector` / `bodySelector` / `heading` overrides assert a representative,
// project-scoped drill-down reached by an in-app navigation past the picker.
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
      const activeOk = active.length === 1 && active[0].label === section.label && active[0].isAmberPill
      const inactiveMarkersOk = nav.links.every((link) => link.label === section.label || !link.isAmberPill)
      const mapLeak = nav.links.some((link) => link.label === 'code map' || link.href === '/map')
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
        ok: nav.ok && !mapLeak && path === normalizePath(expectedPath) && activeOk && inactiveMarkersOk && mountOk && bodyOk && headingOk && themeOk,
        path,
        nav,
        mapLeak,
        activeOk,
        inactiveMarkersOk,
        mountOk,
        bodySelector,
        bodyOk,
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
    `Timed out waiting for default-mode ${section.label} at ${expectedPath}. ` +
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

const waitForFonts = async (page) => {
  await page.evaluate(async (faces) => {
    try { await Promise.all(faces.map((f) => document.fonts.load(f))) } catch {}
    await document.fonts.ready
  }, FONTS)
}

// Opens the command palette (the same OPEN_COMMAND_PALETTE_EVENT the header
// button dispatches), waits until it is genuinely populated with per-project
// "· changes" jumps, then asserts the code map is absent from every command:
// no "go to code map" nav command and no per-project "· map" jump. Populated
// per-project "· changes" jumps make the map absence a real gate, not an empty
// or still-loading list.
const assertPaletteHidesMap = async (page, theme, timeoutMs = 15000) => {
  const opened = await page.evaluate(() => {
    window.dispatchEvent(new Event('peasant:open-command-palette'))
    return !!document.querySelector('[aria-label="Command palette"]')
  })
  if (!opened) {
    // The dispatch is synchronous, but the portal render is not — poll briefly.
    const appeared = await page.waitForSelector('[aria-label="Command palette"]', { visible: true, timeout: 5000 }).catch(() => null)
    if (!appeared) throw new Error('The command palette did not open on peasant:open-command-palette in default mode.')
  }

  const start = Date.now()
  let last = null
  while (Date.now() - start < timeoutMs) {
    last = await page.evaluate(() => {
      const dialog = document.querySelector('[aria-label="Command palette"]')
      if (!dialog) return { ok: false, reason: 'command palette dialog missing', labels: [] }
      const list = dialog.querySelector('ul[role="listbox"]')
      if (!list) return { ok: false, reason: 'command palette listbox missing', labels: [] }
      const options = [...list.querySelectorAll('li[role="option"]')]
      const labels = options.map((li) => (li.querySelector('span')?.textContent || li.textContent || '').replace(/\s+/g, ' ').trim())
      const errorShown = !!dialog.querySelector('[role="alert"]')
      const hasProjectChanges = labels.some((l) => /·\s*changes$/.test(l))
      const hasNavGoTo = labels.some((l) => l.toLowerCase() === 'go to analytics' || l.toLowerCase() === 'go to changes')
      return {
        // Populated = at least one per-project "· changes" jump AND the base nav
        // "go to" commands are present. Only then is map absence meaningful.
        populated: hasProjectChanges && hasNavGoTo && !errorShown,
        errorShown,
        hasProjectChanges,
        hasNavGoTo,
        labels,
      }
    })
    if (last.populated) break
    await pause(150)
  }
  if (!last || !last.populated) {
    throw new Error(
      `The default-mode command palette never populated with per-project "· changes" jumps and the base "go to" nav commands, ` +
      `so a map-absence assertion would be vacuous. Last state: ${JSON.stringify(last)}`
    )
  }

  const mapNavLeak = last.labels.filter((l) => l.toLowerCase() === 'go to code map')
  const mapProjectLeak = last.labels.filter((l) => /·\s*map$/.test(l))
  if (mapNavLeak.length || mapProjectLeak.length) {
    throw new Error(
      `Code map entry points leaked into the default-mode command palette. ` +
      `"go to code map": ${JSON.stringify(mapNavLeak)}; per-project "· map": ${JSON.stringify(mapProjectLeak)}. ` +
      `All labels: ${JSON.stringify(last.labels)}`
    )
  }

  // Close the palette so it does not overlay the subsequent shell captures.
  await page.keyboard.press('Escape')
  await page.waitForFunction(() => !document.querySelector('[aria-label="Command palette"]'), { timeout: 5000 }).catch(() => {})
  console.log(`OK default-mode ${theme} command palette populated (${last.labels.length} commands) with NO map entry points`)
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
      bottom: rect.bottom,
    }
  }, bodySelector, MIN_BODY_WIDTH, MIN_BODY_HEIGHT)
  if (!body.ok) {
    throw new Error(`The ${where} shell body is missing or blank: ${body.reason}.`)
  }
  return body
}

const captureShellFrame = async (page, gate, theme, id, bodySelector, where) => {
  const outDir = join(APP_OUT, theme)
  mkdirSync(outDir, { recursive: true })
  const file = join(outDir, `${id}.png`)
  await waitForFonts(page)
  const body = await assertBodyReady(page, id, bodySelector, where)
  await page.evaluate(() => window.scrollTo(0, 0))
  await pause(100)
  const viewport = page.viewport() || { width: 1460, height: 1000 }
  const height = Math.min(viewport.height, Math.max(MIN_SHELL_CAPTURE_HEIGHT, Math.ceil(body.bottom + 48)))
  await page.screenshot({ path: file, clip: { x: 0, y: 0, width: viewport.width, height } })
  const measured = await gate.assert(id, file, { sel: bodySelector, where: 'shell-nav-default-gate.mjs' })
  console.log(`OK ${where} ${theme}/${id} frame=${viewport.width}x${height} body=${bodySelector} ${Math.round(body.width)}x${Math.round(body.height)} nonbg=${(measured.nonbgRatio * 100).toFixed(1)}% colors=${measured.distinctColors}`)
  return file
}

const captureDefaultShell = async (browser) => {
  const captured = []
  for (const theme of SMOKE_THEMES) {
    const page = await browser.newPage()
    await applyDeterminism(page)
    await page.evaluateOnNewDocument((t) => { try { localStorage.setItem('peasant-theme', t) } catch {} }, theme)
    const gate = new SurfaceGate(page)
    const diagnostics = []
    page.on('console', (m) => { if (shouldCaptureConsoleMessage(m)) diagnostics.push(formatConsoleDiagnostic(m)) })
    page.on('pageerror', (e) => diagnostics.push('pageerr: ' + (e.stack || e.message)))

    // 1. analytics — land on the first default section, assert exactly-two nav.
    const analytics = DEFAULT_SECTIONS[0]
    const response = await page.goto(ORIGIN + analytics.href, { waitUntil: 'domcontentloaded' }).catch((e) => {
      throw new Error(`Could not load ${ORIGIN + analytics.href}: ${e.message}`)
    })
    if (!response || response.status() >= 400) {
      throw new Error(`The running app returned HTTP ${response ? response.status() : 0} for ${ORIGIN + analytics.href}.`)
    }
    await waitForSection(page, analytics, theme)
    captured.push(await captureShellFrame(page, gate, theme, analytics.id, analytics.body || analytics.mount, 'peasant-app-default'))

    // 2. command palette — code map absent from nav AND palette while populated.
    await assertPaletteHidesMap(page, theme)

    // 3. changes — click through, drill into a representative project surface.
    const changes = DEFAULT_SECTIONS[1]
    await clickNav(page, changes)
    await waitForSection(page, changes, theme)
    let changesBody = changes.body || changes.mount
    if (changes.representativePath) {
      const path = changes.representativePath(SHELL_PROJECT)
      const drill = await page.goto(ORIGIN + path, { waitUntil: 'domcontentloaded' }).catch((e) => {
        throw new Error(`Could not load representative ${changes.label} surface at ${ORIGIN + path}: ${e.message}`)
      })
      if (!drill || drill.status() >= 400) {
        throw new Error(`The running app returned HTTP ${drill ? drill.status() : 0} for representative ${changes.label} surface ${ORIGIN + path}.`)
      }
      await waitForSection(page, changes, theme, {
        expectedPath: path,
        mountSelector: changes.representativeBody,
        bodySelector: changes.representativeBody,
        heading: null,
      })
      changesBody = changes.representativeBody
    }
    captured.push(await captureShellFrame(page, gate, theme, changes.id, changesBody, 'peasant-app-default'))

    // 4. route preservation — /map/<project> STILL mounts its body directly even
    // though the section is undiscoverable in the nav and palette.
    const mapPath = MAP_SECTION.representativePath(SHELL_PROJECT)
    const mapResponse = await page.goto(ORIGIN + mapPath, { waitUntil: 'domcontentloaded' }).catch((e) => {
      throw new Error(`Could not load direct map route ${ORIGIN + mapPath}: ${e.message}`)
    })
    if (!mapResponse || mapResponse.status() >= 400) {
      throw new Error(`The direct map route returned HTTP ${mapResponse ? mapResponse.status() : 0} for ${ORIGIN + mapPath} — the discoverability gate must never remove routes.`)
    }
    // Poll the mounted body directly (this route is intentionally NOT in the nav,
    // so there is no active-pill invariant to assert here — only route survival).
    const start = Date.now()
    let mapBody = null
    while (Date.now() - start < 15000) {
      try {
        mapBody = await assertBodyReady(page, MAP_SECTION, MAP_SECTION.representativeBody, 'peasant-app-default map route')
        break
      } catch {
        await pause(200)
      }
    }
    if (!mapBody) {
      throw new Error(`The direct /map route body ${MAP_SECTION.representativeBody} never mounted in default mode at ${ORIGIN + mapPath}.`)
    }
    captured.push(await captureShellFrame(page, gate, theme, 'shell-map-route-preserved', MAP_SECTION.representativeBody, 'peasant-app-default'))

    if (diagnostics.length) {
      throw new Error(`Client diagnostics appeared while driving the ${theme} default-mode peasant shell: ${JSON.stringify(diagnostics.slice(0, 4))}`)
    }
    await page.close()
  }
  return captured
}

// Fail-fast coordinate check BEFORE Puppeteer boots — see validate-mock-coordinates.mjs.
try {
  await assertKnownProject(ORIGIN, SHELL_PROJECT, { where: 'shell-nav-default-gate.mjs' })
} catch (e) {
  console.error(e.message)
  process.exit(2)
}

const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: { width: 1460, height: 1000, deviceScaleFactor: 1 } })

let captured = []
try {
  captured = await captureDefaultShell(browser)
} catch (e) {
  await fail(
    browser,
    `ERROR [shell-nav-default-gate.mjs] default-mode graph shell gate failed.\n` +
    `  What failed: ${e.message}\n` +
    `  Why: a default (non-experimental) server must hide the code map from the nav AND the command palette while keeping the /map routes directly mounted.\n` +
    `  Where: shell-nav-default-gate.mjs while driving the default-mode peasant app at ${ORIGIN}.\n` +
    `  Means: users on a default build may see a leaked code-map entry point, or a route that should stay mounted has regressed.\n` +
    `  Fix: boot ./bin/peasant web start WITHOUT --experimental (with the shell mock store) at PEASANT_REAL_ORIGIN, then fix the reported nav/palette/route wiring.`
  )
}

await browser.close()
console.log(`\nOK [shell-nav-default-gate.mjs] default-mode shell verified: nav = ${EXPECTED_NAV.map((n) => n.label).join(' · ')}; code map absent from nav + palette; /map route preserved.`)
console.log(`Default-mode shell frames captured for ${SMOKE_THEMES.length} themes:`)
for (const file of captured) console.log(`  ${file}`)
