import { cleanup, render, act } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { firstRunRoutes, isFirstRunRoute, resetFirstRun, useFirstRunRedirect } from './useFirstRun';
import { LOCAL_SECTIONS } from '@/lib/nav/sections';
import { loadCases } from '@/test/fixtures/kickstart';
import { KickstartServer } from '@/test/kickstartHarness';

const nav = vi.hoisted(() => ({ pathname: '/', replace: vi.fn() }));
vi.mock('next/navigation', () => ({
  usePathname: () => nav.pathname,
  useSearchParams: () => new URLSearchParams(),
  useRouter: () => ({ push: vi.fn(), replace: nav.replace }),
}));

/**
 * The first-run redirect, mounted on the real hook with the status route played
 * from a fixture. The pages that redirect are derived from the section
 * registry; the fixture states the pages that must and must not.
 */

const STATUS = 'GET /api/v1/kickstart/status';
const PATH = 'src/app/kickstart/testdata/redirect_routes.yaml';

function Probe() {
  useFirstRunRedirect();
  return null;
}

interface RedirectStep {
  status?: unknown;
  statusAnswer?: unknown;
  pages: string[];
  deriveFromRegistry?: boolean;
  redirects: boolean;
  statusReads?: number;
  then?: RedirectStep;
}

async function visit(page: string): Promise<void> {
  nav.pathname = page;
  render(<Probe />);
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
  cleanup();
}

async function play(server: KickstartServer, step: RedirectStep): Promise<void> {
  server.script(STATUS, step.statusAnswer ?? { body: step.status });
  const pages = [...step.pages, ...(step.deriveFromRegistry ? LOCAL_SECTIONS.map((section) => section.href) : [])];
  for (const page of pages) {
    nav.replace.mockClear();
    await visit(page);
    if (step.redirects) {
      expect(nav.replace, `${page} should send an unconfigured install to setup`).toHaveBeenCalledWith('/kickstart');
    } else {
      expect(nav.replace, `${page} should not be redirected`).not.toHaveBeenCalled();
    }
  }
}

beforeEach(() => {
  resetFirstRun();
  nav.replace.mockReset();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('the first-run redirect', () => {
  for (const entry of loadCases(PATH)) {
    if (entry.name === 'settings-rerun-link-prefills') continue;
    it(entry.name, async () => {
      const step = entry as unknown as RedirectStep;
      const server = new KickstartServer();
      server.install();
      await play(server, step);
      if (step.statusReads !== undefined) expect(server.count(STATUS)).toBe(step.statusReads);
      if (step.then) {
        await play(server, step.then);
        // Setup was saved: the status was read again, and it is final from here.
        expect(server.count(STATUS)).toBeGreaterThan(1);
      }
    });
  }
});

describe('the derived set of pages', () => {
  it('is the registry plus the projects page, with no hand-written list', () => {
    const routes = firstRunRoutes();
    for (const section of LOCAL_SECTIONS) expect(routes, `${section.id} is covered`).toContain(section.href);
    expect(routes).toContain('/projects');
    expect(new Set(routes).size).toBe(routes.length);
    // Route-only sections are top-level pages too.
    expect(LOCAL_SECTIONS.some((section) => !section.inNav)).toBe(true);
    for (const section of LOCAL_SECTIONS.filter((candidate) => !candidate.inNav)) expect(isFirstRunRoute(section.href)).toBe(true);
  });

  it('keeps the four exemptions out of the derived set', () => {
    for (const route of firstRunRoutes()) {
      expect(['/kickstart', '/selection', '/share', '/sessions'], `${route} is not an exempt route`).not.toContain(route);
    }
  });
});
