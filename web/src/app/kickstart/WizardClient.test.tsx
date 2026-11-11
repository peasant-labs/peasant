import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { WizardClient } from './WizardClient';
import { loadCases } from '@/test/fixtures/kickstart';
import { playCase, teardown, screen } from '@/test/kickstartHarness';

vi.mock('next/navigation', () => ({
  usePathname: () => '/kickstart',
  useSearchParams: () => new URLSearchParams(),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}));

/**
 * The mounted /kickstart route on the real WebSocket provider, with the HTTP
 * and WebSocket boundary played from fixtures. The page must draw what the
 * server served and decide nothing on top of it.
 */

afterEach(teardown);

const PATH = 'src/app/kickstart/testdata/wizard_render.yaml';

describe('/kickstart renders the served model', () => {
  for (const entry of loadCases(PATH)) {
    if (entry.name === 'both-themes') continue;
    it(entry.name, async () => {
      await playCase(entry, <WizardClient />);
    });
  }

  it('both-themes', async () => {
    const entry = loadCases(PATH).find((candidate) => candidate.name === 'both-themes')!;
    const seen: string[][] = [];
    for (const theme of entry.themes as string[]) {
      document.documentElement.setAttribute('data-theme', theme);
      await playCase(entry, <WizardClient />);
      seen.push(screen.getAllByRole('button').map((button) => button.textContent ?? ''));
      teardown();
    }
    document.documentElement.removeAttribute('data-theme');
    // The same controls in both themes: the theme changes tokens, never structure.
    expect(seen[1]).toEqual(seen[0]);
    // And the stylesheet reaches every colour, size and weight through a token.
    const css = readFileSync(resolve(process.cwd(), 'src/components/kickstart/kickstart.css'), 'utf8');
    expect(css, 'a literal colour in the wizard stylesheet').not.toMatch(/#[0-9a-fA-F]{3,8}\b|rgba?\(|hsla?\(/);
    expect(css, 'a rounded corner in the wizard stylesheet').not.toMatch(/border-radius\s*:\s*(?!0\b)/);
  });
});
