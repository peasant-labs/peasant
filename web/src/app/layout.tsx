import type { Metadata } from 'next';
import { LayoutShell } from '@/components/LayoutShell';
import { atkinsonHyperlegible, atkinsonHyperlegibleMono } from '@/app/fonts';
// The stylesheet loads Tailwind preflight before Fairtrade's canonical base.
// Self-hosted fonts supply the Fairtrade font tokens without a remote import.
import './globals.css';

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
          {/* The offline notice may move focus here (making <main> focusable only for that
              move); it is a container, not a control, so it draws no focus ring. */}
          <main className="min-h-screen pt-[var(--app-header-height)] grid-snap focus:outline-none">{children}</main>
        </LayoutShell>
      </body>
    </html>
  );
}
