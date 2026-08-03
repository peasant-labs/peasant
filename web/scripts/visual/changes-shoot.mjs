/* Capture the lifted Changes / ChangeDetail surfaces (the design-system <Changes> /
   <ChangeDetail> the /review route mounts) from the deterministic dev harness route
   /dev/changes-harness, writing app-side captures into the stitch-sxs layout:
       <base>/<APP_DIR>/<theme>/<surface>.png   (APP_DIR default `peasant`)
   for surface in {gmp-changes, gmp-change-detail} and theme in {dark, light}.

   This is the SAME-ENV regression arm: the captures are byte-deterministic (frozen
   clock/PRNG via determinism.mjs + a static fixture + fonts loaded before the shot), so
   diffing a fresh capture against the COMMITTED baseline (scripts/visual/baseline/changes/)
   nets ~0% — any real change to the surface shows up loud. Each capture is held to the
   shared non-empty floor (surface-gate.mjs) so a blank never passes as a baseline.

   env:
     CHROME_PATH              (required) Chrome/Chromium binary
     PEASANT_HARNESS_ORIGIN   origin serving /dev/changes-harness  (default http://localhost:3000)
     APP_DIR                  subject subdir under <base>          (default `peasant`)
     PUPPETEER_CORE           explicit puppeteer-core module path  (optional)
   usage: CHROME_PATH=/path/to/chrome node changes-shoot.mjs <base-dir>
*/
import { mkdirSync } from 'node:fs'
import { SurfaceGate } from './surface-gate.mjs'
import { applyDeterminism } from './determinism.mjs'
const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default

const CHROME = process.env.CHROME_PATH
const BASE = process.argv[2]
const ORIGIN = (process.env.PEASANT_HARNESS_ORIGIN || 'http://localhost:3000').replace(/\/$/, '')
const APP_DIR = process.env.APP_DIR || 'peasant'
const THEMES = ['dark', 'light']
const SURFACES = [
  { surface: 'gmp-changes', query: 'changes', sel: '.gmp-changes-root', vh: 1000 },
  { surface: 'gmp-change-detail', query: 'change-detail', sel: '.gmp-detail-root', vh: 1100 },
]
const FONTS = [
  '400 16px "Atkinson Hyperlegible"', '700 16px "Atkinson Hyperlegible"',
  '400 16px "Atkinson Hyperlegible Mono"', '500 16px "Atkinson Hyperlegible Mono"',
  '600 16px "Atkinson Hyperlegible Mono"', '700 16px "Atkinson Hyperlegible Mono"',
]

if (!CHROME) { console.error('ERROR [changes-shoot.mjs] CHROME_PATH is unset.'); process.exit(1) }
if (!BASE) { console.error('ERROR [changes-shoot.mjs] missing <base-dir>.\n  usage: CHROME_PATH=... node scripts/visual/changes-shoot.mjs <base-dir>'); process.exit(1) }

const pause = (ms) => new Promise((r) => setTimeout(r, ms))
const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', defaultViewport: { width: 1460, height: 1000, deviceScaleFactor: 1 } })

let made = 0
for (const theme of THEMES) {
  const outDir = `${BASE}/${APP_DIR}/${theme}`
  mkdirSync(outDir, { recursive: true })
  const gate = new SurfaceGate(browser ? await browser.newPage() : null) // measurement page (PNG decoder)
  for (const { surface, query, sel, vh } of SURFACES) {
    const page = await browser.newPage()
    await applyDeterminism(page) // BEFORE navigation: reduced-motion + frozen clock/PRNG → deterministic render
    await page.setViewport({ width: 1460, height: vh, deviceScaleFactor: 1 })
    const url = `${ORIGIN}/dev/changes-harness?surface=${query}&theme=${theme}`
    // Under `next dev` the dev-route client occasionally fails to hydrate within the
    // window (a flaky chunk fetch leaves only the app shell, `mounted` never flips), so
    // RE-NAVIGATE up to 3× until the surface mounts. domcontentloaded (not networkidle0):
    // the root layout's Google-Fonts <link> + preconnect keep the network non-idle under
    // next dev; the explicit selector wait + document.fonts.ready below are the real signals.
    let mounted = null
    for (let attempt = 1; attempt <= 3 && !mounted; attempt++) {
      if (attempt > 1) console.error(`  [${surface}/${theme}] retry ${attempt} (surface had not mounted)`)
      await page.goto(url, { waitUntil: 'domcontentloaded' })
      mounted = await page.waitForSelector(sel, { timeout: 15000 }).catch(() => null)
    }
    if (!mounted) {
      const dbg = await page.evaluate((s) => ({ has: !!document.querySelector(s), changes: !!document.querySelector('.gmp-changes-root'), detail: !!document.querySelector('.gmp-detail-root'), head: document.body?.innerText?.slice(0, 100) }), sel).catch((e) => ({ evalErr: String(e).slice(0, 120) }))
      console.error(`ERROR [changes-shoot.mjs] "${surface}/${theme}" never mounted (${sel}) after 3 attempts. page state: ${JSON.stringify(dbg)}`)
      await browser.close(); process.exit(2)
    }
    // hide the Next dev-tools overlay (no demo equivalent) so it can't bleed into the element capture
    await page.addStyleTag({ content: 'nextjs-portal,[data-nextjs-toast],[data-next-badge-root],[data-nextjs-dev-tools-button],#__next-build-watcher{display:none!important}' }).catch(() => {})
    await page.evaluate(async (faces) => {
      try { await Promise.all(faces.map((f) => document.fonts.load(f))) } catch { /* best effort */ }
      await document.fonts.ready
    }, FONTS)
    await pause(700)
    const el = await page.$(sel)
    const file = `${outDir}/${surface}.png`
    await el.screenshot({ path: file, captureBeyondViewport: true })
    // non-empty floor: a blank/near-empty/duplicate capture FAILS the run (no blank baselines)
    const r = await gate.assert(`${surface}-${theme}`, file, { sel, where: 'changes-shoot.mjs' })
    const box = await el.boundingBox()
    console.log(`OK ${theme}/${surface} → ${file} ${Math.round(box.width)}x${Math.round(box.height)} nonbg=${(r.nonbgRatio * 100).toFixed(1)}% colors=${r.distinctColors}`)
    made++
    await page.close()
  }
}
await browser.close()
console.log(`\nchanges-shoot: wrote ${made} app capture(s) under ${BASE}/${APP_DIR}/<theme>/`)
