import { afterEach, describe, expect, it, vi } from 'vitest';
import { WizardClient } from './WizardClient';
import { settingsSection } from '@/lib/settings/catalog';
import { loadCases } from '@/test/fixtures/kickstart';
import { playCase, teardown } from '@/test/kickstartHarness';

vi.mock('next/navigation', () => ({
  usePathname: () => '/kickstart',
  useSearchParams: () => new URLSearchParams(),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}));

/**
 * The live session, the finish panel, the village sign-in and the privacy
 * example, each played on the mounted /kickstart route with the HTTP and
 * WebSocket boundary scripted from fixtures.
 */

afterEach(teardown);

const FILES = {
  'the live session': 'src/app/kickstart/testdata/live_session.yaml',
  'the finish panel': 'src/app/kickstart/testdata/finish.yaml',
  'a save that was refused': 'src/app/kickstart/testdata/commit_failures.yaml',
  'the village sign-in': 'src/app/kickstart/testdata/village.yaml',
  'the privacy example': 'src/app/kickstart/testdata/privacy.yaml',
} as const;

for (const [title, path] of Object.entries(FILES)) {
  describe(title, () => {
    for (const entry of loadCases(path)) {
      it(entry.name, async () => {
        await playCase(entry, <WizardClient />, ['section']);
        if (typeof entry.section === 'string') {
          // The link lands on a section the settings page really has, not on
          // the default it falls back to for a name it does not know.
          expect(settingsSection(entry.section).id).toBe(entry.section);
        }
      });
    }
  });
}
