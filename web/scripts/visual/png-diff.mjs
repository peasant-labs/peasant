/* Pixel-diff primitive for the two-arm visual-regression gate — a vendored mirror of the fairtrade
   design-system harness's shared diff primitive. Copied here (rather than cross-repo imported) so this
   repo's gate is self-contained; keep it in sync if the upstream primitive changes.

   diffPixels decodes two PNG data: URLs inside a headless page (data: images don't taint the canvas, so
   getImageData works with no extra dependency), draws both onto a canvas, and counts the pixels whose
   per-channel delta exceeds `tol`.

   `tol` semantics: tol=16 means "no channel differs by more than 16/255"; a pixel is counted as differing
   only when at least one of R/G/B exceeds the threshold. This tolerates sub-pixel anti-aliasing shimmer
   from font rendering, so the count reflects real colour change, not AA noise. A return of `diff: 0` means
   no pixel exceeded `tol`. */
import { readFileSync } from 'node:fs'

/* read a PNG off disk as a data: URL the headless page can decode without tainting the canvas */
export const dataUrl = (p) => 'data:image/png;base64,' + readFileSync(p).toString('base64')

/* compare two PNG data: URLs in `page` (any puppeteer Page parked on about:blank works as a
   dependency-free decoder). Returns:
     { dim:true, aw, ah, bw, bh }                              when the two differ in size
     { dim:false, total, diff, minY, maxY, url }               otherwise
   `total` = pixel count, `diff` = pixels exceeding `tol`, `minY..maxY` = the differing band.
   With save=true, `url` is a data: URL of B with differing pixels painted magenta. */
export const diffPixels = (page, dataUrlA, dataUrlB, tol, save = false) =>
  page.evaluate(
    async (au, bu, t, sv) => {
      const load = (u) => new Promise((res) => { const im = new Image(); im.onload = () => res(im); im.src = u })
      const [ia, ib] = await Promise.all([load(au), load(bu)])
      if (ia.width !== ib.width || ia.height !== ib.height) return { dim: true, aw: ia.width, ah: ia.height, bw: ib.width, bh: ib.height }
      const c = document.createElement('canvas'); c.width = ia.width; c.height = ia.height
      const x = c.getContext('2d')
      x.drawImage(ia, 0, 0); const da = x.getImageData(0, 0, c.width, c.height).data
      x.clearRect(0, 0, c.width, c.height)
      x.drawImage(ib, 0, 0); const db = x.getImageData(0, 0, c.width, c.height).data
      let diff = 0, minY = 1e9, maxY = -1
      for (let i = 0; i < da.length; i += 4) {
        if (Math.abs(da[i] - db[i]) > t || Math.abs(da[i + 1] - db[i + 1]) > t || Math.abs(da[i + 2] - db[i + 2]) > t) {
          diff++
          const y = (i / 4 / c.width) | 0
          if (y < minY) minY = y
          if (y > maxY) maxY = y
          if (sv) { db[i] = 255; db[i + 1] = 0; db[i + 2] = 255; db[i + 3] = 255 }
        }
      }
      let url = null
      if (sv) { x.putImageData(new ImageData(db, c.width, c.height), 0, 0); url = c.toDataURL('image/png') }
      return { dim: false, total: c.width * c.height, diff, minY: maxY < 0 ? 0 : minY, maxY, url }
    },
    dataUrlA, dataUrlB, tol, save,
  )
