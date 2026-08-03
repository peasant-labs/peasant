import type { Metadata } from 'next';
import { LayoutShell } from '@/components/LayoutShell';
import { atkinsonHyperlegible, atkinsonHyperlegibleMono } from '@/app/fonts';
import '@peasant-labs/fairtrade/components.css';
// The /map (CodeMap) and /review (Changes/ChangeDetail) routes consume
// @peasant-labs/fairtrade/graph — a SEPARATE CSS bundle from components.css by
// design (bundle-isolation: an app importing the graph surfaces never ships the
// village commons.css selectors, and vice-versa). Nothing auto-loads it — Vite
// library mode never side-effect-injects a CSS import into the JS entry
// (fairtrade's vite.lib-css.config.js emits it as an independent artifact) — so
// without this explicit import every `.gmp-*` rule (the CodeMap legend, the
// Changes/ChangeDetail chrome) silently falls back to unstyled default browser
// layout in production, even though it renders correctly wherever graph.css
// happens to be imported directly (Storybook, dev/changes-harness).
import '@peasant-labs/fairtrade/graph.css';
// NOTE: @peasant-labs/fairtrade/fonts.css is NOT imported — it loads Atkinson via a
// remote `@import url(...)`, which Next's CSS bundling relocates AFTER other rules
// (an invalid @import position) and silently DROPS, leaving design-system text on a
// ui-monospace fallback. The fonts are self-hosted instead via next/font/local
// (src/app/fonts/index.ts) — its `.variable` className below feeds
// --font-atkinson-hyperlegible(-mono), which globals.css uses to override
// fairtrade's --font-body/--font-display/--font-mono tokens. Self-hosting (build-time
// fetch, no runtime request) also removes the earlier <link>-to-Google-Fonts
// approach's runtime CDN dependency and its display:swap flash-of-fallback-font
// while that round-trip resolves.
import './globals.css';
import '@peasant-labs/transcript-browser/styles.css';
// The /analytics route consumes @peasant-labs/fairtrade/analytics — its own
// per-surface CSS bundle, same isolation model as graph.css above; nothing
// auto-loads it, so the dashboard ships unstyled without this import.
import '@peasant-labs/fairtrade/analytics.css';

export const metadata: Metadata = {
  title: 'Peasant',
  description: 'Local-first receipts for agentic work. Sharing is opt-in, redaction-first.',
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html
      lang="en"
      data-theme="dark"
      data-tb-theme="dark"
      className={`${atkinsonHyperlegible.variable} ${atkinsonHyperlegibleMono.variable}`}
    >
      <body className="min-h-screen bg-surface text-ink antialiased">
        <LayoutShell>
          <main className="min-h-screen pt-[var(--app-header-height)] grid-snap">{children}</main>
        </LayoutShell>
      </body>
    </html>
  );
}
