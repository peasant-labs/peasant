import type { Metadata } from 'next';
import { LayoutShell } from '@/components/LayoutShell';
import { atkinsonHyperlegible, atkinsonHyperlegibleMono } from '@/app/fonts';
import { THEME_BOOT_SCRIPT } from '@/lib/themeBoot';
// The stylesheet loads Tailwind preflight before Fairtrade's canonical base.
// Self-hosted fonts supply the Fairtrade font tokens without a remote import.
import './globals.css';

export const metadata: Metadata = {
  title: 'peasant',
  description: 'Local-first receipts for agentic work. Sharing is opt-in, redaction-first.',
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    // data-theme="dark" is what the export renders; the head script below
    // replaces it before the first paint with the person's stored choice, or
    // the system's scheme when there is none, and useTheme keeps it there.
    // The attributes are expected to differ from the export's at hydration.
    <html
      lang="en"
      data-theme="dark"
      data-tb-theme="dark"
      suppressHydrationWarning
      className={`${atkinsonHyperlegible.variable} ${atkinsonHyperlegibleMono.variable}`}
    >
      <head>
        {/* A plain inline script, run as the browser parses the head, before
            the body paints. The text is the fixed constant from
            lib/themeBoot.ts, never anything a person supplied. */}
        <script dangerouslySetInnerHTML={{ __html: THEME_BOOT_SCRIPT }} />
      </head>
      <body className="min-h-screen bg-surface text-ink antialiased">
        <LayoutShell>
          {children}
        </LayoutShell>
      </body>
    </html>
  );
}
