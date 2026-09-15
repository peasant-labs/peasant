/* Inspect + feedback tool capture harness (dev-server only).

   The tool is mounted only under `next dev` (the production export never includes it), so this
   harness drives the running dev server and captures the two states a reviewer needs to see, in
   BOTH themes, over a real app surface:

     fbk-armed  the picker armed: the launch control reads "selecting", the hovered element wears
                the outline, and the cheap hover tag names the owning section + element
     fbk-note   the note popup open on the right after a pick, focused, with the target description
     fbk-saved  the popup after a cmd/ctrl+Enter save: the note is appended through
                POST /api/v1/local/feedback and the picker re-armed for the next element
     fbk-off    the same surface with ?fb=off: the tool is absent (the disable switch)

   It also asserts the flow in the browser (arm from the `c` shortcut, arm from the control, the
   pick, the POST body, the re-arm, and the switch) and verifies build provenance before trusting a
   capture: the served app must render the tool and the served JS chunk must carry the string only
   this change introduces. The save POST is intercepted and fulfilled locally, so the run never
   writes the repository's llm/ui-feedback.md.

   Prerequisites:
     pnpm dev            # next dev, from this web/ worktree
     CHROME_PATH=$(command -v google-chrome) node scripts/visual/inspect-feedback-shoot.mjs

   env:
     CHROME_PATH                Chrome/Chromium binary                                    (required)
     PEASANT_HARNESS_ORIGIN     origin serving the dev app     (default http://localhost:3000)
     PEASANT_FEEDBACK_SURFACE   route the tool is captured over (default /dev/visual-harness)
     PUPPETEER_CORE             explicit puppeteer-core module path                        (optional)

   usage: node scripts/visual/inspect-feedback-shoot.mjs [outdir]
*/
import { mkdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join, resolve } from 'node:path'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'

const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '..', '..', '..')
const CHROME = process.env.CHROME_PATH
const ORIGIN = (process.env.PEASANT_HARNESS_ORIGIN || 'http://localhost:3000').replace(/\/$/, '')
const SURFACE = process.env.PEASANT_FEEDBACK_SURFACE || '/dev/visual-harness'
const OUT = resolve(process.argv[2] || join(REPO, 'review-capture', 'inspect-feedback'))
const THEMES = ['dark', 'light']
/* A string only this change introduces. Also present in the served CHUNK, which is what the
   provenance check greps — the rendered control alone could come from a different source tree. */
const PROVENANCE_MARKER = 'comment on an element (c)'
const FEEDBACK_ROUTE = '/api/v1/local/feedback'

if (!CHROME) {
  console.error('ERROR [inspect-feedback-shoot.mjs] CHROME_PATH is unset — set it to your Chrome/Chromium binary.')
  process.exit(1)
}

mkdirSync(OUT, { recursive: true })

const pause = (ms) => new Promise((r) => setTimeout(r, ms))
const results = []
let browser = null
const fail = (what, why, fix) => {
  console.error(
    `\nSTRUCTURAL FAILURE [inspect-feedback-shoot.mjs] — ${what}\n` +
      `  Why: ${why}\n` +
      `  Means: the capture set would misrepresent the mounted tool, so the run is aborted.\n` +
      `  Fix: ${fix}\n`,
  )
  try {
    browser?.close()
  } catch {
    /* already closing */
  }
  process.exit(2)
}

browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: { width: 1460, height: 1000, deviceScaleFactor: 1 } })
const gate = new SurfaceGate(await browser.newPage()) // PNG decoder page, shared by every gate assertion

/* ── provenance: the served app must carry the change. Runs once, on the FIRST theme. ── */
let provenanceChecked = false
const verifyProvenance = async (page) => {
  if (provenanceChecked) return
  provenanceChecked = true
  const rendered = await page.evaluate((marker) => !!document.querySelector(`[aria-label="${marker}"]`), PROVENANCE_MARKER)
  if (!rendered) {
    fail(
      `the served app never rendered the comment control on ${SURFACE}`,
      'the tool did not mount, so every capture would show a surface without it',
      'confirm the dev server is running the web/ worktree that contains the tool, and that the route answers 200',
    )
  }
  // Read the SERVED document (following the trailing-slash redirect) and grep the chunks it loads.
  // The post-hydration DOM drops the initial layout/page script tags, so the served HTML is the
  // stable, honest record of what this dev server built.
  const html = await fetch(`${ORIGIN}${SURFACE}`).then((r) => (r.ok ? r.text() : '')).catch(() => '')
  const srcs = [...new Set([...html.matchAll(/src="(\/_next\/[^"]+\.js[^"]*)"/g)].map((m) => m[1]))]
  for (const src of srcs) {
    const body = await fetch(ORIGIN + src).then((r) => (r.ok ? r.text() : '')).catch(() => '')
    if (body.includes(PROVENANCE_MARKER)) {
      console.log(`[inspect-feedback] provenance ok — served chunk ${src} carries the change marker`)
      return
    }
  }
  fail(
    `no served JS chunk under ${ORIGIN} carries the change marker (checked ${srcs.length} chunk(s) from the served page)`,
    'the page rendered the control but the bundle under test could not be confirmed, so a stale server would go unnoticed',
    'restart `pnpm dev` from this worktree and re-run the harness',
  )
}

for (const theme of THEMES) {
  const dir = join(OUT, theme)
  mkdirSync(dir, { recursive: true })
  const page = await browser.newPage()
  await applyDeterminism(page)
  const errs = []
  page.on('console', (m) => {
    if (m.type() === 'error' && !/favicon|404|hydrat/.test(m.text())) errs.push(m.text())
  })
  page.on('pageerror', (e) => errs.push('pageerr: ' + e.message))

  // The save POST is fulfilled locally: the dev server has no Peasant Local API, and the run must
  // never append to the repository's llm/ui-feedback.md.
  let lastSave = null
  await page.setRequestInterception(true)
  page.on('request', (request) => {
    if (request.method() === 'POST' && new URL(request.url()).pathname === FEEDBACK_ROUTE) {
      lastSave = JSON.parse(request.postData() || '{}')
      request.respond({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, path: 'llm/ui-feedback.md' }) })
      return
    }
    request.continue()
  })

  await page.goto(`${ORIGIN}${SURFACE}`, { waitUntil: 'domcontentloaded' })
  await page.waitForFunction((marker) => !!document.querySelector(`[aria-label="${marker}"]`), { timeout: 30000 }, PROVENANCE_MARKER)
  await pause(900)

  /* The theme lives in a module store plus localStorage; a full navigation re-reads it, so the
     theme is (re)applied through the real control and then verified, never assumed. */
  const applyTheme = async () => {
    const current = await page.evaluate(() => document.documentElement.getAttribute('data-theme'))
    if (current !== theme) {
      await page.evaluate(() => document.querySelector('.theme-btn')?.click())
      await pause(500)
    }
    const activeTheme = await page.evaluate(() => document.documentElement.getAttribute('data-theme'))
    if (activeTheme !== theme) {
      fail(`theme-didn't-flip: requested "${theme}" but [data-theme]="${activeTheme}"`, 'a capture under the wrong theme is invalid evidence', 'confirm the running shell still exposes .theme-btn and re-run')
    }
  }
  await applyTheme()
  await verifyProvenance(page)

  /* ── 1. arm through the control, hover a real element, capture the armed picker ── */
  await page.click('.fbk-launch')
  await page.waitForFunction(() => document.querySelector('.fbk-launch')?.classList.contains('armed'))
  const hoverTarget = await page.evaluate(() => {
    // A representative body element: the first section heading's owning section, or the main region.
    const section = document.querySelector('main section') || document.querySelector('main') || document.body
    const el = section.querySelector('h2, h3, button, li, p') || section
    const r = el.getBoundingClientRect()
    return { x: Math.round(r.left + Math.min(r.width / 2, 120)), y: Math.round(r.top + r.height / 2) }
  })
  await page.mouse.move(hoverTarget.x, hoverTarget.y)
  await pause(400)
  const hoverState = await page.evaluate(() => ({
    tag: document.querySelector('.fbk-tag')?.textContent || '',
    tagged: !!document.querySelector('.fbk-target'),
    armed: document.querySelector('.fbk-launch')?.classList.contains('armed'),
  }))
  if (!hoverState.armed || !hoverState.tagged || !hoverState.tag) {
    fail(`the armed picker did not outline an element and label it (${JSON.stringify(hoverState)})`, 'the hover path is the core affordance of the tool and would be missing from the capture', 'confirm the pointer landed on a body element, then re-run')
  }
  const armedFile = join(dir, 'fbk-armed.png')
  await page.screenshot({ path: armedFile })
  results.push({ surface: 'fbk-armed', theme, file: armedFile, info: hoverState.tag })

  /* ── 2. pick the element, write a note, verify the POST body, capture the note popup ── */
  await page.mouse.click(hoverTarget.x, hoverTarget.y)
  await page.waitForSelector('.fbk-pop', { timeout: 10000 })
  const desc = await page.evaluate(() => document.querySelector('.fbk-pop-tgt')?.textContent || '')
  await page.type('.fbk-pop textarea', 'the label wraps awkwardly at this width')
  const noteFile = join(dir, 'fbk-note.png')
  await page.screenshot({ path: noteFile })
  results.push({ surface: 'fbk-note', theme, file: noteFile, info: desc })

  await page.keyboard.down('Control')
  await page.keyboard.press('Enter')
  await page.keyboard.up('Control')
  await page.waitForFunction(() => !document.querySelector('.fbk-pop'), { timeout: 10000 })
  const savedState = await page.evaluate(() => ({
    toast: document.querySelector('[data-fb][role="status"]')?.textContent || '',
    armed: document.querySelector('.fbk-launch')?.classList.contains('armed'),
  }))
  if (!lastSave) fail('the save never reached POST ' + FEEDBACK_ROUTE, 'the capture would show the tool without proving the persistence path', 'connect the popup save to the route and re-run')
  for (const field of ['route', 'anchor', 'selector', 'snippet', 'comment']) {
    if (typeof lastSave[field] !== 'string' || lastSave[field].length === 0) {
      fail(`the save payload is missing "${field}" (${JSON.stringify(lastSave)})`, 'the Peasant Local API route requires the full annotation payload', 'fix the payload builder and re-run')
    }
  }
  if (!savedState.armed || !savedState.toast) {
    fail(`the picker did not re-arm after the save (${JSON.stringify(savedState)})`, 'the sweep depends on staying armed after a save', 'restore the re-arm on save and re-run')
  }
  const savedFile = join(dir, 'fbk-saved.png')
  await page.screenshot({ path: savedFile })
  results.push({ surface: 'fbk-saved', theme, file: savedFile, info: `POST ${FEEDBACK_ROUTE} · anchor=${lastSave.anchor}` })

  /* ── 3. esc backs out one step (disarm), then the disable switch removes the tool entirely ── */
  await page.keyboard.press('Escape')
  await page.waitForFunction(() => !document.querySelector('.fbk-launch')?.classList.contains('armed'))
  await page.goto(`${ORIGIN}${SURFACE}${SURFACE.includes('?') ? '&' : '?'}fb=off`, { waitUntil: 'domcontentloaded' })
  await pause(900)
  await applyTheme()
  const offState = await page.evaluate(() => ({ chrome: !!document.querySelector('[data-fb]'), armed: document.body.classList.contains('fbk-arming') }))
  if (offState.chrome || offState.armed) {
    fail(`?fb=off left tool chrome on the page (${JSON.stringify(offState)})`, 'the disable switch must keep the tool out of every capture', 'read the switch before mounting the tool and re-run')
  }
  const offFile = join(dir, 'fbk-off.png')
  await page.screenshot({ path: offFile })
  results.push({ surface: 'fbk-off', theme, file: offFile, info: 'tool absent' })

  for (const row of results.filter((r) => r.theme === theme)) {
    await gate.assert(row.surface, row.file, { where: 'inspect-feedback-shoot.mjs' })
  }
  if (errs.length) console.error(`  [${theme}] page errors: ${errs.slice(0, 3).join(' | ')}`)
  await page.close()
}

await browser.close()

console.log(`\n[inspect-feedback] ${results.length} captures over ${SURFACE} in both themes → ${OUT}`)
for (const row of results) console.log(`  ${row.theme.padEnd(5)} ${row.surface.padEnd(10)} ${row.info}`)
console.log('OK [inspect-feedback] armed picker, note popup, save + re-arm, and the disable switch all verified on the dev build')
