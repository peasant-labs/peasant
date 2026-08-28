/* Probe the peasant visual-harness route DOM so the capture script targets the right selectors:
   prints the tab + view-toggle labels, the composite box sizes, and the transcript stream scroll
   metrics. The shared `<TranscriptViewer>` composite peasant's `SessionDetailV2` adapter renders
   (via the same `adaptTranscript` wire adapter, `@peasant-labs/fairtrade/ui`) uses `.txn-*` classes
   (the `.txn-app` root, `.txn-body-grid` layout, `.txn-center` trace column, `.txn-scorecard`,
   `.txn-graphslot`, the `bs-seg-opt` list/graph toggle, the `.txn-sticky` condensed header) and owns
   exactly ONE bounded inner scroller — `.txn-stream` — rather than scrolling the page (correction: the
   composite used to be `transcript-browser`'s own `<SessionDetail>`, an implementation that
   drifted from the canonical demo and scrolled the page; `SessionDetailV2` moved onto the shared
   composite directly, and this probe was corrected to match). The `.txn-stream-prelude` (peasant's
   toolbar/explanation/steps/scope/error content) renders inside `.txn-stream`, before the first turn,
   so it scrolls away with the transcript — it must never be `position: fixed`/`sticky`. Run this first
   whenever the harness route or the shared composite changes.

   env: PEASANT_URL (default http://localhost:3000/dev/visual-harness), CHROME_PATH (required),
        PUPPETEER_CORE (optional explicit module path to puppeteer-core if a bare import won't resolve). */
const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default

const CHROME = process.env.CHROME_PATH
const URL = process.env.PEASANT_URL || 'http://localhost:3000/dev/visual-harness'

const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: 'new',
  defaultViewport: { width: 1460, height: 1000, deviceScaleFactor: 1 },
})
const page = await browser.newPage()
await page.emulateMediaFeatures([{ name: 'prefers-reduced-motion', value: 'reduce' }])
const errs = []
page.on('console', (m) => { if (m.type() === 'error' && !/favicon|hydrat/.test(m.text())) errs.push(m.text()) })
page.on('pageerror', (e) => errs.push('pageerr: ' + e.message))
await page.goto(URL, { waitUntil: 'networkidle0' })
await new Promise((r) => setTimeout(r, 1200))

const report = await page.evaluate(() => {
  const box = (sel) => { const el = document.querySelector(sel); if (!el) return null; const b = el.getBoundingClientRect(); return { w: Math.round(b.width), h: Math.round(b.height), y: Math.round(b.y) } }
  const count = (sel) => document.querySelectorAll(sel).length
  const tabs = [...document.querySelectorAll('.txn-tab')].map((t) => t.textContent.trim())
  const viewtoggle = [...document.querySelectorAll('.txn-graphslot ~ * [aria-pressed], .txn-center button[aria-pressed]')].map((t) => (t.getAttribute('aria-pressed') === 'true' ? '*' : '') + (t.textContent.trim() || t.getAttribute('aria-label') || '?'))
  const themeBtn = !!document.querySelector('.theme-btn')
  const dataTheme = document.documentElement.getAttribute('data-theme')
  const stream = document.querySelector('.txn-stream')
  const streamStyle = stream ? getComputedStyle(stream) : null
  const prelude = document.querySelector('.txn-stream-prelude')
  const preludeStyle = prelude ? getComputedStyle(prelude) : null
  return {
    dataTheme,
    themeBtn,
    boxes: {
      '.txn-app': box('.txn-app'),
      '.txn-center': box('.txn-center'),
      '.txn-rail-left': box('.txn-rail-left'),
      '.txn-rail-right': box('.txn-rail-right'),
      '.txn-scorecard': box('.txn-scorecard'),
      '.txn-graphslot': box('.txn-graphslot'),
    },
    counts: {
      '.txn-tab': count('.txn-tab'),
      '.txn-viewsw': count('.txn-viewsw'),
      '.txn-scorecard': count('.txn-scorecard'),
      '.txn-sticky': count('.txn-sticky'),
      'label btns': count('button[aria-label="Label this turn"]'),
    },
    tabs,
    viewtoggle,
    // The composite owns exactly one bounded inner scroller (`.txn-stream`) — the page itself does
    // NOT overflow. `.txn-stream-prelude`'s computed position must never be fixed/sticky, or peasant's
    // toolbar/explanation/steps content would stop scrolling away with the transcript.
    streamScroll: stream
      ? { scrollHeight: stream.scrollHeight, clientHeight: stream.clientHeight, overflowY: streamStyle.overflowY, position: streamStyle.position }
      : null,
    preludePosition: preludeStyle ? preludeStyle.position : null,
    pageScroll: { scrollHeight: document.documentElement.scrollHeight, innerHeight: window.innerHeight },
  }
})
console.log(JSON.stringify(report, null, 2))
console.log('theme control:', report.themeBtn ? '.theme-btn' : 'NONE', '· [data-theme]=' + report.dataTheme)
console.log('stream scroll (.txn-stream):', report.streamScroll ? `${report.streamScroll.scrollHeight} > ${report.streamScroll.clientHeight}, overflow-y=${report.streamScroll.overflowY}` : 'NOT FOUND')
console.log('prelude position (.txn-stream-prelude):', report.preludePosition, report.preludePosition === 'fixed' || report.preludePosition === 'sticky' ? '→ BUG: does not scroll away' : '→ ok (scrolls with stream)')
console.log('page-scroll (expected NOT to overflow — the composite owns the inner scroller):', report.pageScroll.scrollHeight, '>', report.pageScroll.innerHeight, '→', report.pageScroll.scrollHeight > report.pageScroll.innerHeight ? 'OVERFLOWS (unexpected — check for a second scroll owner)' : 'does not overflow (expected)')
console.log('console errors:', errs.length ? errs.slice(0, 8) : 'none')
await browser.close()
