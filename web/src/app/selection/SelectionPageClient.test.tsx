import { existsSync } from 'node:fs';
import { resolve } from 'node:path';
import type { ReactNode } from 'react';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import SettingsPageClient from '@/app/settings/SettingsPageClient';
import { WizardClient } from '@/app/kickstart/WizardClient';
import { FirstRunProvider, firstRunRoutes, isFirstRunRoute } from '@/hooks/useFirstRun';
import { kickstartCopy } from '@/lib/kickstart/copy';
import { ALL_KICKSTART_SCOPES, KickstartScope } from '@/lib/kickstart/values';
import { LOCAL_SECTIONS } from '@/lib/nav/sections';
import { loadCases, type FixtureCase } from '@/test/fixtures/kickstart';
import { loadSettingsResponse } from '@/test/fixtures/settings';
import { playCase, playTabs, teardown } from '@/test/kickstartHarness';
import SelectionPage from './page';

// One router for the life of the shell, as the app's router is; `replace` is
// where a redirect to setup shows.
const nav = vi.hoisted(() => {
  const replace = vi.fn();
  return { replace, router: { push: vi.fn(), replace } };
});
vi.mock('next/navigation', () => ({
  usePathname: () => '/selection',
  useSearchParams: () => new URLSearchParams(),
  useRouter: () => nav.router,
}));

/**
 * The standalone selection page, mounted through its route entry (`page.tsx`)
 * with the HTTP and WebSocket boundary played from fixtures. The wizard is
 * mounted beside it where a case needs it, from its own route entry too: the
 * isolation these cases prove is between two real pages.
 *
 * The page is drawn inside the shell's first-run decision, as it is in the app:
 * its body waits for the status answer. Every case but the gate's own is played
 * on a computer whose setup is saved.
 */

afterEach(() => {
  teardown();
  nav.replace.mockReset();
});

const STATUS = 'GET /api/v1/kickstart/status';

/** The shell's first-run decision around a page, the production provider. */
function Shell({ children }: { children: ReactNode }) {
  return <FirstRunProvider>{children}</FirstRunProvider>;
}

/** A case on a computer whose setup is saved, unless the case scripts the status itself. */
function onSavedSetup(entry: FixtureCase): FixtureCase {
  const server = (entry.server ?? {}) as Record<string, unknown>;
  return { ...entry, server: { [STATUS]: { body: { configured: true } }, ...server } };
}

const PATH = 'src/app/selection/testdata/selection_page.yaml';

/** What the fixture says about how a case is played. */
const CASE_KINDS = ['page', 'tabs', 'parity', 'settings', 'copy', 'gate'] as const;
type CaseKind = (typeof CASE_KINDS)[number];

function kindOf(entry: FixtureCase): CaseKind {
  const kind = CASE_KINDS.find((candidate) => candidate === entry.kind);
  if (kind === undefined) {
    throw new Error(`${PATH}: case ${entry.name} has kind ${JSON.stringify(entry.kind)}; it must be one of ${CASE_KINDS.join(', ')}`);
  }
  return kind;
}

const PAGES = {
  wizard: <WizardClient />,
  selection: <Shell><SelectionPage /></Shell>,
  first: <Shell><SelectionPage /></Shell>,
  second: <Shell><SelectionPage /></Shell>,
} as const;

/** The `{name}` placeholders of a parity case, filled in for one page. */
function fill(entry: FixtureCase, values: Record<string, string>): FixtureCase {
  const text = JSON.stringify(entry).replace(/\{(session|model|answered)\}/g, (_, key: string) => values[key]);
  return JSON.parse(text) as FixtureCase;
}

describe('/selection', () => {
  for (const entry of loadCases(PATH)) {
    const kind = kindOf(entry);
    if (kind === 'page') {
      it(entry.name, async () => {
        await playCase(onSavedSetup(entry), <Shell><SelectionPage /></Shell>, ['kind', 'scopeToken']);
        if (typeof entry.scopeToken === 'string') {
          // The token the page opens its session with is the closed set's own.
          expect(KickstartScope.Selection.wire).toBe(entry.scopeToken);
        }
      });
    }
    if (kind === 'tabs') {
      it(entry.name, async () => {
        await playTabs(onSavedSetup(entry), PAGES, ['kind']);
      });
    }
    if (kind === 'parity') {
      it(entry.name, async () => {
        const sides = entry as unknown as {
          rows: string[];
          selectedIds: string[];
          wizard: { session: string; model: string; answered: string; before: unknown[] };
          selection: { session: string; model: string; answered: string; before: unknown[] };
        };
        const seen: Record<'wizard' | 'selection', { rows: (string | null)[]; selected: unknown[] }> = {
          wizard: { rows: [], selected: [] },
          selection: { rows: [], selected: [] },
        };
        for (const side of ['wizard', 'selection'] as const) {
          const values = sides[side];
          const played = fill(entry, { session: values.session, model: values.model, answered: values.answered });
          const steps = [...values.before, ...(played.steps as unknown[])];
          const mounted = await playCase(onSavedSetup({ ...played, steps }), side === 'wizard' ? <WizardClient /> : <Shell><SelectionPage /></Shell>, ['kind', 'rows', 'selectedIds', 'wizard', 'selection']);
          seen[side] = {
            rows: [...mounted.container.querySelectorAll('[data-node-id]')].map((row) => row.getAttribute('data-node-id')),
            selected: mounted.server
              .bodies(`PATCH /api/v1/kickstart/sessions/${values.session}`)
              .map((body) => (body as { selectedIds?: unknown }).selectedIds),
          };
          teardown();
        }
        // The same rows, in the same order, and the same ids sent for the same
        // actions: the page is the wizard's selection step, not a second tree.
        expect(seen.selection.rows).toEqual(sides.rows);
        expect(seen.wizard.rows).toEqual(seen.selection.rows);
        expect(seen.wizard.selected).toEqual([sides.selectedIds]);
        expect(seen.selection.selected).toEqual(seen.wizard.selected);
      });
    }
    if (kind === 'gate') {
      it(entry.name, async () => {
        // The gate's own cases script the status themselves: what the shell is
        // told decides whether the page draws, and the request log says what
        // the page started before that.
        // What the case says about the redirect is a step of the case (it waits on the
        // router's own call); this only checks that the case says it either way.
        const redirects = (entry as unknown as { redirects: boolean }).redirects;
        const steps = entry.steps as { expectRedirect?: string; expectNoRedirect?: true }[];
        expect(
          steps.some((step) => (redirects ? step.expectRedirect === '/kickstart' : step.expectNoRedirect === true)),
          `a gate case that ${redirects ? 'redirects' : 'does not redirect'} says so in a step`,
        ).toBe(true);
        await playCase(entry, <Shell><SelectionPage /></Shell>, ['kind', 'redirects'], { redirect: nav.replace });
      });
    }
    if (kind === 'copy') {
      it(entry.name, () => {
        const scopes = (entry as unknown as { scopes: Record<string, Record<string, string>> }).scopes;
        // The fixture names every scope the module lists, and no other.
        expect(Object.keys(scopes).sort()).toEqual(ALL_KICKSTART_SCOPES.map((scope) => scope.wire).sort());
        const seen = new Set<string>();
        for (const scope of ALL_KICKSTART_SCOPES) {
          const want = scopes[scope.wire];
          const copy = kickstartCopy(scope);
          const got = {
            opening: copy.opening,
            openFailed: copy.openFailed,
            refusedOne: copy.refused(1),
            refusedMany: copy.refused(2),
            expiredTitle: copy.expired.title,
            expiredBody: copy.expired.body,
            reopen: copy.expired.reopen,
            conflictsTitle: copy.conflicts.title,
            conflictsHelp: copy.conflicts.help,
            saved: copy.saved,
          };
          expect(got, `the words of the ${scope.wire} scope`).toEqual(want);
          for (const [name, text] of Object.entries(got)) {
            expect(text, `${scope.wire} ${name} is lowercase chrome`).toBe(text.toLowerCase());
            expect(seen.has(text), `${scope.wire} ${name} repeats another scope's words`).toBe(false);
            seen.add(text);
          }
        }
      });
    }
    if (kind === 'settings') {
      it(entry.name, async () => {
        const { link, coveredPaths } = entry as unknown as { link: { name: string; href: string }; coveredPaths: string[] };
        vi.stubGlobal('fetch', async (input: RequestInfo | URL) => {
          const path = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, 'http://localhost').pathname;
          const body = path === '/api/v1/settings' ? loadSettingsResponse() : { authenticated: false };
          return new Response(JSON.stringify(body), { status: 200 });
        });
        render(<SettingsPageClient section="projects" />);
        await waitFor(() => expect(screen.getByRole('link', { name: link.name })).toBeInTheDocument());
        const href = screen.getByRole('link', { name: link.name }).getAttribute('href');
        expect(href).toBe(link.href);
        cleanup();

        // The link lands on a route this app has: a page, not a dead link.
        expect(existsSync(resolve(process.cwd(), 'src/app', (href ?? '').replace(/^\//, ''), 'page.tsx'))).toBe(true);
        // It is outside the section registry, so no sidebar item or palette
        // entry stands for it, and inside the first-run redirect: it saves
        // into the settings file that setup creates.
        expect(LOCAL_SECTIONS.map((section) => section.href)).not.toContain(href);
        expect(firstRunRoutes()).toContain(href);
        for (const path of coveredPaths) expect(isFirstRunRoute(path), `${path} sends an unconfigured install to setup`).toBe(true);
      });
    }
  }
});
