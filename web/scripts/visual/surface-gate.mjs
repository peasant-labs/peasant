/* non-empty-surface gate — the parity oracle's content assertion (app side).

   ONE implementation, imported by the capture harness (peasant-shoot.mjs, the production path that
   writes the app-side baselines). It mirrors the demo side's gate so demo and app captures are held
   to the SAME non-empty bar; a regression that blanks a surface (the empty-graph class) fails the run
   on whichever side it occurs.

   A blank / near-empty capture has a perfectly valid bounding box but paints only the background
   colour, so a box-size check cannot catch it — that is the silent-blank hole this gate closes. The
   gate decodes the PNG (in a headless page canvas, so there is no extra dependency) and FAILS unless
   the capture carries real content:
     - byte size      >= a per-surface floor (a full-size background-only PNG is ~5.9KB);
     - non-background  >= a minimum share of pixels differ from the dominant (background) colour;
     - distinct colour >= a minimum (a flat fill resolves to a single colour);
     - uniqueness      : no two DISTINCT surfaces may be byte-identical (a stale nav can otherwise
                         produce N identical blanks across totally different surfaces).

   Thresholds are calibrated against measured populations (both themes):
     blank capture : nonbg 0.00%, 1 distinct colour, ~5.9KB full-size
     real content  : nonbg 2.46%..23.2%, 9..161 colours, 35KB..176KB full / 491..645B scrubber
   each bound sits strictly between the two populations, so the gate fails every blank and passes
   every real surface. */

import { readFileSync } from 'node:fs'
import { createHash } from 'node:crypto'

export const DEFAULT_MIN_BYTES = 16 * 1024 // a full-size background-only PNG is ~5.9KB; the smallest real full surface (scorecard) is ~35KB
export const BYTE_FLOORS = {
  'txn-scrubber': 400, // sticky condensed header; small but real, so content signal matters more than byte size
  'shell-changes': 900,
  'shell-map': 900,
  'shell-analytics': 900,
}
export const MIN_NONBG_RATIO = 0.012 // blank = 0.00%; the least-busy real surface diverges from its background by >= 2.46%
export const MIN_DISTINCT_COLORS = 6 // a flat fill resolves to 1 colour; the sparsest real surface (the scrubber) has 9

/* decode a PNG and measure: non-background-pixel ratio, dominant-colour share, distinct colours.
   the dominant colour is the mode over a 4-bit-per-channel quantization — for a blank capture that is
   ~100% of the image; for a real capture the background still wins, but a real fraction of pixels
   diverge from it. `page` is any puppeteer Page (used only as a dependency-free PNG decoder). */
export const measurePng = (page, dataUrl) =>
  page.evaluate(async (url) => {
    const im = await new Promise((res, rej) => { const i = new Image(); i.onload = () => res(i); i.onerror = () => rej(new Error('PNG decode failed')); i.src = url })
    const c = document.createElement('canvas'); c.width = im.width; c.height = im.height
    const x = c.getContext('2d'); x.drawImage(im, 0, 0)
    const d = x.getImageData(0, 0, c.width, c.height).data
    const n = d.length / 4
    const hist = new Map()
    for (let i = 0; i < d.length; i += 4) {
      const key = ((d[i] >> 4) << 8) | ((d[i + 1] >> 4) << 4) | (d[i + 2] >> 4) // 4-bit/channel bucket
      hist.set(key, (hist.get(key) || 0) + 1)
    }
    let bgKey = 0, bgCount = -1
    for (const [k, v] of hist) if (v > bgCount) { bgCount = v; bgKey = k }
    const bgR = ((bgKey >> 8) & 0xf) << 4, bgG = ((bgKey >> 4) & 0xf) << 4, bgB = (bgKey & 0xf) << 4
    let nonbg = 0
    for (let i = 0; i < d.length; i += 4) {
      if (Math.abs(d[i] - bgR) > 24 || Math.abs(d[i + 1] - bgG) > 24 || Math.abs(d[i + 2] - bgB) > 24) nonbg++
    }
    return { w: im.width, h: im.height, pixels: n, nonbgRatio: nonbg / n, bgShare: bgCount / n, distinctColors: hist.size }
  }, dataUrl)

/* SurfaceGate tracks the per-run set of accepted captures so it can reject byte-identical duplicates.
   construct one per capture run; call assert() after writing each surface PNG. */
export class SurfaceGate {
  constructor(page) {
    this.page = page
    this.seen = new Map() // md5(file bytes) -> surface name
  }

  /* measure a PNG without enforcing — returns { bytes, md5, w, h, nonbgRatio, bgShare, distinctColors } */
  async measure(file) {
    const buf = readFileSync(file)
    const m = await measurePng(this.page, 'data:image/png;base64,' + buf.toString('base64'))
    return { bytes: buf.length, md5: createHash('md5').update(buf).digest('hex'), ...m }
  }

  /* enforce the gate for one surface; throws an actionable error on any blank/near-empty/duplicate.
     `where` names the caller (e.g. "peasant-shoot.mjs") so the error points at the right place. */
  async assert(name, file, { sel = '', where = 'surface-gate' } = {}) {
    const r = await this.measure(file)
    const fail = (what, why, fix) => {
      throw new Error(
        `ERROR [${where}] Non-empty-surface assertion failed for "${name}".\n` +
        `  What failed: ${what}\n` +
        `  Why: ${why}\n` +
        `  Where: surface-gate.mjs SurfaceGate.assert("${name}", "${file}")${sel ? ` — selector "${sel}"` : ''}.\n` +
        `  Means: the captured PNG is blank/near-empty/duplicated, so the side-by-side parity diff would\n` +
        `         compare an empty surface and pass vacuously (the failure mode this gate exists to stop).\n` +
        `  Fix: ${fix}`
      )
    }
    /* surfaces are captured as "<surface>" (per-theme dir) and may also be stored as "<surface>-<theme>";
       accept either form when looking up a per-surface floor */
    const baseName = name.replace(/-(dark|light)$/, '')
    const minBytes = BYTE_FLOORS[name] ?? BYTE_FLOORS[baseName] ?? DEFAULT_MIN_BYTES
    if (r.bytes < minBytes) fail(
      `PNG is ${r.bytes} bytes (< ${minBytes} floor for "${name}").`,
      `a background-only capture of this size compresses to ~5.9KB; the surface did not paint content.`,
      `confirm the navigation step that reveals "${name}" ran, and that the live-compositor capture (captureBeyondViewport:false) saw it on-screen.`)
    if (r.nonbgRatio < MIN_NONBG_RATIO) fail(
      `only ${(r.nonbgRatio * 100).toFixed(2)}% of pixels differ from the background (need >= ${(MIN_NONBG_RATIO * 100).toFixed(1)}%); background fills ${(r.bgShare * 100).toFixed(1)}%.`,
      `the capture is a near-uniform fill — the surface rendered blank even though its box was valid.`,
      `ensure the surface actually mounted with content before capture (check the preceding nav/interaction step).`)
    if (r.distinctColors < MIN_DISTINCT_COLORS) fail(
      `only ${r.distinctColors} distinct colours (need >= ${MIN_DISTINCT_COLORS}).`,
      `a real surface (text, icons, borders) resolves to many colours; a flat fill resolves to a few.`,
      `confirm the surface rendered real UI, not an empty/placeholder state.`)
    if (this.seen.has(r.md5)) fail(
      `byte-identical (md5 ${r.md5.slice(0, 12)}) to an already-captured surface "${this.seen.get(r.md5)}".`,
      `two distinct surfaces produced the exact same PNG — at least one captured the wrong (or a blank) view.`,
      `verify the navigation between "${this.seen.get(r.md5)}" and "${name}" actually changed what is on screen.`)
    this.seen.set(r.md5, name)
    return r
  }
}
