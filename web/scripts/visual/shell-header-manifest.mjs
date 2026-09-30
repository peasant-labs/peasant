/* The local shell manifest: loader and checks shared by the component tests and the mounted gates.

   testdata/shell-header.yaml names what the local app header must carry, what must not come
   back, which route-only sections must still resolve, and which palette commands are forbidden
   or required. This module is the one reader of that file. The DOM checks are self-contained
   functions (no imports, no closures) so the same code runs under jsdom in Vitest and in a real
   browser through puppeteer's page.evaluate.
 */
import { existsSync, readFileSync } from 'node:fs'
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

  const hrefs = [...header.querySelectorAll('a[href]')].map((link) => new URL(link.getAttribute('href'), 'http://local.invalid').pathname.replace(/\/+$/, '') || '/')
  for (const path of Object.keys(manifest.routes)) {
    if (hrefs.some((href) => href === path || href.startsWith(`${path}/`))) failures.push(`route ${path}: the header links to it`)
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
