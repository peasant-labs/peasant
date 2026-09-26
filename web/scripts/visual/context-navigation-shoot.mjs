/* MOUNTED CURRENT-PARENT / CONTEXT NAVIGATION — real-binary evidence capture.
 *
 * Boots the exact worktree's `bin/peasant` (assets embedded from this branch's
 * `web/out`) with the mock data store, so the child transcript, its stored
 * context_from source and started_by parent, and its retained earlier-history
 * partition all arrive over the REAL WebSocket `session_detail` payload through
 * the real store -> decoration boundary -> adapter -> composite path. Only the
 * theme and the reading actions are driven from here.
 *
 * Surfaces, per theme:
 *   context-links      the child renders a context-source link and a
 *                      started-by (current-parent) link to two DIFFERENT stored
 *                      sessions, the retained earlier history starts collapsed,
 *                      the disclosure toggle does not re-position the stream,
 *                      and expanding it writes the section to the route.
 *   context-child-scrolled
 *                      the child with a real reading position: the stream
 *                      scrolled, a selected turn, and a typed search query.
 *   parent-session     following the current-parent link opens the EXACT stored
 *                      parent session with its own content.
 *   context-back-restored
 *                      Back returns to the child with its query, retained-history
 *                      disclosure, selected turn, search, and inner stream
 *                      offset restored.
 *   context-reloaded   a reload of the disclosed route keeps the disclosure open.
 *   context-copied-link
 *                      a fresh document opened at the copied disclosed route
 *                      keeps the disclosure open (no session storage to rely on).
 *   unresolved-parent  a child whose current-parent target is no longer stored
 *                      renders an honest unavailable reference (no link) and
 *                      stays readable.
 *
 * Build provenance is asserted BEFORE any capture: the change marker must be
 * present both in the built artifact on disk and in the chunk the server
 * actually serves, so a stale export or the wrong worktree cannot silently
 * invalidate the evidence.
 *
 * Run:  CHROME_PATH=$(command -v google-chrome) node web/scripts/visual/context-navigation-shoot.mjs
 * Env:  PEASANT_CONTEXT_NAV_SKIP_BUILD=1   reuse the existing bin/peasant
 *       PEASANT_CONTEXT_NAV_PORT            server port (default 8793)
 *       PEASANT_CONTEXT_NAV_CAPTURE_DIR     output root (default /tmp/opencode/context-navigation-captures)
 *       PUPPETEER_CORE                      explicit puppeteer-core module path
 */
import { execSync, spawn } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join, relative, resolve } from 'node:path'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'
import { loadContextNavigationFixture } from './context-navigation-fixture.mjs'
import { SMOKE_MOCKS, SMOKE_THEMES } from './smoke-surfaces.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '../../..')
const WEB = join(REPO, 'web')
const BIN = join(REPO, 'bin/peasant')
const OUT = process.env.PEASANT_CONTEXT_NAV_CAPTURE_DIR || '/tmp/opencode/context-navigation-captures'
const PORT = process.env.PEASANT_CONTEXT_NAV_PORT || '8793'
const ORIGIN = `http://localhost:${PORT}`
const CHROME = process.env.CHROME_PATH || 'google-chrome'
const MOCKS = SMOKE_MOCKS
const FIXTURE = loadContextNavigationFixture()

/** Feature bytes only this change introduces, located in the binary + served chunk. */
const FEATURE_BYTES = ['peasant:transcript-reading:']
const LINK_ACTION = 'open current session'
const SEARCH_TEXT = 'durable'
const FONTS = [
  '400 16px "Atkinson Hyperlegible"', '700 16px "Atkinson Hyperlegible"',
  '400 16px "Atkinson Hyperlegible Mono"', '600 16px "Atkinson Hyperlegible Mono"',
]
const THEME_ATTRIBUTES = ['data-theme', 'data-tb-theme']
const CHILD_PATH = `/projects/${FIXTURE.projectHash}/${encodeURIComponent(FIXTURE.childSessionId)}/`
const UNRESOLVED_PATH = `/projects/${FIXTURE.projectHash}/${encodeURIComponent(FIXTURE.unresolvedChildId)}/`
/** The per-session reading record key the host writes; also this change's provenance marker. */
const READING_STATE_KEY = (sessionId) => `${FEATURE_BYTES[0]}${sessionId}`
const pause = (ms) => new Promise((r) => setTimeout(r, ms))

if (!existsSync(CHROME) && !process.env.CHROME_PATH) {
  console.error('ERROR [context-navigation-shoot] CHROME_PATH is unset and google-chrome is not on PATH.')
  process.exit(1)
}

const fail = (step, reason) => new Error(
  `Mounted context navigation capture failed because ${reason} during ${step} in context-navigation-shoot.mjs; the link/Back exit is not proven on this build; inspect the served route and internal/mock/testdata/context_navigation.yaml, fix the production path, rebuild, and rerun.`,
)

function filesBelow(directory) {
  if (!existsSync(directory)) return []
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name)
    return entry.isDirectory() ? filesBelow(path) : [path]
  })
}

/** Prove the served artifact carries this change and not a stale export. */
function assertBuildProvenance() {
  const chunks = join(WEB, 'out/_next/static/chunks')
  if (!existsSync(BIN) || !existsSync(chunks)) throw fail('verifying build provenance', `missing ${relative(REPO, BIN)} or exported chunks; run make build in this worktree first`)
  const javascript = filesBelow(chunks).filter((path) => path.endsWith('.js')).map((path) => ({ path, content: readFileSync(path, 'utf8') }))
  const binary = readFileSync(BIN)
  const missingBinaryBytes = FEATURE_BYTES.filter((signature) => !binary.includes(Buffer.from(signature)))
  if (missingBinaryBytes.length) throw fail('verifying build provenance', `the embedded binary contains no ${missingBinaryBytes.join(', ')} feature bytes; rebuild this exact worktree so bin/peasant matches web/out`)
  const match = javascript.find(({ content }) => FEATURE_BYTES.every((signature) => content.includes(signature)))
  if (!match) throw fail('verifying build provenance', `the built export contains no chunk with the feature bytes ${FEATURE_BYTES.join(', ')}; rebuild this exact worktree`)
  return match.path
}

async function assertServedChunkCarriesMarker(chunkPath) {
  const servedPath = `/_next/static/chunks/${relative(join(WEB, 'out/_next/static/chunks'), chunkPath).split('\\').join('/')}`
  const response = await fetch(`${ORIGIN}${servedPath}`).catch(() => null)
  if (!response || response.status !== 200) throw fail('verifying the served artifact', `GET ${servedPath} returned ${response?.status ?? 0}`)
  const body = await response.text()
  if (!FEATURE_BYTES.every((signature) => body.includes(signature))) {
    throw fail('verifying the served artifact', `the served chunk ${servedPath} does not carry ${FEATURE_BYTES.join(', ')}; the server is not serving the built export under test`)
  }
  return servedPath
}

async function useTheme(page, theme) {
  await page.evaluateOnNewDocument((value) => {
    try { localStorage.setItem('peasant-theme', value) } catch { /* storage disabled */ }
  }, theme)
}

async function assertTheme(page, theme, step) {
  const attrs = await page.evaluate((names) => Object.fromEntries(names.map((name) => [name, document.documentElement.getAttribute(name)])), THEME_ATTRIBUTES)
  if (!THEME_ATTRIBUTES.every((attribute) => attrs[attribute] === theme)) throw fail(step, `theme attributes were ${JSON.stringify(attrs)} instead of ${JSON.stringify(theme)}`)
}

async function assertAtkinson(page, step) {
  await page.evaluate(async (faces) => { try { await Promise.all(faces.map((face) => document.fonts.load(face))) } catch { /* already loaded */ } ; await document.fonts.ready }, FONTS)
  if (!await page.evaluate(() => document.fonts.check('16px "Atkinson Hyperlegible"'))) throw fail(step, 'Atkinson Hyperlegible was not loaded from the layout head')
  const family = await page.evaluate(() => getComputedStyle(document.body).fontFamily)
  if (!/Atkinson/i.test(family)) throw fail(step, `body font-family ${JSON.stringify(family)} does not lead with Atkinson Hyperlegible`)
}

async function waitForSelector(page, selector, step, timeout = 20000) {
  const element = await page.waitForSelector(selector, { visible: true, timeout }).catch(() => null)
  if (!element) throw fail(step, `selector ${JSON.stringify(selector)} never mounted; current URL is ${page.url()}`)
  return element
}

async function waitForPath(page, expected, step) {
  const reached = await page.waitForFunction((value) => window.location.pathname.includes(value), { timeout: 20000 }, expected).catch(() => null)
  if (!reached) throw fail(step, `navigation never reached a path containing ${JSON.stringify(expected)}; current URL is ${page.url()}`)
}

async function capture(page, gate, theme, id, selector, evidence) {
  const element = await waitForSelector(page, selector, `capturing ${id}`)
  const directory = join(OUT, theme)
  mkdirSync(directory, { recursive: true })
  const file = join(directory, `${id}.png`)
  await element.screenshot({ path: file, captureBeyondViewport: false })
  await gate.assert(`${theme}/${id}`, file, { sel: selector, where: 'context-navigation-shoot.mjs' })
  evidence.captures.push(file)
  return file
}

/** The child's rendered context rows: label, whether a link is offered, its status text. */
async function contextRows(page) {
  return page.evaluate(() => [...document.querySelectorAll('.txn-context-source')].map((row) => ({
    label: row.querySelector('.txn-context-row span:not(.txn-context-status)')?.textContent ?? null,
    link: row.querySelector('.txn-context-link')?.textContent ?? null,
    status: row.querySelector('.txn-context-status')?.textContent ?? null,
  })))
}

/**
 * The mounted reading position: the route, the stream offset, the rendered
 * active turn, and the per-turn layout the viewer derives that highlight from.
 *
 * The fairtrade viewer derives the active turn inside its OWN scroll handler:
 * the last visible turn whose `offsetTop - scrollTop` is within 40% of the
 * scroller's height. Reading the highlight without that context is what made an
 * earlier revision of this harness report a mismatch — a search-match jump
 * re-positions the stream without firing the derivation, so the highlight is
 * stale until the next scroll event (which the Back replay itself is).
 */
async function readStreamPosition(page) {
  return page.evaluate(() => {
    const stream = document.querySelector('.txn-stream')
    const active = document.querySelector(".txn-turnwrap:has(.txn-turn.txn-active)") ?? document.querySelector('.txn-turn.txn-active')?.closest('.txn-turnwrap')
    return {
      search: window.location.search,
      scrollTop: stream ? stream.scrollTop : -1,
      scrollHeight: stream ? stream.scrollHeight : -1,
      maxScroll: stream ? stream.scrollHeight - stream.clientHeight : 0,
      clientHeight: stream ? stream.clientHeight : -1,
      turn: active?.getAttribute('data-turn') ?? null,
      turns: [...document.querySelectorAll('.txn-turnwrap[data-turn]')].map((node) => ({
        index: node.getAttribute('data-turn'),
        offsetTop: node.offsetTop,
        viewportTop: Math.round(node.getBoundingClientRect().top),
      })),
    }
  })
}

/** The viewer's own active-turn derivation for one sampled position. */
function viewerDerivedTurn(position) {
  let derived = position.turns[0]?.index ?? '0'
  for (const turn of position.turns) {
    if (turn.offsetTop - position.scrollTop <= position.clientHeight * 0.4) derived = turn.index
  }
  return derived
}

/**
 * Read the mounted position until the highlight and the offset hold steady
 * across two reads, so a comparison is always settled-to-settled: the Back
 * replay re-applies the offset over several frames, and the viewer re-derives
 * the highlight from the offset on each of them.
 */
async function settledStreamPosition(page) {
  let previous = await readStreamPosition(page)
  for (let attempt = 0; attempt < 12; attempt += 1) {
    await pause(200)
    const current = await readStreamPosition(page)
    if (current.turn === previous.turn && current.scrollTop === previous.scrollTop) return current
    previous = current
  }
  return previous
}

async function openSearch(page, step) {
  await page.keyboard.down('Control')
  await page.keyboard.press('f')
  await page.keyboard.up('Control')
  return waitForSelector(page, '.txn-search-input', step)
}

async function runTheme(browser, theme, chunkPath, gate, evidence) {
  const page = await browser.newPage()
  try {
    await applyDeterminism(page)
    await page.setViewport({ width: 1460, height: 1000, deviceScaleFactor: 1 })
    await useTheme(page, theme)
    const servedPath = await assertServedChunkCarriesMarker(chunkPath)
    evidence.servedChunks.push(servedPath)

    /* ── surface 1: the child's stored context and current-parent links ── */
    const response = await page.goto(`${ORIGIN}${CHILD_PATH}`, { waitUntil: 'domcontentloaded' })
    if (response?.status() !== 200) throw fail('opening the child transcript', `HTTP status was ${response?.status() ?? 0}`)
    await waitForSelector(page, '.txn-app', 'mounting the child transcript')
    await waitForSelector(page, '.txn-context-link', 'rendering the stored source and parent links')
    await assertTheme(page, theme, 'capturing the child context links')
    await assertAtkinson(page, 'capturing the child context links')

    const rows = await contextRows(page)
    if (rows.length !== 2) throw fail('rendering the child context rows', `rendered ${JSON.stringify(rows)}, expected the context-source and started-by rows`)
    if (rows[0].label !== FIXTURE.contextLabel || rows[1].label !== FIXTURE.starterLabel) {
      throw fail('rendering the child context rows', `labels ${JSON.stringify(rows.map((row) => row.label))} are not ${JSON.stringify([FIXTURE.contextLabel, FIXTURE.starterLabel])}`)
    }
    if (rows.some((row) => row.link !== LINK_ACTION)) throw fail('rendering the child context rows', `link controls ${JSON.stringify(rows.map((row) => row.link))} are not ${JSON.stringify(LINK_ACTION)}`)

    const collapsed = await page.$eval('.txn-earlier-toggle', (node) => node.getAttribute('aria-expanded'))
    if (collapsed !== 'false') throw fail('capturing the child context links', `retained history was disclosed without a reader action (aria-expanded=${collapsed})`)
    evidence.child = { theme, rows, collapsed }
    // The links and the collapsed disclosure at the top of the stream.
    await capture(page, gate, theme, 'context-links', '.txn-app', evidence)

    /* ── surface 2: the disclosure toggle does not re-position the stream ── */
    const scrolled = await page.evaluate(() => {
      const stream = document.querySelector('.txn-stream')
      if (!stream) return null
      const max = stream.scrollHeight - stream.clientHeight
      stream.scrollTop = Math.floor(max / 2)
      stream.dispatchEvent(new Event('scroll', { bubbles: true }))
      return { target: stream.scrollTop, max }
    })
    if (!scrolled || scrolled.max < 200 || scrolled.target < 100) throw fail('scrolling the child transcript', `the inner stream does not overflow enough to prove restoration (${JSON.stringify(scrolled)})`)
    // Compare SETTLED offsets, and compare them in the frame the reader sees.
    // The disclosed section is inserted ABOVE the scrolled content, so the
    // browser's scroll anchoring moves the numeric offset by exactly that
    // insertion to hold the reader's view: the assertion is that the view held
    // (turn positions unchanged) and that the ONLY numeric movement is the
    // inserted height — never a host-requested jump to another position.
    const anchorOf = (position) => position.turns.map((turn) => turn.viewportTop)
    const beforeToggle = await settledStreamPosition(page)
    // A DOM click, deliberately: puppeteer's `page.click` scrolls the element
    // into view first, which moves the reader before the app is even asked.
    await page.evaluate(() => document.querySelector('.txn-earlier-toggle')?.click())
    await page.waitForFunction(() => document.querySelector('.txn-earlier-toggle')?.getAttribute('aria-expanded') === 'true', { timeout: 10000 })
      .catch(() => { throw fail('expanding the retained earlier history', 'the disclosure did not expand') })
    await waitForSelector(page, '.txn-earlier', 'rendering the disclosed retained history')
    const afterToggle = await settledStreamPosition(page)
    const toggleQuery = new URLSearchParams(afterToggle.search)
    const beforeQuery = new URLSearchParams(beforeToggle.search)
    if (toggleQuery.get('earlier') !== 'earlier-0') throw fail('persisting the retained-history disclosure', `the route ${JSON.stringify(afterToggle.search)} does not name the disclosed section`)
    // The route may differ ONLY in the retained-history parameters: a `turn`
    // target or any re-scoped/re-originated parameter would be the host asking
    // the viewer to move the stream, which this disclosure must never do.
    const routeKeys = new Set([...beforeQuery.keys(), ...toggleQuery.keys()])
    for (const key of routeKeys) {
      if (key === 'earlier') continue
      const beforeValues = beforeQuery.getAll(key)
      const afterValues = toggleQuery.getAll(key)
      if (beforeValues.join(' ') !== afterValues.join(' ')) {
        throw fail('persisting the retained-history disclosure', `disclosing a section changed the route parameter ${key}: ${JSON.stringify(beforeValues)} -> ${JSON.stringify(afterValues)}`)
      }
    }
    const insertedHeight = afterToggle.scrollHeight - beforeToggle.scrollHeight
    const offsetDelta = afterToggle.scrollTop - beforeToggle.scrollTop
    if (!Number.isFinite(insertedHeight) || insertedHeight <= 0) {
      throw fail('persisting the retained-history disclosure', `the disclosed section rendered no measurable height (scrollHeight ${beforeToggle.scrollHeight} -> ${afterToggle.scrollHeight})`)
    }
    // The disclosed section is inserted ABOVE the scrolled content, so the ONLY
    // movement the reader may see is that insertion: either the browser holds
    // the view by moving the numeric offset by the inserted height, or the DOM
    // content shifts down by it (Chrome's scroll-anchoring heuristic decides
    // which, per position). Either way `contentShift + offsetDelta` must equal
    // the inserted height — anything more would be a host- or viewer-initiated
    // jump beyond the disclosure's own content, which is what "without moving
    // the stream position" forbids.
    const beforeAnchor = anchorOf(beforeToggle)
    const afterAnchor = anchorOf(afterToggle)
    if (beforeAnchor.length !== afterAnchor.length) throw fail('persisting the retained-history disclosure', `the disclosed section changed the rendered turn count (${beforeAnchor.length} -> ${afterAnchor.length})`)
    if (beforeAnchor.length === 0 || !beforeAnchor.every(Number.isFinite) || !afterAnchor.every(Number.isFinite)) {
      throw fail('persisting the retained-history disclosure', `the disclosed section left no measurable turn geometry to compare: ${JSON.stringify({ beforeAnchor, afterAnchor })}`)
    }
    const contentShift = afterAnchor[0] - beforeAnchor[0]
    for (let index = 0; index < beforeAnchor.length; index += 1) {
      if (Math.abs((afterAnchor[index] - beforeAnchor[index]) - contentShift) > 2) {
        throw fail('persisting the retained-history disclosure', `disclosing a section moved turn ${index} by ${afterAnchor[index] - beforeAnchor[index]}px while turn 0 moved ${contentShift}px; every turn must move by the same inserted height`)
      }
    }
    if (Math.abs(contentShift + offsetDelta - insertedHeight) > 4) {
      throw fail('persisting the retained-history disclosure', `disclosing a section moved the reader by ${contentShift}px of content and ${offsetDelta}px of offset, but it inserted only ${insertedHeight}px; the disclosure must not re-position the stream beyond its own content`)
    }
    evidence.disclosureToggle = { theme, beforeToggle, afterToggle, insertedHeight, offsetDelta, contentShift, beforeAnchor, afterAnchor, route: { before: beforeToggle.search, after: afterToggle.search } }

    /* ── a real reading position: scrolled stream, selected turn, typed query ── */
    await page.evaluate(() => {
      const stream = document.querySelector('.txn-stream')
      if (stream) { stream.scrollTop = Math.min(600, stream.scrollHeight - stream.clientHeight); stream.dispatchEvent(new Event('scroll', { bubbles: true })) }
    })
    await page.waitForFunction(() => document.querySelector(".txn-turnwrap:has(.txn-turn.txn-active)") != null, { timeout: 10000 })
      .catch(() => { throw fail('selecting a turn', 'no turn became active after scrolling the stream') })
    const searchInput = await openSearch(page, 'opening the transcript search')
    await searchInput.type(SEARCH_TEXT)
    await pause(500)

    // The query jump re-positions the stream to its first match WITHOUT running
    // the viewer's active-turn derivation, so the raw highlight here is stale
    // (it holds the search-match turn while the offset already sits past it).
    // One reader-equivalent scroll gesture settles it against the final layout,
    // which is exactly what the Back replay does when it re-applies the offset.
    const forwardRaw = await readStreamPosition(page)
    await page.evaluate(() => {
      const stream = document.querySelector('.txn-stream')
      if (!stream) return
      stream.scrollTop += 1
      stream.dispatchEvent(new Event('scroll', { bubbles: true }))
      stream.scrollTop -= 1
      stream.dispatchEvent(new Event('scroll', { bubbles: true }))
    })
    const before = await settledStreamPosition(page)

    // The host owns where the reader was, so the per-session record is the
    // expectation Back must reproduce; the DOM is the reader-visible proof that
    // the record tracks them (settled, not stale).
    const record = await page.evaluate((key) => {
      try { return JSON.parse(sessionStorage.getItem(key) ?? 'null') } catch { return null }
    }, READING_STATE_KEY(FIXTURE.childSessionId))
    if (!record) throw fail('setting the reading position', 'the host wrote no per-session reading record before the follow')
    if (!Number.isSafeInteger(record.activeTurn)) throw fail('setting the reading position', `the reading record carries no selected turn: ${JSON.stringify(record)}`)
    if (!(before.scrollTop > 0) || before.turn === null) throw fail('setting the reading position', `the reading position was not established in the mounted viewer (${JSON.stringify(before)})`)
    if (before.turn !== viewerDerivedTurn(before)) throw fail('settling the reading position', `the rendered highlight ${before.turn} is not the turn the viewer derives for offset ${before.scrollTop} (${viewerDerivedTurn(before)})`)
    if (Math.abs(record.scrollTop - before.scrollTop) > 2) throw fail('recording the reading position', `the stored offset ${record.scrollTop} does not track the mounted stream ${before.scrollTop}`)
    if (record.activeTurn !== Number(before.turn)) throw fail('recording the reading position', `the stored selection ${record.activeTurn} does not track the settled mounted active turn ${before.turn}`)
    // PICTURE the asserted state: the settled reading position is the state the
    // assertions above just checked, and re-sampling right after the capture
    // proves the screenshot is that state.
    await capture(page, gate, theme, 'context-child-scrolled', '.txn-app', evidence)
    const forwardAfterCapture = await readStreamPosition(page)
    if (forwardAfterCapture.turn !== before.turn || forwardAfterCapture.scrollTop !== before.scrollTop) {
      throw fail('capturing the child reading position', `the capture is not the asserted state: checked turn ${before.turn} at ${before.scrollTop}px, afterwards turn ${forwardAfterCapture.turn} at ${forwardAfterCapture.scrollTop}px`)
    }

    /* ── surface 3: follow the current-parent link to the exact stored parent ── */
    await page.evaluate((label) => {
      const row = [...document.querySelectorAll('.txn-context-source')].find((candidate) => candidate.textContent?.includes(label))
      row?.querySelector('button.txn-context-link')?.click()
    }, FIXTURE.starterLabel)
    await waitForPath(page, FIXTURE.parentId, 'following the current-parent link')
    await waitForSelector(page, '.txn-app', 'mounting the exact stored parent session')
    await page.waitForFunction((text) => (document.querySelector('.txn-app')?.textContent ?? '').includes(text), { timeout: 20000 }, FIXTURE.parentOpening)
      .catch(() => { throw fail('mounting the exact stored parent session', `the parent's own stored content ${JSON.stringify(FIXTURE.parentOpening)} never rendered`) })
    const parentProbe = await page.evaluate((id) => ({
      pathname: window.location.pathname,
      theme: document.documentElement.getAttribute('data-theme'),
      contextLinks: document.querySelectorAll('.txn-context-link').length,
    }), FIXTURE.parentId)
    if (!parentProbe.pathname.includes(FIXTURE.parentId)) throw fail('following the current-parent link', `the route ${JSON.stringify(parentProbe.pathname)} is not the authorized target ${FIXTURE.parentId}`)
    if (parentProbe.theme !== theme) throw fail('mounting the exact stored parent session', `the parent rendered theme ${parentProbe.theme}`)
    if (parentProbe.contextLinks !== 0) throw fail('mounting the exact stored parent session', "the child's context controls leaked onto the parent session")
    await capture(page, gate, theme, 'parent-session', '.txn-app', evidence)
    evidence.follow = { theme, parentProbe, before }

    /* ── surface 4: Back restores query, disclosure, selection, search, scroll ── */
    await page.goBack({ waitUntil: 'domcontentloaded' }).catch(() => null)
    await waitForPath(page, encodeURIComponent(FIXTURE.childSessionId), 'returning to the child with Back')
    await waitForSelector(page, '.txn-context-link', 're-mounting the child context links after Back')
    await page.waitForFunction(() => document.querySelector('.txn-earlier-toggle')?.getAttribute('aria-expanded') === 'true', { timeout: 15000 })
      .catch(() => { throw fail('restoring the child disclosure on Back', `the retained-history disclosure did not reopen (route ${page.url()})`) })
    const restored = await settledStreamPosition(page)
    const restoredQuery = new URLSearchParams(restored.search)
    if (restoredQuery.get('earlier') !== 'earlier-0') throw fail('restoring the child disclosure on Back', `the route ${JSON.stringify(restored.search)} lost the disclosed section`)
    if (Math.abs(restored.scrollTop - record.scrollTop) > 2) throw fail('restoring the child scroll offset on Back', `scrollTop was ${restored.scrollTop}, expected the stored ${record.scrollTop}`)
    // Settled-to-settled: the Back replay re-applies the offset and the viewer
    // re-derives the highlight from it, so the comparison is between the two
    // settled rendered states (the raw forward read is stale until that
    // derivation runs — see `forwardRaw` in the evidence).
    if (restored.turn === null) throw fail('restoring the child selection on Back', 'the child came back with no selected turn at all')
    if (restored.turn !== before.turn) throw fail('restoring the child selection on Back', `the restored highlight ${restored.turn} is not the settled forward highlight ${before.turn}`)
    if (restored.turn !== viewerDerivedTurn(restored)) throw fail('restoring the child selection on Back', `the restored highlight ${restored.turn} is not the turn the viewer derives for offset ${restored.scrollTop} (${viewerDerivedTurn(restored)})`)
    const restoredRecord = await page.evaluate((key) => {
      try { return JSON.parse(sessionStorage.getItem(key) ?? 'null') } catch { return null }
    }, READING_STATE_KEY(FIXTURE.childSessionId))
    if (!restoredRecord || restoredRecord.search !== SEARCH_TEXT) {
      throw fail('restoring the child search on Back', `the per-session record holds ${JSON.stringify(restoredRecord?.search)}, expected ${JSON.stringify(SEARCH_TEXT)}`)
    }

    // PICTURE the asserted state: nothing may run between the assertion above
    // and the capture, and the state is re-sampled right after the capture to
    // prove the screenshot is the state that was checked.
    await capture(page, gate, theme, 'context-back-restored', '.txn-app', evidence)
    const restoredAfterCapture = await readStreamPosition(page)
    if (restoredAfterCapture.turn !== restored.turn || restoredAfterCapture.scrollTop !== restored.scrollTop) {
      throw fail('capturing the restored child state', `the capture is not the asserted state: checked turn ${restored.turn} at ${restored.scrollTop}px, afterwards turn ${restoredAfterCapture.turn} at ${restoredAfterCapture.scrollTop}px`)
    }

    // SEPARATE, explicitly labelled action: reopening the search panel runs the
    // viewer's own search effect, which scrolls to and selects the first match.
    // The search panel's open/closed disclosure is viewer-local state that the
    // host does not own, so the restored capture above shows the query restored
    // with the panel closed; this step proves the query is really there and
    // pictures the reopened state instead of conflating the two.
    const restoredSearchInput = await openSearch(page, 'reopening the transcript search after Back')
    const reopenedQuery = await restoredSearchInput.evaluate((node) => node.value)
    if (reopenedQuery !== SEARCH_TEXT) throw fail('reopening the transcript search after Back', `the reopened query was ${JSON.stringify(reopenedQuery)}, expected ${JSON.stringify(SEARCH_TEXT)}`)
    const searchReopened = await settledStreamPosition(page)
    await capture(page, gate, theme, 'context-back-search-reopened', '.txn-app', evidence)
    evidence.back = {
      theme,
      restored,
      restoredQuery: restoredRecord.search,
      searchReopened,
      forwardRawTurn: forwardRaw.turn,
      forwardSettledTurn: before.turn,
      viewerDerived: { forward: viewerDerivedTurn(before), restored: viewerDerivedTurn(restored) },
      recordedTurn: record.activeTurn,
    }

    /* ── the disclosure is route state: reload and a copied link reopen it ── */
    const disclosedUrl = page.url()
    await page.reload({ waitUntil: 'domcontentloaded' })
    await waitForSelector(page, '.txn-earlier-toggle', 'reloading the disclosed route')
    await page.waitForFunction(() => document.querySelector('.txn-earlier-toggle')?.getAttribute('aria-expanded') === 'true', { timeout: 15000 })
      .catch(() => { throw fail('reloading the disclosed route', `the retained-history disclosure collapsed on reload of ${disclosedUrl}`) })
    // The route reopens the disclosure AND the session record replays the
    // reader's offset (sessionStorage survives a reload of the same tab). Both
    // are asserted here; a capture is deliberately NOT written, because this
    // state is the same route-plus-record state `context-back-restored` already
    // pictures and would be byte-identical evidence.
    const reloaded = await settledStreamPosition(page)
    if (Math.abs(reloaded.scrollTop - record.scrollTop) > 2) {
      throw fail('reloading the disclosed route', `the reloaded child sits at ${reloaded.scrollTop}px, expected the stored ${record.scrollTop}`)
    }
    if (reloaded.turn === null) throw fail('reloading the disclosed route', 'the reloaded child came back with no selected turn')
    evidence.reload = {
      disclosedUrl,
      restoredOffset: reloaded.scrollTop,
      restoredTurn: reloaded.turn,
      viewerDerived: viewerDerivedTurn(reloaded),
      expanded: true,
      picturedBy: 'context-back-restored (same route + session record; no duplicate capture written)',
    }

    const copied = await browser.newPage()
    try {
      await applyDeterminism(copied)
      await copied.setViewport({ width: 1460, height: 1000, deviceScaleFactor: 1 })
      await useTheme(copied, theme)
      await copied.goto(disclosedUrl, { waitUntil: 'domcontentloaded' })
      await waitForSelector(copied, '.txn-earlier-toggle', 'opening the copied disclosed link')
      await copied.waitForFunction(() => document.querySelector('.txn-earlier-toggle')?.getAttribute('aria-expanded') === 'true', { timeout: 15000 })
        .catch(() => { throw fail('opening the copied disclosed link', `the retained-history disclosure collapsed on a fresh document at ${disclosedUrl}`) })
      // A copied link carries what the URL carries and nothing else: the
      // disclosure reopens from `?earlier=`, while the transient reading state
      // is session-scoped. A fresh tab therefore replays no offset and no query
      // (the hook writes an empty default record on mount), and asserting that
      // boundary keeps the two restoration paths from being conflated.
      const copiedState = await settledStreamPosition(copied)
      const copiedRecord = await copied.evaluate((key) => {
        try { return JSON.parse(sessionStorage.getItem(key) ?? 'null') } catch { return null }
      }, READING_STATE_KEY(FIXTURE.childSessionId))
      const copiedCarriesReaderState = copiedRecord !== null && (copiedRecord.scrollTop !== 0 || (copiedRecord.search ?? '') !== '')
      if (copiedCarriesReaderState) {
        throw fail('opening the copied disclosed link', `a fresh document replayed session reading state: ${JSON.stringify(copiedRecord)}`)
      }
      if (copiedState.scrollTop !== 0) throw fail('opening the copied disclosed link', `a fresh document replayed a stored offset (${copiedState.scrollTop}px) with no reader state to justify it`)
      await capture(copied, gate, theme, 'context-copied-link', '.txn-app', evidence)
      evidence.copiedLink = { disclosedUrl, expanded: true, replayedReaderState: false, offset: copiedState.scrollTop, record: copiedRecord }
    } finally {
      await copied.close()
    }

    /* ── surface 5: an absent current-parent target stays honest and readable ── */
    await page.goto(`${ORIGIN}${UNRESOLVED_PATH}`, { waitUntil: 'domcontentloaded' })
    await waitForSelector(page, '.txn-context-source', 'rendering the unresolved current-parent reference')
    await assertTheme(page, theme, 'capturing the unresolved reference')
    const unresolvedRows = await contextRows(page)
    if (unresolvedRows.length !== 1) throw fail('rendering the unresolved current-parent reference', `rendered ${JSON.stringify(unresolvedRows)}, expected the one started-by row`)
    if (unresolvedRows[0].label !== FIXTURE.starterLabel || unresolvedRows[0].link !== null || unresolvedRows[0].status !== 'source unavailable') {
      throw fail('rendering the unresolved current-parent reference', `the unavailable reference drifted: ${JSON.stringify(unresolvedRows[0])}`)
    }
    const readable = await page.evaluate((text) => {
      const app = document.querySelector('.txn-app')
      if (!app) return false
      const normalize = (value) => value.replace(/\s+/g, ' ').trim()
      return app.querySelectorAll('.txn-turn').length > 0 && normalize(app.textContent ?? '').includes(normalize(text))
    }, FIXTURE.unresolvedOpening.split(' ').slice(0, 6).join(' '))
    if (!readable) throw fail('rendering the unresolved current-parent reference', 'the child did not stay readable while its current-parent target is unavailable')
    await capture(page, gate, theme, 'unresolved-parent', '.txn-app', evidence)
    evidence.unresolved = { theme, rows: unresolvedRows }

    console.log(`OK [${theme}] child links → ${FIXTURE.parentId} → Back ${restored.search} · restored scroll=${restored.scrollTop}/${before.scrollTop} · restored turn=${restored.turn} (derived ${viewerDerivedTurn(restored)}) · query=${JSON.stringify(restoredRecord.search)} · search panel reopened → turn=${searchReopened.turn} at ${searchReopened.scrollTop}px`)
  } finally {
    await page.close()
  }
}

if (!process.env.PEASANT_CONTEXT_NAV_SKIP_BUILD) {
  console.log('[context-navigation-shoot] make build (user canonical path) …')
  execSync('make build', { cwd: REPO, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })
} else {
  console.log('[context-navigation-shoot] PEASANT_CONTEXT_NAV_SKIP_BUILD=1 — reusing existing bin/peasant')
}
if (!existsSync(BIN)) { console.error(`ERROR [context-navigation-shoot] ${BIN} not found — run \`make build\` first.`); process.exit(1) }

const chunkPath = assertBuildProvenance()
console.log(`[context-navigation-shoot] provenance OK: ${relative(REPO, chunkPath)} carries ${FEATURE_BYTES.join(', ')}`)

console.log(`[context-navigation-shoot] starting ${BIN} on :${PORT} (mock store: ${MOCKS}) …`)
const server = spawn(BIN, ['web', 'start', '--port', PORT, '--foreground', '--no-browser', `--mock-data-store=${MOCKS}`], { cwd: REPO, stdio: ['ignore', 'ignore', 'pipe'] })
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
const evidence = { fixture: relative(REPO, resolve(REPO, 'internal/mock/testdata/context_navigation.yaml')), chunk: relative(REPO, chunkPath), featureBytes: FEATURE_BYTES, servedChunks: [], captures: [] }

let healthy = false
for (let i = 0; i < 60 && !healthy; i++) { healthy = (await fetch(`${ORIGIN}/api/v1/health`).catch(() => null))?.status === 200; if (!healthy) await pause(250) }
if (!healthy) { console.error(`ERROR [context-navigation-shoot] the real binary did not become healthy on ${ORIGIN}: ${serverError.trim()}`); await teardown(); process.exit(2) }

try {
  browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: { width: 1460, height: 1000, deviceScaleFactor: 1 } })
  const gate = new SurfaceGate(await browser.newPage())
  for (const theme of SMOKE_THEMES) await runTheme(browser, theme, chunkPath, gate, evidence)
  writeFileSync(join(OUT, 'evidence.json'), JSON.stringify(evidence, null, 2))
  console.log(`\nOK [context-navigation-shoot] ${evidence.captures.length} mounted captures across ${SMOKE_THEMES.length} themes:`)
  for (const file of evidence.captures) console.log(`  ${file}`)
} catch (error) {
  console.error(`FAIL [context-navigation-shoot] ${error.stack || error.message}`)
  await teardown()
  process.exit(1)
} finally {
  await teardown()
}
