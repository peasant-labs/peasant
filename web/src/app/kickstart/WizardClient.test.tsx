import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { WizardClient } from './WizardClient';
import { loadCases } from '@/test/fixtures/kickstart';
import { playCase, teardown } from '@/test/kickstartHarness';

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
    it(entry.name, async () => {
      await playCase(entry, <WizardClient />);
    });
  }
});

// jsdom applies no stylesheet and has no layout, so a test cannot see a theme
// from here: the two themes are judged on the built page in the browser journey.
// What this can hold is two things in the source of the stylesheet itself: it
// writes no colour literal, and it rounds no corner. Sizes and weights are not
// read here.
describe('the wizard stylesheet', () => {
  it('writes no literal colour and rounds no corner', () => {
    const css = readFileSync(resolve(process.cwd(), 'src/components/kickstart/kickstart.css'), 'utf8');
    expect(css, 'a literal colour in the wizard stylesheet').not.toMatch(/#[0-9a-fA-F]{3,8}\b|rgba?\(|hsla?\(/);
    expect(css, 'a rounded corner in the wizard stylesheet').not.toMatch(/border-radius\s*:\s*(?!0\b)/);
  });
});
