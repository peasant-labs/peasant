'use client';

import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import { usePathname, useRouter } from 'next/navigation';
import { closed } from '@peasant-labs/fairtrade/closed-value';
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
 *
 * The decision is made once, in the app shell, and shared: {@link FirstRunProvider}
 * reads the status and redirects, and a page that must not start anything until
 * the decision is made wraps its body in {@link FirstRunGate}.
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

/** Where the first-run decision stands for the page on screen. */
export const FirstRunDecision = Object.freeze({
  /** A covered page whose status has not been answered. Nothing the page does may start yet. */
  Pending: closed('pending', 'pending'),
  /**
   * The page may open: setup is saved, or the page is not covered, or the status
   * could not be read (a failed read redirects nothing; the offline notice owns that state).
   */
  Open: closed('open', 'open'),
  /** Setup is not saved and the person is being sent to the setup page. */
  Redirecting: closed('redirecting', 'redirecting'),
} as const);
export type FirstRunDecision = (typeof FirstRunDecision)[keyof typeof FirstRunDecision];

/**
 * The decision for the current page. On a covered page it reads
 * `GET /api/v1/kickstart/status` and replaces the location with the setup page
 * when setup has not been saved. Mount it once, through {@link FirstRunProvider}.
 *
 * Once the answer is "configured" it cannot become "not configured" while the
 * shell stays mounted, so it is read no more. A "not configured" answer is read
 * again at the next top-level page, because saving setup changes it.
 */
export function useFirstRunDecision(): FirstRunDecision {
  const pathname = usePathname() ?? '/';
  const router = useRouter();
  // The effect below must not start over when the router object is replaced, so
  // it reads the router through a ref that follows the latest one.
  const latestRouter = useRef(router);
  useEffect(() => {
    latestRouter.current = router;
  });
  const [configured, setConfigured] = useState(false);
  const covered = isFirstRunRoute(pathname);
  const [answer, setAnswer] = useState<{ path: string; decision: FirstRunDecision } | null>(null);
  // An answer belongs to the page it was given for: leaving the page forgets it,
  // so coming back asks again.
  if (answer !== null && answer.path !== pathname) setAnswer(null);

  useEffect(() => {
    if (configured || !covered) return;
    let current = true;
    fetchKickstartStatus().then(
      (status) => {
        if (status.configured) {
          setConfigured(true);
          return;
        }
        if (!current) return;
        setAnswer({ path: pathname, decision: FirstRunDecision.Redirecting });
        latestRouter.current.replace(KICKSTART_ROUTE);
      },
      () => {
        // A failed read is not "not configured". Nothing is redirected, and the page opens.
        if (current) setAnswer({ path: pathname, decision: FirstRunDecision.Open });
      },
    );
    return () => {
      current = false;
    };
  }, [pathname, covered, configured]);

  if (!covered || configured) return FirstRunDecision.Open;
  return answer !== null && answer.path === pathname ? answer.decision : FirstRunDecision.Pending;
}

// A page outside the shell has no decision to wait for it, so it never opens:
// the default is the closed one.
const FirstRunContext = createContext<FirstRunDecision>(FirstRunDecision.Pending);

/**
 * Mount once in the app shell around the pages. It makes the first-run
 * decision and shares it with {@link FirstRunGate}.
 */
export function FirstRunProvider({ children }: { children: ReactNode }) {
  return <FirstRunContext.Provider value={useFirstRunDecision()}>{children}</FirstRunContext.Provider>;
}

/**
 * Draws its children once the first-run decision lets the page open, and
 * nothing before. A page that starts something on mount (the selection page
 * opens a session of its own) wraps its body here, so an install with no setup
 * is sent to setup without that page having started anything.
 */
export function FirstRunGate({ children }: { children: ReactNode }) {
  return useContext(FirstRunContext) === FirstRunDecision.Open ? <>{children}</> : null;
}
