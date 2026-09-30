/* The local shell manifest: loader and checks shared by the component tests and the mounted gates.

   testdata/shell-header.yaml names what the local app header must carry, what must not come
   back, which route-only sections must still resolve, and which palette commands are forbidden
   or required. This module is the one reader of that file. The DOM checks are self-contained
   functions (no imports, no closures) so the same code runs under jsdom in Vitest and in a real
   browser through puppeteer's page.evaluate.
 */
import { existsSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import YAML from 'yaml'

const HERE = dirname(fileURLToPath(import.meta.url))
export const WEB_ROOT = resolve(HERE, '..', '..')
export const SHELL_HEADER_FIXTURE = join(HERE, 'testdata', 'shell-header.yaml')

/** The names the manifest must carry. A row deleted from the fixture fails the loader. */
export const REQUIRED_NAMES = Object.freeze({
  show: Object.freeze(['brand', 'search', 'theme', 'settings']),
  hide: Object.freeze(['connection-pill', 'share-button', 'section-nav']),
  routes: Object.freeze(['/analytics', '/review', '/map']),
  paletteForbid: Object.freeze(['proj-changes:', 'proj-map:']),
  paletteRequire: Object.freeze(['nav:/', 'action:theme']),
})

/**
 * @typedef {{ selector: string, text?: string, labels?: { light: string, dark: string }, page?: string }} ShowItem
 * @typedef {{ selector?: string, text?: string[] }} HideItem
 * @typedef {{ mount: string, project?: boolean }} RouteItem
 * @typedef {{ forbid: string[], require: string[] }} PaletteRules
 * @typedef {{ show: Record<string, ShowItem>, hide: Record<string, HideItem>, routes: Record<string, RouteItem>, palette: PaletteRules }} ShellHeaderManifest
 */

const fail = (what) => {
  throw new Error(`${SHELL_HEADER_FIXTURE}: ${what}`)
}

const record = (value, path) => {
  if (!value || typeof value !== 'object' || Array.isArray(value)) fail(`${path} must be a mapping`)
  return value
}

const exactKeys = (value, allowed, required, path) => {
  const unknown = Object.keys(value).filter((key) => !allowed.includes(key))
  if (unknown.length) fail(`${path} has unknown fields: ${unknown.join(', ')}`)
  const missing = required.filter((key) => !(key in value))
  if (missing.length) fail(`${path} is missing required fields: ${missing.join(', ')}`)
}

const nonEmptyString = (value, path) => {
  if (typeof value !== 'string' || value.trim() === '') fail(`${path} must be a non-empty string`)
  return value
}

const stringList = (value, path) => {
  if (!Array.isArray(value) || value.length === 0) fail(`${path} must be a non-empty list`)
  value.forEach((entry, index) => nonEmptyString(entry, `${path}[${index}]`))
  return value
}

const requireNames = (section, names, path) => {
  const missing = names.filter((name) => !(name in section))
  if (missing.length) fail(`${path} is missing required names: ${missing.join(', ')}`)
}

/**
 * Parses and validates the manifest. Strict: unknown fields, missing required names and
 * malformed rows all throw.
 * @param {string} [source]
 * @returns {ShellHeaderManifest}
 */
export function loadShellHeaderManifest(source = readFileSync(SHELL_HEADER_FIXTURE, 'utf8')) {
  const document = YAML.parseDocument(source, { strict: true, uniqueKeys: true })
  if (document.errors.length) fail(`invalid YAML: ${document.errors.map((error) => error.message).join('; ')}`)
  const root = record(document.toJS(), 'root')
  exactKeys(root, ['show', 'hide', 'routes', 'palette'], ['show', 'hide', 'routes', 'palette'], 'root')

  const show = record(root.show, 'show')
  requireNames(show, REQUIRED_NAMES.show, 'show')
  for (const [name, value] of Object.entries(show)) {
    const item = record(value, `show.${name}`)
    exactKeys(item, ['selector', 'text', 'labels', 'page'], ['selector'], `show.${name}`)
    nonEmptyString(item.selector, `show.${name}.selector`)
    if ('text' in item) nonEmptyString(item.text, `show.${name}.text`)
    if ('page' in item) nonEmptyString(item.page, `show.${name}.page`)
    if ('labels' in item) {
      const labels = record(item.labels, `show.${name}.labels`)
      exactKeys(labels, ['light', 'dark'], ['light', 'dark'], `show.${name}.labels`)
      nonEmptyString(labels.light, `show.${name}.labels.light`)
      nonEmptyString(labels.dark, `show.${name}.labels.dark`)
    }
  }

  const hide = record(root.hide, 'hide')
  requireNames(hide, REQUIRED_NAMES.hide, 'hide')
  for (const [name, value] of Object.entries(hide)) {
    const item = record(value, `hide.${name}`)
    exactKeys(item, ['selector', 'text'], [], `hide.${name}`)
    if (!('selector' in item) && !('text' in item)) fail(`hide.${name} needs a selector or text to detect the item by`)
    if ('selector' in item) nonEmptyString(item.selector, `hide.${name}.selector`)
    if ('text' in item) stringList(item.text, `hide.${name}.text`)
  }

  const routes = record(root.routes, 'routes')
  requireNames(routes, REQUIRED_NAMES.routes, 'routes')
  for (const [path, value] of Object.entries(routes)) {
    if (!/^\/[a-z-]+$/.test(path)) fail(`routes.${path} must be a single-segment absolute path`)
    const item = record(value, `routes.${path}`)
    exactKeys(item, ['mount', 'project'], ['mount'], `routes.${path}`)
    nonEmptyString(item.mount, `routes.${path}.mount`)
    if ('project' in item && typeof item.project !== 'boolean') fail(`routes.${path}.project must be a boolean`)
  }

  const palette = record(root.palette, 'palette')
  exactKeys(palette, ['forbid', 'require'], ['forbid', 'require'], 'palette')
  stringList(palette.forbid, 'palette.forbid')
  stringList(palette.require, 'palette.require')
  const missingForbid = REQUIRED_NAMES.paletteForbid.filter((id) => !palette.forbid.includes(id))
  if (missingForbid.length) fail(`palette.forbid is missing required ids: ${missingForbid.join(', ')}`)
  const missingRequire = REQUIRED_NAMES.paletteRequire.filter((id) => !palette.require.includes(id))
  if (missingRequire.length) fail(`palette.require is missing required ids: ${missingRequire.join(', ')}`)

  return /** @type {ShellHeaderManifest} */ (root)
}

/**
 * Per show item, whether the page it links to exists in this web app's source. An item with no
 * `page` is always expected.
 * @param {ShellHeaderManifest} manifest
 * @returns {Record<string, boolean>}
 */
export function shippedItems(manifest, webRoot = WEB_ROOT) {
  return Object.fromEntries(
    Object.entries(manifest.show).map(([name, item]) => [name, item.page === undefined || existsSync(join(webRoot, item.page))]),
  )
}

/**
 * The source file of the page that serves a route-only path, or null when the app has none.
 * @param {string} path
 */
export function routePageFile(path, webRoot = WEB_ROOT) {
  const candidates = [`src/app${path}/page.tsx`, `src/app${path}/[[...segments]]/page.tsx`]
  return candidates.find((candidate) => existsSync(join(webRoot, candidate))) ?? null
}

/**
 * Checks the mounted header against the manifest and returns every failure (empty when it holds).
 * Self-contained so it can run in a browser via page.evaluate.
 * @param {ShellHeaderManifest} manifest
 * @param {{ theme: 'light' | 'dark', shipped: Record<string, boolean> }} context
 * @returns {string[]}
 */
export function headerFailures(manifest, context) {
  const failures = []
  const header = document.querySelector('header')
  if (!header) return ['no <header> is mounted']
  const textOf = (element) => (element.textContent || '').replace(/\s+/g, ' ').trim()

  for (const [name, item] of Object.entries(manifest.show)) {
    const element = document.querySelector(item.selector)
    if (context.shipped[name] === false) {
      if (element) failures.push(`${name}: shows, but ${item.page} does not exist, so the link is dead`)
      continue
    }
    if (!element) {
      failures.push(`${name}: missing (${item.selector})`)
      continue
    }
    if (item.text !== undefined && textOf(element) !== item.text) {
      failures.push(`${name}: reads ${JSON.stringify(textOf(element))}, expected ${JSON.stringify(item.text)}`)
    }
    if (item.labels) {
      const expected = item.labels[context.theme]
      const actual = element.getAttribute('aria-label')
      if (actual !== expected) failures.push(`${name}: labelled ${JSON.stringify(actual)} in the ${context.theme} theme, expected ${JSON.stringify(expected)}`)
    }
  }

  const headerText = textOf(header)
  for (const [name, item] of Object.entries(manifest.hide)) {
    if (item.selector && document.querySelector(item.selector)) failures.push(`${name}: present (${item.selector})`)
    for (const text of item.text || []) {
      if (headerText.includes(text)) failures.push(`${name}: the header reads ${JSON.stringify(text)}`)
    }
  }

  // The persistent chrome is the header and, while it shows, the offline notice under it: neither
  // may link to a route-only section. Page bodies may (a code-map breadcrumb links to /map).
  const chrome = [header, document.querySelector('section[aria-label="peasant is not running"]')].filter(Boolean)
  const hrefs = chrome.flatMap((part) => [...part.querySelectorAll('a[href]')]).map((link) => new URL(link.getAttribute('href'), 'http://local.invalid').pathname.replace(/\/+$/, '') || '/')
  for (const path of Object.keys(manifest.routes)) {
    if (hrefs.some((href) => href === path || href.startsWith(`${path}/`))) failures.push(`route ${path}: the persistent chrome links to it`)
  }
  return failures
}

/**
 * Checks the open command palette's commands against the manifest's palette rules and returns
 * every failure. Self-contained so it can run in a browser via page.evaluate.
 * @param {ShellHeaderManifest} manifest
 * @returns {string[]}
 */
export function paletteFailures(manifest) {
  const dialog = document.querySelector('[role="dialog"][aria-label="Command palette"]')
  if (!dialog) return ['the command palette is not open']
  const ids = [...dialog.querySelectorAll('[data-command-id]')].map((element) => element.getAttribute('data-command-id') || '')
  const failures = []
  for (const prefix of manifest.palette.forbid) {
    const offending = ids.filter((id) => id.startsWith(prefix))
    if (offending.length) failures.push(`the palette offers ${offending.join(', ')}`)
  }
  for (const id of manifest.palette.require) {
    if (!ids.includes(id)) failures.push(`the palette does not offer ${id}`)
  }
  const routeLinks = ids.filter((id) => Object.keys(manifest.routes).some((path) => id === `nav:${path}` || id.startsWith(`nav:${path}/`)))
  if (routeLinks.length) failures.push(`the palette links to route-only sections: ${routeLinks.join(', ')}`)
  return failures
}

/* ── Browser-only probes ─────────────────────────────────────────────────────────────────────────
   These read layout (rects, hit-testing, computed styles), which jsdom does not compute, so only the
   mounted gates run them. Self-contained for page.evaluate like the checks above. */

/**
 * Checks the header's geometry: one row of fairtrade's --nav-h at the current width, no horizontal
 * overflow of the header itself (the page body's width is its own page's concern), and every item the manifest expects visible, inside the header row, and reachable by a
 * pointer (the element at its centre is the item or inside it). Returns every failure.
 * @param {ShellHeaderManifest} manifest
 * @param {{ shipped: Record<string, boolean> }} context
 * @returns {string[]}
 */
export function headerGeometryFailures(manifest, context) {
  const failures = []
  const header = document.querySelector('header')
  if (!header) return ['no <header> is mounted']
  const navHeight = Number.parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--nav-h'))
  const box = header.getBoundingClientRect()
  if (!Number.isFinite(navHeight) || navHeight <= 0) failures.push('--nav-h is not defined on the root')
  else if (Math.abs(box.height - navHeight) > 0.5) failures.push(`the header is ${box.height}px tall, not one ${navHeight}px row`)
  if (box.left < 0 || box.right > window.innerWidth + 0.5) failures.push(`the header spans ${box.left}..${box.right}px in a ${window.innerWidth}px viewport`)
  if (header.scrollWidth > header.clientWidth) failures.push(`the header overflows: scrollWidth ${header.scrollWidth} > clientWidth ${header.clientWidth}`)

  for (const [name, item] of Object.entries(manifest.show)) {
    if (context.shipped[name] === false) continue
    const element = document.querySelector(item.selector)
    if (!element) continue // headerFailures reports a missing item
    const rect = element.getBoundingClientRect()
    const style = getComputedStyle(element)
    if (style.display === 'none' || style.visibility === 'hidden' || rect.width === 0 || rect.height === 0) {
      failures.push(`${name}: not visible`)
      continue
    }
    if (rect.left < 0 || rect.right > window.innerWidth + 0.5) failures.push(`${name}: clipped horizontally (${rect.left}..${rect.right}px)`)
    if (rect.top < box.top - 0.5 || rect.bottom > box.bottom + 0.5) failures.push(`${name}: outside the header row (${rect.top}..${rect.bottom}px vs ${box.top}..${box.bottom}px)`)
    const x = Math.max(0, Math.min(window.innerWidth - 1, rect.left + rect.width / 2))
    const y = Math.max(0, Math.min(window.innerHeight - 1, rect.top + rect.height / 2))
    const hit = document.elementFromPoint(x, y)
    if (!hit || (hit !== element && !element.contains(hit))) failures.push(`${name}: covered at its centre by ${hit ? hit.tagName.toLowerCase() + (hit.className ? '.' + String(hit.className).split(/\s+/).join('.') : '') : 'nothing'}`)
  }
  return failures
}

/**
 * Checks that the page body clears the chrome above it. Only the one-row header is fixed; the
 * offline notice, while it shows, sits under it at the top of the page and scrolls with it (so it is
 * never position:fixed, and at scroll 0 its top is the header's bottom). <main>'s top padding must
 * equal the header height plus the notice height (--app-header-height), or the header height alone
 * when no notice shows. Returns every failure, and the measured heights for the log.
 * @returns {{ failures: string[], chrome: number, header: number, notice: number, mainPaddingTop: number }}
 */
export function chromeClearance() {
  const failures = []
  const header = document.querySelector('header')
  const main = document.querySelector('main')
  if (!header || !main) return { failures: ['the header or <main> is not mounted'], chrome: 0, header: 0, notice: 0, mainPaddingTop: 0 }
  const headerBox = header.getBoundingClientRect()
  const headerHeight = headerBox.height
  if (getComputedStyle(header).position !== 'fixed') failures.push(`the header is position:${getComputedStyle(header).position}, not fixed`)
  const section = document.querySelector('section[aria-label="peasant is not running"]')
  // The host wrapper around fairtrade's banner is what the page positions and measures.
  const notice = section ? section.parentElement : null
  let noticeHeight = 0
  if (notice) {
    const box = notice.getBoundingClientRect()
    noticeHeight = box.height
    for (let el = notice; el && el !== document.documentElement; el = el.parentElement) {
      if (getComputedStyle(el).position === 'fixed') {
        failures.push(`the offline notice rides in a fixed element (${el.tagName.toLowerCase()}); it must scroll with the page`)
        break
      }
    }
    if (header.contains(notice)) failures.push('the offline notice is inside the header')
    const documentTop = box.top + window.scrollY
    if (Math.abs(documentTop - headerHeight) > 1) failures.push(`the notice starts ${documentTop}px down the page, not at the ${headerHeight}px header bottom`)
  }
  const mainPaddingTop = Number.parseFloat(getComputedStyle(main).paddingTop)
  const expected = headerHeight + noticeHeight
  if (Math.abs(mainPaddingTop - expected) > 1) {
    failures.push(`<main> clears ${mainPaddingTop}px but the header${notice ? ' plus the notice' : ''} is ${expected}px`)
  }
  return { failures, chrome: expected, header: headerHeight, notice: noticeHeight, mainPaddingTop }
}

/* ── Node-side: build provenance ────────────────────────────────────────────────────────────────── */

/** Markers only this shell brings into the served bundle: the notice's height variable, the
 * fairtrade banner's accessible name, and the host live region's recovery announcement. */
export const SHELL_PROVENANCE_MARKERS = Object.freeze(['--app-notice-height', 'peasant is not running', 'peasant is running again'])

const chunksOf = (html) => [...new Set(html.match(/\/_next\/static\/chunks\/[^"']+\.js/g) || [])].sort()

/**
 * Proves the server at `origin` serves this checkout's web/out before a gate trusts a capture:
 * the served page references exactly the chunks web/out/index.html does, the served chunks carry
 * every marker, and (when `bin` is given) the binary is not older than web/out. Throws with the
 * mismatch; returns the evidence for the log.
 * @param {{ origin: string, markers?: readonly string[], bin?: string, webRoot?: string }} options
 * @returns {Promise<{ chunks: string[], markerChunks: Record<string, string[]> }>}
 */
export async function assertServedBuild({ origin, markers = SHELL_PROVENANCE_MARKERS, bin, webRoot = WEB_ROOT }) {
  const outIndex = join(webRoot, 'out', 'index.html')
  if (!existsSync(outIndex)) throw new Error(`${outIndex} is missing; run make build`)
  if (bin) {
    const binTime = statSync(bin).mtimeMs
    if (binTime < statSync(outIndex).mtimeMs) throw new Error(`${bin} is older than ${outIndex}: the binary embeds an earlier web build; run make build`)
  }
  const localChunks = chunksOf(readFileSync(outIndex, 'utf8'))
  const servedChunks = chunksOf(await (await fetch(`${origin}/`)).text())
  if (JSON.stringify(servedChunks) !== JSON.stringify(localChunks)) {
    throw new Error(`the server at ${origin} serves chunks ${JSON.stringify(servedChunks)}, not this checkout's web/out ${JSON.stringify(localChunks)}; it is a stale server or another checkout`)
  }
  const bodies = await Promise.all(servedChunks.map(async (chunk) => [chunk, await (await fetch(`${origin}${chunk}`)).text()]))
  const markerChunks = Object.fromEntries(markers.map((marker) => [marker, bodies.filter(([, body]) => body.includes(marker)).map(([chunk]) => chunk)]))
  const missing = markers.filter((marker) => markerChunks[marker].length === 0)
  if (missing.length) throw new Error(`the bundle served at ${origin} lacks ${JSON.stringify(missing)}: it predates this shell`)
  return { chunks: servedChunks, markerChunks }
}
