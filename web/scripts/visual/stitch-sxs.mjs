/* Build labeled, HEIGHT-MATCHED side-by-side composites (REFERENCE | SUBJECT) per surface per theme.
   Dependency-free: decodes the two PNGs as data: URLs onto a <canvas> inside headless Chrome, draws
   them with labels, exports a PNG.

   HEIGHT-MATCH (so rows line up for row-by-row comparison, no ragged panes):
     - both panes are drawn at the SAME height = max(refH, appH), TOP-ALIGNED.
     - the shorter pane is PADDED (never scaled/distorted) down to that height with its OWN
       background colour, sampled from the capture's border (margins/gutters). The pad colour
       comes from the pixels themselves — no design-token value is hardcoded — so it stays
       seamless across dark/light and across the reference vs subject surfaces.
     - a faint dashed hairline marks where the shorter capture actually ends, so the padded
       region is obvious and not mistaken for empty UI.
   Where the subject side has no capture (a recorded gap), it draws a full-height placeholder panel
   with the reason, so the side-by-side set stays complete and self-explanatory.

   It pairs a REFERENCE set against the SUBJECT app captures (<base-dir>/<APP_DIR>/<theme>/) — both
   produced elsewhere; this script only composites them. The transcript reference (see README
   "Oracle"):
     - REF_DIR=demo (default) vs APP_DIR=peasant → design-language sanity ref: the fairtrade demo
       renders its TranscriptViewer (.txn-*) over the same fixture — cross-component, so treat the
       %-number as INFORMATIONAL, not surface-parity. (The retired pre-composite-migration `tb`
       golden depicted a composer the app no longer renders and was removed; the real transcript
       regression gate is the smoke-baseline arm, SURFACE_SET=smoke.)

   env:
     CHROME_PATH     (required) Chrome/Chromium binary puppeteer drives
     REF_DIR         reference (left) capture subdir   (default `demo`)
     REF_LABEL       the reference-pane caption               (default the same-component baseline caption)
     APP_DIR         subject (right) app capture subdir        (default `peasant`)
     APP_LABEL       the subject-pane caption                 (default the peasant app caption)
     PUPPETEER_CORE  explicit module path to puppeteer-core   (optional)
   usage: CHROME_PATH=/path/to/chrome node stitch-sxs.mjs <base-dir>
*/
import { writeFileSync, existsSync, mkdirSync, unlinkSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { diffPixels, dataUrl } from './png-diff.mjs'
import { GRAPH_SHELL_SURFACE_LABELS, GRAPH_SHELL_SURFACE_SET, SMOKE_SURFACE_LABELS, SMOKE_SURFACE_SET } from './smoke-surfaces.mjs'
const puppeteer = (await import(process.env.PUPPETEER_CORE || 'puppeteer-core')).default

// imgdiff gate (additive to the human-glance composite below): per [surface, theme], pixel-diff the RAW
// reference vs the RAW app capture and FAIL the run if any pair diverges past the threshold.
//   IMGDIFF_TOL      per-channel tolerance (out of 255) that absorbs sub-pixel anti-aliasing shimmer.
//   IMGDIFF_FAIL_PCT max share of differing pixels (%) a surface may have before it FAILs.
//   IMGDIFF_MODE     threshold (default) enforces pixel match; presence fails only when a side is missing.
const IMGDIFF_TOL = 16
const IMGDIFF_FAIL_PCT = 0.5
const IMGDIFF_MODE = process.env.IMGDIFF_MODE || 'threshold'
const ENFORCE_PIXEL_DIFF = IMGDIFF_MODE !== 'presence'

const CHROME = process.env.CHROME_PATH
const CAPTURE_ROOT = process.argv[2]
// Committed reference baselines live beside this script (scripts/visual/baseline/<REF_DIR>/<theme>/) so
// the no-regression gate is self-contained (no /tmp dependency). A staged <base-dir>/<REF_DIR>/<theme>/
// overrides the committed golden if present (e.g. a freshly staged/regenerated baseline, or REF_DIR=demo).
const COMMITTED_ROOT = join(dirname(fileURLToPath(import.meta.url)), 'baseline')
// Prefer a set staged under the capture base (<base>/<REF_DIR>/<theme>/), else the
// COMMITTED golden next to this script. A non-existent committed path is fine — the
// caller's existsSync(refPath) check logs the skip.
const refDirFor = (theme) => {
  const staged = `${CAPTURE_ROOT}/${REF_DIR}/${theme}`
  return existsSync(staged) ? staged : join(COMMITTED_ROOT, REF_DIR, theme)
}
// The LEFT (reference) pane and the RIGHT (subject) pane are both parameterized. The default pairing
// (REF_DIR=demo vs APP_DIR=peasant) puts the fairtrade demo render on the left — a DIFFERENT component
// (TranscriptViewer .txn-*) over the same fixture, so it is a design-language sanity ref, NOT
// surface-parity (see README). Same-component regression gates use SURFACE_SET=changes (baseline/changes)
// and SURFACE_SET=smoke (baseline/smoke-baseline).
const REF_DIR = process.env.REF_DIR || 'demo'
const REF_LABEL = process.env.REF_LABEL || 'fairtrade demo TranscriptViewer reference (cross-component)'
const APP_DIR = process.env.APP_DIR || 'peasant'
const APP_LABEL = process.env.APP_LABEL || 'PEASANT-APP  (session_detail → SessionDetailV2 → SessionDetail)'
const THEMES = ['dark', 'light']
// Surface SETS, selected by SURFACE_SET (default `transcript`). Each entry is
// [surface, gapReason]: gapReason is shown ONLY if the app capture is missing (null = none authored,
// a generic placeholder is drawn instead).
//   transcript — mirrors the demo's transcript surfaces 1:1 (REF_DIR=demo, cross-component ref).
//   changes    — the lifted Changes/ChangeDetail SAME-ENV regression gate: run with
//                SURFACE_SET=changes REF_DIR=changes APP_DIR=peasant, pairing the COMMITTED app-render
//                baseline (baseline/changes/<theme>/, blessed by changes-shoot.mjs) against a fresh
//                changes-shoot capture under <base>/peasant/<theme>/ → expect ~0% (deterministic render).
//   smoke      — the real-binary smoke surfaces from full-app-smoke.mjs. App captures live under
//                <base>/smoke/<theme>/; missing references render as labeled placeholders so visual
//                review can cover every first-class surface before a baseline exists.
//   shell      — the persistent graph shell frames captured by shell-nav-gate.mjs. App captures live under
//                <base>/shell/<theme>/ and prove the shared nav hosts mounted body content in both themes.
const SURFACE_SETS = {
  transcript: [
    ['txn-highlights', null],
    ['txn-scorecard', null],
    ['txn-trace-canvas', null],
    ['txn-scrubber', null],
    ['txn-rails', null],
    ['txn-label-popover', null],
    ['txn-graph', null],
    ['txn-diffs', null],
    ['txn-files', null],
    ['txn-annotations', null],
  ],
  changes: [
    ['gmp-changes', null],
    ['gmp-change-detail', null],
  ],
  smoke: SMOKE_SURFACE_SET,
  shell: GRAPH_SHELL_SURFACE_SET,
}
const SURFACE_SET = process.env.SURFACE_SET || 'transcript'
const SURFACES = SURFACE_SETS[SURFACE_SET]
if (!SURFACES) {
  console.error(`ERROR [stitch-sxs.mjs] unknown SURFACE_SET="${SURFACE_SET}" (known: ${Object.keys(SURFACE_SETS).join(', ')}).`)
  process.exit(1)
}
// Output subdir, PER SURFACE_SET by default, so smoke-SxS and shell/nav-SxS composites never
// land in the same folder and get confused for one another (or silently overwritten — `map.png`
// from `smoke` vs `shell-map.png` from `shell` look adjacent but are DIFFERENT evidence). Override
// with SXS_OUT_SUBDIR for a custom location.
const DEFAULT_OUT_SUBDIR = { smoke: 'sxs-smoke', shell: 'sxs-shell' }[SURFACE_SET] || 'sxs'
const OUT_SUBDIR = process.env.SXS_OUT_SUBDIR || DEFAULT_OUT_SUBDIR

// Per-pane captions, PARAMETERIZED per SURFACE_SET (and per-surface for `changes`). The
// transcript set keeps its env-overridable same-component baseline captions; the `changes`
// fidelity composite names each pane by what it is — "fairtrade demo · <Surface>" (reference)
// vs "peasant /review · <Surface>" (subject) — so a bare `SURFACE_SET=changes` run is correct
// without env overrides (env REF_LABEL/APP_LABEL still win if explicitly set).
const SURFACE_PRETTY = { 'gmp-changes': 'Changes', 'gmp-change-detail': 'Change detail', ...SMOKE_SURFACE_LABELS, ...GRAPH_SHELL_SURFACE_LABELS }
const captionsFor = (surface) => {
  if (SURFACE_SET === 'changes') {
    const name = SURFACE_PRETTY[surface] || surface
    return {
      ref: process.env.REF_LABEL || `fairtrade demo · ${name}`,
      app: process.env.APP_LABEL || `peasant /review · ${name}`,
    }
  }
  if (SURFACE_SET === 'smoke') {
    const name = SURFACE_PRETTY[surface] || surface
    return {
      // Explicit "committed app baseline (regression ref)" — NOT "reference" alone — so this
      // same-component, same-binary regression arm can never be misread as the shell arm's
      // canonical fairtrade-demo reference (a glancer seeing just "reference" could conflate
      // the two; see scripts/visual/baseline/smoke-baseline/).
      ref: process.env.REF_LABEL || `committed app baseline (regression ref) · ${name}`,
      app: process.env.APP_LABEL || `real bin/peasant · ${name}`,
    }
  }
  if (SURFACE_SET === 'shell') {
    const name = SURFACE_PRETTY[surface] || surface
    return {
      ref: process.env.REF_LABEL || `fairtrade in-use demo · ${name}`,
      app: process.env.APP_LABEL || `current peasant app · ${name}`,
    }
  }
  return { ref: REF_LABEL, app: APP_LABEL }
}

const missingReferenceReason = (surface) => {
  if (SURFACE_SET === 'smoke') {
    return `No smoke reference image for ${surface}. Stage one at <base>/${REF_DIR}/<theme>/${surface}.png or scripts/visual/baseline/${REF_DIR}/<theme>/${surface}.png.`
  }
  if (SURFACE_SET === 'shell') {
    return `No fairtrade graph shell reference image for ${surface}. Run shell-nav-gate with DEMO_URL pointing at the fairtrade demo so <base>/${REF_DIR}/<theme>/${surface}.png exists.`
  }
  return `No reference image for ${surface}. Expected <base>/${REF_DIR}/<theme>/${surface}.png or scripts/visual/baseline/${REF_DIR}/<theme>/${surface}.png.`
}

if (!CHROME) {
  console.error('ERROR [stitch-sxs.mjs] CHROME_PATH is unset — set it to your Chrome/Chromium binary.')
  process.exit(1)
}
if (!CAPTURE_ROOT) {
  console.error('ERROR [stitch-sxs.mjs] missing <base-dir> argument.\n  usage: CHROME_PATH=... node scripts/visual/stitch-sxs.mjs <base-dir>')
  process.exit(1)
}

const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new' })
const page = await browser.newPage()
await page.goto('about:blank')

let made = 0
let skipped = 0
const diffResults = [] // { theme, surface, status:'compared'|'dim'|'no-ref'|'no-app', pct, diff, total }
if (!['threshold', 'presence'].includes(IMGDIFF_MODE)) {
  console.error(`ERROR [stitch-sxs.mjs] unknown IMGDIFF_MODE="${IMGDIFF_MODE}" (known: threshold, presence).`)
  process.exit(1)
}
// SMOKE SxS is durable review evidence (committed/deterministic baselines, §README "Real-binary
// smoke SxS") — a missing reference or app capture must FAIL CLOSED with NO placeholder composite
// written, so a reviewer browsing the output directory can never mistake a labeled "not staged"
// panel for real evidence. (The other arms — transcript/changes/shell — keep their existing
// tiered placeholder-for-gaps behavior; see the README "Two-tier failure contract".)
const NO_PLACEHOLDER_ON_GAP = SURFACE_SET === 'smoke'
for (const theme of THEMES) {
  const outDir = `${CAPTURE_ROOT}/${OUT_SUBDIR}/${theme}`
  mkdirSync(outDir, { recursive: true })
  for (const [surface, gap] of SURFACES) {
    const cap = captionsFor(surface)
    const refPath = `${refDirFor(theme)}/${surface}.png`
    const appPath = `${CAPTURE_ROOT}/${APP_DIR}/${theme}/${surface}.png`
    const refUrl = existsSync(refPath) ? dataUrl(refPath) : null
    const appUrl = existsSync(appPath) ? dataUrl(appPath) : null
    if (NO_PLACEHOLDER_ON_GAP && (!refUrl || !appUrl)) {
      const missing = !refUrl && !appUrl ? 'reference AND app' : !refUrl ? 'reference' : 'app'
      console.error(
        `FAIL [stitch-sxs.mjs] ${theme}/${surface}: missing ${missing} capture — NOT writing a placeholder composite ` +
        `(SURFACE_SET=smoke requires durable, fully-paired evidence). ` +
        `${!refUrl ? missingReferenceReason(surface) : ''}${!appUrl ? ` No app capture at ${appPath}.` : ''}`
      )
      // A surface that fails closed must not leave a STALE composite from a prior good run
      // sitting in the output dir — a reviewer browsing the folder could mistake it for current
      // evidence. Remove it so a missing/skipped surface reads as genuinely absent.
      const stalePath = `${outDir}/${surface}.png`
      if (existsSync(stalePath)) unlinkSync(stalePath)
      diffResults.push({ theme, surface, status: !refUrl ? 'no-ref' : 'no-app', pct: Infinity, diff: 0, total: 0 })
      skipped++
      continue
    }
    if (!refUrl) console.error('placeholder (no reference):', theme, surface)
    const meta = await page.evaluate(async (refUrl, appUrl, gap, refGap, title, appLabel, refLabel, surfaceLabel) => {
      const load = (u) => new Promise((res, rej) => { const i = new Image(); i.onload = () => res(i); i.onerror = rej; i.src = u })
      const a = refUrl ? await load(refUrl) : null
      const b = appUrl ? await load(appUrl) : null

      /* sample a robust "page background" = the most common colour around an image's border
         (margins/gutters), weighted toward the BOTTOM row where the padding goes. Used to pad
         the shorter pane seamlessly. Colour is read from the capture — no token is hardcoded. */
      const sampleBg = (img) => {
        const tc = document.createElement('canvas'); tc.width = img.width; tc.height = img.height
        const tx = tc.getContext('2d'); tx.drawImage(img, 0, 0)
        const W = img.width, H = img.height
        const counts = new Map()
        const tally = (px, py) => { const d = tx.getImageData(px, py, 1, 1).data; const k = d[0] + ',' + d[1] + ',' + d[2]; counts.set(k, (counts.get(k) || 0) + 1) }
        const sx = Math.max(1, Math.floor(W / 100)), sy = Math.max(1, Math.floor(H / 100))
        for (let px = 0; px < W; px += sx) { tally(px, H - 1); tally(px, H - 2); tally(px, 0) }
        for (let py = 0; py < H; py += sy) { tally(0, py); tally(W - 1, py) }
        let best = '20,20,22', bestN = -1
        for (const [k, n] of counts) if (n > bestN) { bestN = n; best = k }
        return 'rgb(' + best + ')'
      }

      const drawPlaceholder = (ctx, left, top, width, height, heading, reason) => {
        ctx.fillStyle = '#202022'; ctx.fillRect(left, top, width, height)
        ctx.strokeStyle = '#3a3a3d'; ctx.lineWidth = 2; ctx.strokeRect(left + 1, top + 1, width - 2, height - 2)
        ctx.fillStyle = '#e06c5e'; ctx.font = 'bold 18px ui-monospace, monospace'
        ctx.fillText(heading, left + 20, top + 40)
        ctx.fillStyle = sub; ctx.font = '14px ui-monospace, monospace'
        reason.split('\n').forEach((line, i) => ctx.fillText(line, left + 20, top + 74 + i * 22))
      }

      const pad = 28, gapW = 28, labelH = 64, frame = '#161616', ink = '#f2f2f2', sub = '#9aa0a6'
      const aW = a ? a.width : (b ? b.width : 960)
      const bW = b ? b.width : (a ? Math.max(560, Math.round(a.width * 0.8)) : 960)
      const aH = a ? a.height : (b ? b.height : 640)
      const bH = b ? b.height : aH
      const targetH = Math.max(aH, bH)   // HEIGHT-MATCH to the taller pane
      const w = aW + bW + gapW + pad * 2
      const h = targetH + labelH + pad * 2
      const c = document.createElement('canvas'); c.width = w; c.height = h
      const x = c.getContext('2d')
      x.fillStyle = frame; x.fillRect(0, 0, w, h)
      // title bar — `surface · theme` top-LEFT (kept)
      x.fillStyle = ink; x.font = 'bold 22px ui-sans-serif, system-ui, sans-serif'
      x.fillText(title, pad, 34)
      // SURFACE-UNDER-SCRUTINY label — prominent, TOP-RIGHT (the surface this composite scrutinises)
      x.save()
      x.font = 'bold 28px ui-sans-serif, system-ui, sans-serif'; x.fillStyle = ink; x.textAlign = 'right'
      x.fillText(surfaceLabel, w - pad, 36)
      x.restore()
      // column captions
      x.font = 'bold 16px ui-monospace, monospace'; x.fillStyle = sub
      x.fillText(refLabel, pad, labelH - 8)
      x.fillText(appLabel, pad + aW + gapW, labelH - 8)

      const bodyY = labelH + pad
      const appX = pad + aW + gapW

      // REFERENCE pane — bg-pad to targetH, then draw top-aligned
      if (a) {
        x.fillStyle = sampleBg(a); x.fillRect(pad, bodyY, a.width, targetH)
        x.drawImage(a, pad, bodyY)
      } else {
        drawPlaceholder(x, pad, bodyY, aW, targetH, 'reference not staged', refGap)
      }

      if (b) {
        // SUBJECT pane — bg-pad to targetH, draw top-aligned so rows line up from the top
        x.fillStyle = sampleBg(b); x.fillRect(appX, bodyY, b.width, targetH)
        x.drawImage(b, appX, bodyY)
        // dashed hairline at the shorter pane's content bottom → padded region is obvious
        x.strokeStyle = 'rgba(150,150,150,0.5)'; x.lineWidth = 1; x.setLineDash([6, 5])
        if (a && a.height < targetH) { const yy = bodyY + a.height + 0.5; x.beginPath(); x.moveTo(pad, yy); x.lineTo(pad + a.width, yy); x.stroke() }
        if (b.height < targetH) { const yy = bodyY + b.height + 0.5; x.beginPath(); x.moveTo(appX, yy); x.lineTo(appX + b.width, yy); x.stroke() }
        x.setLineDash([])
      } else {
        const reason = gap || 'No app capture for this surface — see the run log and the manifest gaps section.'
        drawPlaceholder(x, appX, bodyY, bW, targetH, 'surface not captured', reason)
      }
      return { url: c.toDataURL('image/png'), aH: a ? a.height : null, bH: b ? b.height : null, targetH }
    }, refUrl, appUrl, gap, missingReferenceReason(surface), `${surface}  ·  ${theme}`, cap.app, cap.ref, surface)
    const b64 = meta.url.replace(/^data:image\/png;base64,/, '')
    writeFileSync(`${outDir}/${surface}.png`, Buffer.from(b64, 'base64'))
    made++
    const padNote = meta.aH == null ? 'NO-REF|app' : meta.bH == null ? 'ref|GAP' : (meta.aH === meta.bH ? 'equal' : `pad ${meta.aH < meta.bH ? 'REF' : 'APP'} +${Math.abs(meta.aH - meta.bH)}px → ${meta.targetH}`)
    console.log('sxs', `${theme}/${surface}`.padEnd(34), padNote)

    // imgdiff arm: pixel-diff the RAW reference vs RAW app capture (NOT the height-matched composite).
    // A missing app capture cannot be compared → fail closed; a size mismatch (dim) → pct = Infinity.
    if (!refUrl) {
      diffResults.push({ theme, surface, status: 'no-ref', pct: Infinity, diff: 0, total: 0 })
    } else if (!appUrl) {
      diffResults.push({ theme, surface, status: 'no-app', pct: Infinity, diff: 0, total: 0 })
    } else {
      const r = await diffPixels(page, refUrl, appUrl, IMGDIFF_TOL, false)
      const pct = r.dim ? Infinity : (100 * r.diff) / r.total
      diffResults.push({ theme, surface, status: r.dim ? 'dim' : 'compared', pct, diff: r.dim ? 0 : r.diff, total: r.dim ? 0 : r.total })
    }
  }
}
console.log(
  `\nbuilt ${made} height-matched side-by-side composites under ${CAPTURE_ROOT}/${OUT_SUBDIR}/  (REF_DIR=${REF_DIR} vs APP_DIR=${APP_DIR})` +
  (skipped ? `  — ${skipped} surface(s) SKIPPED (no placeholder; missing ref/app, see FAIL lines above; the run still fails closed below)` : '')
)
await browser.close()

/* ── imgdiff summary + pass/fail gate (additive to the composites above) ──────────────────────────────
   The composites are for human glance; THIS is the automated pass/fail. A surface PASSES iff it was
   comparable (ref + app both present, same size) AND its differing-pixel share is within IMGDIFF_FAIL_PCT.
   Fail closed: a non-comparable surface (no ref / no app / dim size mismatch) FAILs, and a run that
   compared nothing is a FAILURE, never a vacuous 0.0000%. */
console.log(`\n=== imgdiff (mode=${IMGDIFF_MODE}, TOL=${IMGDIFF_TOL}/255${ENFORCE_PIXEL_DIFF ? `, FAIL > ${IMGDIFF_FAIL_PCT.toFixed(2)}% differing pixels` : ', missing sides fail; pixel drift is visual-review evidence'}) ===`)
let worst = 0
let failures = 0
let comparedCount = 0
let pairedCount = 0
for (const d of diffResults) {
  let tag
  if (d.status === 'no-ref') { tag = 'NO-REF'; failures++ }
  else if (d.status === 'no-app') { tag = 'NO-APP'; failures++ }
  else if (d.status === 'dim') {
    pairedCount++
    tag = ENFORCE_PIXEL_DIFF ? 'DIM!' : 'DIM'
    if (ENFORCE_PIXEL_DIFF) failures++
  }
  else {
    pairedCount++
    comparedCount++
    worst = Math.max(worst, d.pct)
    if (d.pct > IMGDIFF_FAIL_PCT) {
      tag = ENFORCE_PIXEL_DIFF ? 'DIFF!' : 'DIFF'
      if (ENFORCE_PIXEL_DIFF) failures++
    }
    else tag = d.pct === 0 ? 'IDENTICAL' : d.pct < 0.05 ? 'ok~' : 'CHECK'
  }
  const pctStr = Number.isFinite(d.pct) ? `${d.pct.toFixed(4)}%` : '   --   '
  const counts = d.status === 'compared' ? ` (${d.diff}/${d.total})` : ''
  console.log(`${tag.padEnd(10)} ${`${d.theme}/${d.surface}`.padEnd(34)} ${pctStr}${counts}`)
}
console.log(`\nworst: ${worst.toFixed(4)}%  compared: ${comparedCount}/${diffResults.length}  paired: ${pairedCount}/${diffResults.length}  failures: ${failures}`)

if ((ENFORCE_PIXEL_DIFF ? comparedCount : pairedCount) === 0) {
  const smokeNoRefHint = SURFACE_SET === 'smoke'
    ? `  For SURFACE_SET=smoke, this usually means current smoke captures exist but no smoke reference baseline is staged.\n` +
      `  Fix: run full-app-smoke for current captures, then stage references under ${CAPTURE_ROOT}/${REF_DIR}/<theme>/ or scripts/visual/baseline/${REF_DIR}/<theme>/.`
    : SURFACE_SET === 'shell'
      ? `  For SURFACE_SET=shell, this usually means current shell captures exist but no fairtrade demo reference is staged.\n` +
        `  Fix: run shell-nav-gate with DEMO_URL pointing at the fairtrade demo so references exist under ${CAPTURE_ROOT}/${REF_DIR}/<theme>/.`
    : `  Fix: run the shoot for ${APP_DIR} in both themes so ${CAPTURE_ROOT}/${APP_DIR}/<theme>/<surface>.png exist, then re-stitch.`
  console.error(
    `\nFAIL [stitch-sxs.mjs] imgdiff compared ZERO surfaces — the gate would pass vacuously.\n` +
    `  Means: no [surface, theme] pair had BOTH a reference (baseline/${REF_DIR}/) and an app capture (${APP_DIR}/).\n` +
    smokeNoRefHint
  )
  process.exit(1)
}
if (failures > 0) {
  console.error(
    `\nFAIL [stitch-sxs.mjs] imgdiff gate did not pass cleanly (${failures} failing surface(s); worst ${worst.toFixed(4)}% > ${IMGDIFF_FAIL_PCT.toFixed(2)}%).\n` +
    `  NO-REF/NO-APP = a surface could not be compared (missing baseline or app capture) — fail closed.\n` +
    `  DIM!          = the reference and app capture differ in size.\n` +
    `  DIFF!         = the differing-pixel share exceeds ${IMGDIFF_FAIL_PCT.toFixed(2)}%.\n` +
    `  Fix: inspect the flagged rows + the matching composite under ${CAPTURE_ROOT}/${OUT_SUBDIR}/<theme>/, and correct the visual regression (or restage the missing capture).`
  )
  process.exit(1)
}
