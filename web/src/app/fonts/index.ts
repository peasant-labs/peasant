import localFont from 'next/font/local';

/**
 * Self-hosted fairtrade typefaces (Atkinson Hyperlegible / …Mono) — replaces the
 * prior runtime stylesheet link to the Google Fonts CDN (see the removed
 * FAIRTRADE_FONTS_HREF constant in layout.tsx's git history).
 *
 * Two problems with that prior approach:
 *  1. FOUT: the browser paints with a fallback font (`ui-sans-serif`/`ui-monospace`)
 *     until the Google Fonts round-trip (a `<link>` request, then the actual woff2)
 *     completes. On a slow/first connection, the fallback can be visible for a
 *     noticeable flash before the swap — this looks like a "wrong font" bug even
 *     though the FINAL computed style is correct once loaded.
 *  2. External dependency: every real-binary page load phones home to
 *     fonts.googleapis.com / fonts.gstatic.com — in tension with peasant's
 *     local-first "nothing has left it" positioning, and a real failure mode if
 *     that origin is unreachable (offline dev, a corporate/ad-blocker firewall).
 *
 * next/font/local self-hosts the exact same woff2 files (fetched once, vendored
 * under this directory) at BUILD time — Next inlines the @font-face + a
 * generated, collision-free font-family name, and injects the <link rel=preload>
 * for the font file itself into the document, with NO runtime call to Google.
 * This survives `next build --output=export` (next/font is Next's own supported
 * mechanism, unlike the CSS `@import` that the Lightning CSS bundler drops — see
 * src/test/font-import-guard.test.ts).
 *
 * `next/font/google` was considered first (the more common wrapper) but its
 * curated font list does not include Atkinson Hyperlegible or Atkinson
 * Hyperlegible Mono (not present in the installed next/font/google/index.d.ts) —
 * so the fonts are vendored as local files instead, sourced from the same
 * fonts.gstatic.com URLs the design system's own fonts.css `@import` references
 * (latin subset only — the app's UI copy is English).
 */
export const atkinsonHyperlegible = localFont({
  src: [
    { path: './atkinson-hyperlegible-400.woff2', weight: '400', style: 'normal' },
    { path: './atkinson-hyperlegible-700.woff2', weight: '700', style: 'normal' },
    { path: './atkinson-hyperlegible-400-italic.woff2', weight: '400', style: 'italic' },
    { path: './atkinson-hyperlegible-700-italic.woff2', weight: '700', style: 'italic' },
  ],
  variable: '--font-atkinson-hyperlegible',
  display: 'swap',
  preload: true,
});

export const atkinsonHyperlegibleMono = localFont({
  src: [
    { path: './atkinson-hyperlegible-mono-400.woff2', weight: '400 700', style: 'normal' },
    { path: './atkinson-hyperlegible-mono-400-italic.woff2', weight: '400', style: 'italic' },
  ],
  variable: '--font-atkinson-hyperlegible-mono',
  display: 'swap',
  preload: true,
});
