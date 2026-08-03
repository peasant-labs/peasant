/* Capture the DEMO REFERENCE for the lifted Changes/ChangeDetail FIDELITY composite — the
 * design-system demo (fairtrade `pnpm dev`) rendering the SAME lifted <Changes>/<ChangeDetail>
 * the /review route mounts, over the demo's fixture. This is the fidelity target the app
 * SUBJECT (changes-shoot.mjs, same fixture) is compared against side-by-side.
 *
 * Writes the stitch-sxs layout: <capture-root>/demo/<theme>/gmp-{changes,change-detail}.png
 * Then stitch with: SURFACE_SET=changes REF_DIR=demo APP_DIR=peasant node stitch-sxs.mjs <capture-root>
 *
 * Shell handling (per-surface): the GraphApp shell `.iu-subnav` is a sticky bar.
 *   - change-detail: it paints OVER the component's own .crumb → HIDE it so the ref shows the
 *     crumb (matching the app), like-for-like.
 *   - changes: the changes-root sits BELOW the subnav (not overlapped) → do NOT hide it; hiding
 *     only reflows ~48px of trailing whitespace into the box (a DIM vs the app's height). Leaving
 *     it shown keeps the demo box dimension-matched to the app (content is identical either way).
 *
 * env: CHROME_PATH (required) · DEMO_URL (default http://localhost:5180) · PUPPETEER_CORE
 * usage: CHROME_PATH=$(command -v google-chrome) node scripts/visual/demo-shoot.mjs <capture-root>
 */
import { mkdirSync } from 'node:fs'
import { SurfaceGate } from './surface-gate.mjs'
const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default

const CAPTURE_ROOT = process.argv[2]
const DEMO = (process.env.DEMO_URL || 'http://localhost:5180').replace(/\/$/, '')
const CHROME = process.env.CHROME_PATH
if (!CHROME) { console.error('ERROR [demo-shoot.mjs] CHROME_PATH is unset.'); process.exit(1) }
if (!CAPTURE_ROOT) { console.error('ERROR [demo-shoot.mjs] missing <capture-root> dir.\n  usage: CHROME_PATH=... node scripts/visual/demo-shoot.mjs <capture-root>'); process.exit(1) }

const pause = (ms) => new Promise((r) => setTimeout(r, ms))
const HIDE_SHELL = '.iu-subnav,.iu-topbar,.iu-app-nav,.iu-tabs,[class*="iu-subnav"]{display:none!important}'
const FONTS = [
  '400 16px "Atkinson Hyperlegible"', '700 16px "Atkinson Hyperlegible"',
  '400 16px "Atkinson Hyperlegible Mono"', '500 16px "Atkinson Hyperlegible Mono"',
  '600 16px "Atkinson Hyperlegible Mono"', '700 16px "Atkinson Hyperlegible Mono"',
]
const CELLS = [
  { surface: 'gmp-changes', sel: '.gmp-changes-root', vh: 1000, hideShell: false },
  { surface: 'gmp-change-detail', sel: '.gmp-detail-root', vh: 1100, hideShell: true },
]

const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: { width: 1460, height: 1000, deviceScaleFactor: 1 } })
for (const theme of ['dark', 'light']) {
  const outDir = `${CAPTURE_ROOT}/demo/${theme}`
  mkdirSync(outDir, { recursive: true })
  const gate = new SurfaceGate(await browser.newPage())
  for (const cell of CELLS) {
    const page = await browser.newPage()
    await page.setViewport({ width: 1460, height: cell.vh, deviceScaleFactor: 1 })
    await page.goto(`${DEMO}/?fb=off${theme === 'light' ? '&theme=light' : ''}`, { waitUntil: 'networkidle0' })
    await pause(400)
    await page.click('#iu-tab-graph'); await pause(300)
    await page.evaluate(() => { const b = [...document.querySelectorAll('.iu-subnav-item')].find((x) => x.textContent.trim().toLowerCase() === 'changes'); b && b.click() })
    await pause(500)
    if (cell.surface === 'gmp-change-detail') {
      const nav = await page.evaluate(() => { const r = document.querySelector('.gmp-changes-root .cg-row'); if (r) { r.click(); return true } return false })
      if (!nav) { console.error(`ERROR [demo-shoot.mjs] [${theme}/${cell.surface}] no scoped .cg-row to open the change detail.`); await browser.close(); process.exit(2) }
      await page.waitForSelector('.gmp-detail-root', { timeout: 6000 }).catch(() => {})
      await pause(400)
    }
    const mounted = await page.waitForSelector(cell.sel, { timeout: 10000 }).catch(() => null)
    if (!mounted) { console.error(`ERROR [demo-shoot.mjs] [${theme}/${cell.surface}] ${cell.sel} never mounted — is the demo dev server up at ${DEMO}?`); await browser.close(); process.exit(2) }
    await page.evaluate(async (faces) => { try { await Promise.all(faces.map((f) => document.fonts.load(f))) } catch { /* best effort */ } await document.fonts.ready }, FONTS)
    if (cell.hideShell) await page.addStyleTag({ content: HIDE_SHELL }).catch(() => {})
    await pause(500)
    const el = await page.$(cell.sel)
    const f = `${outDir}/${cell.surface}.png`
    await el.screenshot({ path: f, captureBeyondViewport: true })
    const r = await gate.assert(`${cell.surface}-${theme}`, f, { sel: cell.sel, where: 'demo-shoot.mjs' })
    const box = await el.boundingBox()
    console.log(`OK ${theme}/${cell.surface} -> ${f} (${Math.round(box.width)}x${Math.round(box.height)}) nonbg=${(r.nonbgRatio * 100).toFixed(1)}%`)
    await page.close()
  }
}
await browser.close()
console.log('\ndemo-shoot: wrote the demo reference set under', `${CAPTURE_ROOT}/demo/<theme>/`)
