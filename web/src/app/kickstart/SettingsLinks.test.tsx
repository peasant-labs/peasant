import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import SettingsPageClient from '@/app/settings/SettingsPageClient';
import { WizardClient } from './WizardClient';
import { loadCases } from '@/test/fixtures/kickstart';
import { loadSettingsResponse } from '@/test/fixtures/settings';
import { playCase, teardown } from '@/test/kickstartHarness';

const route = vi.hoisted(() => ({ search: '' }));
vi.mock('next/navigation', () => ({
  usePathname: () => '/kickstart',
  useSearchParams: () => new URLSearchParams(route.search),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}));

/**
 * The two links the settings page gains, and the setup page opened from the
 * first of them. The settings page itself is unchanged: its fields and saving
 * are covered by its own suite.
 */

afterEach(() => {
  teardown();
  route.search = '';
});

function entry() {
  return loadCases('src/app/kickstart/testdata/redirect_routes.yaml').find((candidate) => candidate.name === 'settings-rerun-link-prefills')!;
}

describe('settings-rerun-link-prefills', () => {
  it('links to setup with its origin, and to the selection page', async () => {
    const { link, selectionLink } = entry() as unknown as {
      link: { name: string; href: string };
      selectionLink: { name: string; href: string };
    };
    vi.stubGlobal('fetch', async (input: RequestInfo | URL) => {
      const path = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, 'http://localhost').pathname;
      const body = path === '/api/v1/settings' ? loadSettingsResponse() : { authenticated: false };
      return new Response(JSON.stringify(body), { status: 200 });
    });
    render(<SettingsPageClient section="projects" />);
    await waitFor(() => expect(screen.getByRole('link', { name: selectionLink.name })).toBeInTheDocument());
    expect(screen.getByRole('link', { name: link.name })).toHaveAttribute('href', link.href);
    expect(screen.getByRole('link', { name: selectionLink.name })).toHaveAttribute('href', selectionLink.href);
    cleanup();
  });

  it('opens setup from settings with the saved choices drawn as the server served them', async () => {
    const { origin } = entry() as unknown as { origin: string };
    route.search = `from=${origin}`;
    await playCase(entry(), <WizardClient />, ['link', 'selectionLink', 'origin']);
  });
});
