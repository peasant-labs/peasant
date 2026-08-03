import { GRAPH_APP_SECTIONS } from '@peasant-labs/fairtrade/graph';

/**
 * App navigation sections — the single source of truth for the top nav, and
 * the seam the breadcrumbs and a future Cmd+K command palette read from, so
 * "what sections exist and where they live" is defined once.
 *
 * Graph shell IA: Analytics · Changes · Code map (nav order — from
 * @peasant-labs/fairtrade/graph's GRAPH_APP_SECTIONS, analytics-first). Home
 * (`/`) is still the changes-first project picker regardless of nav order, so
 * Changes owns `/` and stays active across /review/*. Code map owns /map plus
 * /projects/{name}/{id} viewer deep links. Share is a persistent top-nav action
 * outside this graph-section registry and routes to `/share`.
 */

export interface NavSection {
  id: string;
  href: string;
  label: string;
  /** Extra pathname prefixes that keep this section active. */
  activePrefixes: string[];
  /** First-run tour anchor, when the section participates in the tour. */
  tourId?: string;
  /** Hover description (also reusable as a palette hint). */
  title?: string;
}

type GraphSectionId = 'analytics' | 'map' | 'changes';

type GraphShellSection = { id: string; label: string };

const GRAPH_NAV = GRAPH_APP_SECTIONS as readonly GraphShellSection[];

const ROUTES: Record<GraphSectionId, Omit<NavSection, 'id' | 'label'>> = {
  changes: {
    href: '/',
    activePrefixes: ['/review'],
    title: 'Your projects and the lines of work moving through them.',
  },
  map: {
    href: '/map',
    activePrefixes: ['/map', '/projects'],
    title: 'See a project as a map of its code areas and how they connect.',
  },
  analytics: {
    href: '/analytics',
    activePrefixes: ['/analytics'],
    title: 'Read project-level session volume, outcomes, duration, and contributor signals.',
  },
};

function assertKnownGraphSection(section: GraphShellSection): asserts section is GraphShellSection & { id: GraphSectionId } {
  if (!(section.id in ROUTES)) {
    throw new Error(
      `Unknown graph shell section "${section.id}" from @peasant-labs/fairtrade/graph in web/src/lib/nav/sections.ts. ` +
      `Update the explicit route metadata for analytics, map, and changes before exposing a new section.`,
    );
  }
}

export const NAV_SECTIONS: NavSection[] = GRAPH_NAV.map((section) => {
  assertKnownGraphSection(section);
  const route = ROUTES[section.id];
  return { ...route, id: section.id, label: section.label };
});

/**
 * Whether `pathname` is within a section. Changes owns `/` exactly (plus its
 * activePrefixes); the others match their href prefix plus any extra prefixes.
 */
export function isSectionActive(section: NavSection, pathname: string): boolean {
  const base =
    section.href === '/' ? pathname === '/' : pathname.startsWith(section.href);
  return base || section.activePrefixes.some((p) => pathname.startsWith(p));
}
