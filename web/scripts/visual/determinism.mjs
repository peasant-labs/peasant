/* Deterministic, time-frozen + reduced-motion page fixture for the two-arm visual-regression gate.

   A pixel diff is only meaningful if the two arms (and successive runs) render byte-stable output. Two
   things otherwise drift every run: the wall clock (relative timestamps like "3 minutes ago", anything
   derived from Date.now()/new Date()) and Math.random (ids, jitter, shuffles). This helper pins both and
   keeps the existing reduced-motion emulation, so a capture is reproducible and the imgdiff is stable.

   applyDeterminism(page) must be called on a fresh Page BEFORE page.goto(), because the freeze is
   installed via page.evaluateOnNewDocument — it runs before any document script on every navigation. */

/* A fixed, clearly-named wall-clock every harness page is pinned to (2024-01-01T12:00:00.000Z). Any
   "now"-derived rendering resolves to this instant on every run and on both arms. */
export const FROZEN_EPOCH_MS = Date.UTC(2024, 0, 1, 12, 0, 0)

/* Deterministic PRNG seed; Math.random, crypto.getRandomValues, and crypto.randomUUID are all replaced
   with a single seeded mulberry32 so random-driven layout + ids are reproducible across runs. */
export const PRNG_SEED = 0x9e3779b9

/* (a) keep prefers-reduced-motion: reduce (no animation shimmer at capture time), and
   (b) freeze Date + Math.random + crypto randomness on every new document, before any page script runs. */
export const applyDeterminism = async (page, { epochMs = FROZEN_EPOCH_MS, seed = PRNG_SEED } = {}) => {
  await page.emulateMediaFeatures([{ name: 'prefers-reduced-motion', value: 'reduce' }])
  await page.evaluateOnNewDocument((FIXED, SEED) => {
    /* mulberry32 — a small, fast, deterministic PRNG seeded once per document */
    let s = SEED >>> 0
    const prng = () => {
      s |= 0; s = (s + 0x6D2B79F5) | 0
      let t = Math.imul(s ^ (s >>> 15), 1 | s)
      t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296
    }
    Math.random = prng

    /* crypto: seed getRandomValues + randomUUID off the SAME PRNG so crypto-based
       element ids (libs that key DOM nodes off crypto.randomUUID / getRandomValues)
       can't reintroduce per-run non-determinism the screenshot would catch. */
    try {
      if (typeof crypto !== 'undefined') {
        if (typeof crypto.getRandomValues === 'function') {
          crypto.getRandomValues = (arr) => {
            const bytes = new Uint8Array(arr.buffer, arr.byteOffset, arr.byteLength)
            for (let i = 0; i < bytes.length; i++) bytes[i] = (prng() * 256) | 0
            return arr
          }
        }
        crypto.randomUUID = () => {
          // RFC-4122 v4 shape with digits drawn from the seeded PRNG (deterministic)
          let out = ''
          for (let i = 0; i < 36; i++) {
            if (i === 8 || i === 13 || i === 18 || i === 23) { out += '-'; continue }
            if (i === 14) { out += '4'; continue }
            const r = (prng() * 16) | 0
            out += (i === 19 ? (r & 0x3) | 0x8 : r).toString(16)
          }
          return out
        }
      }
    } catch {
      /* a locked-down crypto (non-writable method) — fall back to the real one; Math.random is still seeded */
    }

    /* pin the clock: new Date() with no args and Date.now() both resolve to FIXED; explicit args and
       Date.parse/Date.UTC keep their real behaviour so the page can still parse payload timestamps. */
    const RealDate = Date
    class FrozenDate extends RealDate {
      constructor(...args) {
        if (args.length === 0) super(FIXED)
        else super(...args)
      }
      static now() { return FIXED }
    }
    FrozenDate.parse = RealDate.parse
    FrozenDate.UTC = RealDate.UTC
    window.Date = FrozenDate
    globalThis.Date = FrozenDate
  }, epochMs, seed)
}
