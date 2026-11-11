'use client';

import { useEffect, useRef } from 'react';
import { usePathname, useRouter } from 'next/navigation';
import { fetchKickstartStatus } from '@/lib/kickstart/client';
import { KICKSTART_ROUTE, SELECTION_ROUTE } from '@/lib/kickstart/entry';
import { LOCAL_SECTIONS } from '@/lib/nav/sections';

/**
 * The first-run redirect: while no setup has been saved, every top-level page
 * of the app sends the person to the setup page, and once setup is saved it
 * never does again.
 *
 * Which pages count is derived, not listed. Every registry section this app
 * has a page for (in the nav or reached by route only) is a top-level page, so
 * a section added to the registry redirects with no second edit. `/projects`
 * and the standalone selection page are pages without being sections, and are
 * the two additions. The selection page is covered because it saves into the
 * settings file that setup creates: with no setup saved there is nothing for it
 * to change, so the person is sent to setup first.
 */

/** The page that lists projects. It is a page of the app, not a registry section. */
const PROJECTS_ROUTE = '/projects';

/**
 * Routes that are never redirected, each with everything beneath it: the setup
 * page itself, the publish wizard, and the legacy session links, which are
 * deep links into recorded transcripts.
 */
const EXEMPT_PREFIXES: readonly string[] = [KICKSTART_ROUTE, '/share', '/sessions'];

/** The top-level routes the redirect covers: the registry's, plus the two pages that are not sections. */
export function firstRunRoutes(): readonly string[] {
  return [...new Set([...LOCAL_SECTIONS.map((section) => section.href), PROJECTS_ROUTE, SELECTION_ROUTE])];
}

function normalize(pathname: string): string {
  const trimmed = pathname.replace(/\/+$/, '');
  return trimmed === '' ? '/' : trimmed;
}

function isUnder(path: string, prefix: string): boolean {
  return path === prefix || path.startsWith(`${prefix}/`);
}

/**
 * Whether a page is one the first-run redirect covers. A section covers its own
 * pages (`/review/<project>`); `/` and `/projects` cover only themselves,
 * because beneath `/projects` are project and transcript deep links, which are
 * honoured like `/sessions/*`.
 */
export function isFirstRunRoute(pathname: string): boolean {
  const path = normalize(pathname);
  if (EXEMPT_PREFIXES.some((prefix) => isUnder(path, prefix))) return false;
  if (path === PROJECTS_ROUTE) return true;
  if (isUnder(path, SELECTION_ROUTE)) return true;
  return LOCAL_SECTIONS.some((section) => (section.href === '/' ? path === '/' : isUnder(path, section.href)));
}

/**
 * Mount once in the app shell. On a top-level page it reads
 * `GET /api/v1/kickstart/status` and replaces the location with the setup page
 * when setup has not been saved. A read that fails redirects nothing: the
 * offline notice owns that state.
 *
 * Once the answer is "configured" it cannot become "not configured" while the
 * shell stays mounted, so it is read no more. A "not configured" answer is read
 * again at the next top-level page, because saving setup changes it.
 */
export function useFirstRunRedirect(): void {
  const pathname = usePathname() ?? '/';
  const router = useRouter();
  const configured = useRef(false);
  useEffect(() => {
    if (configured.current || !isFirstRunRoute(pathname)) return;
    let current = true;
    fetchKickstartStatus()
      .then((status) => {
        if (status.configured) {
          configured.current = true;
          return;
        }
        if (current) router.replace(KICKSTART_ROUTE);
      })
      .catch(() => {
        // A failed read is not "not configured". Nothing is redirected.
      });
    return () => {
      current = false;
    };
  }, [pathname, router]);
}
